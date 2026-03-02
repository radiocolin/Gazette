package main

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"strings"
	"sync"
	"time"
)

type Item struct {
	ID         string            `json:"id"`
	ThreadID   string            `json:"thread_id"`
	IntID      uint64            `json:"int_id"`
	Sender     string            `json:"sender"`
	SenderName string            `json:"sender_name"`
	Subject    string            `json:"subject"`
	Snippet    string            `json:"snippet"`
	Body       string            `json:"body"`
	CleanBody  string            `json:"clean_body"`
	CidMap     map[string]string `json:"cid_map"` // CID -> Base64 Data URI
	Timestamp  time.Time         `json:"timestamp"`
	IsRead     bool              `json:"is_read"`
}

type Subscription struct {
	ID    string `json:"id"`    // feed/email-address
	Title string `json:"title"` // Sender Name
}

type Cache struct {
	Items            map[string]*Item         `json:"items"`          // Gmail ID -> Item
	IntToGmailID     map[uint64]string         `json:"int_to_gmail"`   // Int ID -> Gmail ID
	Subscriptions    map[string]*Subscription `json:"subscriptions"` // Sender Email -> Subscription
	ExcludedSenders  map[string]bool          `json:"excluded_senders"`
	ProcessedThreads map[string]string        `json:"processed_threads"` // ThreadID -> First MessageID
	NextIntID        uint64                   `json:"next_int_id"`
	mu               sync.RWMutex
}

func NewCache() *Cache {
	c := &Cache{
		Items:            make(map[string]*Item),
		IntToGmailID:     make(map[uint64]string),
		Subscriptions:    make(map[string]*Subscription),
		ExcludedSenders:  make(map[string]bool),
		ProcessedThreads: make(map[string]string),
		NextIntID:        1,
	}
	c.load()

	if c.ProcessedThreads == nil {
		c.ProcessedThreads = make(map[string]string)
	}

	if c.IntToGmailID == nil {
		c.IntToGmailID = make(map[uint64]string)
	}

	// Ensure all maps are populated from Items if not loaded
	for _, item := range c.Items {
		c.IntToGmailID[item.IntID] = item.ID
	}

	// Cleanup existing titles
	for _, s := range c.Subscriptions {
		s.Title = strings.Trim(s.Title, "\" '“”")
	}

	return c
}

func (c *Cache) load() {
	f, err := os.Open("/app/data/cache.json")
	if err != nil {
		if !os.IsNotExist(err) {
			log.Printf("Error opening cache: %v", err)
		}
		return
	}
	defer f.Close()
	if err := json.NewDecoder(f).Decode(c); err != nil {
		log.Printf("Error decoding cache: %v", err)
	}
}

func (c *Cache) Save() {
	c.mu.RLock()
	data, err := json.Marshal(c)
	c.mu.RUnlock()

	if err != nil {
		log.Printf("Error encoding cache: %v", err)
		return
	}

	err = os.WriteFile("/app/data/cache.json", data, 0644)
	if err != nil {
		log.Printf("Error saving cache: %v", err)
	}
}

func (c *Cache) GetOrCreateItem(gmailID string) *Item {
	c.mu.Lock()
	defer c.mu.Unlock()

	if item, ok := c.Items[gmailID]; ok {
		return item
	}

	intID := c.NextIntID
	c.NextIntID++

	item := &Item{
		ID:    gmailID,
		IntID: intID,
	}
	c.Items[gmailID] = item
	c.IntToGmailID[intID] = gmailID
	return item
}

func (c *Cache) GetItemByInt(intID uint64) *Item {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if gmailID, ok := c.IntToGmailID[intID]; ok {
		return c.Items[gmailID]
	}
	return nil
}

func (c *Cache) AddSubscription(email, name string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	name = strings.Trim(name, "\" '“”")
	if s, ok := c.Subscriptions[email]; ok {
		s.Title = name
	} else {
		c.Subscriptions[email] = &Subscription{
			ID:    "feed/" + email,
			Title: name,
		}
	}
}
