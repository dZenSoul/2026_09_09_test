package document

import (
	"context"
	"errors"
	"fmt"
	"time"

	"documents/internal/blob"
	"documents/internal/domain"
	"documents/internal/repository"
)

type CleanupWorkerConfig struct {
	BatchSize        int
	PollInterval     time.Duration
	OperationTimeout time.Duration
	InitialBackoff   time.Duration
	MaxBackoff       time.Duration
	Now              func() time.Time
}

// CleanupWorker retries durable blob cleanup tasks. Run returns promptly when
// ctx is canceled and never holds a PostgreSQL transaction during blob I/O.
type CleanupWorker struct {
	tasks repository.BlobCleanupRepository
	blobs blob.Storage
	cfg   CleanupWorkerConfig
}

func NewCleanupWorker(tasks repository.BlobCleanupRepository, blobs blob.Storage, cfg CleanupWorkerConfig) (*CleanupWorker, error) {
	if tasks == nil || blobs == nil {
		return nil, fmt.Errorf("configure cleanup worker: %w", domain.ErrInvalidArgument)
	}
	if cfg.BatchSize <= 0 {
		cfg.BatchSize = 16
	}
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = time.Second
	}
	if cfg.OperationTimeout <= 0 {
		cfg.OperationTimeout = 5 * time.Second
	}
	if cfg.InitialBackoff <= 0 {
		cfg.InitialBackoff = time.Second
	}
	if cfg.MaxBackoff <= 0 {
		cfg.MaxBackoff = time.Minute
	}
	if cfg.MaxBackoff < cfg.InitialBackoff {
		return nil, fmt.Errorf("configure cleanup worker: %w", domain.ErrInvalidArgument)
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &CleanupWorker{tasks: tasks, blobs: blobs, cfg: cfg}, nil
}

func (w *CleanupWorker) Run(ctx context.Context) error {
	for {
		processed, err := w.ProcessBatch(ctx)
		if err != nil && ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil || processed == 0 {
			timer := time.NewTimer(w.cfg.PollInterval)
			select {
			case <-ctx.Done():
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
				return ctx.Err()
			case <-timer.C:
			}
		}
	}
}

// ProcessBatch claims and processes at most BatchSize tasks. It is exported to
// make one deterministic worker iteration testable without sleeps.
func (w *CleanupWorker) ProcessBatch(ctx context.Context) (int, error) {
	now := w.cfg.Now().UTC()
	// Tasks are processed sequentially, so the lease covers the worst-case
	// duration of the complete claimed batch, not merely its first item.
	const maxDuration = time.Duration(1<<63 - 1)
	leaseDuration := maxDuration
	if time.Duration(w.cfg.BatchSize) <= maxDuration/w.cfg.OperationTimeout {
		leaseDuration = w.cfg.OperationTimeout * time.Duration(w.cfg.BatchSize)
	}
	tasks, err := w.tasks.ClaimBlobCleanupTasks(ctx, w.cfg.BatchSize, now.Add(leaseDuration))
	if err != nil {
		return 0, err
	}
	var batchErrors []error
	for _, task := range tasks {
		if err := w.process(ctx, task); err != nil {
			batchErrors = append(batchErrors, err)
			if ctx.Err() != nil {
				return len(tasks), ctx.Err()
			}
		}
	}
	return len(tasks), errors.Join(batchErrors...)
}

func (w *CleanupWorker) process(parent context.Context, task repository.BlobCleanupTask) error {
	ctx, cancel := context.WithTimeout(parent, w.cfg.OperationTimeout)
	defer cancel()
	err := w.blobs.Delete(ctx, task.StorageKey)
	if err == nil || errors.Is(err, blob.ErrNotFound) {
		return w.tasks.CompleteBlobCleanupTask(ctx, task.ID)
	}
	if parent.Err() != nil {
		return parent.Err()
	}
	return w.tasks.RetryBlobCleanupTask(ctx, task.ID, w.cfg.Now().UTC().Add(w.backoff(task.Attempts)))
}

func (w *CleanupWorker) backoff(attempts int) time.Duration {
	if attempts < 1 {
		attempts = 1
	}
	delay := w.cfg.InitialBackoff
	for i := 1; i < attempts && delay < w.cfg.MaxBackoff; i++ {
		if delay > w.cfg.MaxBackoff/2 {
			return w.cfg.MaxBackoff
		}
		delay *= 2
	}
	if delay > w.cfg.MaxBackoff {
		return w.cfg.MaxBackoff
	}
	return delay
}
