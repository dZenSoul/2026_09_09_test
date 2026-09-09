package httptransport

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"sync"

	responsecache "documents/internal/cache"
	"documents/internal/document"
	"documents/internal/domain"
)

type documentMetadataReader interface {
	GetMetadata(context.Context, domain.User, string) (domain.Document, error)
}

const fileCopyBufferBytes = 32 << 10

func (h *handler) readDocument(w http.ResponseWriter, r *http.Request, id string) {
	if h.deps.Auth == nil || h.deps.Documents == nil {
		writeAPIError(w, http.StatusNotImplemented, errorCodeNotImplemented, "operation not implemented")
		return
	}
	query, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil || len(query) > 1 {
		writeAPIError(w, http.StatusBadRequest, errorCodeBadRequest, "bad request")
		return
	}
	token := ""
	if len(query) == 1 {
		var ok bool
		token, ok = singleQueryValue(query, "token")
		if !ok {
			writeAPIError(w, http.StatusBadRequest, errorCodeBadRequest, "bad request")
			return
		}
	}
	if len(query["token"]) > 1 {
		writeAPIError(w, http.StatusBadRequest, errorCodeBadRequest, "bad request")
		return
	}
	requester, err := h.deps.Auth.Authorize(r.Context(), token)
	if err != nil {
		writeDomainError(w, err)
		return
	}
	if h.deps.Cache != nil {
		reader, supported := h.deps.Documents.(documentMetadataReader)
		if supported {
			metadata, err := reader.GetMetadata(r.Context(), requester, id)
			if err != nil {
				writeDomainError(w, err)
				return
			}
			if metadata.IsFile {
				h.readFileWithCache(w, r, requester, id, metadata)
				return
			}
			entry, hit := h.cachedResponse(r.Context(), h.documentCacheKey(id, metadata.Version), []string{"document:" + id}, r.Method == http.MethodHead, func(dst http.ResponseWriter) bool {
				return h.loadDocumentContent(dst, r, requester, id)
			})
			h.exposeCacheStatus(w, hit)
			writeCachedResponse(w, entry)
			return
		}
	}

	if r.Method == http.MethodHead {
		if reader, supported := h.deps.Documents.(documentMetadataReader); supported {
			metadata, err := reader.GetMetadata(r.Context(), requester, id)
			if err != nil {
				writeDomainError(w, err)
				return
			}
			writeDocumentMetadata(w, metadata)
			return
		}
		// HEAD must never fall back to Get because Get opens file blobs. The
		// production document service always implements documentMetadataReader.
		writeAPIError(w, http.StatusInternalServerError, errorCodeInternal, "internal server error")
		return
	}

	h.loadDocumentContent(w, r, requester, id)
}

type cacheEntrySizer interface {
	EntryFits(key string, entry responsecache.Entry, bodyBytes int64) bool
}

func (h *handler) readFileWithCache(w http.ResponseWriter, r *http.Request, requester domain.User, id string, metadata domain.Document) {
	key := h.documentCacheKey(id, metadata.Version)
	tags := []string{"document:" + id}
	entry, found := h.deps.Cache.Get(r.Context(), key)

	if r.Method == http.MethodHead {
		if found {
			h.observeCache(r.Context(), true)
			h.exposeCacheStatus(w, true)
			writeCachedResponse(w, entry)
			return
		}
		entry = fileMetadataEntry(metadata, tags)
		h.deps.Cache.Set(context.WithoutCancel(r.Context()), key, entry, h.deps.CacheTTL)
		h.observeCache(r.Context(), false)
		h.observeCacheSize()
		h.exposeCacheStatus(w, false)
		writeCachedResponse(w, entry)
		return
	}

	if found && !entry.BodyOmitted {
		h.observeCache(r.Context(), true)
		h.exposeCacheStatus(w, true)
		writeCachedFileResponse(w, entry)
		return
	}

	h.observeCache(r.Context(), false)
	h.exposeCacheStatus(w, false)
	content, err := h.deps.Documents.Get(r.Context(), requester, id)
	if err != nil {
		writeDomainError(w, err)
		return
	}
	if content.File == nil {
		writeAPIError(w, http.StatusInternalServerError, errorCodeInternal, "internal server error")
		return
	}
	content.File = newCloseOnceReader(content.File)
	defer content.File.Close()

	entry = fileMetadataEntry(content.Document, tags)
	cacheBody := false
	if sizer, ok := h.deps.Cache.(cacheEntrySizer); ok {
		cacheBody = sizer.EntryFits(key, entry, content.Document.SizeBytes)
	}
	if !cacheBody {
		h.deps.Cache.Set(context.WithoutCancel(r.Context()), key, entry, h.deps.CacheTTL)
		h.observeCacheSize()
		_ = writeFileStream(r.Context(), w, content, nil)
		return
	}

	capture := &boundedCapture{remaining: content.Document.SizeBytes}
	if err := writeFileStream(r.Context(), w, content, capture); err != nil || capture.overflow || int64(capture.buffer.Len()) != content.Document.SizeBytes {
		return
	}
	entry.Body = capture.buffer.Bytes()
	entry.BodyOmitted = false
	h.deps.Cache.Set(context.WithoutCancel(r.Context()), key, entry, h.deps.CacheTTL)
	h.observeCacheSize()
}

