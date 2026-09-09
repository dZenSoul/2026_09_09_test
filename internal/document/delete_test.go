package document

import (
	"context"
	"errors"
	"io"
	"testing"

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
		if !errors.Is(err, domain.ErrForbidden) || repo.deleteCalls != 0 {
			t.Fatalf("error=%v delete calls=%d", err, repo.deleteCalls)
		}
	})
}

func TestDeleteFileFailureAndRetry(t *testing.T) {
	owner := domain.User{ID: "owner-id", Login: "owner000"}
	document := domain.Document{ID: "doc", OwnerID: owner.ID, IsFile: true, StorageKey: "opaque"}

	t.Run("blob failure keeps metadata", func(t *testing.T) {
		repo := &deleteDocuments{document: document}
		storage := &deleteBlobs{err: errors.New("disk failure")}
		service := newTestService(t, repo, &fakeUsers{}, storage, 10)
		if err := service.Delete(context.Background(), owner, "doc"); err == nil {
			t.Fatal("expected blob error")
		}
		if repo.deleted || repo.deleteCalls != 0 {
			t.Fatalf("metadata removed after blob failure: %#v", repo)
		}
	})

	t.Run("database failure can be retried after blob removal", func(t *testing.T) {
		repo := &deleteDocuments{document: document, deleteErr: repository.ErrInternal}
		storage := &deleteBlobs{}
		service := newTestService(t, repo, &fakeUsers{}, storage, 10)
		if err := service.Delete(context.Background(), owner, "doc"); err == nil || repo.deleted {
			t.Fatalf("first error=%v deleted=%v", err, repo.deleted)
		}
		if storage.calls != 1 || storage.key != "opaque" {
			t.Fatalf("blob calls=%d key=%q", storage.calls, storage.key)
		}
		repo.deleteErr = nil
		storage.err = blob.ErrNotFound
		if err := service.Delete(context.Background(), owner, "doc"); err != nil {
			t.Fatalf("retry: %v", err)
		}
		if !repo.deleted || storage.calls != 2 {
			t.Fatalf("deleted=%v blob calls=%d", repo.deleted, storage.calls)
		}
	})
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
func (f *deleteDocuments) Delete(context.Context, string, string) (domain.Document, error) {
	f.deleteCalls++
	if f.deleteErr != nil {
		return domain.Document{}, f.deleteErr
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
