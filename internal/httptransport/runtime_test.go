package httptransport

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"documents/internal/observability"
)

func TestRequestLogRedactsURLAndFormSecrets(t *testing.T) {
	var output bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&output, nil))
	h := NewHandler(Dependencies{Auth: &fakeAuth{}, Logger: logger})

	request := httptest.NewRequest(http.MethodPost, "/api/register?token=query-secret", strings.NewReader("token=form-secret&login=testuser&pswd=Password1%21"))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	h.ServeHTTP(httptest.NewRecorder(), request)
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodDelete, "/api/auth/path-secret", nil))

	logs := output.String()
	for _, secret := range []string{"query-secret", "form-secret", "Password1!", "path-secret"} {
		if strings.Contains(logs, secret) {
			t.Fatalf("logs contain secret %q: %s", secret, logs)
		}
	}
	if !strings.Contains(logs, `"route":"/api/auth/{token}"`) || !strings.Contains(logs, `"request_id":`) {
		t.Fatalf("logs lack normalized route or request ID: %s", logs)
	}
}

func TestMetricsUseOnlyNormalizedRoutes(t *testing.T) {
	metrics := observability.NewMetrics()
	h := NewHandler(Dependencies{Metrics: metrics})
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/api/docs/sensitive-document-id?token=sensitive-token", nil))

	recorder := httptest.NewRecorder()
	metrics.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	body := recorder.Body.String()
	if strings.Contains(body, "sensitive-document-id") || strings.Contains(body, "sensitive-token") {
		t.Fatalf("metrics contain high-cardinality or secret input: %s", body)
	}
	if !strings.Contains(body, `route="/api/docs/{id}"`) {
		t.Fatalf("metrics lack normalized route: %s", body)
	}
}

func TestReadinessReflectsDependencyState(t *testing.T) {
	h := NewHandler(Dependencies{Readiness: func(_ context.Context) error { return errors.New("database details") }})
	recorder := httptest.NewRecorder()
	h.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/health/ready", nil))
	if recorder.Code != http.StatusServiceUnavailable || strings.Contains(recorder.Body.String(), "database") {
		t.Fatalf("unsafe readiness response: %d %s", recorder.Code, recorder.Body.String())
	}
}

func TestProcessingTimeoutStopsARequest(t *testing.T) {
	h := NewHandler(Dependencies{
		ProcessingTimeout: 20 * time.Millisecond,
		Readiness: func(ctx context.Context) error {
			<-ctx.Done()
			return ctx.Err()
		},
	})
	recorder := httptest.NewRecorder()
	started := time.Now()
	h.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/health/ready", nil))
	want := "{\"error\":{\"code\":503,\"text\":\"request timed out\"}}\n"
	if recorder.Code != http.StatusServiceUnavailable || recorder.Body.String() != want ||
		recorder.Header().Get("Content-Type") != "application/json; charset=utf-8" ||
		recorder.Header().Get("Content-Length") != "50" || recorder.Header().Get("X-Request-ID") == "" ||
		time.Since(started) > time.Second {
		t.Fatalf("timeout response: status=%d duration=%s", recorder.Code, time.Since(started))
	}
}

func TestProcessingTimeoutHEADHasRepresentationHeadersAndNoBody(t *testing.T) {
	h := NewHandler(Dependencies{}).(*handler)
	request := httptest.NewRequest(http.MethodHead, "/api/docs", nil)
	ctx, cancel := context.WithTimeout(request.Context(), 10*time.Millisecond)
	request = request.WithContext(ctx)
	recorder := httptest.NewRecorder()
	h.serveHTTP(recorder, request, http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}), cancel)

	if recorder.Code != http.StatusServiceUnavailable || recorder.Body.Len() != 0 ||
		recorder.Header().Get("Content-Type") != "application/json; charset=utf-8" ||
		recorder.Header().Get("Content-Length") != "50" {
		t.Fatalf("HEAD timeout: status=%d headers=%v body=%q", recorder.Code, recorder.Header(), recorder.Body)
	}
}

func TestStreamingTimeoutDoesNotAppendJSONAndRejectsLaterWrites(t *testing.T) {
	h := NewHandler(Dependencies{}).(*handler)
	request := httptest.NewRequest(http.MethodGet, "/api/docs/id", nil)
	ctx, cancel := context.WithTimeout(request.Context(), 10*time.Millisecond)
	request = request.WithContext(ctx)
	recorder := httptest.NewRecorder()
	lateWrite := make(chan error, 1)
	h.serveHTTP(recorder, request, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		beginStreaming(w)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("partial"))
		<-r.Context().Done()
		_, err := w.Write([]byte("late"))
		lateWrite <- err
	}), cancel)

	if err := <-lateWrite; !errors.Is(err, http.ErrHandlerTimeout) {
		t.Fatalf("late write error=%v", err)
	}
	if recorder.Code != http.StatusOK || recorder.Body.String() != "partial" || strings.Contains(recorder.Body.String(), "request timed out") {
		t.Fatalf("streaming timeout: status=%d body=%q", recorder.Code, recorder.Body)
	}
}
