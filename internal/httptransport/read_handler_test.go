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

func TestReadFileStreamsBeforeSourceEOF(t *testing.T) {
	reader := &stagedReadCloser{
		first:   []byte("first"),
		second:  []byte("second"),
		release: make(chan struct{}),
		closed:  make(chan struct{}),
	}
	metadata := domain.Document{Name: "stream.bin", MIME: "application/octet-stream", IsFile: true, SizeBytes: 11}
	handler := NewHandler(Dependencies{
		Auth:      &readAuth{user: domain.User{ID: "reader-id", Login: "reader000"}},
		Documents: &readDocumentService{content: document.Content{Document: metadata, File: reader}},
	})
	w := newSignallingWriter()
	done := make(chan struct{})
	go func() {
		handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/docs/doc-id?token=session", nil))
		close(done)
	}()

	select {
	case <-w.wrote:
		if got := w.bodyString(); got != "first" {
			t.Fatalf("first streamed bytes = %q", got)
		}
	case <-time.After(time.Second):
		t.Fatal("no bytes reached the writer before source EOF")
	}
	close(reader.release)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("stream did not finish")
	}
	if got := w.bodyString(); got != "firstsecond" {
		t.Fatalf("streamed body = %q", got)
	}
	select {
	case <-reader.closed:
	default:
		t.Fatal("source was not closed")
	}
}

func TestReadFileHEADCacheMissUsesOnlyMetadata(t *testing.T) {
	metadata := domain.Document{Name: "large.bin", MIME: "application/octet-stream", IsFile: true, SizeBytes: 4096, Version: 2}
	documents := &readDocumentService{metadata: metadata}
	handler := NewHandler(Dependencies{
		Auth:              &readAuth{user: domain.User{ID: "reader-id", Login: "reader000"}},
		Documents:         documents,
		Cache:             responsecache.NewMemory(1024, 10),
		CacheTTL:          time.Minute,
		ExposeCacheHeader: true,
	})

	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodHead, "/api/docs/doc-id?token=session", nil))
	if response.Code != http.StatusOK || response.Body.Len() != 0 || response.Header().Get("Content-Length") != "4096" {
		t.Fatalf("HEAD response: status=%d headers=%v body=%q", response.Code, response.Header(), response.Body)
	}
	if documents.getCalls != 0 || documents.metadataCalls != 1 {
		t.Fatalf("get calls=%d metadata calls=%d", documents.getCalls, documents.metadataCalls)
	}
}

func TestLargeFileCacheStoresMetadataWithoutCoalescingBody(t *testing.T) {
	metadata := domain.Document{ID: "doc", Name: "large.bin", MIME: "application/octet-stream", IsFile: true, SizeBytes: 4096, Version: 3}
	documents := &countingDocuments{metadata: metadata}
	handler := NewHandler(Dependencies{
		Auth:              &countingAuth{user: domain.User{ID: "reader-id", Login: "reader"}},
		Documents:         documents,
		Cache:             responsecache.NewMemory(1024, 10),
		CacheTTL:          time.Minute,
		ExposeCacheHeader: true,
	})

	for range 2 {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/docs/doc?token=session", nil))
		if response.Code != http.StatusOK || response.Body.String() != "body" || response.Header().Get("X-Cache") != "MISS" {
			t.Fatalf("GET response: status=%d cache=%q body=%q", response.Code, response.Header().Get("X-Cache"), response.Body)
		}
	}
	if documents.GetCalls() != 2 {
		t.Fatalf("large body Get calls=%d, want 2", documents.GetCalls())
	}

	head := httptest.NewRecorder()
	handler.ServeHTTP(head, httptest.NewRequest(http.MethodHead, "/api/docs/doc?token=session", nil))
	if head.Header().Get("X-Cache") != "HIT" || head.Body.Len() != 0 || documents.GetCalls() != 2 {
		t.Fatalf("HEAD cache=%q body=%q Get calls=%d", head.Header().Get("X-Cache"), head.Body, documents.GetCalls())
	}
}

