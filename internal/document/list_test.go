package document

import (
	"context"
	"errors"
	"testing"
	"time"

	"documents/internal/domain"
	"documents/internal/repository"
)

func TestListValidatesAndDelegates(t *testing.T) {
	repo := &listDocumentsRepository{documents: nil}
	service := newTestService(t, repo, &fakeUsers{}, newFakeBlobs(), 4)
	requester := domain.User{ID: "requester-id", Login: "reader000"}
	filter := domain.DocumentFilter{OwnerLogin: "owner000", Key: "public", Value: "true", Limit: 3}

	documents, err := service.List(context.Background(), requester, filter)
	if err != nil {
		t.Fatal(err)
	}
	if documents == nil || len(documents) != 0 {
		t.Fatalf("documents=%#v", documents)
	}
	if repo.requesterID != requester.ID || repo.filter != filter {
		t.Fatalf("requester=%q filter=%#v", repo.requesterID, repo.filter)
	}
}

func TestListRejectsInvalidFiltersBeforeRepository(t *testing.T) {
	tests := []domain.DocumentFilter{
		{Key: "unknown", Value: "x"},
		{Key: "name"},
		{Value: "x"},
		{Key: "id", Value: "not-a-uuid"},
		{Key: "file", Value: "1"},
		{Key: "created", Value: "tomorrow"},
		{Limit: -1},
		{Limit: 101},
	}
	for _, filter := range tests {
		repo := &listDocumentsRepository{}
		service := newTestService(t, repo, &fakeUsers{}, newFakeBlobs(), 4)
		_, err := service.List(context.Background(), domain.User{ID: "id", Login: "reader000"}, filter)
		if !errors.Is(err, domain.ErrInvalidArgument) || repo.calls != 0 {
			t.Errorf("filter=%#v err=%v calls=%d", filter, err, repo.calls)
		}
	}
}

func TestListMapsRepositoryErrorsAndNormalizesDocuments(t *testing.T) {
	created := time.Date(2026, 9, 9, 13, 15, 0, 0, time.FixedZone("offset", 3*60*60))
	repo := &listDocumentsRepository{documents: []domain.Document{{ID: "doc", CreatedAt: created}}}
	service := newTestService(t, repo, &fakeUsers{}, newFakeBlobs(), 4)
	documents, err := service.List(context.Background(), domain.User{ID: "id", Login: "reader000"}, domain.DocumentFilter{})
	if err != nil || documents[0].CreatedAt.Location() != time.UTC || documents[0].Grants == nil {
		t.Fatalf("documents=%#v err=%v", documents, err)
	}

	repo.err = repository.ErrNotFound
	if _, err := service.List(context.Background(), domain.User{ID: "id", Login: "reader000"}, domain.DocumentFilter{}); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("error=%v", err)
	}
}

type listDocumentsRepository struct {
	documents   []domain.Document
	requesterID string
	filter      domain.DocumentFilter
	calls       int
	err         error
}

func (*listDocumentsRepository) Create(context.Context, domain.Document) (domain.Document, error) {
	return domain.Document{}, errors.New("unused")
}
func (*listDocumentsRepository) ByID(context.Context, string) (domain.Document, error) {
	return domain.Document{}, errors.New("unused")
}
func (*listDocumentsRepository) ByIDAccessible(context.Context, string, string) (domain.Document, error) {
	return domain.Document{}, errors.New("unused")
}
func (f *listDocumentsRepository) List(_ context.Context, requesterID string, filter domain.DocumentFilter) ([]domain.Document, error) {
	f.calls++
	f.requesterID, f.filter = requesterID, filter
	return f.documents, f.err
}
func (*listDocumentsRepository) Delete(context.Context, string, string) (domain.Document, error) {
	return domain.Document{}, errors.New("unused")
}
