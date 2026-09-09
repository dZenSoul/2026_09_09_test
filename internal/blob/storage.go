// Package blob defines the binary object storage boundary.
package blob

import (
	"context"
	"errors"
	"io"
)

var (
	ErrInvalidKey = errors.New("blob: invalid key")
	ErrNotFound   = errors.New("blob: not found")
)

type Object struct {
	Body io.ReadCloser
	Size int64
}

type Storage interface {
	// Put publishes the object atomically: on error no object may be visible at
	// key. Implementations must not replace an existing object.
	Put(ctx context.Context, key string, source io.Reader) (size int64, err error)
	Open(ctx context.Context, key string) (Object, error)
	Delete(ctx context.Context, key string) error
}
