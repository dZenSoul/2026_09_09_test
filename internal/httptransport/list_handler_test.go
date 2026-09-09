package httptransport

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"documents/internal/document"
	"documents/internal/domain"
)

func TestDocumentListHTTPAndHEAD(t *testing.T) {
	auth := &listAuth{user: domain.User{ID: "reader-id", Login: "reader000"}}
	documents := &listDocumentService{documents: []domain.Document{
		{ID: "one", Name: "a", IsPublic: true, CreatedAt: time.Date(2026, 9, 9, 13, 30, 0, 0, time.FixedZone("offset", 3*60*60))},
		{ID: "two", Name: "b.pdf", MIME: "application/pdf", IsFile: true, Grants: []string{"reader000"}, CreatedAt: time.Date(2026, 9, 9, 10, 0, 0, 0, time.UTC)},
	}}
	handler := NewHandler(Dependencies{Auth: auth, Documents: documents, Limits: Limits{MaxListLimit: 10}})
	path := "/api/docs?token=session&login=owner000&key=public&value=true&limit=2"

	get := httptest.NewRecorder()
	handler.ServeHTTP(get, httptest.NewRequest(http.MethodGet, path, nil))
	if get.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", get.Code, get.Body)
	}
	if auth.token != "session" || documents.requester != auth.user || documents.filter != (domain.DocumentFilter{OwnerLogin: "owner000", Key: "public", Value: "true", Limit: 2}) {
		t.Fatalf("token=%q requester=%#v filter=%#v", auth.token, documents.requester, documents.filter)
	}
	var body struct {
		Data struct {
			Docs []map[string]any `json:"docs"`
		} `json:"data"`
	}
	if err := json.Unmarshal(get.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Data.Docs) != 2 || body.Data.Docs[0]["created"] != "2026-09-09 10:30:00" {
		t.Fatalf("body=%s", get.Body)
	}
	if _, present := body.Data.Docs[0]["mime"]; present {
		t.Fatalf("mime must be omitted: %s", get.Body)
	}

	head := httptest.NewRecorder()
	handler.ServeHTTP(head, httptest.NewRequest(http.MethodHead, path, nil))
	if head.Code != get.Code || head.Header().Get("Content-Type") != get.Header().Get("Content-Type") || head.Header().Get("Content-Length") != get.Header().Get("Content-Length") || head.Body.Len() != 0 {
		t.Fatalf("GET=%v HEAD=%v head body=%q", get.Result(), head.Result(), head.Body)
	}
}

func TestDocumentListHTTPEmptyAndValidation(t *testing.T) {
	auth := &listAuth{user: domain.User{ID: "reader-id", Login: "reader000"}}
	documents := &listDocumentService{}
	handler := NewHandler(Dependencies{Auth: auth, Documents: documents, Limits: Limits{MaxListLimit: 3}})

	empty := httptest.NewRecorder()
	handler.ServeHTTP(empty, httptest.NewRequest(http.MethodGet, "/api/docs?token=session", nil))
	if empty.Code != http.StatusOK || empty.Body.String() != "{\"data\":{\"docs\":[]}}\n" {
		t.Fatalf("status=%d body=%q", empty.Code, empty.Body)
	}

	badPaths := []string{
		"/api/docs?token=session&key=name",
		"/api/docs?token=session&value=x",
		"/api/docs?token=session&key=public&value=yes",
		"/api/docs?token=session&limit=0",
		"/api/docs?token=session&limit=4",
		"/api/docs?token=session&extra=x",
		"/api/docs?token=session&token=again",
	}
	for _, path := range badPaths {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
		if response.Code != http.StatusBadRequest {
			t.Errorf("path=%q status=%d body=%s", path, response.Code, response.Body)
		}
	}
}

func TestDocumentListAuthorizesBeforeFilterValidation(t *testing.T) {
	auth := &listAuth{err: domain.ErrUnauthorized}
	documents := &listDocumentService{}
	handler := NewHandler(Dependencies{Auth: auth, Documents: documents})
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/docs?token=expired&limit=bad", nil))
	if response.Code != http.StatusUnauthorized || documents.calls != 0 {
		t.Fatalf("status=%d list calls=%d", response.Code, documents.calls)
	}
}

type listAuth struct {
	user  domain.User
	token string
	err   error
}

func (*listAuth) Register(context.Context, string, string, string) (domain.User, error) {
	return domain.User{}, errors.New("unused")
}
func (*listAuth) Authenticate(context.Context, string, string) (string, error) {
	return "", errors.New("unused")
}
func (f *listAuth) Authorize(_ context.Context, token string) (domain.User, error) {
	f.token = token
	return f.user, f.err
}
func (*listAuth) Logout(context.Context, string) error { return errors.New("unused") }

type listDocumentService struct {
	documents []domain.Document
	requester domain.User
	filter    domain.DocumentFilter
	calls     int
	err       error
}

func (*listDocumentService) Upload(context.Context, domain.User, document.Upload) (domain.Document, error) {
	return domain.Document{}, errors.New("unused")
}
func (f *listDocumentService) List(_ context.Context, requester domain.User, filter domain.DocumentFilter) ([]domain.Document, error) {
	f.calls++
	f.requester, f.filter = requester, filter
	return f.documents, f.err
}
func (*listDocumentService) Get(context.Context, domain.User, string) (document.Content, error) {
	return document.Content{}, errors.New("unused")
}
func (*listDocumentService) Delete(context.Context, domain.User, string) error {
	return errors.New("unused")
}
