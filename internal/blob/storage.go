// Package blob defines the binary object storage boundary.
package blob

import (
	"context"
	"io"
)

type Object struct {
	Body io.ReadCloser
	Size int64
}

type Storage interface {
	Put(ctx context.Context, key string, source io.Reader) (size int64, err error)
	Open(ctx context.Context, key string) (Object, error)
	Delete(ctx context.Context, key string) error
}
