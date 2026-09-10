package httptransport

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	responsecache "documents/internal/cache"
	"documents/internal/document"
	"documents/internal/domain"
)

func TestConcurrentGETDeleteHasOnlyLinearizableOutcomes(t *testing.T) {
	t.Run("cached GET completes before commit", func(t *testing.T) {
		harness := newDeleteRaceHarness(t, []byte("complete-body"), 4096)
		harness.warmCaches(t)
		harness.documents.beforeCommit = make(chan struct{})
		harness.documents.deleteStarted = make(chan struct{})

		deleteDone := harness.deleteAsync()
		waitForSignal(t, harness.documents.deleteStarted)
		response := acceptanceGet(t, harness.server.Client(), harness.server.URL+"/api/docs/doc", "session")
		assertRaceResponse(t, response, http.StatusOK, "HIT", []byte("complete-body"))
		close(harness.documents.beforeCommit)
		assertRaceRecorder(t, awaitRaceRecorder(t, deleteDone), http.StatusOK)
		harness.assertDeleted(t)
	})

	t.Run("metadata read before commit cannot use invalidated cache", func(t *testing.T) {
		harness := newDeleteRaceHarness(t, []byte("complete-body"), 4096)
		harness.warmCaches(t)
		harness.documents.metadataRead = make(chan struct{})
		harness.documents.releaseMetadata = make(chan struct{})
		harness.documents.committed = make(chan struct{})
		harness.documents.afterCommit = make(chan struct{})

		getDone := harness.getAsync()
		waitForSignal(t, harness.documents.metadataRead)
		deleteDone := harness.deleteAsync()
		waitForSignal(t, harness.documents.committed)
		close(harness.documents.releaseMetadata)
		assertRaceRecorder(t, awaitRaceRecorder(t, getDone), http.StatusNotFound)
		close(harness.documents.afterCommit)
		assertRaceRecorder(t, awaitRaceRecorder(t, deleteDone), http.StatusOK)
		harness.assertDeleted(t)
	})

	t.Run("opened stream finishes while delete commits", func(t *testing.T) {
		harness := newDeleteRaceHarness(t, []byte("first-second"), 256)
		harness.warmCaches(t)
		reader := &stagedReadCloser{
			first: []byte("first-"), second: []byte("second"),
			release: make(chan struct{}), closed: make(chan struct{}),
		}
		harness.documents.nextReader = reader
		writer := newSignallingWriter()
		getDone := make(chan struct{})
		go func() {
			harness.handler.ServeHTTP(writer, httptest.NewRequest(http.MethodGet, "/api/docs/doc?token=session", nil))
			close(getDone)
		}()
		waitForSignal(t, writer.wrote)

		deleteDone := harness.deleteAsync()
		assertRaceRecorder(t, awaitRaceRecorder(t, deleteDone), http.StatusOK)
		close(reader.release)
		waitForSignal(t, getDone)
		if writer.status != http.StatusOK || writer.bodyString() != "first-second" {
			t.Fatalf("in-flight GET: status=%d body=%q", writer.status, writer.bodyString())
		}
		harness.assertDeleted(t)

		// A new file with the same logical name remains fully usable after the race.
		response, err := acceptanceUploadNoFail(harness.server.Client(), harness.server.URL+"/api/docs", map[string]any{
			"name": "same.bin", "file": true, "public": false, "token": "session",
			"mime": "application/octet-stream", "grant": []string{},
		}, nil, []byte("replacement"))
		if err != nil {
			t.Fatal(err)
		}
		acceptanceCloseStatus(t, response, http.StatusOK)
		response = acceptanceGet(t, harness.server.Client(), harness.server.URL+"/api/docs/doc", "session")
		assertRaceResponse(t, response, http.StatusOK, "MISS", []byte("replacement"))
		response = acceptanceForm(t, harness.server.Client(), http.MethodDelete, harness.server.URL+"/api/docs/doc",
			url.Values{"token": {"session"}})
		acceptanceCloseStatus(t, response, http.StatusOK)
		harness.assertDeleted(t)
	})
}

