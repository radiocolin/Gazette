package main

import (
	"encoding/json"
	"encoding/xml"
	"fmt"
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
	mux.HandleFunc("/view", handleView)
	mux.HandleFunc("/feed", handleFeed)
	mux.HandleFunc("/accounts/ClientLogin", handleLogin)
	mux.HandleFunc("/reader/api/0/token", handleToken)
	mux.HandleFunc("/reader/api/0/tag/list", handleTagList)
	mux.HandleFunc("/reader/api/0/subscription/list", handleSubscriptionList)
	mux.HandleFunc("/reader/api/0/subscription/edit", handleSubscriptionEdit)
	mux.HandleFunc("/reader/api/0/stream/items/ids", handleItemIDs)
	mux.HandleFunc("/reader/api/0/stream/items/contents", handleItemContents)
	mux.HandleFunc("/reader/api/0/edit-tag", handleEditTag)

	// FreshRSS specific paths
	mux.HandleFunc("/api/greader.php/view", handleView)
	mux.HandleFunc("/api/greader.php/feed", handleFeed)
	mux.HandleFunc("/api/greader.php/accounts/ClientLogin", handleLogin)
	mux.HandleFunc("/api/greader.php/reader/api/0/token", handleToken)
	mux.HandleFunc("/api/greader.php/reader/api/0/tag/list", handleTagList)
	mux.HandleFunc("/api/greader.php/reader/api/0/subscription/list", handleSubscriptionList)
	mux.HandleFunc("/api/greader.php/reader/api/0/subscription/edit", handleSubscriptionEdit)
	mux.HandleFunc("/api/greader.php/reader/api/0/stream/items/ids", handleItemIDs)
	mux.HandleFunc("/api/greader.php/reader/api/0/stream/items/contents", handleItemContents)
	mux.HandleFunc("/api/greader.php/reader/api/0/edit-tag", handleEditTag)
}

type RSS struct {
	XMLName xml.Name    `xml:"rss"`
	Version string      `xml:"version,attr"`
	Channel *RSSChannel `xml:"channel"`
}

type RSSChannel struct {
	Title       string    `xml:"title"`
	Link        string    `xml:"link"`
	Description string    `xml:"description"`
	Items       []RSSItem `xml:"item"`
}

type RSSItem struct {
	Title       string   `xml:"title"`
	Link        string   `xml:"link"`
	Description string   `xml:"description"`
	PubDate     string   `xml:"pubDate"`
	GUID        *RSSGUID `xml:"guid"`
}

type RSSGUID struct {
	Value       string `xml:",chardata"`
	IsPermaLink bool   `xml:"isPermaLink,attr"`
}

func handleFeed(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id") // feed/email@example.com
	if id == "" {
		http.Error(w, "Missing ID", http.StatusBadRequest)
		return
	}

	email := strings.TrimPrefix(id, "feed/")

	cache.mu.RLock()
	var items []*Item
	sub := cache.Subscriptions[email]
	for _, item := range cache.Items {
		if item.Sender == email {
			items = append(items, item)
		}
	}
	cache.mu.RUnlock()

	if sub == nil {
		http.Error(w, "Feed not found", http.StatusNotFound)
		return
	}

	sort.Slice(items, func(i, j int) bool {
		return items[i].Timestamp.After(items[j].Timestamp)
	})

	if len(items) > 100 {
		items = items[:100]
	}

	rss := RSS{
		Version: "2.0",
		Channel: &RSSChannel{
			Title:       sub.Title,
			Link:        strings.TrimSuffix(config.Gmail.PublicURL, "/"),
			Description: "Newsletters from " + sub.Title,
		},
	}

	for _, item := range items {
		viewURL := fmt.Sprintf("%s/view?id=%d", strings.TrimSuffix(config.Gmail.PublicURL, "/"), item.IntID)
		body := item.CleanBody
		if body == "" {
			body = item.Body
		}
		rss.Channel.Items = append(rss.Channel.Items, RSSItem{
			Title:       item.Subject,
			Link:        viewURL,
			Description: body,
			PubDate:     item.Timestamp.Format(time.RFC1123Z),
			GUID: &RSSGUID{
				Value:       fmt.Sprintf("%d", item.IntID),
				IsPermaLink: false,
			},
		})
	}

	w.Header().Set("Content-Type", "application/rss+xml; charset=utf-8")
	w.Write([]byte(xml.Header))
	enc := xml.NewEncoder(w)
	enc.Indent("", "  ")
	if err := enc.Encode(rss); err != nil {
		log.Printf("Error encoding RSS: %v", err)
	}
}