func fileMetadataEntry(metadata domain.Document, tags []string) responsecache.Entry {
	header := make(http.Header)
	writeFileHeaders(headerWriter{header: header}, metadata)
	return responsecache.Entry{
		Status:      http.StatusOK,
		Header:      header,
		BodyOmitted: true,
		Tags:        append([]string(nil), tags...),
	}
}

type headerWriter struct{ header http.Header }

func (w headerWriter) Header() http.Header          { return w.header }
func (headerWriter) WriteHeader(int)                {}
func (headerWriter) Write(data []byte) (int, error) { return len(data), nil }

// boundedCapture accepts the complete stream but retains at most the declared
// cache budget. A blob larger than its metadata is still sent to the client and
// simply becomes ineligible for body caching.
type boundedCapture struct {
	buffer    bytes.Buffer
	remaining int64
	overflow  bool
}

func (w *boundedCapture) Write(data []byte) (int, error) {
	if int64(len(data)) > w.remaining {
		if w.remaining > 0 {
			_, _ = w.buffer.Write(data[:int(w.remaining)])
		}
		w.remaining = 0
		w.overflow = true
		return len(data), nil
	}
	w.remaining -= int64(len(data))
	_, _ = w.buffer.Write(data)
	return len(data), nil
}

func (h *handler) loadDocumentContent(w http.ResponseWriter, r *http.Request, requester domain.User, id string) bool {
	content, err := h.deps.Documents.Get(r.Context(), requester, id)
	if err != nil {
		writeDomainError(w, err)
		return false
	}
	if content.File != nil {
		content.File = newCloseOnceReader(content.File)
		defer content.File.Close()
	}
	return writeDocumentContent(r.Context(), w, content) == nil
}

func writeDocumentMetadata(w http.ResponseWriter, metadata domain.Document) {
	if !metadata.IsFile {
		writeData(w, http.StatusOK, json.RawMessage(metadata.JSON))
		return
	}
	writeFileHeaders(w, metadata)
	w.WriteHeader(http.StatusOK)
}

func writeDocumentContent(ctx context.Context, w http.ResponseWriter, content document.Content) error {
	if !content.Document.IsFile {
		writeData(w, http.StatusOK, json.RawMessage(content.Document.JSON))
		return nil
	}
	if content.File == nil {
		writeAPIError(w, http.StatusInternalServerError, errorCodeInternal, "internal server error")
		return errors.New("file document has no body")
	}
	return writeFileStream(ctx, w, content, nil)
}

func writeFileStream(ctx context.Context, w http.ResponseWriter, content document.Content, capture io.Writer) error {
	writeFileHeaders(w, content.Document)
	beginStreaming(w)
	w.WriteHeader(http.StatusOK)
	stopClose := closeReaderOnCancellation(ctx, content.File)
	defer stopClose()
	destination := io.Writer(w)
	if capture != nil {
		destination = io.MultiWriter(w, capture)
	}
	// Hide optional WriterTo implementations so every storage driver follows
	// the same bounded-copy path instead of being allowed to allocate or write a
	// whole object in one call.
	_, err := io.CopyBuffer(destination, readerOnly{Reader: content.File}, make([]byte, fileCopyBufferBytes))
	return err
}

type closeOnceReader struct {
	io.ReadCloser
	once sync.Once
}

func newCloseOnceReader(reader io.ReadCloser) io.ReadCloser {
	if _, ok := reader.(*closeOnceReader); ok {
		return reader
	}
	return &closeOnceReader{ReadCloser: reader}
}

func (r *closeOnceReader) Close() error {
	var err error
	r.once.Do(func() { err = r.ReadCloser.Close() })
	return err
}

func closeReaderOnCancellation(ctx context.Context, reader io.ReadCloser) func() {
	if ctx.Done() == nil {
		return func() {}
	}
	stopped := make(chan struct{})
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		select {
		case <-ctx.Done():
			_ = reader.Close()
		case <-stopped:
		}
	}()
	return func() {
		close(stopped)
		<-finished
	}
}

type readerOnly struct{ io.Reader }

func writeCachedFileResponse(w http.ResponseWriter, entry responsecache.Entry) {
	for name, values := range entry.Header {
		w.Header()[name] = append([]string(nil), values...)
	}
	beginStreaming(w)
	status := entry.Status
	if status == 0 {
		status = http.StatusOK
	}
	w.WriteHeader(status)
	_, _ = w.Write(entry.Body)
}

func beginStreaming(w http.ResponseWriter) {
	if streaming, ok := w.(interface{ startStreaming() }); ok {
		streaming.startStreaming()
	}
}

func writeFileHeaders(w http.ResponseWriter, metadata domain.Document) {
	w.Header().Set("Content-Type", metadata.MIME)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	// Uploaded active content is returned as data, never trusted as application UI.
	w.Header().Set("Content-Security-Policy", "sandbox; default-src 'none'")
	w.Header().Set("Content-Length", strconv.FormatInt(metadata.SizeBytes, 10))
	// FormatMediaType quotes and escapes ASCII special characters and emits an
	// RFC 5987 filename parameter for names that cannot safely fit in a header.
	w.Header().Set("Content-Disposition", mime.FormatMediaType("inline", map[string]string{"filename": metadata.Name}))
}
