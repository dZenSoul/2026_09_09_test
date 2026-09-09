// Package cache implements the process-local response cache.
package cache

import (
	"container/list"
	"context"
	"sync"
	"time"
)

// Entry is an HTTP response together with its invalidation tags.
type Entry struct {
	Status int
	Header map[string][]string
	Body   []byte
	// BodyOmitted marks a metadata-only entry created when the complete body
	// exceeds the configured byte budget. It may satisfy HEAD, never GET.
	BodyOmitted bool
	StoredAt    time.Time
	Tags        []string
}

// Memory is a bounded, concurrency-safe LRU cache. Expired entries are
// removed lazily and the least recently used entry is evicted first.
type Memory struct {
	mu       sync.Mutex
	items    map[string]*list.Element
	lru      list.List
	maxBytes int64
	maxItems int
	bytes    int64
	now      func() time.Time
}

type item struct {
	key       string
	entry     Entry
	expiresAt time.Time
	size      int64
}

func NewMemory(maxBytes int64, maxItems int) *Memory {
	return newMemory(maxBytes, maxItems, time.Now)
}

func newMemory(maxBytes int64, maxItems int, now func() time.Time) *Memory {
	return &Memory{items: make(map[string]*list.Element), maxBytes: maxBytes, maxItems: maxItems, now: now}
}

func (c *Memory) Get(ctx context.Context, key string) (Entry, bool) {
	if ctx.Err() != nil {
		return Entry{}, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	element, ok := c.items[key]
	if !ok {
		return Entry{}, false
	}
	value := element.Value.(*item)
	if !c.now().Before(value.expiresAt) {
		c.remove(element)
		return Entry{}, false
	}
	c.lru.MoveToFront(element)
	return cloneEntry(value.entry), true
}

func (c *Memory) Set(ctx context.Context, key string, entry Entry, ttl time.Duration) {
	if ctx.Err() != nil || key == "" || ttl <= 0 || c.maxBytes <= 0 || c.maxItems <= 0 {
		return
	}
	entry = cloneEntry(entry)
	entry.StoredAt = c.now()
	value := &item{key: key, entry: entry, expiresAt: entry.StoredAt.Add(ttl)}
	value.size = entrySize(key, entry)
	if value.size > c.maxBytes && len(entry.Body) != 0 {
		entry.Body = nil
		entry.BodyOmitted = true
		value.entry = entry
		value.size = entrySize(key, entry)
	}
	if value.size > c.maxBytes {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if old, ok := c.items[key]; ok {
		c.remove(old)
	}
	c.items[key] = c.lru.PushFront(value)
	c.bytes += value.size
	for c.lru.Len() > c.maxItems || c.bytes > c.maxBytes {
		c.remove(c.lru.Back())
	}
}

func (c *Memory) Delete(ctx context.Context, key string) {
	if ctx.Err() != nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if element, ok := c.items[key]; ok {
		c.remove(element)
	}
}

func (c *Memory) Invalidate(ctx context.Context, tags ...string) {
	if ctx.Err() != nil || len(tags) == 0 {
		return
	}
	wanted := make(map[string]struct{}, len(tags))
	for _, tag := range tags {
		wanted[tag] = struct{}{}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for element := c.lru.Back(); element != nil; {
		previous := element.Prev()
		if hasTag(element.Value.(*item).entry.Tags, wanted) {
			c.remove(element)
		}
		element = previous
	}
}

func (c *Memory) remove(element *list.Element) {
	if element == nil {
		return
	}
	value := element.Value.(*item)
	delete(c.items, value.key)
	c.bytes -= value.size
	c.lru.Remove(element)
}

func hasTag(tags []string, wanted map[string]struct{}) bool {
	for _, tag := range tags {
		if _, ok := wanted[tag]; ok {
			return true
		}
	}
	return false
}

func entrySize(key string, entry Entry) int64 {
	size := int64(len(key) + len(entry.Body))
	for name, values := range entry.Header {
		size += int64(len(name))
		for _, value := range values {
			size += int64(len(value))
		}
	}
	for _, tag := range entry.Tags {
		size += int64(len(tag))
	}
	return size
}

func cloneEntry(entry Entry) Entry {
	result := entry
	result.Body = append([]byte(nil), entry.Body...)
	result.Tags = append([]string(nil), entry.Tags...)
	result.Header = make(map[string][]string, len(entry.Header))
	for name, values := range entry.Header {
		result.Header[name] = append([]string(nil), values...)
	}
	return result
}

var _ Cache = (*Memory)(nil)

type Cache interface {
	Get(ctx context.Context, key string) (Entry, bool)
	Set(ctx context.Context, key string, entry Entry, ttl time.Duration)
	Delete(ctx context.Context, key string)
	Invalidate(ctx context.Context, tags ...string)
}
