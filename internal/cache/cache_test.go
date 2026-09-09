package cache

import (
	"context"
	"testing"
	"time"
)

func TestMemoryTTLAndCopies(t *testing.T) {
	now := time.Unix(100, 0)
	cache := newMemory(1024, 10, func() time.Time { return now })
	entry := Entry{Status: 200, Header: map[string][]string{"X-Test": {"one"}}, Body: []byte("body"), Tags: []string{"docs"}}
	cache.Set(context.Background(), "key", entry, time.Minute)
	entry.Body[0] = 'X'

	got, ok := cache.Get(context.Background(), "key")
	if !ok || string(got.Body) != "body" || !got.StoredAt.Equal(now) {
		t.Fatalf("Get() = %#v, %v", got, ok)
	}
	got.Header["X-Test"][0] = "changed"
	got.Body[0] = 'Y'
	got, _ = cache.Get(context.Background(), "key")
	if got.Header["X-Test"][0] != "one" || string(got.Body) != "body" {
		t.Fatal("Get returned aliases to internal cache data")
	}

	now = now.Add(time.Minute)
	if _, ok := cache.Get(context.Background(), "key"); ok {
		t.Fatal("expired entry was returned")
	}
}

func TestMemoryLRUEvictionAndInvalidation(t *testing.T) {
	cache := NewMemory(1024, 2)
	ctx := context.Background()
	cache.Set(ctx, "one", Entry{Body: []byte("1"), Tags: []string{"odd"}}, time.Hour)
	cache.Set(ctx, "two", Entry{Body: []byte("2"), Tags: []string{"even"}}, time.Hour)
	_, _ = cache.Get(ctx, "one") // one is now most recently used
	cache.Set(ctx, "three", Entry{Body: []byte("3"), Tags: []string{"odd"}}, time.Hour)
	if _, ok := cache.Get(ctx, "two"); ok {
		t.Fatal("least recently used entry was not evicted")
	}
	cache.Invalidate(ctx, "odd")
	for _, key := range []string{"one", "three"} {
		if _, ok := cache.Get(ctx, key); ok {
			t.Fatalf("tagged entry %q survived invalidation", key)
		}
	}
}

func TestMemoryRejectsEntryOverByteLimit(t *testing.T) {
	cache := NewMemory(8, 10)
	cache.Set(context.Background(), "key", Entry{Body: []byte("too large")}, time.Hour)
	entry, ok := cache.Get(context.Background(), "key")
	if !ok || !entry.BodyOmitted || len(entry.Body) != 0 {
		t.Fatalf("oversized body was not reduced to metadata: %#v, %v", entry, ok)
	}
}
