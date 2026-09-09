package document

import (
	"context"
	"errors"
	"fmt"
	"io"
	"testing"
	"time"

	"documents/internal/blob"
	"documents/internal/domain"
	"documents/internal/repository"
)

func TestDeleteJSONAndOwnership(t *testing.T) {
	owner := domain.User{ID: "owner-id", Login: "owner000"}

	t.Run("owner deletes JSON metadata", func(t *testing.T) {
		repo := &deleteDocuments{document: domain.Document{ID: "doc", OwnerID: owner.ID}}
		storage := &deleteBlobs{}
		service := newTestService(t, repo, &fakeUsers{}, storage, 10)
		if err := service.Delete(context.Background(), owner, "doc"); err != nil {
			t.Fatal(err)
		}
		if !repo.deleted || storage.calls != 0 {
			t.Fatalf("deleted=%v blob calls=%d", repo.deleted, storage.calls)
		}
		if err := service.Delete(context.Background(), owner, "doc"); !errors.Is(err, domain.ErrNotFound) {
			t.Fatalf("repeat delete error=%v", err)
		}
	})

	t.Run("public and grants do not let another user delete", func(t *testing.T) {
		repo := &deleteDocuments{document: domain.Document{
			ID: "doc", OwnerID: owner.ID, IsPublic: true, Grants: []string{"reader000"},
		}}
		service := newTestService(t, repo, &fakeUsers{}, &deleteBlobs{}, 10)
		err := service.Delete(context.Background(), domain.User{ID: "reader-id", Login: "reader000"}, "doc")
		if !errors.Is(err, domain.ErrForbidden) || repo.deleteCalls != 1 {
			t.Fatalf("error=%v delete calls=%d", err, repo.deleteCalls)
		}
	})
}

func TestDeleteFileFailureAndRetry(t *testing.T) {
	owner := domain.User{ID: "owner-id", Login: "owner000"}
	document := domain.Document{ID: "doc", OwnerID: owner.ID, IsFile: true, StorageKey: "opaque"}

	t.Run("blob failure keeps cleanup work after logical deletion", func(t *testing.T) {
		repo := &deleteDocuments{document: document}
		storage := &deleteBlobs{err: errors.New("disk failure")}
		service := newTestService(t, repo, &fakeUsers{}, storage, 10)
		if err := service.Delete(context.Background(), owner, "doc"); err == nil {
			t.Fatal("expected blob error")
		}
		if !repo.deleted || repo.deleteCalls != 1 {
			t.Fatalf("metadata was not removed before blob failure: %#v", repo)
		}
	})

	t.Run("database failure does not touch blob and can be retried", func(t *testing.T) {
		repo := &deleteDocuments{document: document, deleteErr: repository.ErrInternal}
		storage := &deleteBlobs{}
		service := newTestService(t, repo, &fakeUsers{}, storage, 10)
		if err := service.Delete(context.Background(), owner, "doc"); err == nil || repo.deleted {
			t.Fatalf("first error=%v deleted=%v", err, repo.deleted)
		}
		if storage.calls != 0 {
			t.Fatalf("blob calls=%d key=%q", storage.calls, storage.key)
		}
		repo.deleteErr = nil
		if err := service.Delete(context.Background(), owner, "doc"); err != nil {
			t.Fatalf("retry: %v", err)
		}
		if !repo.deleted || storage.calls != 1 {
			t.Fatalf("deleted=%v blob calls=%d", repo.deleted, storage.calls)
		}
	})
}

