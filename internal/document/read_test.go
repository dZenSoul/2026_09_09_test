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

func TestGetJSONDoesNotOpenBlob(t *testing.T) {
	repo := &readDocumentsRepository{document: domain.Document{ID: "doc", JSON: jsonRaw(`[1,"two",null]`)}}
	storage := &readBlobs{}
	service := newTestService(t, repo, &fakeUsers{}, storage, 10)
	requester := domain.User{ID: "reader-id", Login: "reader000"}

	content, err := service.Get(context.Background(), requester, "doc")
	if err != nil {
		t.Fatal(err)
	}
	if string(content.Document.JSON) != `[1,"two",null]` || content.File != nil || storage.opens != 0 {
		t.Fatalf("content=%#v opens=%d", content, storage.opens)
	}
	if repo.id != "doc" || repo.requesterID != requester.ID {
		t.Fatalf("id=%q requester=%q", repo.id, repo.requesterID)
	}
}

func TestGetFileOpensBlobAfterAccessCheck(t *testing.T) {
	repo := &readDocumentsRepository{document: domain.Document{
		ID: "doc", IsFile: true, StorageKey: "opaque", SizeBytes: 99,
	}}
	storage := &readBlobs{object: blob.Object{Body: io.NopCloser(&zeroReader{}), Size: 7}}
	service := newTestService(t, repo, &fakeUsers{}, storage, 10)

	content, err := service.Get(context.Background(), domain.User{ID: "reader-id", Login: "reader000"}, "doc")
	if err != nil {
		t.Fatal(err)
	}
	defer content.File.Close()
	if storage.opens != 1 || storage.key != "opaque" || content.Document.SizeBytes != 7 {
		t.Fatalf("content=%#v opens=%d key=%q", content, storage.opens, storage.key)
	}
}

func TestGetDoesNotOpenBlobWhenAccessFails(t *testing.T) {
	for _, repositoryError := range []error{repository.ErrNotFound, repository.ErrForbidden} {
		repo := &readDocumentsRepository{err: repositoryError}
		storage := &readBlobs{}
		service := newTestService(t, repo, &fakeUsers{}, storage, 10)
		_, err := service.Get(context.Background(), domain.User{ID: "reader-id", Login: "reader000"}, "doc")
		want := domain.ErrNotFound
		if errors.Is(repositoryError, repository.ErrForbidden) {
			want = domain.ErrForbidden
		}
		if !errors.Is(err, want) || storage.opens != 0 {
			t.Errorf("repository error=%v service error=%v opens=%d", repositoryError, err, storage.opens)
		}
	}
}

func TestGetMetadataNeverOpensBlob(t *testing.T) {
	repo := &readDocumentsRepository{document: domain.Document{ID: "doc", IsFile: true, StorageKey: "opaque"}}
	storage := &readBlobs{}
	service := newTestService(t, repo, &fakeUsers{}, storage, 10).(*service)
	if _, err := service.GetMetadata(context.Background(), domain.User{ID: "reader-id", Login: "reader000"}, "doc"); err != nil {
		t.Fatal(err)
	}
	if storage.opens != 0 {
		t.Fatalf("blob opens=%d", storage.opens)
	}
}

type readDocumentsRepository struct {
	document    domain.Document
	id          string
	requesterID string
	err         error
}

func (*readDocumentsRepository) Create(context.Context, domain.Document) (domain.Document, error) {
	return domain.Document{}, errors.New("unused")
}
func (*readDocumentsRepository) ByID(context.Context, string) (domain.Document, error) {
	return domain.Document{}, errors.New("unused")
}
func (f *readDocumentsRepository) ByIDAccessible(_ context.Context, id, requesterID string) (domain.Document, error) {
	f.id, f.requesterID = id, requesterID
	return f.document, f.err
}
func (*readDocumentsRepository) List(context.Context, string, domain.DocumentFilter) ([]domain.Document, error) {
	return nil, errors.New("unused")
}
func (*readDocumentsRepository) Delete(context.Context, string, string) (domain.Document, error) {
	return domain.Document{}, errors.New("unused")
}

type readBlobs struct {
	object blob.Object
	key    string
	opens  int
	err    error
}

func (*readBlobs) Put(context.Context, string, io.Reader) (int64, error) {
	return 0, errors.New("unused")
}
func (f *readBlobs) Open(_ context.Context, key string) (blob.Object, error) {
	f.opens++
	f.key = key
	return f.object, f.err
}
func (*readBlobs) Delete(context.Context, string) error { return errors.New("unused") }

type zeroReader struct{}

func (*zeroReader) Read([]byte) (int, error) { return 0, io.EOF }
