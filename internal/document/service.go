// Package document defines document use cases consumed by transports.
package document

import (
	"context"
	"io"

	"documents/internal/domain"
)

type Upload struct {
	Document domain.Document
	File     io.Reader
}

type Content struct {
	Document domain.Document
	File     io.ReadCloser
}

type Service interface {
	Upload(ctx context.Context, requester domain.User, input Upload) (domain.Document, error)
	List(ctx context.Context, requester domain.User, filter domain.DocumentFilter) ([]domain.Document, error)
	Get(ctx context.Context, requester domain.User, id string) (Content, error)
	Delete(ctx context.Context, requester domain.User, id string) error
}
