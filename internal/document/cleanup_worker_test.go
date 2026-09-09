package document

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"documents/internal/blob"
	"documents/internal/repository"
)

func TestCleanupWorkerCompletesMissingBlobAndRetriesFailure(t *testing.T) {
	now := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	tasks := &cleanupTasks{claimed: []repository.BlobCleanupTask{
		{ID: 1, StorageKey: "gone", Attempts: 1},
		{ID: 2, StorageKey: "unavailable", Attempts: 3},
	}}
	storage := &cleanupStorage{errors: map[string]error{
		"gone":        blob.ErrNotFound,
		"unavailable": errors.New("storage unavailable"),
	}}
	worker, err := NewCleanupWorker(tasks, storage, CleanupWorkerConfig{
		BatchSize: 2, OperationTimeout: time.Second,
		InitialBackoff: time.Second, MaxBackoff: time.Minute,
		Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}

	processed, err := worker.ProcessBatch(context.Background())
	if err != nil || processed != 2 {
		t.Fatalf("ProcessBatch() = %d, %v", processed, err)
	}
	if len(tasks.completed) != 1 || tasks.completed[0] != 1 {
		t.Fatalf("completed = %v", tasks.completed)
	}
	if tasks.retriedID != 2 || !tasks.retryAt.Equal(now.Add(4*time.Second)) {
		t.Fatalf("retry = %d at %v", tasks.retriedID, tasks.retryAt)
	}
}

type cleanupTasks struct {
	claimed   []repository.BlobCleanupTask
	completed []int64
	retriedID int64
	retryAt   time.Time
}

func (f *cleanupTasks) ClaimBlobCleanupTasks(context.Context, int, time.Time) ([]repository.BlobCleanupTask, error) {
	return append([]repository.BlobCleanupTask(nil), f.claimed...), nil
}
func (f *cleanupTasks) CompleteBlobCleanupTask(_ context.Context, id int64) error {
	f.completed = append(f.completed, id)
	return nil
}
func (*cleanupTasks) CompleteBlobCleanupByStorageKey(context.Context, string) error { return nil }
func (f *cleanupTasks) RetryBlobCleanupTask(_ context.Context, id int64, at time.Time) error {
	f.retriedID, f.retryAt = id, at
	return nil
}

type cleanupStorage struct{ errors map[string]error }

func (*cleanupStorage) Put(context.Context, string, io.Reader) (int64, error) {
	return 0, errors.New("unused")
}
func (*cleanupStorage) Open(context.Context, string) (blob.Object, error) {
	return blob.Object{}, errors.New("unused")
}
func (f *cleanupStorage) Delete(_ context.Context, key string) error { return f.errors[key] }
