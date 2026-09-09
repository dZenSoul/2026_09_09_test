// Package httptransport owns HTTP-specific request and response handling.
package httptransport

import (
	"encoding/json"
	"log/slog"
	"net/http"

	"documents/internal/auth"
	"documents/internal/document"
)

type Dependencies struct {
	Auth      auth.Service
	Documents document.Service
	Logger    *slog.Logger
}

// NewHandler is the HTTP composition point. API routes are intentionally left
// for the API foundation task; the liveness endpoint makes this scaffold
// runnable and independently testable.
func NewHandler(deps Dependencies) http.Handler {
	logger := deps.Logger
	if logger == nil {
		logger = slog.Default()
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health/live", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		logger.Debug("route not found", "method", r.Method, "path", r.URL.Path)
		http.NotFound(w, r)
	})
	return mux
}
