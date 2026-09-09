// Package httptransport owns HTTP-specific request and response handling.
package httptransport

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"documents/internal/auth"
	responsecache "documents/internal/cache"
	"documents/internal/document"

	"github.com/google/uuid"
	"golang.org/x/sync/singleflight"
)

const defaultMaxRequestBytes int64 = 32 << 20

const (
	defaultMaxFileBytes  int64 = 25 << 20
	defaultMaxJSONBytes  int64 = 1 << 20
	defaultMaxGrantItems       = 100
	defaultMaxListLimit        = 100
)

// Limits contains transport-level and handler-level input limits. Only the
// overall request limit is enforced here; narrower limits are available to the
// operation handlers so files and JSON can be checked independently.
type Limits struct {
	MaxRequestBytes int64
	MaxFileBytes    int64
	MaxJSONBytes    int64
	MaxGrantItems   int
	MaxListLimit    int
}

type Dependencies struct {
	Auth              auth.Service
	Documents         document.Service
	Cache             responsecache.Cache
	CacheTTL          time.Duration
	ExposeCacheHeader bool
	Logger            *slog.Logger
	Metrics           interface {
		ObserveRequest(route, method string, status int, seconds float64)
		ObserveCache(hit bool)
		SetCacheSize(bytes int64, items int)
		ObserveFile(direction string, bytes int64)
	}
	Readiness func(context.Context) error
	// ProcessingTimeout bounds complete handler execution independently of
	// socket read/write deadlines. Zero leaves it to an outer server in tests.
	ProcessingTimeout time.Duration
	Limits            Limits
}

type handler struct {
	deps       Dependencies
	loads      singleflight.Group
	cacheEpoch atomic.Uint64
}

type contextKey uint8

const requestIDKey contextKey = iota

type requestObservation struct{ cache string }

const observationKey contextKey = requestIDKey + 1

// RequestID returns the server-generated request identifier associated with
// ctx. It returns an empty string outside an HTTP request.
func RequestID(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey).(string)
	return id
}

// NewHandler builds the API transport. Business handlers are introduced by
// the following tasks; until then every declared operation is an explicit 501.
func NewHandler(deps Dependencies) http.Handler {
	if deps.Logger == nil {
		deps.Logger = slog.Default()
	}
	if deps.Limits.MaxRequestBytes <= 0 {
		deps.Limits.MaxRequestBytes = defaultMaxRequestBytes
	}
	if deps.Limits.MaxFileBytes <= 0 {
		deps.Limits.MaxFileBytes = defaultMaxFileBytes
	}
	if deps.Limits.MaxJSONBytes <= 0 {
		deps.Limits.MaxJSONBytes = defaultMaxJSONBytes
	}
	if deps.Limits.MaxGrantItems <= 0 {
		deps.Limits.MaxGrantItems = defaultMaxGrantItems
	}
	if deps.Limits.MaxListLimit <= 0 {
		deps.Limits.MaxListLimit = defaultMaxListLimit
	}
	if deps.Cache != nil && deps.CacheTTL <= 0 {
		deps.CacheTTL = 5 * time.Minute
	}
	return &handler{deps: deps}
}

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if h.deps.ProcessingTimeout > 0 {
		ctx, cancel := context.WithTimeout(r.Context(), h.deps.ProcessingTimeout)
		defer cancel()
		r = r.WithContext(ctx)
	}
	h.serveHTTP(w, r, http.HandlerFunc(h.route))
}

func (h *handler) serveHTTP(w http.ResponseWriter, r *http.Request, next http.Handler) {
	started := time.Now()
	requestID := uuid.NewString()
	w.Header().Set("X-Request-ID", requestID)
	observation := &requestObservation{cache: "none"}
	ctx := context.WithValue(r.Context(), requestIDKey, requestID)
	r = r.WithContext(context.WithValue(ctx, observationKey, observation))

	response := newObservedResponse(w, r.Method == http.MethodHead)
	defer func() {
		if recovered := recover(); recovered != nil {
			h.deps.Logger.Error("HTTP handler panic recovered",
				"request_id", requestID,
				"route", normalizedRoute(r.URL.Path),
				"method", r.Method,
			)
			// Once streaming has started the status and partial body are already on
			// the wire. Appending a JSON error would only corrupt that response.
			if !response.committed {
				requestIDHeader := response.Header().Get("X-Request-ID")
				response.reset()
				response.Header().Set("X-Request-ID", requestIDHeader)
				writeAPIError(response, http.StatusInternalServerError, errorCodeInternal, "internal server error")
			}
		}
		response.finish()
		status := response.status
		if status == 0 {
			status = http.StatusOK
		}
		duration := time.Since(started)
		log := h.deps.Logger.Info
		if status >= 500 {
			log = h.deps.Logger.Error
		}
		log("HTTP request completed", "request_id", requestID, "route", normalizedRoute(r.URL.Path),
			"method", r.Method, "status", status, "duration_ms", duration.Milliseconds(), "cache", observation.cache)
		if h.deps.Metrics != nil {
			h.deps.Metrics.ObserveRequest(normalizedRoute(r.URL.Path), r.Method, status, duration.Seconds())
			if disposition := response.Header().Get("Content-Disposition"); r.Method != http.MethodHead && disposition != "" {
				h.deps.Metrics.ObserveFile("download", response.bytesWritten)
			}
		}
	}()

	if r.ContentLength > h.deps.Limits.MaxRequestBytes {
		writeAPIError(response, http.StatusBadRequest, errorCodeBadRequest, "request body is too large")
		return
	}
	if r.Body != nil {
		r.Body = http.MaxBytesReader(response, r.Body, h.deps.Limits.MaxRequestBytes)
	}

	next.ServeHTTP(response, r)
}

