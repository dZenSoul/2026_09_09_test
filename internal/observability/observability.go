// Package observability provides logging and metrics boundaries.
package observability

import (
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

type Metrics interface {
	ObserveRequest(route, method string, status int, seconds float64)
	ObserveCache(hit bool)
	SetCacheSize(bytes int64, items int)
	ObserveFile(direction string, bytes int64)
}

// Registry exports Prometheus text metrics with labels selected only from
// server-owned route templates and enums, keeping cardinality bounded.
type Registry struct {
	mu         sync.Mutex
	requests   map[requestKey]*requestValue
	cacheHits  [2]uint64
	cacheBytes int64
	cacheItems int
	dbCount    uint64
	dbErrors   uint64
	dbSeconds  float64
	fileBytes  map[string]uint64
}

type requestKey struct {
	route, method string
	status        int
}
type requestValue struct {
	count   uint64
	seconds float64
}

func NewMetrics() *Registry {
	return &Registry{requests: make(map[requestKey]*requestValue), fileBytes: make(map[string]uint64)}
}

func (m *Registry) ObserveRequest(route, method string, status int, seconds float64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	k := requestKey{route, method, status}
	v := m.requests[k]
	if v == nil {
		v = &requestValue{}
		m.requests[k] = v
	}
	v.count++
	v.seconds += seconds
}

func (m *Registry) ObserveCache(hit bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if hit {
		m.cacheHits[1]++
	} else {
		m.cacheHits[0]++
	}
}

func (m *Registry) SetCacheSize(bytes int64, items int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.cacheBytes, m.cacheItems = bytes, items
}

func (m *Registry) ObserveDatabase(elapsed time.Duration, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.dbCount++
	m.dbSeconds += elapsed.Seconds()
	if err != nil {
		m.dbErrors++
	}
}

func (m *Registry) ObserveFile(direction string, bytes int64) {
	if bytes < 0 || (direction != "upload" && direction != "download") {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.fileBytes[direction] += uint64(bytes)
}

func (m *Registry) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	m.mu.Lock()
	keys := make([]requestKey, 0, len(m.requests))
	for k := range m.requests {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		a, b := keys[i], keys[j]
		if a.route != b.route {
			return a.route < b.route
		}
		if a.method != b.method {
			return a.method < b.method
		}
		return a.status < b.status
	})
	var b strings.Builder
	for _, k := range keys {
		v := *m.requests[k]
		labels := fmt.Sprintf("route=%q,method=%q,status=%q", k.route, k.method, strconv.Itoa(k.status))
		fmt.Fprintf(&b, "documents_http_requests_total{%s} %d\n", labels, v.count)
		fmt.Fprintf(&b, "documents_http_request_duration_seconds_sum{%s} %g\n", labels, v.seconds)
		fmt.Fprintf(&b, "documents_http_request_duration_seconds_count{%s} %d\n", labels, v.count)
		if k.status >= 500 {
			fmt.Fprintf(&b, "documents_http_errors_total{%s} %d\n", labels, v.count)
		}
	}
	fmt.Fprintf(&b, "documents_cache_requests_total{result=\"miss\"} %d\ndocuments_cache_requests_total{result=\"hit\"} %d\n", m.cacheHits[0], m.cacheHits[1])
	fmt.Fprintf(&b, "documents_cache_size_bytes %d\ndocuments_cache_items %d\n", m.cacheBytes, m.cacheItems)
	fmt.Fprintf(&b, "documents_database_operation_duration_seconds_sum %g\ndocuments_database_operation_duration_seconds_count %d\ndocuments_database_errors_total %d\n", m.dbSeconds, m.dbCount, m.dbErrors)
	fmt.Fprintf(&b, "documents_file_bytes_total{direction=\"upload\"} %d\ndocuments_file_bytes_total{direction=\"download\"} %d\n", m.fileBytes["upload"], m.fileBytes["download"])
	m.mu.Unlock()
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, b.String())
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
