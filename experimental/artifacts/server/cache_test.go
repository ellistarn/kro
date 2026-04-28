package server

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCache_PutAndGet(t *testing.T) {
	c := NewCache(0) // unbounded

	entry := &CacheEntry{
		URI:        "registry.example.com/repo:v1",
		Digest:     "sha256:aaa",
		Content:    map[string]any{"nodes": []any{"a"}},
		ResolvedAt: time.Now(),
	}
	c.Put(entry)

	got, ok := c.Get("registry.example.com/repo:v1")
	require.True(t, ok)
	assert.Equal(t, "sha256:aaa", got.Digest)
	assert.Equal(t, entry.Content, got.Content)
}

func TestCache_GetMiss(t *testing.T) {
	c := NewCache(0)

	_, ok := c.Get("does-not-exist")
	assert.False(t, ok)
}

func TestCache_UpdateExisting(t *testing.T) {
	c := NewCache(0)

	c.Put(&CacheEntry{
		URI:        "registry.example.com/repo:v1",
		Digest:     "sha256:aaa",
		Content:    map[string]any{"v": 1},
		ResolvedAt: time.Now(),
	})
	c.Put(&CacheEntry{
		URI:        "registry.example.com/repo:v1",
		Digest:     "sha256:bbb",
		Content:    map[string]any{"v": 2},
		ResolvedAt: time.Now(),
	})

	got, ok := c.Get("registry.example.com/repo:v1")
	require.True(t, ok)
	assert.Equal(t, "sha256:bbb", got.Digest)

	// Should still be only one entry.
	assert.Len(t, c.All(), 1)
}

func TestCache_LRUEviction(t *testing.T) {
	c := NewCache(2)

	c.Put(&CacheEntry{URI: "a", Digest: "sha256:a", ResolvedAt: time.Now()})
	c.Put(&CacheEntry{URI: "b", Digest: "sha256:b", ResolvedAt: time.Now()})

	// Access "a" to make it recently used.
	_, ok := c.Get("a")
	require.True(t, ok)

	// Insert "c" — should evict "b" (least recently used).
	c.Put(&CacheEntry{URI: "c", Digest: "sha256:c", ResolvedAt: time.Now()})

	_, ok = c.Get("a")
	assert.True(t, ok, "a should still be cached")

	_, ok = c.Get("b")
	assert.False(t, ok, "b should have been evicted")

	_, ok = c.Get("c")
	assert.True(t, ok, "c should be cached")
}

func TestCache_LRUEvictsOldest(t *testing.T) {
	c := NewCache(2)

	c.Put(&CacheEntry{URI: "first", Digest: "sha256:1", ResolvedAt: time.Now()})
	c.Put(&CacheEntry{URI: "second", Digest: "sha256:2", ResolvedAt: time.Now()})

	// Don't access either — "first" is oldest.
	c.Put(&CacheEntry{URI: "third", Digest: "sha256:3", ResolvedAt: time.Now()})

	_, ok := c.Get("first")
	assert.False(t, ok, "first should have been evicted")

	_, ok = c.Get("second")
	assert.True(t, ok, "second should still be cached")
}

func TestCache_All(t *testing.T) {
	c := NewCache(0)

	c.Put(&CacheEntry{URI: "a", Digest: "sha256:a", ResolvedAt: time.Now()})
	c.Put(&CacheEntry{URI: "b", Digest: "sha256:b", ResolvedAt: time.Now()})
	c.Put(&CacheEntry{URI: "c", Digest: "sha256:c", ResolvedAt: time.Now()})

	all := c.All()
	assert.Len(t, all, 3)

	uris := make(map[string]bool)
	for _, e := range all {
		uris[e.URI] = true
	}
	assert.True(t, uris["a"])
	assert.True(t, uris["b"])
	assert.True(t, uris["c"])
}

func TestCache_ConcurrentAccess(t *testing.T) {
	c := NewCache(100)

	var wg sync.WaitGroup
	// Concurrent writers.
	for i := range 50 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			uri := fmt.Sprintf("registry.example.com/repo:%d", i)
			c.Put(&CacheEntry{
				URI:        uri,
				Digest:     fmt.Sprintf("sha256:%d", i),
				ResolvedAt: time.Now(),
			})
		}(i)
	}
	// Concurrent readers.
	for i := range 50 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			uri := fmt.Sprintf("registry.example.com/repo:%d", i)
			c.Get(uri)
		}(i)
	}
	// Concurrent All() calls.
	for range 10 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c.All()
		}()
	}
	wg.Wait()

	// Verify the cache is in a consistent state.
	all := c.All()
	assert.LessOrEqual(t, len(all), 100)
	assert.Greater(t, len(all), 0)
}
