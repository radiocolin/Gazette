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

		shouldRetry := false
		waitDur := time.Duration(1<<i) * time.Second

		if gerr, ok := err.(*googleapi.Error); ok {
			if gerr.Code == 429 {
				shouldRetry = true
				retryAfter := gerr.Header.Get("Retry-After")
				if retryAfter != "" {
					if d, err := strconv.Atoi(retryAfter); err == nil {
						waitDur = time.Duration(d) * time.Second
					} else if t, err := http.ParseTime(retryAfter); err == nil {
						waitDur = time.Until(t)
					}
				}
			} else if gerr.Code >= 500 {
				shouldRetry = true
			}
		}

		if shouldRetry {
			log.Printf("API Error (attempt %d/5): %v. Retrying in %v...", i+1, err, waitDur)
			time.Sleep(waitDur)
			continue
		}
		return res, err
	}
	return fn()
}

func pollGmail(ctx context.Context) {
	interval := time.Duration(config.Gmail.PollingInterval) * time.Second

	var wg sync.WaitGroup
	wg.Add(2)

	// Goroutine 1: fetch new messages at every polling interval
	go func() {
		defer wg.Done()

		gmailMu.RLock()
		svc := gmailSvc
		gmailMu.RUnlock()
		if svc != nil {
			fetchNewMessages(ctx, svc)
		} else {
			log.Printf("Waiting for authentication...")
		}

		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				gmailMu.RLock()
				svc := gmailSvc
				gmailMu.RUnlock()
				if svc != nil {
					fetchNewMessages(ctx, svc)
				} else {
					log.Printf("Waiting for authentication...")
				}
			}
		}
	}()

	// Goroutine 2: sync read status every 10x polling interval (also runs immediately)
	go func() {
		defer wg.Done()

		gmailMu.RLock()
		svc := gmailSvc
		gmailMu.RUnlock()
		if svc != nil {
			syncReadStatus(svc)
		}

		ticker := time.NewTicker(interval * 10)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				gmailMu.RLock()
				svc := gmailSvc
				gmailMu.RUnlock()
				if svc != nil {
					syncReadStatus(svc)
				}
			}
		}
	}()

	wg.Wait()
}

type job struct {
	index int
	msg   *gmail.Message
}

func fetchNewMessages(ctx context.Context, svc *gmail.Service) {
	cache.mu.RLock()
	histID := cache.HistoryID
	cache.mu.RUnlock()

	if histID == 0 {
		log.Printf("First run: performing full message sync")
		fullMessageSync(svc)
		profile, err := withRetry(func() (*gmail.Profile, error) {
			return svc.Users.GetProfile("me").Do()
		})
		if err != nil {
			log.Printf("Error getting profile for history ID: %v", err)
			return
		}
		cache.mu.Lock()
		cache.HistoryID = profile.HistoryId
		cache.mu.Unlock()
		cache.Save()
		log.Printf("Stored history ID: %d", profile.HistoryId)
	} else {
		ok := incrementalSync(svc)
		if !ok {
			log.Printf("Falling back to full message sync")
			fullMessageSync(svc)
			profile, err := withRetry(func() (*gmail.Profile, error) {
				return svc.Users.GetProfile("me").Do()
			})
			if err != nil {
				log.Printf("Error getting profile for history ID: %v", err)
				return
			}
			cache.mu.Lock()
			cache.HistoryID = profile.HistoryId
			cache.mu.Unlock()
			cache.Save()
			log.Printf("Stored history ID: %d", profile.HistoryId)
		}
	}
}