func TestParallelLargeFileStreamsAreIndependent(t *testing.T) {
	metadata := domain.Document{ID: "doc", Name: "large.bin", MIME: "application/octet-stream", IsFile: true, SizeBytes: 4096, Version: 4}
	slow := &stagedReadCloser{
		first: []byte("slow-"), second: []byte("done"),
		release: make(chan struct{}), closed: make(chan struct{}),
	}
	documents := &parallelFileDocuments{metadata: metadata, first: slow}
	handler := NewHandler(Dependencies{
		Auth:      &countingAuth{user: domain.User{ID: "reader-id", Login: "reader"}},
		Documents: documents,
		Cache:     responsecache.NewMemory(1024, 10), CacheTTL: time.Minute,
	})

	slowWriter := newSignallingWriter()
	slowDone := make(chan struct{})
	go func() {
		handler.ServeHTTP(slowWriter, httptest.NewRequest(http.MethodGet, "/api/docs/doc?token=session", nil))
		close(slowDone)
	}()
	select {
	case <-slowWriter.wrote:
	case <-time.After(time.Second):
		t.Fatal("slow request did not begin streaming")
	}

	fastResponse := httptest.NewRecorder()
	fastDone := make(chan struct{})
	go func() {
		handler.ServeHTTP(fastResponse, httptest.NewRequest(http.MethodGet, "/api/docs/doc?token=session", nil))
		close(fastDone)
	}()
	select {
	case <-fastDone:
		if fastResponse.Body.String() != "fast" {
			t.Fatalf("fast body=%q", fastResponse.Body)
		}
	case <-time.After(time.Second):
		t.Fatal("fast request was blocked by an independent slow stream")
	}

	close(slow.release)
	select {
	case <-slowDone:
	case <-time.After(time.Second):
		t.Fatal("slow request did not finish")
	}
	if slowWriter.bodyString() != "slow-done" || documents.GetCalls() != 2 {
		t.Fatalf("slow body=%q Get calls=%d", slowWriter.bodyString(), documents.GetCalls())
	}
}

func TestFileOpenFailureReturnsJSONBeforeStreaming(t *testing.T) {
	metadata := domain.Document{ID: "doc", Name: "file.bin", MIME: "application/octet-stream", IsFile: true, SizeBytes: 4, Version: 1}
	handler := NewHandler(Dependencies{
		Auth:      &countingAuth{user: domain.User{ID: "reader-id", Login: "reader"}},
		Documents: &countingDocuments{metadata: metadata, getErr: errors.New("blob unavailable")},
		Cache:     responsecache.NewMemory(4096, 10), CacheTTL: time.Minute,
	})
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/docs/doc?token=session", nil))
	if response.Code != http.StatusInternalServerError || response.Header().Get("Content-Type") != "application/json; charset=utf-8" || response.Body.String() != "{\"error\":{\"code\":500,\"text\":\"internal server error\"}}\n" {
		t.Fatalf("response: status=%d headers=%v body=%q", response.Code, response.Header(), response.Body)
	}
}

