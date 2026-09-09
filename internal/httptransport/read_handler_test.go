package httptransport

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"documents/internal/document"
	"documents/internal/domain"
)

func TestReadJSONPreservesEveryJSONShape(t *testing.T) {
	values := []string{`{"key":"value"}`, `[1,2]`, `"text"`, `42`, `true`, `null`}
	for _, value := range values {
		auth := &readAuth{user: domain.User{ID: "reader-id", Login: "reader000"}}
		documents := &readDocumentService{content: document.Content{Document: domain.Document{JSON: []byte(value)}}}
		handler := NewHandler(Dependencies{Auth: auth, Documents: documents})
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/docs/doc-id?token=session", nil))
		want := `{"data":` + value + "}\n"
		if response.Code != http.StatusOK || response.Body.String() != want {
			t.Errorf("value=%s status=%d body=%q want=%q", value, response.Code, response.Body, want)
		}
	}
}

func TestReadFileAndHEAD(t *testing.T) {
	metadata := domain.Document{Name: `report "final".txt`, MIME: "text/plain; charset=utf-8", IsFile: true, SizeBytes: 5}
	auth := &readAuth{user: domain.User{ID: "reader-id", Login: "reader000"}}
	documents := &readDocumentService{
		content:  document.Content{Document: metadata, File: io.NopCloser(bytes.NewReader([]byte{0, 1, 2, 3, 255}))},
		metadata: metadata,
	}
	handler := NewHandler(Dependencies{Auth: auth, Documents: documents})

	get := httptest.NewRecorder()
	handler.ServeHTTP(get, httptest.NewRequest(http.MethodGet, "/api/docs/doc-id?token=session", nil))
	if get.Code != http.StatusOK || !bytes.Equal(get.Body.Bytes(), []byte{0, 1, 2, 3, 255}) {
		t.Fatalf("status=%d body=%v", get.Code, get.Body.Bytes())
	}
	if get.Header().Get("Content-Type") != metadata.MIME || get.Header().Get("Content-Length") != "5" || get.Header().Get("Content-Disposition") != `inline; filename="report \"final\".txt"` {
		t.Fatalf("headers=%v", get.Header())
	}

	head := httptest.NewRecorder()
	handler.ServeHTTP(head, httptest.NewRequest(http.MethodHead, "/api/docs/doc-id?token=session", nil))
	if head.Code != get.Code || head.Header().Get("Content-Type") != get.Header().Get("Content-Type") || head.Header().Get("Content-Length") != get.Header().Get("Content-Length") || head.Header().Get("Content-Disposition") != get.Header().Get("Content-Disposition") || head.Body.Len() != 0 {
		t.Fatalf("GET=%v HEAD=%v body=%q", get.Result(), head.Result(), head.Body)
	}
	if documents.getCalls != 1 || documents.metadataCalls != 1 {
		t.Fatalf("get calls=%d metadata calls=%d", documents.getCalls, documents.metadataCalls)
	}
}

func TestReadAuthorizationAndErrors(t *testing.T) {
	tests := []struct {
		name   string
		path   string
		auth   error
		read   error
		status int
	}{
		{"anonymous", "/api/docs/doc-id", domain.ErrUnauthorized, nil, 401},
		{"expired", "/api/docs/doc-id?token=expired", domain.ErrUnauthorized, nil, 401},
		{"forbidden", "/api/docs/doc-id?token=session", nil, domain.ErrForbidden, 403},
		{"missing", "/api/docs/doc-id?token=session", nil, domain.ErrNotFound, 404},
		{"duplicate token", "/api/docs/doc-id?token=a&token=b", nil, nil, 400},
		{"token outside query", "/api/docs/doc-id?other=session", nil, nil, 400},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			auth := &readAuth{user: domain.User{ID: "reader-id", Login: "reader000"}, err: test.auth}
			documents := &readDocumentService{err: test.read}
			handler := NewHandler(Dependencies{Auth: auth, Documents: documents})
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, test.path, nil))
			if response.Code != test.status {
				t.Fatalf("status=%d body=%s", response.Code, response.Body)
			}
			if test.auth != nil && documents.getCalls != 0 {
				t.Fatalf("read before successful authorization")
			}
		})
	}
}

func TestReadErrorHEADHasNoBody(t *testing.T) {
	handler := NewHandler(Dependencies{
		Auth:      &readAuth{user: domain.User{ID: "reader-id", Login: "reader000"}},
		Documents: &readDocumentService{err: domain.ErrForbidden},
	})
	get := httptest.NewRecorder()
	handler.ServeHTTP(get, httptest.NewRequest(http.MethodGet, "/api/docs/doc-id?token=session", nil))
	head := httptest.NewRecorder()
	handler.ServeHTTP(head, httptest.NewRequest(http.MethodHead, "/api/docs/doc-id?token=session", nil))
	if head.Code != get.Code || head.Header().Get("Content-Type") != get.Header().Get("Content-Type") || head.Header().Get("Content-Length") != get.Header().Get("Content-Length") || head.Body.Len() != 0 {
		t.Fatalf("GET=%v HEAD=%v head body=%q", get.Result(), head.Result(), head.Body)
	}
}

type readAuth struct {
	user  domain.User
	token string
	err   error
}

func (*readAuth) Register(context.Context, string, string, string) (domain.User, error) {
	return domain.User{}, errors.New("unused")
}
func (*readAuth) Authenticate(context.Context, string, string) (string, error) {
	return "", errors.New("unused")
}
func (f *readAuth) Authorize(_ context.Context, token string) (domain.User, error) {
	f.token = token
	return f.user, f.err
}
func (*readAuth) Logout(context.Context, string) error { return errors.New("unused") }

type readDocumentService struct {
	content       document.Content
	metadata      domain.Document
	err           error
	getCalls      int
	metadataCalls int
}

func (*readDocumentService) Upload(context.Context, domain.User, document.Upload) (domain.Document, error) {
	return domain.Document{}, errors.New("unused")
}
func (*readDocumentService) List(context.Context, domain.User, domain.DocumentFilter) ([]domain.Document, error) {
	return nil, errors.New("unused")
}
func (f *readDocumentService) Get(context.Context, domain.User, string) (document.Content, error) {
	f.getCalls++
	return f.content, f.err
}
func (f *readDocumentService) GetMetadata(context.Context, domain.User, string) (domain.Document, error) {
	f.metadataCalls++
	return f.metadata, f.err
}
func (*readDocumentService) Delete(context.Context, domain.User, string) error {
	return errors.New("unused")
}
