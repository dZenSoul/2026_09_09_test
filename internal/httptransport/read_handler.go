package httptransport

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strconv"

	"documents/internal/document"
	"documents/internal/domain"
)

type documentMetadataReader interface {
	GetMetadata(context.Context, domain.User, string) (domain.Document, error)
}

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
	}

	h.loadDocumentContent(w, r, requester, id)
}

func (h *handler) loadDocumentContent(w http.ResponseWriter, r *http.Request, requester domain.User, id string) bool {
	content, err := h.deps.Documents.Get(r.Context(), requester, id)
	if err != nil {
		writeDomainError(w, err)
		return false
	}
	if content.File != nil {
		defer content.File.Close()
	}
	return writeDocumentContent(w, content) == nil
}

func writeDocumentMetadata(w http.ResponseWriter, metadata domain.Document) {
	if !metadata.IsFile {
		writeData(w, http.StatusOK, json.RawMessage(metadata.JSON))
		return
	}
	writeFileHeaders(w, metadata)
	w.WriteHeader(http.StatusOK)
}

func writeDocumentContent(w http.ResponseWriter, content document.Content) error {
	if !content.Document.IsFile {
		writeData(w, http.StatusOK, json.RawMessage(content.Document.JSON))
		return nil
	}
	if content.File == nil {
		writeAPIError(w, http.StatusInternalServerError, errorCodeInternal, "internal server error")
		return errors.New("file document has no body")
	}
	writeFileHeaders(w, content.Document)
	w.WriteHeader(http.StatusOK)
	_, err := io.Copy(w, content.File)
	return err
}

func writeFileHeaders(w http.ResponseWriter, metadata domain.Document) {
	w.Header().Set("Content-Type", metadata.MIME)
	w.Header().Set("Content-Length", strconv.FormatInt(metadata.SizeBytes, 10))
	// FormatMediaType quotes and escapes ASCII special characters and emits an
	// RFC 5987 filename parameter for names that cannot safely fit in a header.
	w.Header().Set("Content-Disposition", mime.FormatMediaType("inline", map[string]string{"filename": metadata.Name}))
}
