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
	if recorder.Code != http.StatusServiceUnavailable || time.Since(started) > time.Second {
		t.Fatalf("timeout response: status=%d duration=%s", recorder.Code, time.Since(started))
	}
}
