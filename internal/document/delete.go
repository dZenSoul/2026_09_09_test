package document

import (
	"context"
	"errors"
	"fmt"

	"documents/internal/blob"
	"documents/internal/domain"
)

// Delete removes a document owned by requester. File blobs are removed before
// metadata so a successful return can never leave an orphaned object. If the
// metadata deletion fails afterwards, retrying is safe because a missing blob
// is treated as already cleaned up.
func (s *service) Delete(ctx context.Context, requester domain.User, id string) error {
	if requester.ID == "" || requester.Login == "" || id == "" {
		return domain.ErrInvalidArgument
	}

	document, err := s.documents.ByID(ctx, id)
	if err != nil {
		return mapRepositoryError(err)
	}
	if document.OwnerID != requester.ID {
		return domain.ErrForbidden
	}

	if document.IsFile {
		err := s.blobs.Delete(ctx, document.StorageKey)
		if err != nil && !errors.Is(err, blob.ErrNotFound) {
			return fmt.Errorf("delete document blob: %w", err)
		}
	}

	if _, err := s.documents.Delete(ctx, id, requester.ID); err != nil {
		return mapRepositoryError(err)
	}
	return nil
}