type deleteRaceHarness struct {
	handler   http.Handler
	server    *httptest.Server
	documents *deleteRaceDocuments
}

func newDeleteRaceHarness(t *testing.T, body []byte, cacheBytes int64) *deleteRaceHarness {
	t.Helper()
	cache := responsecache.NewMemory(cacheBytes, 16)
	epoch := &atomic.Uint64{}
	documents := &deleteRaceDocuments{
		document: domain.Document{
			ID: "doc", OwnerID: "owner-id", Name: "same.bin", MIME: "application/octet-stream",
			IsFile: true, SizeBytes: int64(len(body)), Version: 1,
		},
		body: append([]byte(nil), body...), exists: true, cache: cache, epoch: epoch,
	}
	handler := NewHandler(Dependencies{
		Auth: &readAuth{user: domain.User{ID: "owner-id", Login: "owner000"}}, Documents: documents,
		Cache: cache, CacheEpoch: epoch, CacheTTL: time.Minute, ExposeCacheHeader: true,
		Limits: Limits{MaxRequestBytes: 4096, MaxFileBytes: 1024, MaxJSONBytes: 128, MaxGrantItems: 4, MaxListLimit: 10},
	})
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return &deleteRaceHarness{handler: handler, server: server, documents: documents}
}

func (h *deleteRaceHarness) warmCaches(t *testing.T) {
	t.Helper()
	response := acceptanceGet(t, h.server.Client(), h.server.URL+"/api/docs/doc", "session")
	assertRaceResponse(t, response, http.StatusOK, "MISS", h.documents.body)
	if docs := acceptanceList(t, h.server.Client(), h.server.URL, "session", ""); len(docs) != 1 {
		t.Fatalf("warm list=%#v", docs)
	}
}

func (h *deleteRaceHarness) getAsync() <-chan *httptest.ResponseRecorder {
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		response := httptest.NewRecorder()
		h.handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/docs/doc?token=session", nil))
		done <- response
	}()
	return done
}

func (h *deleteRaceHarness) deleteAsync() <-chan *httptest.ResponseRecorder {
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- deleteFormRequest(h.handler, "/api/docs/doc", url.Values{"token": {"session"}}) }()
	return done
}

func (h *deleteRaceHarness) assertDeleted(t *testing.T) {
	t.Helper()
	for _, method := range []string{http.MethodGet, http.MethodHead} {
		request, _ := http.NewRequest(method, h.server.URL+"/api/docs/doc?token=session", nil)
		response, err := h.server.Client().Do(request)
		if err != nil {
			t.Fatal(err)
		}
		if response.StatusCode != http.StatusNotFound || response.Header.Get("X-Cache") == "HIT" {
			body, _ := io.ReadAll(response.Body)
			_ = response.Body.Close()
			t.Fatalf("%s deleted document: status=%d cache=%q body=%q", method, response.StatusCode, response.Header.Get("X-Cache"), body)
		}
		_ = response.Body.Close()
	}
	if docs := acceptanceList(t, h.server.Client(), h.server.URL, "session", ""); len(docs) != 0 {
		t.Fatalf("deleted document remains in list: %#v", docs)
	}
}

func assertRaceResponse(t *testing.T, response *http.Response, status int, cache string, body []byte) {
	t.Helper()
	defer response.Body.Close()
	actual, err := io.ReadAll(response.Body)
	if err != nil || response.StatusCode != status || response.Header.Get("X-Cache") != cache || !bytes.Equal(actual, body) {
		t.Fatalf("response: status=%d cache=%q body=%q read=%v", response.StatusCode, response.Header.Get("X-Cache"), actual, err)
	}
}

