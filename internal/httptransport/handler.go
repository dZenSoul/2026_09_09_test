// Package httptransport owns HTTP-specific request and response handling.
package httptransport

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"documents/internal/auth"
	"documents/internal/document"

	"github.com/google/uuid"
)

const defaultMaxRequestBytes int64 = 32 << 20

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
	Auth      auth.Service
	Documents document.Service
	Logger    *slog.Logger
	Limits    Limits
}

type handler struct {
	deps Dependencies
}

type contextKey uint8

const requestIDKey contextKey = iota

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
	return &handler{deps: deps}
}

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.serveHTTP(w, r, http.HandlerFunc(h.route))
}

func (h *handler) serveHTTP(w http.ResponseWriter, r *http.Request, next http.Handler) {
	requestID := uuid.NewString()
	w.Header().Set("X-Request-ID", requestID)
	r = r.WithContext(context.WithValue(r.Context(), requestIDKey, requestID))

	buffer := newBufferedResponse()
	defer func() {
		if recover() != nil {
			h.deps.Logger.Error("HTTP handler panic recovered",
				"request_id", requestID,
				"method", r.Method,
			)
			buffer = newBufferedResponse()
			writeAPIError(buffer, http.StatusInternalServerError, errorCodeInternal, "internal server error")
		}
		buffer.commit(w, r.Method == http.MethodHead)
	}()

	if r.ContentLength > h.deps.Limits.MaxRequestBytes {
		writeAPIError(buffer, http.StatusBadRequest, errorCodeBadRequest, "request body is too large")
		return
	}
	if r.Body != nil {
		r.Body = http.MaxBytesReader(buffer, r.Body, h.deps.Limits.MaxRequestBytes)
	}

	next.ServeHTTP(buffer, r)
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
		h.operation(w, r, http.MethodGet, http.MethodHead, http.MethodPost)
	case singlePathValue(path, "/api/docs/"):
		h.operation(w, r, http.MethodDelete, http.MethodGet, http.MethodHead)
	default:
		writeAPIError(w, http.StatusNotFound, errorCodeNotFound, "resource not found")
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

// bufferedResponse prevents a panic after a partial write from leaking a
// truncated response. It also lets HEAD calculate GET's content length while
// suppressing every body byte, including error bodies.
type bufferedResponse struct {
	header http.Header
	body   bytes.Buffer
	status int
}

func newBufferedResponse() *bufferedResponse {
	return &bufferedResponse{header: make(http.Header)}
}

func (w *bufferedResponse) Header() http.Header { return w.header }

func (w *bufferedResponse) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
}

func (w *bufferedResponse) Write(data []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.body.Write(data)
}

func (w *bufferedResponse) commit(dst http.ResponseWriter, head bool) {
	for name, values := range w.header {
		dst.Header()[name] = append([]string(nil), values...)
	}
	status := w.status
	if status == 0 {
		status = http.StatusOK
	}
	if dst.Header().Get("Content-Length") == "" && responseMayHaveBody(status) {
		dst.Header().Set("Content-Length", strconv.Itoa(w.body.Len()))
	}
	dst.WriteHeader(status)
	if !head && responseMayHaveBody(status) {
		_, _ = dst.Write(w.body.Bytes())
	}
}

func responseMayHaveBody(status int) bool {
	return status >= 200 && status != http.StatusNoContent && status != http.StatusNotModified
}