func handleView(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	if id == "" {
		http.Error(w, "Missing ID", http.StatusBadRequest)
		return
	}

	var item *Item
	if intID, err := strconv.ParseUint(id, 10, 64); err == nil {
		item = cache.GetItemByInt(intID)
	}

	if item == nil {
		http.Error(w, "Item not found", http.StatusNotFound)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write([]byte(item.Body))
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
	removeTags := r.Form["r"]

	log.Printf("EDIT TAG REQUEST: IDs=%v AddTags=%v RemoveTags=%v", ids, addTags, removeTags)

	markRead := false
	for _, a := range addTags {
		if a == "user/-/state/com.google/read" {
			markRead = true
			break
		}
	}
	markUnread := false
	for _, rv := range removeTags {
		if rv == "user/-/state/com.google/read" {
			markUnread = true
			break
		}
	}

	if markRead {
		var gmailIDs []string
		var items []*Item

		for _, id := range ids {
			cleanID := id
			if strings.Contains(id, "/") {
				parts := strings.Split(id, "/")
				cleanID = parts[len(parts)-1]
			}

			if intID, err := strconv.ParseUint(cleanID, 16, 64); err == nil {
				if item := cache.GetItemByInt(intID); item != nil && !item.IsRead {
					gmailIDs = append(gmailIDs, item.ID)
					items = append(items, item)
				}
			}
		}

		if len(gmailIDs) > 0 {
			gmailMu.RLock()
			svc := gmailSvc
			gmailMu.RUnlock()

			if svc != nil {
				_, err := withRetry(func() (interface{}, error) {
					return nil, svc.Users.Messages.BatchModify("me", &gmail.BatchModifyMessagesRequest{
						Ids:            gmailIDs,
						RemoveLabelIds: []string{"UNREAD"},
					}).Do()
				})
				if err == nil {
					cache.mu.Lock()
					for _, it := range items {
						it.IsRead = true
					}
					cache.mu.Unlock()
					log.Printf("MARKED %d ITEMS AS READ IN GMAIL", len(gmailIDs))
					cache.Save()
				} else {
					log.Printf("Error batch marking read: %v", err)
				}
			}
		}
	}

	if markUnread {
		var gmailIDs []string
		var items []*Item

		for _, id := range ids {
			cleanID := id
			if strings.Contains(id, "/") {
				parts := strings.Split(id, "/")
				cleanID = parts[len(parts)-1]
			}

			if intID, err := strconv.ParseUint(cleanID, 16, 64); err == nil {
				if item := cache.GetItemByInt(intID); item != nil && item.IsRead {
					gmailIDs = append(gmailIDs, item.ID)
					items = append(items, item)
				}
			}
		}

		if len(gmailIDs) > 0 {
			gmailMu.RLock()
			svc := gmailSvc
			gmailMu.RUnlock()

			if svc != nil {
				_, err := withRetry(func() (interface{}, error) {
					return nil, svc.Users.Messages.BatchModify("me", &gmail.BatchModifyMessagesRequest{
						Ids:          gmailIDs,
						AddLabelIds:  []string{"UNREAD"},
					}).Do()
				})
				if err == nil {
					cache.mu.Lock()
					for _, it := range items {
						it.IsRead = false
					}
					cache.mu.Unlock()
					log.Printf("MARKED %d ITEMS AS UNREAD IN GMAIL", len(gmailIDs))
					cache.Save()
				} else {
					log.Printf("Error batch marking unread: %v", err)
				}
			}
		}
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

		domain := email
		if parts := strings.Split(email, "@"); len(parts) == 2 {
			domain = parts[1]
		}

		subs = append(subs, GSub{
			ID:    s.ID,
			Title: s.Title,
			Categories: []map[string]string{
				{"id": "user/-/label/Newsletters", "label": "Newsletters"},
			},
			URL:     fmt.Sprintf("%s/feed?id=%s", strings.TrimSuffix(config.Gmail.PublicURL, "/"), s.ID),
			HtmlURL: "https://" + domain,
			IconURL: "https://www.google.com/s2/favicons?domain=" + domain,
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
	if len(ids) > 250 {
		ids = ids[:250]
	}

	type GEntry struct {
		ID            string              `json:"id"`
		Title         string              `json:"title"`
		Published     float64             `json:"published"`
		CrawlTimeMsec int64               `json:"crawlTimeMsec,string"`
		TimestampUsec int64               `json:"timestampUsec,string"`
		Author        string              `json:"author"`
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

		// IDs arrive as hex — NNW converts decimal IDs from stream/items/ids to hex
		// before requesting contents (per GReader protocol).
		var item *Item
		if intID, err := strconv.ParseUint(cleanID, 16, 64); err == nil {
			item = cache.GetItemByInt(intID)
		}

		if item == nil {
			continue
		}

		msec := item.Timestamp.UnixMilli()
		viewURL := fmt.Sprintf("%s/view?id=%d", strings.TrimSuffix(config.Gmail.PublicURL, "/"), item.IntID)
		body := item.CleanBody
		if body == "" {
			body = item.Body
		}
		entry := GEntry{
			ID:            fmt.Sprintf("tag:google.com,2005:reader/item/%016x", item.IntID),
			Title:         item.Subject,
			Published:     float64(item.Timestamp.Unix()),
			CrawlTimeMsec: msec,
			TimestampUsec: msec * 1000,
			Author:        item.SenderName,
			Summary:       map[string]string{"content": body},
			Content:       map[string]string{"content": body},
			Alternate:     []map[string]string{{"href": viewURL, "type": "text/html"}},
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