// incrementalSync fetches history since cache.HistoryID.
// Returns false if history has expired and a full sync is needed.
func incrementalSync(svc *gmail.Service) bool {
	cache.mu.RLock()
	histID := cache.HistoryID
	cache.mu.RUnlock()

	log.Printf("Incremental sync from history ID %d", histID)

	newItems := false
	changed := false
	pageToken := ""
	var latestHistoryID uint64

	for {
		req := svc.Users.History.List("me").
			StartHistoryId(histID).
			HistoryTypes("messageAdded", "labelAdded", "labelRemoved").
			MaxResults(500)
		if pageToken != "" {
			req = req.PageToken(pageToken)
		}

		resp, err := withRetry(func() (*gmail.ListHistoryResponse, error) {
			return req.Do()
		})
		if err != nil {
			if gerr, ok := err.(*googleapi.Error); ok && gerr.Code == 404 {
				log.Printf("History expired (404), falling back to full sync")
				return false
			}
			log.Printf("Error fetching history: %v", err)
			return true
		}

		if resp.HistoryId > latestHistoryID {
			latestHistoryID = resp.HistoryId
		}

		log.Printf("Incremental sync: %d history records on this page", len(resp.History))

		for _, record := range resp.History {
			for _, added := range record.MessagesAdded {
				msgID := added.Message.Id
				cache.mu.RLock()
				item, exists := cache.Items[msgID]
				fullyProcessed := exists && item.Body != ""
				cache.mu.RUnlock()

				if fullyProcessed {
					continue
				}

				fullMsg, err := withRetry(func() (*gmail.Message, error) {
					return svc.Users.Messages.Get("me", msgID).Format("full").Do()
				})
				if err != nil {
					log.Printf("Error fetching message %s: %v", msgID, err)
					continue
				}
				processMessage(svc, fullMsg)
				newItems = true
			}

			for _, removed := range record.LabelsRemoved {
				for _, lbl := range removed.LabelIds {
					if lbl == "UNREAD" {
						msgID := removed.Message.Id
						cache.mu.Lock()
						item, exists := cache.Items[msgID]
						if !exists {
							log.Printf("READ [history]: UNREAD removed for %s — not in cache", msgID)
						} else if item.Body == "" {
							log.Printf("READ [history]: UNREAD removed for %s — not fully processed", msgID)
						} else if item.IsRead {
							log.Printf("READ [history]: UNREAD removed for %s (%s) — already read", msgID, item.Subject)
						} else {
							item.IsRead = true
							changed = true
							log.Printf("READ [history]: Marked read: %s (%s)", msgID, item.Subject)
						}
						cache.mu.Unlock()
					}
				}
			}

			for _, added := range record.LabelsAdded {
				for _, lbl := range added.LabelIds {
					if lbl == "UNREAD" {
						msgID := added.Message.Id
						cache.mu.Lock()
						item, exists := cache.Items[msgID]
						if !exists {
							log.Printf("READ [history]: UNREAD added for %s — not in cache", msgID)
						} else if item.Body == "" {
							log.Printf("READ [history]: UNREAD added for %s — not fully processed", msgID)
						} else if !item.IsRead {
							log.Printf("READ [history]: UNREAD added for %s (%s) — already unread", msgID, item.Subject)
						} else {
							item.IsRead = false
							changed = true
							log.Printf("READ [history]: Marked unread: %s (%s)", msgID, item.Subject)
						}
						cache.mu.Unlock()
					}
				}
			}
		}

		pageToken = resp.NextPageToken
		if pageToken == "" {
			break
		}
	}

	if latestHistoryID > 0 {
		cache.mu.Lock()
		cache.HistoryID = latestHistoryID
		cache.mu.Unlock()
	}

	if newItems || changed {
		cache.Save()
	}

	return true
}

func fullMessageSync(svc *gmail.Service) {
	log.Printf("Full message sync...")

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
		req := svc.Users.Messages.List("me").MaxResults(500)
		if query != "" {
			req = req.Q(query)
		}
		if pageToken != "" {
			req = req.PageToken(pageToken)
		}

		r, err := withRetry(func() (*gmail.ListMessagesResponse, error) {
			return req.Do()
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

		for w := 0; w < 10; w++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for j := range jobs {
					m := j.msg
					cache.mu.RLock()
					item, exists := cache.Items[m.Id]
					fullyProcessed := exists && item.Sender != "" && item.Body != ""
					cache.mu.RUnlock()

					if fullyProcessed {
						continue
					}

					metadata, err := withRetry(func() (*gmail.Message, error) {
						return svc.Users.Messages.Get("me", m.Id).Format("metadata").MetadataHeaders("From", "Subject").Do()
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

					fullMsg, err := withRetry(func() (*gmail.Message, error) {
						return svc.Users.Messages.Get("me", m.Id).Format("full").Do()
					})
					if err != nil {
						continue
					}

					log.Printf("[%d/%d] PROCESSED: %s", j.index+1, len(r.Messages), subject)
					processMessage(svc, fullMsg)
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
			log.Printf("Reached limit of %d messages. Stopping.", maxToProcess)
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

func syncReadStatus(svc *gmail.Service) {
	log.Printf("Syncing read status...")

	query := ""
	if config.Gmail.Label != "" {
		query = fmt.Sprintf("label:%s", config.Gmail.Label)
	}

	unreadIDs := make(map[string]bool)
	pageToken := ""

	for {
		req := svc.Users.Messages.List("me").LabelIds("UNREAD").MaxResults(500)
		if query != "" {
			req = req.Q(query)
		}
		if pageToken != "" {
			req = req.PageToken(pageToken)
		}

		r, err := withRetry(func() (*gmail.ListMessagesResponse, error) {
			return req.Do()
		})
		if err != nil {
			log.Printf("Error listing unread messages: %v", err)
			return
		}

		for _, m := range r.Messages {
			unreadIDs[m.Id] = true
		}

		pageToken = r.NextPageToken
		if pageToken == "" {
			break
		}
	}

	log.Printf("Found %d unread messages in Gmail", len(unreadIDs))

	changed := false
	cache.mu.Lock()
	for _, item := range cache.Items {
		if item.Body == "" {
			continue
		}
		inUnreadSet := unreadIDs[item.ID]
		if !item.IsRead && !inUnreadSet {
			log.Printf("READ [full sync]: Marking read: %s (%s - %s)", item.ID, item.Sender, item.Subject)
			item.IsRead = true
			changed = true
		} else if item.IsRead && inUnreadSet {
			log.Printf("READ [full sync]: Marking unread: %s (%s - %s)", item.ID, item.Sender, item.Subject)
			item.IsRead = false
			changed = true
		}
	}
	cache.mu.Unlock()

	if changed {
		cache.Save()
		log.Printf("Read status synced, cache updated")
	} else {
		log.Printf("Read status in sync, no changes needed")
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