func (h *handler) route(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	switch {
	case path == "/health/live":
		if r.Method != http.MethodGet {
			methodNotAllowed(w, http.MethodGet)
			return
		}
		writeResponse(w, http.StatusOK, map[string]string{"status": "ok"})
	case path == "/health/ready":
		if r.Method != http.MethodGet {
			methodNotAllowed(w, http.MethodGet)
			return
		}
		if h.deps.Readiness != nil {
			if err := h.deps.Readiness(r.Context()); err != nil {
				writeAPIError(w, http.StatusServiceUnavailable, errorCodeInternal, "service unavailable")
				return
			}
		}
		writeResponse(w, http.StatusOK, map[string]string{"status": "ok"})
	case path == "/api/register":
		if r.Method != http.MethodPost {
			methodNotAllowed(w, http.MethodPost)
			return
		}
		h.register(w, r)
	case path == "/api/auth":
		if r.Method != http.MethodPost {
			methodNotAllowed(w, http.MethodPost)
			return
		}
		h.authenticate(w, r)
	case singlePathValue(path, "/api/auth/"):
		if r.Method != http.MethodDelete {
			methodNotAllowed(w, http.MethodDelete)
			return
		}
		h.logout(w, r, strings.TrimPrefix(path, "/api/auth/"))
	case path == "/api/docs":
		if r.Method == http.MethodPost {
			h.uploadDocument(w, r)
			return
		}
		if r.Method == http.MethodGet || r.Method == http.MethodHead {
			h.listDocuments(w, r)
			return
		}
		h.operation(w, r, http.MethodGet, http.MethodHead, http.MethodPost)
	case singlePathValue(path, "/api/docs/"):
		if r.Method == http.MethodGet || r.Method == http.MethodHead {
			h.readDocument(w, r, strings.TrimPrefix(path, "/api/docs/"))
			return
		}
		if r.Method == http.MethodDelete {
			h.deleteDocument(w, r, strings.TrimPrefix(path, "/api/docs/"))
			return
		}
		methodNotAllowed(w, http.MethodDelete, http.MethodGet, http.MethodHead)
	default:
		writeAPIError(w, http.StatusNotFound, errorCodeNotFound, "resource not found")
	}
}

// normalizedRoute deliberately never returns path parameters: session tokens
// and document IDs therefore cannot enter logs or metric labels.
func normalizedRoute(path string) string {
	switch {
	case path == "/health/live", path == "/health/ready", path == "/metrics", path == "/api/register", path == "/api/auth", path == "/api/docs":
		return path
	case singlePathValue(path, "/api/auth/"):
		return "/api/auth/{token}"
	case singlePathValue(path, "/api/docs/"):
		return "/api/docs/{id}"
	default:
		return "not_found"
	}
}

func (h *handler) register(w http.ResponseWriter, r *http.Request) {
	if h.deps.Auth == nil {
		writeAPIError(w, http.StatusNotImplemented, errorCodeNotImplemented, "operation not implemented")
		return
	}
	if !parseURLForm(w, r) {
		return
	}
	user, err := h.deps.Auth.Register(r.Context(), r.PostForm.Get("token"), r.PostForm.Get("login"), r.PostForm.Get("pswd"))
	if err != nil {
		writeDomainError(w, err)
		return
	}
	writeResponse(w, http.StatusOK, map[string]string{"login": user.Login})
}

func (h *handler) authenticate(w http.ResponseWriter, r *http.Request) {
	if h.deps.Auth == nil {
		writeAPIError(w, http.StatusNotImplemented, errorCodeNotImplemented, "operation not implemented")
		return
	}
	if !parseURLForm(w, r) {
		return
	}
	token, err := h.deps.Auth.Authenticate(r.Context(), r.PostForm.Get("login"), r.PostForm.Get("pswd"))
	if err != nil {
		writeDomainError(w, err)
		return
	}
	writeResponse(w, http.StatusOK, map[string]string{"token": token})
}

