package document

import (
	"context"
	"fmt"

	"documents/internal/domain"
)

// Get returns an accessible document and opens its blob only for file
// documents. Callers own Content.File and must close it.
func (s *service) Get(ctx context.Context, requester domain.User, id string) (Content, error) {
	document, err := s.getMetadata(ctx, requester, id)
	if err != nil {
		return Content{}, err
	}
	if !document.IsFile {
		return Content{Document: document}, nil
	}

	object, err := s.blobs.Open(ctx, document.StorageKey)
	if err != nil {
		return Content{}, fmt.Errorf("open document blob: %w", err)
	}
	if object.Body == nil || object.Size < 0 {
		if object.Body != nil {
			_ = object.Body.Close()
		}
		return Content{}, fmt.Errorf("open document blob: invalid object")
	}
	document.SizeBytes = object.Size
	return Content{Document: document, File: object.Body}, nil
}

// GetMetadata performs the same identity and access checks as Get without
// opening a file. The HTTP transport uses it for HEAD requests.
func (s *service) GetMetadata(ctx context.Context, requester domain.User, id string) (domain.Document, error) {
	return s.getMetadata(ctx, requester, id)
}

func (s *service) getMetadata(ctx context.Context, requester domain.User, id string) (domain.Document, error) {
	if requester.ID == "" || requester.Login == "" || id == "" {
		return domain.Document{}, domain.ErrInvalidArgument
	}
	document, err := s.documents.ByIDAccessible(ctx, id, requester.ID)
	if err != nil {
		return domain.Document{}, mapRepositoryError(err)
	}
	return document, nil
}
