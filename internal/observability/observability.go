// Package observability provides logging and metrics boundaries.
package observability

import (
	"io"
	"log/slog"
)

type Metrics interface {
	ObserveRequest(route, method string, status int, seconds float64)
	ObserveCache(hit bool)
	SetCacheSize(bytes int64, items int)
}

func NewLogger(output io.Writer, level string) *slog.Logger {
	var slogLevel slog.Level
	switch level {
	case "debug":
		slogLevel = slog.LevelDebug
	case "warn":
		slogLevel = slog.LevelWarn
	case "error":
		slogLevel = slog.LevelError
	default:
		slogLevel = slog.LevelInfo
	}
	return slog.New(slog.NewJSONHandler(output, &slog.HandlerOptions{Level: slogLevel}))
}
