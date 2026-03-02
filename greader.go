package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"google.golang.org/api/gmail/v1"
)

func registerGReaderHandlers(mux *http.ServeMux) {
	// Standard paths
	mux.HandleFunc("/proxy", handleProxy)
	mux.HandleFunc("/accounts/ClientLogin", handleLogin)
	mux.HandleFunc("/reader/api/0/token", handleToken)
	mux.HandleFunc("/reader/api/0/tag/list", handleTagList)
	mux.HandleFunc("/reader/api/0/subscription/list", handleSubscriptionList)
	mux.HandleFunc("/reader/api/0/subscription/edit", handleSubscriptionEdit)
	mux.HandleFunc("/reader/api/0/stream/items/ids", handleItemIDs)
	mux.HandleFunc("/reader/api/0/stream/items/contents", handleItemContents)
	mux.HandleFunc("/reader/api/0/edit-tag", handleEditTag)

	// FreshRSS specific paths
	mux.HandleFunc("/api/greader.php/reader/api/0/proxy", handleProxy)
	mux.HandleFunc("/api/greader.php/accounts/ClientLogin", handleLogin)
	mux.HandleFunc("/api/greader.php/reader/api/0/token", handleToken)
	mux.HandleFunc("/api/greader.php/reader/api/0/tag/list", handleTagList)
	mux.HandleFunc("/api/greader.php/reader/api/0/subscription/list", handleSubscriptionList)
	mux.HandleFunc("/api/greader.php/reader/api/0/subscription/edit", handleSubscriptionEdit)
	mux.HandleFunc("/api/greader.php/reader/api/0/stream/items/ids", handleItemIDs)
	mux.HandleFunc("/api/greader.php/reader/api/0/stream/items/contents", handleItemContents)
	mux.HandleFunc("/api/greader.php/reader/api/0/edit-tag", handleEditTag)
}

func handleProxy(w http.ResponseWriter, r *http.Request) {
	u := r.URL.Query().Get("u")
	if u == "" {
		http.Error(w, "Missing URL", http.StatusBadRequest)
		return
	}

	log.Printf("PROXY REQUEST: %s", u)

	req, err := http.NewRequest("GET", u, nil)
	if err != nil {
		log.Printf("PROXY ERROR (NewRequest): %v", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// Use a modern User-Agent to avoid being blocked by CDNs
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/91.0.4472.124 Safari/537.36")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		log.Printf("PROXY ERROR (Do): %v", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer resp.Body.Close()

	log.Printf("PROXY RESPONSE: %d %s", resp.StatusCode, resp.Header.Get("Content-Type"))

	if resp.StatusCode != http.StatusOK {
		http.Error(w, fmt.Sprintf("Remote server returned status: %d", resp.StatusCode), resp.StatusCode)
		return
	}

	for k, v := range resp.Header {
		if strings.EqualFold(k, "Content-Type") {
			w.Header()[k] = v
		}
	}
	w.Header().Set("Content-Disposition", "inline")
	w.Header().Set("Cache-Control", "public, max-age=31536000") // Cache for 1 year
	io.Copy(w, resp.Body)
}

func handleSubscriptionEdit(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	streamID := r.FormValue("s")
	action := r.FormValue("ac")

	if action == "unsubscribe" && strings.HasPrefix(streamID, "feed/") {
		email := strings.TrimPrefix(streamID, "feed/")
		cache.mu.Lock()
		if cache.ExcludedSenders == nil {
			cache.ExcludedSenders = make(map[string]bool)
		}
		cache.ExcludedSenders[email] = true
		delete(cache.Subscriptions, email)
		cache.mu.Unlock()
		cache.Save()
		log.Printf("EXCLUDED SENDER: %s", email)
	}

	w.Header().Set("Content-Type", "text/plain")
	fmt.Fprintf(w, "OK")
}

func handleEditTag(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	ids := r.Form["i"]
	addTags := r.Form["a"]
	
	log.Printf("EDIT TAG REQUEST: IDs=%v AddTags=%v", ids, addTags)

	isRead := false
	for _, a := range addTags {
		if a == "user/-/state/com.google/read" {
			isRead = true
			break
		}
	}

	if isRead {
		for _, id := range ids {
			cleanID := id
			if strings.Contains(id, "/") {
				parts := strings.Split(id, "/")
				cleanID = parts[len(parts)-1]
			}

			// Find the item
			var item *Item
			cache.mu.RLock()
			if gmailID, ok := cache.HexToGmailID[cleanID]; ok {
				item = cache.Items[gmailID]
			} else {
				for _, it := range cache.Items {
					if fmt.Sprintf("%d", it.IntID) == cleanID {
						item = it
						break
					}
				}
			}
			cache.mu.RUnlock()

			if item != nil && !item.IsRead {
				gmailMu.RLock()
				svc := gmailSvc
				gmailMu.RUnlock()

				if svc != nil {
					err := svc.Users.Messages.BatchModify("me", &gmail.BatchModifyMessagesRequest{
						Ids:            []string{item.ID},
						RemoveLabelIds: []string{"UNREAD"},
					}).Do()
					if err == nil {
						cache.mu.Lock()
						item.IsRead = true
						cache.mu.Unlock()
						log.Printf("MARKED READ IN GMAIL: %s", item.ID)
					}
				}
			}
		}
		cache.Save()
	}

	w.Header().Set("Content-Type", "text/plain")
	fmt.Fprintf(w, "OK")
}

func handleTagList(w http.ResponseWriter, r *http.Request) {
	type GTag struct {
		ID string `json:"id"`
	}

	tags := []GTag{
		{ID: "user/-/label/Newsletters"},
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"tags": tags,
	})
}

func handleLogin(w http.ResponseWriter, r *http.Request) {
	user := r.FormValue("Email")
	pass := r.FormValue("Passwd")
	
	log.Printf("LOGIN ATTEMPT: User='%s' Pass='%s'", user, pass)

	if user == config.Server.User && pass == config.Server.Pass {
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprintf(w, "Auth=fake-token-12345\n")
		return
	}
	log.Printf("LOGIN FAILED: Expected '%s':'%s'", config.Server.User, config.Server.Pass)
	http.Error(w, "Unauthorized", http.StatusUnauthorized)
}

func handleToken(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain")
	fmt.Fprintf(w, "fake-token-12345\n")
}

func handleSubscriptionList(w http.ResponseWriter, r *http.Request) {
	cache.mu.RLock()
	defer cache.mu.RUnlock()

	type GSub struct {
		ID        string   `json:"id"`
		Title     string   `json:"title"`
		Categories []map[string]string `json:"categories"`
		URL       string   `json:"url"`
		HtmlURL   string   `json:"htmlUrl"`
		IconURL   string   `json:"iconUrl"`
	}

	subs := []GSub{}
	for email, s := range cache.Subscriptions {
		if cache.ExcludedSenders[email] {
			continue
		}
		subs = append(subs, GSub{
			ID:    s.ID,
			Title: s.Title,
			Categories: []map[string]string{
				{"id": "user/-/label/Newsletters", "label": "Newsletters"},
			},
			URL:     "https://" + email,
			HtmlURL: "https://" + email,
			IconURL: "https://www.google.com/s2/favicons?domain=" + email,
		})
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"subscriptions": subs,
	})
}

