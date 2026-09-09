package httptransport

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"documents/internal/document"
	"documents/internal/domain"
)

func TestDeleteDocumentUsesTokenFromBody(t *testing.T) {
	auth := &readAuth{user: domain.User{ID: "owner-id", Login: "owner000"}}
	documents := &deleteDocumentService{}
	handler := NewHandler(Dependencies{Auth: auth, Documents: documents})
	response := deleteFormRequest(handler, "/api/docs/doc-id", url.Values{"token": {"secret-session"}})

	if response.Code != http.StatusOK || response.Body.String() != "{\"response\":{\"doc-id\":true}}\n" {
		t.Fatalf("status=%d body=%q", response.Code, response.Body)
	}
	if auth.token != "secret-session" || documents.id != "doc-id" || documents.requester.ID != "owner-id" {
		t.Fatalf("token=%q id=%q requester=%#v", auth.token, documents.id, documents.requester)
	}
}

func TestDeleteDocumentRejectsTokenInURLAndMalformedBody(t *testing.T) {
	tests := []struct {
		name        string
		path        string
		contentType string
		body        string
	}{
		{"query token", "/api/docs/doc-id?token=secret", "application/x-www-form-urlencoded", ""},
		{"missing content type", "/api/docs/doc-id", "", "token=secret"},
		{"unknown field", "/api/docs/doc-id", "application/x-www-form-urlencoded", "token=secret&other=x"},
		{"duplicate token", "/api/docs/doc-id", "application/x-www-form-urlencoded", "token=a&token=b"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			auth := &readAuth{user: domain.User{ID: "owner-id", Login: "owner000"}}
			documents := &deleteDocumentService{}
			handler := NewHandler(Dependencies{Auth: auth, Documents: documents})
			request := httptest.NewRequest(http.MethodDelete, test.path, strings.NewReader(test.body))
			if test.contentType != "" {
				request.Header.Set("Content-Type", test.contentType)
			}
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != http.StatusBadRequest || documents.calls != 0 || auth.token != "" {
				t.Fatalf("status=%d auth token=%q delete calls=%d", response.Code, auth.token, documents.calls)
			}
		})
	}
}

func TestDeleteDocumentMapsAuthorizationAndServiceErrors(t *testing.T) {
	tests := []struct {
		name   string
		auth   error
		remove error
		status int
	}{
		{"unauthorized", domain.ErrUnauthorized, nil, http.StatusUnauthorized},
		{"forbidden", nil, domain.ErrForbidden, http.StatusForbidden},
		{"missing", nil, domain.ErrNotFound, http.StatusNotFound},
		{"failure", nil, errors.New("storage unavailable"), http.StatusInternalServerError},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			auth := &readAuth{user: domain.User{ID: "owner-id", Login: "owner000"}, err: test.auth}
			documents := &deleteDocumentService{err: test.remove}
			response := deleteFormRequest(NewHandler(Dependencies{Auth: auth, Documents: documents}), "/api/docs/doc-id", url.Values{"token": {"secret"}})
			if response.Code != test.status {
				t.Fatalf("status=%d body=%s", response.Code, response.Body)
			}
			if test.auth != nil && documents.calls != 0 {
				t.Fatal("delete called before authorization")
			}
		})
	}
}

func deleteFormRequest(handler http.Handler, path string, values url.Values) *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodDelete, path, strings.NewReader(values.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

type deleteDocumentService struct {
	requester domain.User
	id        string
	calls     int
	err       error
}

func (*deleteDocumentService) Upload(context.Context, domain.User, document.Upload) (domain.Document, error) {
	return domain.Document{}, errors.New("unused")
}
func (*deleteDocumentService) List(context.Context, domain.User, domain.DocumentFilter) ([]domain.Document, error) {
	return nil, errors.New("unused")
}
func (*deleteDocumentService) Get(context.Context, domain.User, string) (document.Content, error) {
	return document.Content{}, errors.New("unused")
}
func (f *deleteDocumentService) Delete(_ context.Context, requester domain.User, id string) error {
	f.calls++
	f.requester, f.id = requester, id
	return f.err
}
