package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"mime/quotedprintable"
	"net/http"
	"net/mail"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/html"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
	"google.golang.org/api/gmail/v1"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/option"
)

var (
	gmailSvc   *gmail.Service
	gmailMu    sync.RWMutex
	oauthConf  *oauth2.Config
	userEmail  string
)

func initGmail(ctx context.Context) {
	var conf *oauth2.Config

	if config.Gmail.ClientID != "" && config.Gmail.ClientSecret != "" {
		redirectURL := strings.TrimSuffix(config.Gmail.PublicURL, "/") + "/auth/callback"
		conf = &oauth2.Config{
			ClientID:     config.Gmail.ClientID,
			ClientSecret: config.Gmail.ClientSecret,
			RedirectURL:  redirectURL,
			Scopes:       []string{gmail.GmailReadonlyScope, gmail.GmailModifyScope},
			Endpoint:     google.Endpoint,
		}
	} else {
		b, err := os.ReadFile(config.Gmail.CredentialsFile)
		if err != nil {
			log.Printf("INFO: Missing OAuth configuration. Please visit %s to set up Gmail access.", config.Gmail.PublicURL)
			return
		}
		conf, err = google.ConfigFromJSON(b, gmail.GmailReadonlyScope, gmail.GmailModifyScope)
		if err != nil {
			log.Printf("CRITICAL: Unable to parse credentials: %v", err)
			return
		}
	}
	
	oauthConf = conf

	tok, err := tokenFromFile(config.Gmail.TokenFile)
	if err != nil {
		log.Printf("AUTH REQUIRED: No token found. Please visit %s to authorize.", config.Gmail.PublicURL)
		return
	}

	svc, err := gmail.NewService(ctx, option.WithHTTPClient(oauthConf.Client(ctx, tok)))
	if err != nil {
		log.Printf("Error creating Gmail service: %v", err)
		return
	}

	profile, err := svc.Users.GetProfile("me").Do()
	if err == nil {
		userEmail = profile.EmailAddress
		log.Printf("Authenticated as: %s", userEmail)
	}

	gmailMu.Lock()
	gmailSvc = svc
	gmailMu.Unlock()
}

func tokenFromFile(file string) (*oauth2.Token, error) {
	f, err := os.Open(file)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	tok := &oauth2.Token{}
	err = json.NewDecoder(f).Decode(tok)
	return tok, err
}

func saveToken(path string, token *oauth2.Token) {
	log.Printf("Saving token to: %s", path)
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0600)
	if err != nil {
		log.Printf("Unable to save oauth token: %v", err)
		return
	}
	defer f.Close()
	json.NewEncoder(f).Encode(token)
}

func withRetry[T any](fn func() (T, error)) (T, error) {
	for i := 0; i < 5; i++ {
		res, err := fn()
		if err == nil {
			return res, nil
		}

		if gerr, ok := err.(*googleapi.Error); ok && gerr.Code == 429 {
			retryAfter := gerr.Header.Get("Retry-After")
			waitDur := 2 * time.Second // Default fallback
			if retryAfter != "" {
				if d, err := strconv.Atoi(retryAfter); err == nil {
					waitDur = time.Duration(d) * time.Second
				} else if t, err := http.ParseTime(retryAfter); err == nil {
					waitDur = time.Until(t)
				}
			}
			log.Printf("Rate limited (429). Waiting %v per Retry-After header...", waitDur)
			time.Sleep(waitDur)
			continue
		}
		return res, err
	}
	return fn()
}