func TestDeletePostCommitOrderAndIndependentCleanupContext(t *testing.T) {
	owner := domain.User{ID: "owner-id", Login: "owner000"}
	order := []string{}
	requestCtx, cancelRequest := context.WithCancel(context.Background())
	repo := &orderedDeleteRepository{
		deleteDocuments: &deleteDocuments{document: domain.Document{
			ID: "doc", OwnerID: owner.ID, IsFile: true, StorageKey: "opaque",
		}},
		order: &order, cancelRequest: cancelRequest,
	}
	storage := &orderedDeleteStorage{order: &order}
	service, err := NewService(repo, &fakeUsers{}, storage, Config{
		MaxFileBytes: 10, MaxGrantItems: 10, CleanupTimeout: time.Second,
		InvalidateDelete: func(ctx context.Context, _ string) error {
			if ctx.Err() != nil {
				t.Fatalf("invalidation inherited canceled request: %v", ctx.Err())
			}
			order = append(order, "invalidate")
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Delete(requestCtx, owner, "doc"); err != nil {
		t.Fatal(err)
	}
	if got, want := fmt.Sprint(order), "[commit invalidate blob acknowledge]"; got != want {
		t.Fatalf("operation order = %s, want %s", got, want)
	}
}

type deleteDocuments struct {
	document    domain.Document
	byIDErr     error
	deleteErr   error
	deleted     bool
	deleteCalls int
}

func (*deleteDocuments) Create(context.Context, domain.Document) (domain.Document, error) {
	return domain.Document{}, errors.New("unused")
}
func (f *deleteDocuments) ByID(context.Context, string) (domain.Document, error) {
	if f.deleted {
		return domain.Document{}, repository.ErrNotFound
	}
	return f.document, f.byIDErr
}
func (*deleteDocuments) ByIDAccessible(context.Context, string, string) (domain.Document, error) {
	return domain.Document{}, errors.New("unused")
}
func (*deleteDocuments) List(context.Context, string, domain.DocumentFilter) ([]domain.Document, error) {
	return nil, errors.New("unused")
}
func (f *deleteDocuments) Delete(_ context.Context, _ string, ownerID string) (domain.Document, error) {
	f.deleteCalls++
	if f.deleted {
		return domain.Document{}, repository.ErrNotFound
	}
	if f.deleteErr != nil {
		return domain.Document{}, f.deleteErr
	}
	if f.document.OwnerID != ownerID {
		return domain.Document{}, repository.ErrForbidden
	}
	f.deleted = true
	return f.document, nil
}

type deleteBlobs struct {
	key   string
	calls int
	err   error
}

func (*deleteBlobs) Put(context.Context, string, io.Reader) (int64, error) {
	return 0, errors.New("unused")
}
func (*deleteBlobs) Open(context.Context, string) (blob.Object, error) {
	return blob.Object{}, errors.New("unused")
}
func (f *deleteBlobs) Delete(_ context.Context, key string) error {
	f.calls++
	f.key = key
	return f.err
}

type orderedDeleteRepository struct {
	*deleteDocuments
	order         *[]string
	cancelRequest context.CancelFunc
}

func (f *orderedDeleteRepository) Delete(ctx context.Context, id, ownerID string) (domain.Document, error) {
	document, err := f.deleteDocuments.Delete(ctx, id, ownerID)
	if err == nil {
		*f.order = append(*f.order, "commit")
		f.cancelRequest()
	}
	return document, err
}
func (*orderedDeleteRepository) ClaimBlobCleanupTasks(context.Context, int, time.Time) ([]repository.BlobCleanupTask, error) {
	return nil, nil
}
func (*orderedDeleteRepository) CompleteBlobCleanupTask(context.Context, int64) error { return nil }
func (f *orderedDeleteRepository) CompleteBlobCleanupByStorageKey(context.Context, string) error {
	*f.order = append(*f.order, "acknowledge")
	return nil
}
func (*orderedDeleteRepository) RetryBlobCleanupTask(context.Context, int64, time.Time) error {
	return nil
}

type orderedDeleteStorage struct{ order *[]string }

func (*orderedDeleteStorage) Put(context.Context, string, io.Reader) (int64, error) {
	return 0, errors.New("unused")
}
func (*orderedDeleteStorage) Open(context.Context, string) (blob.Object, error) {
	return blob.Object{}, errors.New("unused")
}
func (f *orderedDeleteStorage) Delete(ctx context.Context, _ string) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	*f.order = append(*f.order, "blob")
	return nil
}
