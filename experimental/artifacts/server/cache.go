package server

import (
	"container/list"
	"sync"
	"time"
)

// CacheEntry holds a resolved artifact.
type CacheEntry struct {
	URI        string
	Digest     string
	Content    map[string]any // deserialized Graph spec
	ResolvedAt time.Time
	element    *list.Element
}

// Cache is a thread-safe in-memory LRU cache for artifact content.
type Cache struct {
	mu      sync.RWMutex
	items   map[string]*CacheEntry // keyed by OCI URI
	lru     *list.List
	maxSize int // max number of entries; 0 = unbounded
}

// NewCache creates a new LRU cache. A maxSize of 0 means unbounded.
func NewCache(maxSize int) *Cache {
	return &Cache{
		items:   make(map[string]*CacheEntry),
		lru:     list.New(),
		maxSize: maxSize,
	}
}

// Get returns the cache entry for the given URI, or nil if not found.
// Accessing an entry promotes it to the front of the LRU list.
func (c *Cache) Get(uri string) (*CacheEntry, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	entry, ok := c.items[uri]
	if !ok {
		return nil, false
	}
	// Promote to front (most recently used).
	c.lru.MoveToFront(entry.element)
	return entry, true
}

// Put inserts or updates a cache entry. If the cache is at capacity, the
// least-recently-used entry is evicted.
func (c *Cache) Put(entry *CacheEntry) {
	c.mu.Lock()
	defer c.mu.Unlock()

	// Update existing entry.
	if existing, ok := c.items[entry.URI]; ok {
		existing.Digest = entry.Digest
		existing.Content = entry.Content
		existing.ResolvedAt = entry.ResolvedAt
		c.lru.MoveToFront(existing.element)
		return
	}

	// Evict if at capacity.
	if c.maxSize > 0 && len(c.items) >= c.maxSize {
		c.evictLocked()
	}

	// Insert new entry.
	entry.element = c.lru.PushFront(entry)
	c.items[entry.URI] = entry
}

// All returns all cached entries in no particular order.
func (c *Cache) All() []*CacheEntry {
	c.mu.RLock()
	defer c.mu.RUnlock()

	entries := make([]*CacheEntry, 0, len(c.items))
	for _, entry := range c.items {
		entries = append(entries, entry)
	}
	return entries
}

// evictLocked removes the least-recently-used entry. Must be called with
// c.mu held.
func (c *Cache) evictLocked() {
	back := c.lru.Back()
	if back == nil {
		return
	}
	entry := back.Value.(*CacheEntry)
	c.lru.Remove(back)
	delete(c.items, entry.URI)
}
