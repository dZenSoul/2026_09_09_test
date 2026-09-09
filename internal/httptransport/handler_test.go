package httptransport

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"documents/internal/domain"
)

func TestDeclaredRoutesAndMethods(t *testing.T) {
	tests := []struct {
		name    string
		path    string
		allowed []string
	}{
		{"register", "/api/register", []string{http.MethodPost}},
		{"authenticate", "/api/auth", []string{http.MethodPost}},
		{"logout", "/api/auth/session-token", []string{http.MethodDelete}},
		{"documents", "/api/docs", []string{http.MethodGet, http.MethodHead, http.MethodPost}},
		{"document", "/api/docs/document-id", []string{http.MethodDelete, http.MethodGet, http.MethodHead}},
	}
	allMethods := []string{http.MethodDelete, http.MethodGet, http.MethodHead, http.MethodPatch, http.MethodPost, http.MethodPut}
	h := NewHandler(Dependencies{})

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			allow := strings.Join(tt.allowed, ", ")
			for _, method := range allMethods {
				recorder := httptest.NewRecorder()
				h.ServeHTTP(recorder, httptest.NewRequest(method, tt.path, nil))
				if contains(tt.allowed, method) {
					if recorder.Code != http.StatusNotImplemented {
						t.Errorf("%s status = %d, want 501", method, recorder.Code)
					}
				} else {
					if recorder.Code != http.StatusMethodNotAllowed {
						t.Errorf("%s status = %d, want 405", method, recorder.Code)
					}
					if got := recorder.Header().Get("Allow"); got != allow {
						t.Errorf("%s Allow = %q, want %q", method, got, allow)
					}
				}
				if method == http.MethodHead {
					if recorder.Body.Len() != 0 {
						t.Errorf("HEAD body length = %d", recorder.Body.Len())
					}
				} else {
					assertErrorEnvelope(t, recorder)
				}
			}
		})
	}
}

func TestJSONEnvelopeVariants(t *testing.T) {
	tests := []struct {
		name  string
		write func(http.ResponseWriter)
		key   string
	}{
		{"response", func(w http.ResponseWriter) { writeResponse(w, 200, map[string]bool{"ok": true}) }, "response"},
		{"data", func(w http.ResponseWriter) { writeData(w, 200, nil) }, "data"},
		{"error", func(w http.ResponseWriter) { writeDomainError(w, domain.ErrForbidden) }, "error"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			tt.write(recorder)
			if got := recorder.Header().Get("Content-Type"); got != "application/json; charset=utf-8" {
				t.Fatalf("Content-Type = %q", got)
			}
			var body map[string]json.RawMessage
			if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
				t.Fatalf("invalid JSON: %v", err)
			}
			if len(body) != 1 || body[tt.key] == nil {
				t.Fatalf("body fields = %v, want only %q", body, tt.key)
			}
		})
	}
}

func TestDomainErrorMapping(t *testing.T) {
	tests := []struct {
		err    error
		status int
	}{
		{domain.ErrInvalidArgument, 400},
		{domain.ErrUnauthorized, 401},
		{domain.ErrForbidden, 403},
		{domain.ErrNotFound, 404},
		{domain.ErrNotImplemented, 501},
		{errors.New("database query included a secret path"), 500},
	}
	for _, tt := range tests {
		recorder := httptest.NewRecorder()
		writeDomainError(recorder, tt.err)
		if recorder.Code != tt.status {
			t.Errorf("writeDomainError(%v) status = %d, want %d", tt.err, recorder.Code, tt.status)
		}
		if strings.Contains(recorder.Body.String(), "database") || strings.Contains(recorder.Body.String(), "path") {
			t.Errorf("internal details leaked: %s", recorder.Body.String())
		}
	}
}

func TestHEADHasGETHeadersAndNoBody(t *testing.T) {
	h := NewHandler(Dependencies{})
	get := httptest.NewRecorder()
	h.ServeHTTP(get, httptest.NewRequest(http.MethodGet, "/api/docs", nil))
	head := httptest.NewRecorder()
	h.ServeHTTP(head, httptest.NewRequest(http.MethodHead, "/api/docs", nil))

	if head.Code != get.Code || head.Header().Get("Content-Type") != get.Header().Get("Content-Type") ||
		head.Header().Get("Content-Length") != get.Header().Get("Content-Length") {
		t.Fatalf("HEAD metadata differs: GET=%v HEAD=%v", get.Result(), head.Result())
	}
	if head.Body.Len() != 0 {
		t.Fatalf("HEAD body length = %d", head.Body.Len())
	}
}

func TestRequestSizeLimit(t *testing.T) {
	h := NewHandler(Dependencies{Limits: Limits{MaxRequestBytes: 3}})
	for _, test := range []struct {
		name   string
		body   string
		status int
	}{
		{"below", "12", http.StatusNotImplemented},
		{"at limit", "123", http.StatusNotImplemented},
		{"above", "1234", http.StatusBadRequest},
	} {
		t.Run(test.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			h.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/api/register", strings.NewReader(test.body)))
			if recorder.Code != test.status {
				t.Fatalf("status = %d, want %d", recorder.Code, test.status)
			}
			assertErrorEnvelope(t, recorder)
		})
	}
}

func TestPanicIsSafeAndSubsequentRequestsWork(t *testing.T) {
	h := NewHandler(Dependencies{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}).(*handler)
	panicResponse := httptest.NewRecorder()
	h.serveHTTP(panicResponse, httptest.NewRequest(http.MethodGet, "/api/docs", nil), http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("sensitive partial response"))
		panic("database SQL and internal path")
	}))
	if panicResponse.Code != http.StatusInternalServerError || strings.Contains(panicResponse.Body.String(), "SQL") {
		t.Fatalf("unsafe panic response: %d %q", panicResponse.Code, panicResponse.Body.String())
	}
	assertErrorEnvelope(t, panicResponse)

	next := httptest.NewRecorder()
	h.ServeHTTP(next, httptest.NewRequest(http.MethodGet, "/health/live", nil))
	if next.Code != http.StatusOK {
		t.Fatalf("subsequent status = %d", next.Code)
	}
}

func TestRequestIDIsGeneratedAndAvailableInContext(t *testing.T) {
	h := NewHandler(Dependencies{}).(*handler)
	recorder := httptest.NewRecorder()
	var contextID string
	h.serveHTTP(recorder, httptest.NewRequest(http.MethodGet, "/", nil), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		contextID = RequestID(r.Context())
		writeData(w, http.StatusOK, true)
	}))
	if contextID == "" || recorder.Header().Get("X-Request-ID") != contextID {
		t.Fatalf("header ID %q, context ID %q", recorder.Header().Get("X-Request-ID"), contextID)
	}
}

func contains(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func assertErrorEnvelope(t *testing.T, recorder *httptest.ResponseRecorder) {
	t.Helper()
	if recorder.Header().Get("Content-Type") != "application/json; charset=utf-8" {
		t.Errorf("Content-Type = %q", recorder.Header().Get("Content-Type"))
	}
	var body map[string]json.RawMessage
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("invalid JSON error: %v; body=%q", err, recorder.Body.String())
	}
	if len(body) != 1 || body["error"] == nil {
		t.Fatalf("error response fields = %v", body)
	}
}
