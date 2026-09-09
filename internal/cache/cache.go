// Package cache defines the replaceable response-cache boundary.
package cache

import (
	"context"
	"time"
)

type Entry struct {
	Status   int
	Header   map[string][]string
	Body     []byte
	StoredAt time.Time
}

type Cache interface {
	Get(ctx context.Context, key string) (Entry, bool)
	Set(ctx context.Context, key string, entry Entry, ttl time.Duration)
	Delete(ctx context.Context, key string)
	Invalidate(ctx context.Context, tags ...string)
}