func handleItemIDs(w http.ResponseWriter, r *http.Request) {
	cache.mu.RLock()
	defer cache.mu.RUnlock()

	stream := r.URL.Query().Get("s")
	excludeTarget := r.URL.Query().Get("xt")
	nStr := r.URL.Query().Get("n")
	limit := 250
	if n, err := strconv.Atoi(nStr); err == nil && n > 0 {
		limit = n
	}

	type GItemRef struct {
		ID string `json:"id"`
	}

	// Collect items
	var items []*Item
	for _, item := range cache.Items {
		if cache.ExcludedSenders[item.Sender] {
			continue
		}
		if excludeTarget == "user/-/state/com.google/read" && item.IsRead {
			continue
		}
		if stream == "" || stream == "user/-/state/com.google/reading-list" || stream == "feed/"+item.Sender {
			items = append(items, item)
		}
	}

	// Efficient Sort
	sort.Slice(items, func(i, j int) bool {
		return items[i].Timestamp.After(items[j].Timestamp)
	})

	// Apply limit
	if len(items) > limit {
		items = items[:limit]
	}

	refs := []GItemRef{}
	for _, item := range items {
		refs = append(refs, GItemRef{ID: fmt.Sprintf("%d", item.IntID)})
	}

	if refs == nil {
		refs = []GItemRef{}
	}

	w.Header().Set("Content-Type", "application/json")
	// NNW expects { "itemRefs": [ {"id": "..."}, ... ] }
	json.NewEncoder(w).Encode(map[string]interface{}{
		"itemRefs": refs,
	})
}

func handleItemContents(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	ids := r.Form["i"]
	cache.mu.RLock()
	defer cache.mu.RUnlock()

	type GEntry struct {
		ID            string   `json:"id"`
		Title         string   `json:"title"`
		Published     float64  `json:"published"`
		CrawlTimeMsec int64    `json:"crawlTimeMsec,string"`
		TimestampUsec int64    `json:"timestampUsec,string"`
		Author        string   `json:"author"`
		Summary       map[string]string   `json:"summary"`
		Content       map[string]string   `json:"content"`
		Alternate     []map[string]string `json:"alternate"`
		Categories    []string            `json:"categories"`
		Origin        map[string]string   `json:"origin"`
	}

	entries := []GEntry{}
	for _, id := range ids {
		cleanID := id
		if strings.Contains(id, "/") {
			parts := strings.Split(id, "/")
			cleanID = parts[len(parts)-1]
		}

		// Try to lookup by hex first
		item := cache.GetItemByHex(cleanID)
		if item == nil {
			// If not found, try lookup by decimal (NNW might send decimal)
			var gmailID string
			cache.mu.RLock()
			for _, it := range cache.Items {
				if fmt.Sprintf("%d", it.IntID) == cleanID {
					gmailID = it.ID
					break
				}
			}
			if gmailID != "" {
				item = cache.Items[gmailID]
			}
			cache.mu.RUnlock()
		}

		if item == nil {
			continue
		}

		msec := item.Timestamp.UnixMilli()
		entry := GEntry{
			ID:            "tag:google.com,2005:reader/item/" + item.HexID,
			Title:         item.Subject,
			Published:     float64(item.Timestamp.Unix()),
			CrawlTimeMsec: msec,
			TimestampUsec: msec * 1000,
			Author:        item.SenderName,
			Summary:       map[string]string{"content": item.Body},
			Content:       map[string]string{"content": item.Body},
			Alternate:     []map[string]string{{"href": "https://mail.google.com/mail/u/0/#inbox/" + item.ID, "type": "text/html"}},
			Origin: map[string]string{
				"streamId": "feed/" + item.Sender,
				"title":    item.SenderName,
			},
			Categories: []string{"user/-/state/com.google/reading-list"},
		}
		if item.IsRead {
			entry.Categories = append(entry.Categories, "user/-/state/com.google/read")
		}
		entries = append(entries, entry)
	}

	if entries == nil {
		entries = []GEntry{}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"id":      "reading-list",
		"updated": time.Now().Unix(),
		"items":   entries,
	})
}