func TestFileWriteFailureClosesSourceAndCountsWrittenBytes(t *testing.T) {
	closed := make(chan struct{})
	reader := &closeTrackingReader{Reader: bytes.NewReader([]byte("abcdef")), closed: closed}
	metadata := domain.Document{Name: "file.bin", MIME: "application/octet-stream", IsFile: true, SizeBytes: 6}
	metrics := &fileMetricSpy{}
	handler := NewHandler(Dependencies{
		Auth:      &readAuth{user: domain.User{ID: "reader-id", Login: "reader000"}},
		Documents: &readDocumentService{content: document.Content{Document: metadata, File: reader}},
		Metrics:   metrics,
	})
	w := &failingResponseWriter{header: make(http.Header), limit: 2}
	handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/docs/doc-id?token=session", nil))
	select {
	case <-closed:
	default:
		t.Fatal("source was not closed after client write failure")
	}
	if w.status != http.StatusOK || w.body.String() != "ab" {
		t.Fatalf("partial response: status=%d body=%q", w.status, w.body.String())
	}
	if metrics.downloaded != 2 {
		t.Fatalf("download metric=%d, want actual 2 bytes", metrics.downloaded)
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

type stagedReadCloser struct {
	first, second []byte
	release       chan struct{}
	closed        chan struct{}
	reads         int
	closeOnce     sync.Once
}

func (r *stagedReadCloser) Read(dst []byte) (int, error) {
	switch r.reads {
	case 0:
		r.reads++
		return copy(dst, r.first), nil
	case 1:
		r.reads++
		<-r.release
		return copy(dst, r.second), nil
	default:
		return 0, io.EOF
	}
}

func (r *stagedReadCloser) Close() error {
	r.closeOnce.Do(func() { close(r.closed) })
	return nil
}

type signallingWriter struct {
	mu     sync.Mutex
	header http.Header
	status int
	body   bytes.Buffer
	wrote  chan struct{}
	once   sync.Once
}

func newSignallingWriter() *signallingWriter {
	return &signallingWriter{header: make(http.Header), wrote: make(chan struct{})}
}

func (w *signallingWriter) Header() http.Header { return w.header }
func (w *signallingWriter) WriteHeader(status int) {
	w.mu.Lock()
	w.status = status
	w.mu.Unlock()
}
func (w *signallingWriter) Write(data []byte) (int, error) {
	w.mu.Lock()
	n, err := w.body.Write(data)
	w.mu.Unlock()
	w.once.Do(func() { close(w.wrote) })
	return n, err
}
func (w *signallingWriter) bodyString() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.body.String()
}

type closeTrackingReader struct {
	*bytes.Reader
	closed chan struct{}
	once   sync.Once
}

func (r *closeTrackingReader) Close() error {
	r.once.Do(func() { close(r.closed) })
	return nil
}

type failingResponseWriter struct {
	header http.Header
	status int
	body   bytes.Buffer
	limit  int
}

func (w *failingResponseWriter) Header() http.Header { return w.header }
func (w *failingResponseWriter) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
}
func (w *failingResponseWriter) Write(data []byte) (int, error) {
	remaining := w.limit - w.body.Len()
	if remaining <= 0 {
		return 0, errors.New("client disconnected")
	}
	if len(data) > remaining {
		_, _ = w.body.Write(data[:remaining])
		return remaining, errors.New("client disconnected")
	}
	return w.body.Write(data)
}

type fileMetricSpy struct{ downloaded int64 }

func (*fileMetricSpy) ObserveRequest(string, string, int, float64) {}
func (*fileMetricSpy) ObserveCache(bool)                           {}
func (*fileMetricSpy) SetCacheSize(int64, int)                     {}
func (m *fileMetricSpy) ObserveFile(direction string, bytes int64) {
	if direction == "download" {
		m.downloaded += bytes
	}
}

type parallelFileDocuments struct {
	mu       sync.Mutex
	metadata domain.Document
	first    io.ReadCloser
	gets     int
}

func (*parallelFileDocuments) Upload(context.Context, domain.User, document.Upload) (domain.Document, error) {
	return domain.Document{}, errors.New("unused")
}
func (*parallelFileDocuments) List(context.Context, domain.User, domain.DocumentFilter) ([]domain.Document, error) {
	return nil, errors.New("unused")
}
func (d *parallelFileDocuments) Get(context.Context, domain.User, string) (document.Content, error) {
	d.mu.Lock()
	d.gets++
	call := d.gets
	d.mu.Unlock()
	if call == 1 {
		return document.Content{Document: d.metadata, File: d.first}, nil
	}
	return document.Content{Document: d.metadata, File: io.NopCloser(bytes.NewReader([]byte("fast")))}, nil
}
func (d *parallelFileDocuments) GetMetadata(context.Context, domain.User, string) (domain.Document, error) {
	return d.metadata, nil
}
func (*parallelFileDocuments) Delete(context.Context, domain.User, string) error {
	return errors.New("unused")
}
func (d *parallelFileDocuments) GetCalls() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.gets
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