func (h *handler) logout(w http.ResponseWriter, r *http.Request, token string) {
	if h.deps.Auth == nil {
		writeAPIError(w, http.StatusNotImplemented, errorCodeNotImplemented, "operation not implemented")
		return
	}
	if err := h.deps.Auth.Logout(r.Context(), token); err != nil {
		writeDomainError(w, err)
		return
	}
	writeResponse(w, http.StatusOK, map[string]bool{token: true})
}

func parseURLForm(w http.ResponseWriter, r *http.Request) bool {
	contentType := r.Header.Get("Content-Type")
	if mediaType := strings.TrimSpace(strings.SplitN(contentType, ";", 2)[0]); mediaType != "application/x-www-form-urlencoded" {
		writeAPIError(w, http.StatusBadRequest, errorCodeBadRequest, "bad request")
		return false
	}
	if err := r.ParseForm(); err != nil {
		writeAPIError(w, http.StatusBadRequest, errorCodeBadRequest, "bad request")
		return false
	}
	return true
}

func singlePathValue(path, prefix string) bool {
	value := strings.TrimPrefix(path, prefix)
	return value != path && value != "" && !strings.Contains(value, "/")
}

func (h *handler) operation(w http.ResponseWriter, r *http.Request, allowed ...string) {
	for _, method := range allowed {
		if r.Method == method {
			writeAPIError(w, http.StatusNotImplemented, errorCodeNotImplemented, "operation not implemented")
			return
		}
	}
	methodNotAllowed(w, allowed...)
}

func methodNotAllowed(w http.ResponseWriter, allowed ...string) {
	w.Header().Set("Allow", strings.Join(allowed, ", "))
	writeAPIError(w, http.StatusMethodNotAllowed, errorCodeMethodNotAllowed, "method not allowed")
}

// observedResponse keeps ordinary small responses atomic until the handler
// returns. File handlers explicitly switch it to streaming after every
// fallible precondition (authorization, metadata lookup and blob open) passes.
type observedResponse struct {
	dst          http.ResponseWriter
	head         bool
	header       http.Header
	body         bytes.Buffer
	status       int
	wroteHeader  bool
	committed    bool
	bytesWritten int64
}

func newObservedResponse(dst http.ResponseWriter, head bool) *observedResponse {
	return &observedResponse{dst: dst, head: head, header: cloneHeader(dst.Header())}
}

func (w *observedResponse) Header() http.Header {
	if w.committed {
		return w.dst.Header()
	}
	return w.header
}

func (w *observedResponse) WriteHeader(status int) {
	if w.wroteHeader {
		return
	}
	w.status = status
	w.wroteHeader = true
	if w.committed {
		w.dst.WriteHeader(status)
	}
}

func (w *observedResponse) Write(data []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	if w.head {
		return len(data), nil
	}
	if !w.committed {
		return w.body.Write(data)
	}
	n, err := w.dst.Write(data)
	w.bytesWritten += int64(n)
	return n, err
}

func (w *observedResponse) Flush() {
	w.startStreaming()
	if flusher, ok := w.dst.(http.Flusher); ok {
		flusher.Flush()
	}
}

func (w *observedResponse) startStreaming() {
	if w.committed {
		return
	}
	if !w.wroteHeader {
		w.status = http.StatusOK
		w.wroteHeader = true
	}
	copyResponseHeader(w.dst.Header(), w.header)
	w.dst.WriteHeader(w.status)
	w.committed = true
	if !w.head && w.body.Len() != 0 {
		n, _ := w.dst.Write(w.body.Bytes())
		w.bytesWritten += int64(n)
	}
	w.body.Reset()
}

func (w *observedResponse) finish() {
	if w.committed {
		return
	}
	if !w.wroteHeader {
		w.status = http.StatusOK
		w.wroteHeader = true
	}
	if w.header.Get("Content-Length") == "" && responseMayHaveBody(w.status) {
		w.header.Set("Content-Length", strconv.Itoa(w.body.Len()))
	}
	copyResponseHeader(w.dst.Header(), w.header)
	w.dst.WriteHeader(w.status)
	w.committed = true
	if !w.head && responseMayHaveBody(w.status) {
		n, _ := w.dst.Write(w.body.Bytes())
		w.bytesWritten += int64(n)
	}
	w.body.Reset()
}

func (w *observedResponse) reset() {
	w.header = make(http.Header)
	w.body.Reset()
	w.status = 0
	w.wroteHeader = false
}

func copyResponseHeader(dst, src http.Header) {
	for name := range dst {
		delete(dst, name)
	}
	for name, values := range src {
		dst[name] = append([]string(nil), values...)
	}
}

func responseMayHaveBody(status int) bool {
	return status >= 200 && status != http.StatusNoContent && status != http.StatusNotModified
}
