package document

import (
	"context"
	"errors"
	"fmt"

	"documents/internal/blob"
	"documents/internal/domain"
)

// Delete first commits logical deletion (and, for files, its outbox task).
// Cache invalidation and physical deletion only start after that commit. Thus a
// cleanup failure can leave an unreachable orphan, never accessible metadata
// that points at a missing blob.
func (s *service) Delete(ctx context.Context, requester domain.User, id string) error {
	if requester.ID == "" || requester.Login == "" || id == "" {
		return domain.ErrInvalidArgument
	}

	document, err := s.documents.Delete(ctx, id, requester.ID)
	if err != nil {
		return mapRepositoryError(err)
	}

	// Neither a canceled request nor a disconnected client may suppress the
	// post-commit work. Each operation remains tightly bounded.
	cleanupCtx, cancel := context.WithTimeout(context.Background(), s.cleanupTimeout)
	defer cancel()
	var result error
	if s.invalidateDelete != nil {
		if err := s.invalidateDelete(cleanupCtx, id); err != nil {
			result = fmt.Errorf("invalidate deleted document cache: %w", err)
		}
	}
	if !document.IsFile {
		return result
	}

	if err := s.blobs.Delete(cleanupCtx, document.StorageKey); err != nil && !errors.Is(err, blob.ErrNotFound) {
		return errors.Join(result, fmt.Errorf("delete document blob: %w", err))
	}
	if s.cleanup != nil {
		if err := s.cleanup.CompleteBlobCleanupByStorageKey(cleanupCtx, document.StorageKey); err != nil {
			return errors.Join(result, fmt.Errorf("confirm document blob cleanup: %w", err))
		}
	}
	return result
}