func assertRaceRecorder(t *testing.T, response *httptest.ResponseRecorder, status int) {
	t.Helper()
	if response.Code != status {
		t.Fatalf("response: status=%d body=%q", response.Code, response.Body)
	}
}

func awaitRaceRecorder(t *testing.T, done <-chan *httptest.ResponseRecorder) *httptest.ResponseRecorder {
	t.Helper()
	select {
	case response := <-done:
		return response
	case <-time.After(time.Second):
		t.Fatal("concurrent HTTP request did not finish")
		return nil
	}
}

func waitForSignal(t *testing.T, signal <-chan struct{}) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(time.Second):
		t.Fatal("synchronization point was not reached")
	}
}

type deleteRaceDocuments struct {
	mu       sync.Mutex
	document domain.Document
	body     []byte
	exists   bool
	cache    responsecache.Cache
	epoch    *atomic.Uint64

	beforeCommit    chan struct{}
	deleteStarted   chan struct{}
	committed       chan struct{}
	afterCommit     chan struct{}
	metadataRead    chan struct{}
	releaseMetadata chan struct{}
	nextReader      io.ReadCloser
	metadataOnce    sync.Once
	deleteOnce      sync.Once
	commitOnce      sync.Once
}

func (d *deleteRaceDocuments) Upload(_ context.Context, _ domain.User, input document.Upload) (domain.Document, error) {
	if !input.Document.IsFile || input.File == nil {
		return domain.Document{}, domain.ErrInvalidArgument
	}
	body, err := io.ReadAll(input.File)
	if err != nil {
		return domain.Document{}, err
	}
	d.mu.Lock()
	d.document.Name = input.Document.Name
	d.document.MIME = input.Document.MIME
	d.document.SizeBytes = int64(len(body))
	d.document.Version++
	d.body = body
	d.exists = true
	result := d.document
	d.mu.Unlock()
	return result, nil
}

func (d *deleteRaceDocuments) List(context.Context, domain.User, domain.DocumentFilter) ([]domain.Document, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.exists {
		return []domain.Document{}, nil
	}
	return []domain.Document{d.document}, nil
}

func (d *deleteRaceDocuments) Get(context.Context, domain.User, string) (document.Content, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.exists {
		return document.Content{}, domain.ErrNotFound
	}
	reader := d.nextReader
	if reader != nil {
		d.nextReader = nil
	} else {
		reader = io.NopCloser(bytes.NewReader(append([]byte(nil), d.body...)))
	}
	return document.Content{Document: d.document, File: reader}, nil
}

func (d *deleteRaceDocuments) GetMetadata(context.Context, domain.User, string) (domain.Document, error) {
	d.mu.Lock()
	exists, metadata := d.exists, d.document
	d.mu.Unlock()
	if !exists {
		return domain.Document{}, domain.ErrNotFound
	}
	if d.metadataRead != nil {
		d.metadataOnce.Do(func() {
			close(d.metadataRead)
			<-d.releaseMetadata
		})
	}
	return metadata, nil
}

func (d *deleteRaceDocuments) Delete(context.Context, domain.User, string) error {
	if d.deleteStarted != nil {
		d.deleteOnce.Do(func() { close(d.deleteStarted) })
	}
	if d.beforeCommit != nil {
		<-d.beforeCommit
	}
	d.mu.Lock()
	if !d.exists {
		d.mu.Unlock()
		return domain.ErrNotFound
	}
	d.exists = false
	d.mu.Unlock()
	d.epoch.Add(1)
	d.cache.Invalidate(context.Background(), listCacheTag, "document:doc")
	if d.committed != nil {
		d.commitOnce.Do(func() { close(d.committed) })
	}
	if d.afterCommit != nil {
		<-d.afterCommit
	}
	return nil
}

func (*deleteRaceDocuments) InvalidatesDeleteCache() bool { return true }

var _ document.Service = (*deleteRaceDocuments)(nil)
var _ documentMetadataReader = (*deleteRaceDocuments)(nil)