func pollGmail(ctx context.Context) {
	ticker := time.NewTicker(time.Duration(config.Gmail.PollingInterval) * time.Second)
	defer ticker.Stop()

	for {
		gmailMu.RLock()
		svc := gmailSvc
		gmailMu.RUnlock()

		if svc != nil {
			fetchMessages(ctx, svc)
		} else {
			log.Printf("Waiting for authentication...")
		}

		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

type job struct {
	index int
	msg   *gmail.Message
}

func fetchMessages(ctx context.Context, srv *gmail.Service) {
	log.Printf("Fetching messages...")

	query := ""
	if config.Gmail.Label != "" {
		query = fmt.Sprintf("label:%s", config.Gmail.Label)
	}

	pageToken := ""
	pageNum := 1
	newItems := false
	processedCount := 0
	const maxToProcess = 500

	for {
		r, err := withRetry(func() (*gmail.ListMessagesResponse, error) {
			return srv.Users.Messages.List("me").Q(query).PageToken(pageToken).Do()
		})
		if err != nil {
			log.Printf("Error listing messages: %v", err)
			return
		}

		if len(r.Messages) == 0 {
			log.Printf("No messages found.")
			break
		}

		log.Printf("Page %d: Found %d messages", pageNum, len(r.Messages))

		jobs := make(chan job, len(r.Messages))
		var wg sync.WaitGroup

		numWorkers := 10
		for w := 0; w < numWorkers; w++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for j := range jobs {
					m := j.msg
					cache.mu.RLock()
					item, exists := cache.Items[m.Id]
					cache.mu.RUnlock()

					// 1. Check for read status updates if fully processed.
					// Optimization: If it's already read in cache, don't waste API quota checking it again.
					if exists && item.Sender != "" && item.Body != "" {
						if item.IsRead {
							continue
						}

						msgMetadata, err := withRetry(func() (*gmail.Message, error) {
							return srv.Users.Messages.Get("me", m.Id).Format("metadata").Do()
						})
						if err == nil {
							isUnread := false
							for _, lbl := range msgMetadata.LabelIds {
								if lbl == "UNREAD" {
									isUnread = true
									break
								}
							}
							cache.mu.Lock()
							item.IsRead = !isUnread
							cache.mu.Unlock()
						}
						continue
					}

					// 2. Fetch metadata to check sender
					metadata, err := withRetry(func() (*gmail.Message, error) {
						return srv.Users.Messages.Get("me", m.Id).Format("metadata").MetadataHeaders("From", "Subject").Do()
					})
					if err != nil {
						continue
					}

					from := ""
					subject := ""
					for _, h := range metadata.Payload.Headers {
						if h.Name == "From" {
							from = h.Value
						} else if h.Name == "Subject" {
							subject = h.Value
						}
					}
					_, email := parseFrom(from)

					// 3. Filters
					if userEmail != "" && strings.EqualFold(email, userEmail) {
						if !exists {
							cache.GetOrCreateItem(m.Id)
						}
						continue
					}

					lowerSub := strings.ToLower(subject)
					if strings.HasPrefix(lowerSub, "re:") || strings.HasPrefix(lowerSub, "fwd:") {
						if !exists {
							cache.GetOrCreateItem(m.Id)
						}
						continue
					}

					cache.mu.RLock()
					excluded := cache.ExcludedSenders[email]
					cache.mu.RUnlock()
					if excluded {
						if !exists {
							cache.GetOrCreateItem(m.Id)
						}
						continue
					}

					// 4. Fetch full content
					fullMsg, err := withRetry(func() (*gmail.Message, error) {
						return srv.Users.Messages.Get("me", m.Id).Format("full").Do()
					})
					if err != nil {
						continue
					}

					log.Printf("[%d/%d] PROCESSED: %s", j.index+1, len(r.Messages), subject)
					processMessage(srv, fullMsg)
					newItems = true
				}
			}()
		}

		for i, m := range r.Messages {
			jobs <- job{index: i, msg: m}
		}
		close(jobs)
		wg.Wait()

		processedCount += len(r.Messages)
		if processedCount >= maxToProcess {
			log.Printf("Reached limit of %d messages. Stopping poll.", maxToProcess)
			break
		}

		pageToken = r.NextPageToken
		if pageToken == "" {
			log.Printf("Finished processing all pages.")
			break
		}
		pageNum++
	}

	if newItems {
		cache.Save()
	}
}

func processMessage(srv *gmail.Service, msg *gmail.Message) {
	item := cache.GetOrCreateItem(msg.Id)
	item.ThreadID = msg.ThreadId
	item.Timestamp = time.Unix(msg.InternalDate/1000, 0)
	item.Snippet = msg.Snippet
	item.CidMap = make(map[string]string)

	isUnread := false
	for _, lbl := range msg.LabelIds {
		if lbl == "UNREAD" {
			isUnread = true
			break
		}
	}
	item.IsRead = !isUnread

	for _, h := range msg.Payload.Headers {
		switch h.Name {
		case "Subject":
			item.Subject = h.Value
		case "From":
			item.SenderName, item.Sender = parseFrom(h.Value)
			cache.AddSubscription(item.Sender, item.SenderName)
		}
	}

	// Extract body - decoding only, no further processing
	body := extractBody(srv, msg.Id, msg.Payload)
	// Surgically strip only this specific tag
	item.Body = strings.ReplaceAll(body, "<o:PixelsPerInch>96</o:PixelsPerInch>", "")
	item.CleanBody = cleanHTML(item.Body)
}

