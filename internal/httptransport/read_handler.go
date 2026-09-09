package httptransport

import (
	"context"
	"encoding/json"
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

	content, err := h.deps.Documents.Get(r.Context(), requester, id)
	if err != nil {
		writeDomainError(w, err)
		return
	}
	if content.File != nil {
		defer content.File.Close()
	}
	if r.Method == http.MethodHead {
		writeDocumentMetadata(w, content.Document)
		return
	}
	writeDocumentContent(w, content)
}

func writeDocumentMetadata(w http.ResponseWriter, metadata domain.Document) {
	if !metadata.IsFile {
		writeData(w, http.StatusOK, json.RawMessage(metadata.JSON))
		return
	}
	writeFileHeaders(w, metadata)
	w.WriteHeader(http.StatusOK)
}

func writeDocumentContent(w http.ResponseWriter, content document.Content) {
	if !content.Document.IsFile {
		writeData(w, http.StatusOK, json.RawMessage(content.Document.JSON))
		return
	}
	if content.File == nil {
		writeAPIError(w, http.StatusInternalServerError, errorCodeInternal, "internal server error")
		return
	}
	writeFileHeaders(w, content.Document)
	w.WriteHeader(http.StatusOK)
	_, _ = io.Copy(w, content.File)
}

func writeFileHeaders(w http.ResponseWriter, metadata domain.Document) {
	w.Header().Set("Content-Type", metadata.MIME)
	w.Header().Set("Content-Length", strconv.FormatInt(metadata.SizeBytes, 10))
	// FormatMediaType quotes and escapes ASCII special characters and emits an
	// RFC 5987 filename parameter for names that cannot safely fit in a header.
	w.Header().Set("Content-Disposition", mime.FormatMediaType("inline", map[string]string{"filename": metadata.Name}))
}
