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
	"net/mail"
	"os"
	"strings"
	"sync"
	"time"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
	"google.golang.org/api/gmail/v1"
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

func fetchMessages(ctx context.Context, srv *gmail.Service) {
	query := fmt.Sprintf("label:%s", config.Gmail.Label)
	newItems := false

	pageToken := ""
	for {
		call := srv.Users.Messages.List("me").Q(query).MaxResults(500)
		if pageToken != "" {
			call.PageToken(pageToken)
		}
		r, err := call.Do()
		if err != nil {
			log.Printf("Unable to retrieve messages: %v", err)
			return
		}

		for _, m := range r.Messages {
			cache.mu.RLock()
			item, exists := cache.Items[m.Id]
			cache.mu.RUnlock()

			// 1. If it already exists and is fully processed, just check for read status updates
			if exists && item.Sender != "" && item.Body != "" {
				msgMetadata, err := srv.Users.Messages.Get("me", m.Id).Format("metadata").Do()
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

			// 2. Fetch metadata to check sender and if it's a valid newsletter
			metadata, err := srv.Users.Messages.Get("me", m.Id).Format("metadata").MetadataHeaders("From", "Subject").Do()
			if err != nil {
				log.Printf("Unable to retrieve message metadata %v: %v", m.Id, err)
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

			// 3. SKIP if the sender is the user themselves (Filter replies/forwards)
			if userEmail != "" && strings.EqualFold(email, userEmail) {
				if !exists {
					cache.GetOrCreateItem(m.Id)
				}
				continue
			}

			// 4. SKIP if it's clearly a human reply or forward (Subject starts with Re: or Fwd:)
			lowerSub := strings.ToLower(subject)
			if strings.HasPrefix(lowerSub, "re:") || strings.HasPrefix(lowerSub, "fwd:") {
				if !exists {
					cache.GetOrCreateItem(m.Id)
				}
				continue
			}

			// 5. Check if sender is excluded
			cache.mu.RLock()
			excluded := cache.ExcludedSenders[email]
			cache.mu.RUnlock()
			if excluded {
				if !exists {
					cache.GetOrCreateItem(m.Id)
				}
				continue
			}

			// 5. Process the message
			fullMsg, err := srv.Users.Messages.Get("me", m.Id).Format("full").Do()
			if err != nil {
				log.Printf("Unable to retrieve message %v: %v", m.Id, err)
				continue
			}

			log.Printf("PROCESSING NEWSLETTER: From='%s' Subject='%s' Msg=%s", from, fullMsg.Snippet, m.Id)
			processMessage(srv, fullMsg)
			newItems = true
		}

		pageToken = r.NextPageToken
		if pageToken == "" {
			break
		}
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

	// Extract body and collect CIDs
	item.Body = extractBody(msg.Payload)
	collectCids(srv, msg.Id, msg.Payload, item.CidMap)

	// Replace CIDs in body with data URIs
	for cid, dataURI := range item.CidMap {
		item.Body = strings.ReplaceAll(item.Body, "cid:"+cid, dataURI)
	}
}

func collectCids(srv *gmail.Service, msgID string, part *gmail.MessagePart, cidMap map[string]string) {
	if part.Body.AttachmentId != "" {
		cid := ""
		for _, h := range part.Headers {
			if strings.EqualFold(h.Name, "Content-ID") {
				cid = strings.Trim(h.Value, "<>")
				break
			}
		}
		if cid != "" {
			attach, err := srv.Users.Messages.Attachments.Get("me", msgID, part.Body.AttachmentId).Do()
			if err == nil {
				dataURI := fmt.Sprintf("data:%s;base64,%s", part.MimeType, attach.Data)
				cidMap[cid] = dataURI
			}
		}
	}

	for _, subPart := range part.Parts {
		collectCids(srv, msgID, subPart, cidMap)
	}
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

func extractBody(part *gmail.MessagePart) string {
	// If this is a leaf node with data, return it
	if part.Body.Data != "" {
		data, err := base64.RawURLEncoding.DecodeString(part.Body.Data)
		if err != nil {
			data, err = base64.URLEncoding.DecodeString(part.Body.Data)
		}
		
		if err == nil {
			content := string(data)
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

	// If it's a multipart, prioritize HTML
	if strings.HasPrefix(part.MimeType, "multipart/") {
		var htmlBody, plainBody string
		for _, subPart := range part.Parts {
			body := extractBody(subPart)
			if subPart.MimeType == "text/html" {
				htmlBody = body
			} else if subPart.MimeType == "text/plain" && plainBody == "" {
				plainBody = body
			} else if strings.HasPrefix(subPart.MimeType, "multipart/") && body != "" {
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