func cleanHTML(input string) string {
	doc, err := html.Parse(strings.NewReader(input))
	if err != nil {
		return input
	}

	var buf strings.Builder
	var f func(*html.Node)
	f = func(n *html.Node) {
		if n.Type == html.ElementNode {
			tag := strings.ToLower(n.Data)
			if tag == "style" || tag == "script" || tag == "head" || tag == "meta" || tag == "link" {
				return
			}

			allowed := false
			switch tag {
			case "p", "br", "h1", "h2", "h3", "h4", "h5", "h6", "b", "i", "strong", "em", "ul", "ol", "li", "a", "img", "blockquote", "code", "pre", "hr":
				allowed = true
			}

			// Logical Filtering: Skip common footer noise
			if tag == "a" {
				var linkText strings.Builder
				var findText func(*html.Node)
				findText = func(m *html.Node) {
					if m.Type == html.TextNode {
						linkText.WriteString(m.Data)
					}
					for c := m.FirstChild; c != nil; c = c.NextSibling {
						findText(c)
					}
				}
				findText(n)
				txt := strings.ToLower(linkText.String())
				if strings.Contains(txt, "unsubscribe") || strings.Contains(txt, "view in browser") || strings.Contains(txt, "update preferences") {
					return
				}
			}

			if allowed {
				buf.WriteString("<")
				buf.WriteString(n.Data)
				for _, a := range n.Attr {
					key := strings.ToLower(a.Key)
					if key == "href" || key == "src" {
						// Skip cid: images as they won't render in RSS readers
						if key == "src" && strings.HasPrefix(strings.ToLower(a.Val), "cid:") {
							continue
						}
						buf.WriteString(fmt.Sprintf(" %s=\"%s\"", a.Key, html.EscapeString(a.Val)))
					} else if key == "alt" || key == "title" {
						buf.WriteString(fmt.Sprintf(" %s=\"%s\"", a.Key, html.EscapeString(a.Val)))
					}
				}
				buf.WriteString(">")
			}

			for c := n.FirstChild; c != nil; c = c.NextSibling {
				f(c)
			}

			if allowed && n.Data != "br" && n.Data != "img" && n.Data != "hr" {
				buf.WriteString("</")
				buf.WriteString(n.Data)
				buf.WriteString(">")
			}
		} else if n.Type == html.TextNode {
			// Collapse excessive whitespace common in newsletter HTML
			text := n.Data
			if strings.TrimSpace(text) == "" {
				if strings.Contains(text, "\n") {
					buf.WriteString("\n")
				} else {
					buf.WriteString(" ")
				}
			} else {
				buf.WriteString(html.EscapeString(text))
			}
		} else {
			for c := n.FirstChild; c != nil; c = c.NextSibling {
				f(c)
			}
		}
	}
	f(doc)

	// Final cleanup: remove triple newlines and trim
	res := buf.String()
	for strings.Contains(res, "\n\n\n") {
		res = strings.ReplaceAll(res, "\n\n\n", "\n\n")
	}
	return strings.TrimSpace(res)
}

func parseFrom(from string) (name, email string) {
	addr, err := mail.ParseAddress(from)
	if err != nil {
		if !strings.Contains(from, "<") {
			name = strings.Trim(from, "\" '“”")
			return name, from
		}
		parts := strings.Split(from, "<")
		name = strings.TrimSpace(parts[0])
		name = strings.Trim(name, "\" '“”")
		email = strings.Trim(parts[1], "> ")
		return name, email
	}

	name = addr.Name
	email = addr.Address
	if name == "" {
		name = email
	}

	name = strings.Trim(name, "\" '“”")
	log.Printf("PARSED FROM: Raw='%s' -> Name='%s' Email='%s'", from, name, email)
	return name, email
}

func extractBody(srv *gmail.Service, msgID string, part *gmail.MessagePart) string {
	// 1. Check if this part has data (either inline or as an attachment)
	var rawData string
	if part.Body.Data != "" {
		rawData = part.Body.Data
	} else if part.Body.AttachmentId != "" {
		// Large newsletters are often stored as attachments
		attach, err := srv.Users.Messages.Attachments.Get("me", msgID, part.Body.AttachmentId).Do()
		if err == nil {
			rawData = attach.Data
		}
	}

	if rawData != "" {
		data, err := base64.RawURLEncoding.DecodeString(rawData)
		if err != nil {
			data, err = base64.URLEncoding.DecodeString(rawData)
		}
		
		if err == nil {
			content := string(data)
			// Gmail API returns the raw bytes of the part. If the original email was
			// quoted-printable, these bytes still contain QP encoding.
			for _, h := range part.Headers {
				if strings.EqualFold(h.Name, "Content-Transfer-Encoding") && strings.EqualFold(h.Value, "quoted-printable") {
					decoded, err := io.ReadAll(quotedprintable.NewReader(bytes.NewReader(data)))
					if err == nil {
						content = string(decoded)
					}
					break
				}
			}
			return content
		}
	}

	// 2. If it's a multipart, prioritize HTML
	if strings.HasPrefix(strings.ToLower(part.MimeType), "multipart/") {
		var htmlBody, plainBody string
		for _, subPart := range part.Parts {
			body := extractBody(srv, msgID, subPart)
			if strings.EqualFold(subPart.MimeType, "text/html") {
				if body != "" {
					htmlBody = body
				}
			} else if strings.EqualFold(subPart.MimeType, "text/plain") {
				if body != "" && plainBody == "" {
					plainBody = body
				}
			} else if strings.HasPrefix(strings.ToLower(subPart.MimeType), "multipart/") && body != "" {
				if htmlBody == "" {
					htmlBody = body
				}
			}
		}
		if htmlBody != "" {
			return htmlBody
		}
		return plainBody
	}

	return ""
}
