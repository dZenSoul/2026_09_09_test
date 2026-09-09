package httptransport

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	responsecache "documents/internal/cache"
	"documents/internal/document"
	"documents/internal/domain"
)

func TestResponseCacheGETAndHEADShareEntryAndReauthorize(t *testing.T) {
	auth := &countingAuth{user: domain.User{ID: "reader-id", Login: "reader"}}
	documents := &countingDocuments{metadata: domain.Document{ID: "doc", Version: 7, IsFile: true, Name: "x.txt", MIME: "text/plain", SizeBytes: 4}}
	handler := NewHandler(Dependencies{Auth: auth, Documents: documents, Cache: responsecache.NewMemory(4096, 10), CacheTTL: time.Minute, ExposeCacheHeader: true})

	get := httptest.NewRecorder()
	handler.ServeHTTP(get, httptest.NewRequest(http.MethodGet, "/api/docs/doc?token=secret", nil))
	head := httptest.NewRecorder()
	handler.ServeHTTP(head, httptest.NewRequest(http.MethodHead, "/api/docs/doc?token=secret", nil))

	if get.Body.String() != "body" || head.Body.Len() != 0 || head.Header().Get("Content-Length") != "4" {
		t.Fatalf("GET body=%q HEAD=%v", get.Body, head.Result())
	}
	if get.Header().Get("X-Cache") != "MISS" || head.Header().Get("X-Cache") != "HIT" {
		t.Fatalf("cache headers: GET=%q HEAD=%q", get.Header().Get("X-Cache"), head.Header().Get("X-Cache"))
	}
	if auth.Calls() != 2 || documents.MetadataCalls() != 2 || documents.GetCalls() != 1 {
		t.Fatalf("authorize=%d metadata=%d get=%d", auth.Calls(), documents.MetadataCalls(), documents.GetCalls())
	}
}

func TestResponseCacheDoesNotCacheErrors(t *testing.T) {
	auth := &countingAuth{user: domain.User{ID: "reader-id", Login: "reader"}}
	documents := &countingDocuments{metadata: domain.Document{ID: "doc", Version: 1}, getErr: domain.ErrNotFound}
	handler := NewHandler(Dependencies{Auth: auth, Documents: documents, Cache: responsecache.NewMemory(4096, 10), CacheTTL: time.Minute})
	for range 2 {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/docs/doc?token=secret", nil))
		if response.Code != http.StatusNotFound {
			t.Fatalf("status=%d", response.Code)
		}
	}
	if documents.GetCalls() != 2 {
		t.Fatalf("Get calls=%d, want 2", documents.GetCalls())
	}
}

func TestResponseCacheCoalescesConcurrentJSONMisses(t *testing.T) {
	auth := &countingAuth{user: domain.User{ID: "reader-id", Login: "reader"}}
	documents := &countingDocuments{
		metadata: domain.Document{ID: "doc", Version: 1, JSON: []byte(`"body"`)},
		gate:     make(chan struct{}), started: make(chan struct{}),
	}
	handler := NewHandler(Dependencies{Auth: auth, Documents: documents, Cache: responsecache.NewMemory(4096, 10), CacheTTL: time.Minute})

	const requests = 12
	var wait sync.WaitGroup
	wait.Add(requests)
	for range requests {
		go func() {
			defer wait.Done()
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/docs/doc?token=secret", nil))
			if response.Code != http.StatusOK || response.Body.String() != "{\"data\":\"body\"}\n" {
				t.Errorf("status=%d body=%q", response.Code, response.Body)
			}
		}()
	}
	<-documents.started
	close(documents.gate)
	wait.Wait()
	if documents.GetCalls() != 1 {
		t.Fatalf("Get calls=%d, want one single-flight load", documents.GetCalls())
	}
}

type countingAuth struct {
	mu    sync.Mutex
	user  domain.User
	calls int
}

func (*countingAuth) Register(context.Context, string, string, string) (domain.User, error) {
	return domain.User{}, errors.New("unused")
}
func (*countingAuth) Authenticate(context.Context, string, string) (string, error) {
	return "", errors.New("unused")
}
func (a *countingAuth) Authorize(context.Context, string) (domain.User, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.calls++
	return a.user, nil
}
func (*countingAuth) Logout(context.Context, string) error { return errors.New("unused") }
func (a *countingAuth) Calls() int                         { a.mu.Lock(); defer a.mu.Unlock(); return a.calls }

type countingDocuments struct {
	mu                 sync.Mutex
	metadata           domain.Document
	getErr             error
	gets, metadataGets int
	gate, started      chan struct{}
	startOnce          sync.Once
}

func (*countingDocuments) Upload(context.Context, domain.User, document.Upload) (domain.Document, error) {
	return domain.Document{}, errors.New("unused")
}
func (*countingDocuments) List(context.Context, domain.User, domain.DocumentFilter) ([]domain.Document, error) {
	return nil, errors.New("unused")
}
func (d *countingDocuments) Get(context.Context, domain.User, string) (document.Content, error) {
	d.mu.Lock()
	d.gets++
	d.mu.Unlock()
	if d.started != nil {
		d.startOnce.Do(func() { close(d.started) })
	}
	if d.gate != nil {
		<-d.gate
	}
	if d.getErr != nil {
		return document.Content{}, d.getErr
	}
	return document.Content{Document: d.metadata, File: io.NopCloser(bytes.NewBufferString("body"))}, nil
}
func (d *countingDocuments) GetMetadata(context.Context, domain.User, string) (domain.Document, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.metadataGets++
	return d.metadata, nil
}
func (*countingDocuments) Delete(context.Context, domain.User, string) error {
	return errors.New("unused")
}
func (d *countingDocuments) GetCalls() int { d.mu.Lock(); defer d.mu.Unlock(); return d.gets }
func (d *countingDocuments) MetadataCalls() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.metadataGets
}
