package postgres

import (
	"context"
	"fmt"
	"strings"
	"time"

	"documents/internal/repository"

	"github.com/jackc/pgx/v5"
)

// ClaimBlobCleanupTasks atomically leases a bounded set of due tasks. Locked
// rows are skipped, allowing multiple application instances to run workers.
func (s *Store) ClaimBlobCleanupTasks(ctx context.Context, limit int, leaseUntil time.Time) ([]repository.BlobCleanupTask, error) {
	if limit <= 0 || leaseUntil.IsZero() {
		return nil, fmt.Errorf("claim blob cleanup tasks: %w", repository.ErrInvalidArgument)
	}
	rows, err := s.executor(ctx).Query(ctx, `
        WITH due AS (
            SELECT id
            FROM blob_cleanup_tasks
            WHERE next_attempt_at <= CURRENT_TIMESTAMP
            ORDER BY next_attempt_at, id
            FOR UPDATE SKIP LOCKED
            LIMIT $1
        )
        UPDATE blob_cleanup_tasks task
        SET attempts = task.attempts + 1,
            next_attempt_at = $2
        FROM due
        WHERE task.id = due.id
        RETURNING task.id, task.storage_key, task.attempts, task.next_attempt_at`, limit, leaseUntil.UTC())
	if err != nil {
		return nil, classify("claim blob cleanup tasks", err)
	}
	tasks, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (repository.BlobCleanupTask, error) {
		var task repository.BlobCleanupTask
		err := row.Scan(&task.ID, &task.StorageKey, &task.Attempts, &task.NextAttemptAt)
		task.NextAttemptAt = task.NextAttemptAt.UTC()
		return task, err
	})
	if err != nil {
		return nil, classify("read claimed blob cleanup tasks", err)
	}
	return tasks, nil
}

func (s *Store) CompleteBlobCleanupTask(ctx context.Context, id int64) error {
	if id <= 0 {
		return fmt.Errorf("complete blob cleanup task: %w", repository.ErrInvalidArgument)
	}
	_, err := s.executor(ctx).Exec(ctx, "DELETE FROM blob_cleanup_tasks WHERE id = $1", id)
	if err != nil {
		return classify("complete blob cleanup task", err)
	}
	return nil
}

func (s *Store) CompleteBlobCleanupByStorageKey(ctx context.Context, storageKey string) error {
	if strings.TrimSpace(storageKey) == "" {
		return fmt.Errorf("complete blob cleanup task: %w", repository.ErrInvalidArgument)
	}
	_, err := s.executor(ctx).Exec(ctx, "DELETE FROM blob_cleanup_tasks WHERE storage_key = $1", storageKey)
	if err != nil {
		return classify("complete blob cleanup task", err)
	}
	return nil
}

func (s *Store) RetryBlobCleanupTask(ctx context.Context, id int64, nextAttemptAt time.Time) error {
	if id <= 0 || nextAttemptAt.IsZero() {
		return fmt.Errorf("retry blob cleanup task: %w", repository.ErrInvalidArgument)
	}
	_, err := s.executor(ctx).Exec(ctx, `
        UPDATE blob_cleanup_tasks
        SET next_attempt_at = $2
        WHERE id = $1`, id, nextAttemptAt.UTC())
	if err != nil {
		return classify("retry blob cleanup task", err)
	}
	return nil
}

var _ repository.BlobCleanupRepository = (*Store)(nil)
