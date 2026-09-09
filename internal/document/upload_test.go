package document

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"

	"documents/internal/blob"
	"documents/internal/domain"
	"documents/internal/repository"
)

func TestUploadJSONAndGrantValidation(t *testing.T) {
	repo := &fakeDocuments{}
	users := &fakeUsers{existing: []string{"reader000"}}
	storage := newFakeBlobs()
	service := newTestService(t, repo, users, storage, 10)
	owner := domain.User{ID: "owner-id", Login: "owner000"}

	created, err := service.Upload(context.Background(), owner, Upload{Document: domain.Document{
		Name: "record", JSON: jsonRaw(`null`), Grants: []string{"reader000", "reader000", "owner000"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if string(created.JSON) != "null" || len(created.Grants) != 1 || created.Grants[0] != "reader000" {
		t.Fatalf("created=%#v", created)
	}
	if repo.created.ID == "" || repo.created.OwnerID != owner.ID || repo.created.StorageKey != "" {
		t.Fatalf("repository input=%#v", repo.created)
	}

	_, err = service.Upload(context.Background(), owner, Upload{Document: domain.Document{
		Name: "record", JSON: jsonRaw(`{}`), Grants: []string{"missing00"},
	}})
	if !errors.Is(err, domain.ErrInvalidArgument) || repo.calls != 1 {
		t.Fatalf("missing grant error=%v repository calls=%d", err, repo.calls)
	}
}

func TestUploadFileLimitAndCompensation(t *testing.T) {
	owner := domain.User{ID: "owner-id", Login: "owner000"}
	users := &fakeUsers{}

	t.Run("maximum size succeeds", func(t *testing.T) {
		repo := &fakeDocuments{}
		storage := newFakeBlobs()
		service := newTestService(t, repo, users, storage, 4)
		created, err := service.Upload(context.Background(), owner, Upload{
			Document: domain.Document{Name: "file.bin", IsFile: true, MIME: "APPLICATION/OCTET-STREAM"},
			File:     bytes.NewReader([]byte("1234")),
		})
		if err != nil || created.SizeBytes != 4 || created.MIME != "application/octet-stream" {
			t.Fatalf("created=%#v err=%v", created, err)
		}
		if created.StorageKey == "" || created.StorageKey == created.Name || string(storage.objects[created.StorageKey]) != "1234" {
			t.Fatalf("unsafe or missing blob: %#v %#v", created, storage.objects)
		}
	})

	t.Run("empty file succeeds", func(t *testing.T) {
		repo := &fakeDocuments{}
		storage := newFakeBlobs()
		service := newTestService(t, repo, users, storage, 4)
		created, err := service.Upload(context.Background(), owner, Upload{
			Document: domain.Document{Name: "empty.bin", IsFile: true, MIME: "application/octet-stream"},
			File:     bytes.NewReader(nil),
		})
		if err != nil || created.SizeBytes != 0 {
			t.Fatalf("created=%#v err=%v", created, err)
		}
	})

	t.Run("oversized blob is removed", func(t *testing.T) {
		repo := &fakeDocuments{}
		storage := newFakeBlobs()
		service := newTestService(t, repo, users, storage, 4)
		_, err := service.Upload(context.Background(), owner, Upload{
			Document: domain.Document{Name: "file.bin", IsFile: true, MIME: "application/octet-stream"},
			File:     bytes.NewReader([]byte("12345")),
		})
		if !errors.Is(err, domain.ErrInvalidArgument) || repo.calls != 0 || len(storage.objects) != 0 || storage.deletes != 1 {
			t.Fatalf("error=%v calls=%d blobs=%v deletes=%d", err, repo.calls, storage.objects, storage.deletes)
		}
	})

	t.Run("database failure removes blob", func(t *testing.T) {
		repo := &fakeDocuments{err: repository.ErrInternal}
		storage := newFakeBlobs()
		service := newTestService(t, repo, users, storage, 4)
		_, err := service.Upload(context.Background(), owner, Upload{
			Document: domain.Document{Name: "file.bin", IsFile: true, MIME: "application/octet-stream"},
			File:     bytes.NewReader([]byte("1234")),
		})
		if err == nil || len(storage.objects) != 0 || storage.deletes != 1 {
			t.Fatalf("error=%v blobs=%v deletes=%d", err, storage.objects, storage.deletes)
		}
	})

	t.Run("blob failure never creates metadata", func(t *testing.T) {
		repo := &fakeDocuments{}
		storage := newFakeBlobs()
		storage.putErr = errors.New("disk full")
		service := newTestService(t, repo, users, storage, 4)
		_, err := service.Upload(context.Background(), owner, Upload{
			Document: domain.Document{Name: "file.bin", IsFile: true, MIME: "application/octet-stream"},
			File:     bytes.NewReader([]byte("1234")),
		})
		if err == nil || repo.calls != 0 || len(storage.objects) != 0 {
			t.Fatalf("error=%v calls=%d blobs=%v", err, repo.calls, storage.objects)
		}
	})
}

func TestUploadRejectsInvalidDocumentMetadata(t *testing.T) {
	owner := domain.User{ID: "owner-id", Login: "owner000"}
	tests := []Upload{
		{Document: domain.Document{Name: "../escape", JSON: jsonRaw(`{}`)}},
		{Document: domain.Document{Name: "bad\nname", JSON: jsonRaw(`{}`)}},
		{Document: domain.Document{Name: "value", JSON: jsonRaw(`{`)}},
		{Document: domain.Document{Name: "file.bin", IsFile: true, MIME: "not a mime"}, File: bytes.NewReader(nil)},
		{Document: domain.Document{Name: "file.bin", IsFile: true, MIME: "text/plain"}},
	}
	for index, input := range tests {
		repo := &fakeDocuments{}
		service := newTestService(t, repo, &fakeUsers{}, newFakeBlobs(), 4)
		if _, err := service.Upload(context.Background(), owner, input); !errors.Is(err, domain.ErrInvalidArgument) {
			t.Errorf("case %d error=%v", index, err)
		}
		if repo.calls != 0 {
			t.Errorf("case %d created metadata", index)
		}
	}
}

func newTestService(t *testing.T, documents repository.DocumentRepository, users repository.UserRepository, storage blob.Storage, maxFile int64) Service {
	t.Helper()
	service, err := NewService(documents, users, storage, Config{
		MaxFileBytes: maxFile, MaxGrantItems: 3,
		Rand: bytes.NewReader(bytes.Repeat([]byte{7}, 256)),
	})
	if err != nil {
		t.Fatal(err)
	}
	return service
}

func jsonRaw(value string) []byte { return []byte(value) }

type fakeDocuments struct {
	created domain.Document
	calls   int
	err     error
}

func (f *fakeDocuments) Create(_ context.Context, input domain.Document) (domain.Document, error) {
	f.calls++
	f.created = input
	return input, f.err
}
func (*fakeDocuments) ByID(context.Context, string) (domain.Document, error) {
	return domain.Document{}, errors.New("unused")
}
func (*fakeDocuments) ByIDAccessible(context.Context, string, string) (domain.Document, error) {
	return domain.Document{}, errors.New("unused")
}
func (*fakeDocuments) List(context.Context, string, domain.DocumentFilter) ([]domain.Document, error) {
	return nil, errors.New("unused")
}
func (*fakeDocuments) Delete(context.Context, string, string) (domain.Document, error) {
	return domain.Document{}, errors.New("unused")
}

type fakeUsers struct{ existing []string }

func (*fakeUsers) Create(context.Context, string, []byte) (domain.User, error) {
	return domain.User{}, errors.New("unused")
}
func (*fakeUsers) ByLogin(context.Context, string) (domain.User, []byte, error) {
	return domain.User{}, nil, errors.New("unused")
}
func (f *fakeUsers) ExistingLogins(context.Context, []string) ([]string, error) {
	return f.existing, nil
}

type fakeBlobs struct {
	objects map[string][]byte
	deletes int
	putErr  error
}

func newFakeBlobs() *fakeBlobs { return &fakeBlobs{objects: make(map[string][]byte)} }
func (f *fakeBlobs) Put(_ context.Context, key string, source io.Reader) (int64, error) {
	content, err := io.ReadAll(source)
	if err == nil && f.putErr == nil {
		f.objects[key] = content
	}
	return int64(len(content)), errors.Join(err, f.putErr)
}
func (f *fakeBlobs) Open(_ context.Context, key string) (blob.Object, error) {
	content, found := f.objects[key]
	if !found {
		return blob.Object{}, blob.ErrNotFound
	}
	return blob.Object{Body: io.NopCloser(bytes.NewReader(content)), Size: int64(len(content))}, nil
}
func (f *fakeBlobs) Delete(_ context.Context, key string) error {
	f.deletes++
	delete(f.objects, key)
	return nil
}
