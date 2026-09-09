package httptransport

import (
	"io"
	"net/http"
	"net/url"
	"strings"
)

func (h *handler) deleteDocument(w http.ResponseWriter, r *http.Request, id string) {
	if h.deps.Auth == nil || h.deps.Documents == nil {
		writeAPIError(w, http.StatusNotImplemented, errorCodeNotImplemented, "operation not implemented")
		return
	}
	// Authentication material for DELETE is deliberately accepted only in the
	// request body so it does not leak through URLs, browser history or access logs.
	if r.URL.RawQuery != "" || !parseDeleteForm(w, r) {
		if r.URL.RawQuery != "" {
			writeAPIError(w, http.StatusBadRequest, errorCodeBadRequest, "bad request")
		}
		return
	}
	if len(r.PostForm) != 1 || len(r.PostForm["token"]) != 1 {
		writeAPIError(w, http.StatusBadRequest, errorCodeBadRequest, "bad request")
		return
	}
	token := r.PostForm.Get("token")
	requester, err := h.deps.Auth.Authorize(r.Context(), token)
	if err != nil {
		writeDomainError(w, err)
		return
	}
	if err := h.deps.Documents.Delete(r.Context(), requester, id); err != nil {
		writeDomainError(w, err)
		return
	}
	writeResponse(w, http.StatusOK, map[string]bool{id: true})
}

func parseDeleteForm(w http.ResponseWriter, r *http.Request) bool {
	contentType := r.Header.Get("Content-Type")
	if mediaType := strings.TrimSpace(strings.SplitN(contentType, ";", 2)[0]); mediaType != "application/x-www-form-urlencoded" {
		writeAPIError(w, http.StatusBadRequest, errorCodeBadRequest, "bad request")
		return false
	}
	encoded, err := io.ReadAll(r.Body)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, errorCodeBadRequest, "bad request")
		return false
	}
	form, err := url.ParseQuery(string(encoded))
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, errorCodeBadRequest, "bad request")
		return false
	}
	r.PostForm = form
	return true
}
