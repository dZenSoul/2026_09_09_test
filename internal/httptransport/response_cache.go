package httptransport

import (
	"bytes"
	"context"
	"fmt"
	"net/http"

	responsecache "documents/internal/cache"
)

const listCacheTag = "documents:list"

// bufferedResponse is intentionally limited to small, non-streaming responses
// such as list and JSON documents. File responses use the direct streaming
// path in read_handler.go.
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

func (h *handler) cachedResponse(ctx context.Context, key string, tags []string, acceptBodyOmitted bool, load func(http.ResponseWriter) bool) (responsecache.Entry, bool) {
	if entry, ok := h.deps.Cache.Get(ctx, key); ok && (acceptBodyOmitted || !entry.BodyOmitted) {
		h.observeCache(ctx, true)
		return entry, true
	}
	result := h.loads.DoChan(key, func() (any, error) {
		if entry, ok := h.deps.Cache.Get(ctx, key); ok && (acceptBodyOmitted || !entry.BodyOmitted) {
			return cacheLoad{entry: entry, hit: true}, nil
		}
		response := newBufferedResponse()
		complete := load(response)
		entry := responsecache.Entry{Status: response.status, Header: cloneHeader(response.header), Body: append([]byte(nil), response.body.Bytes()...), Tags: append([]string(nil), tags...)}
		if complete && entry.Status >= 200 && entry.Status < 300 {
			h.deps.Cache.Set(context.WithoutCancel(ctx), key, entry, h.deps.CacheTTL)
		}
		return cacheLoad{entry: entry}, nil
	})
	select {
	case <-ctx.Done():
		return responsecache.Entry{}, false
	case loaded := <-result:
		if loaded.Err != nil {
			return responsecache.Entry{}, false
		}
		value := loaded.Val.(cacheLoad)
		h.observeCache(ctx, value.hit)
		h.observeCacheSize()
		return value.entry, value.hit
	}
}

func (h *handler) observeCache(ctx context.Context, hit bool) {
	if state, ok := ctx.Value(observationKey).(*requestObservation); ok {
		if hit {
			state.setCache("hit")
		} else {
			state.setCache("miss")
		}
	}
	if h.deps.Metrics != nil {
		h.deps.Metrics.ObserveCache(hit)
	}
}

func (h *handler) observeCacheSize() {
	if h.deps.Metrics == nil {
		return
	}
	if cache, ok := h.deps.Cache.(interface{ Stats() (int64, int) }); ok {
		bytes, items := cache.Stats()
		h.deps.Metrics.SetCacheSize(bytes, items)
	}
}

type cacheLoad struct {
	entry responsecache.Entry
	hit   bool
}

func writeCachedResponse(w http.ResponseWriter, entry responsecache.Entry) {
	for name, values := range entry.Header {
		w.Header()[name] = append([]string(nil), values...)
	}
	status := entry.Status
	if status == 0 {
		status = http.StatusOK
	}
	w.WriteHeader(status)
	_, _ = w.Write(entry.Body)
}

func cloneHeader(header http.Header) http.Header {
	result := make(http.Header, len(header))
	for name, values := range header {
		result[name] = append([]string(nil), values...)
	}
	return result
}

func (h *handler) listCacheKey(userID, owner, filterKey, value string, limit int) string {
	return fmt.Sprintf("list:v%d:u=%q:o=%q:k=%q:value=%q:limit=%d", h.cacheEpoch.Load(), userID, owner, filterKey, normalizedFilterValue(filterKey, value), limit)
}

func (h *handler) documentCacheKey(id string, version int64) string {
	return fmt.Sprintf("document:v%d:id=%q:version=%d", h.cacheEpoch.Load(), id, version)
}

func (h *handler) exposeCacheStatus(w http.ResponseWriter, hit bool) {
	if !h.deps.ExposeCacheHeader {
		return
	}
	if hit {
		w.Header().Set("X-Cache", "HIT")
	} else {
		w.Header().Set("X-Cache", "MISS")
	}
}
