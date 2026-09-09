package httptransport

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"documents/internal/domain"
)

const (
	errorCodeBadRequest       = 400
	errorCodeUnauthorized     = 401
	errorCodeForbidden        = 403
	errorCodeNotFound         = 404
	errorCodeMethodNotAllowed = 405
	errorCodeInternal         = 500
	errorCodeNotImplemented   = 501
	errorCodeTimeout          = 503
)

type errorPayload struct {
	Code int    `json:"code"`
	Text string `json:"text"`
}

type errorEnvelope struct {
	Error errorPayload `json:"error"`
}

func writeResponse(w http.ResponseWriter, status int, response any) {
	writeJSON(w, status, map[string]any{"response": response})
}

// writeData deliberately includes data even when it is nil: JSON documents
// may validly contain null, which must not be lost through omitempty.
func writeData(w http.ResponseWriter, status int, data any) {
	writeJSON(w, status, map[string]any{"data": data})
}

func writeAPIError(w http.ResponseWriter, status, code int, text string) {
	writeJSON(w, status, errorEnvelope{Error: errorPayload{Code: code, Text: text}})
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	encoded, err := json.Marshal(value)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, errorCodeInternal, "internal server error")
		return
	}
	encoded = append(encoded, '\n')
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Content-Length", strconv.Itoa(len(encoded)))
	w.WriteHeader(status)
	_, _ = w.Write(encoded)
}

func writeDomainError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, domain.ErrInvalidArgument):
		writeAPIError(w, http.StatusBadRequest, errorCodeBadRequest, "bad request")
	case errors.Is(err, domain.ErrUnauthorized):
		writeAPIError(w, http.StatusUnauthorized, errorCodeUnauthorized, "unauthorized")
	case errors.Is(err, domain.ErrForbidden):
		writeAPIError(w, http.StatusForbidden, errorCodeForbidden, "forbidden")
	case errors.Is(err, domain.ErrNotFound):
		writeAPIError(w, http.StatusNotFound, errorCodeNotFound, "resource not found")
	case errors.Is(err, domain.ErrNotImplemented):
		writeAPIError(w, http.StatusNotImplemented, errorCodeNotImplemented, "operation not implemented")
	default:
		writeAPIError(w, http.StatusInternalServerError, errorCodeInternal, "internal server error")
	}
}
