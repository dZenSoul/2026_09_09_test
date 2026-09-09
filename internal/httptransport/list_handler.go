package httptransport

import (
	"net/http"
	"net/url"
	"strconv"
	"time"

	"documents/internal/domain"

	"github.com/google/uuid"
)

const responseDocumentTimeFormat = "2006-01-02 15:04:05"

type documentListItem struct {
	ID      string   `json:"id"`
	Name    string   `json:"name"`
	MIME    string   `json:"mime,omitempty"`
	File    bool     `json:"file"`
	Public  bool     `json:"public"`
	Created string   `json:"created"`
	Grant   []string `json:"grant"`
}

func (h *handler) listDocuments(w http.ResponseWriter, r *http.Request) {
	if h.deps.Auth == nil || h.deps.Documents == nil {
		writeAPIError(w, http.StatusNotImplemented, errorCodeNotImplemented, "operation not implemented")
		return
	}

	query, parseErr := url.ParseQuery(r.URL.RawQuery)
	if parseErr != nil {
		writeAPIError(w, http.StatusBadRequest, errorCodeBadRequest, "bad request")
		return
	}
	token, ok := singleQueryValue(query, "token")
	if !ok && len(query["token"]) > 1 {
		writeAPIError(w, http.StatusBadRequest, errorCodeBadRequest, "bad request")
		return
	}
	requester, err := h.deps.Auth.Authorize(r.Context(), token)
	if err != nil {
		writeDomainError(w, err)
		return
	}

	filter, ok := h.parseDocumentListFilter(query)
	if !ok {
		writeAPIError(w, http.StatusBadRequest, errorCodeBadRequest, "bad request")
		return
	}
	documents, err := h.deps.Documents.List(r.Context(), requester, filter)
	if err != nil {
		writeDomainError(w, err)
		return
	}

	items := make([]documentListItem, len(documents))
	for index, document := range documents {
		grants := document.Grants
		if grants == nil {
			grants = []string{}
		}
		items[index] = documentListItem{
			ID: document.ID, Name: document.Name, MIME: document.MIME,
			File: document.IsFile, Public: document.IsPublic,
			Created: document.CreatedAt.UTC().Format(responseDocumentTimeFormat), Grant: grants,
		}
	}
	writeData(w, http.StatusOK, map[string]any{"docs": items})
}

func (h *handler) parseDocumentListFilter(query url.Values) (domain.DocumentFilter, bool) {
	for name, values := range query {
		switch name {
		case "token", "login", "key", "value", "limit":
		default:
			return domain.DocumentFilter{}, false
		}
		if len(values) != 1 {
			return domain.DocumentFilter{}, false
		}
	}

	login, loginPresent := queryValue(query, "login")
	key, keyPresent := queryValue(query, "key")
	value, valuePresent := queryValue(query, "value")
	if (keyPresent && (!valuePresent || key == "" || value == "")) || (!keyPresent && valuePresent) {
		return domain.DocumentFilter{}, false
	}
	if loginPresent && login == "" {
		return domain.DocumentFilter{}, false
	}
	if keyPresent && !validDocumentListFilterValue(key, value) {
		return domain.DocumentFilter{}, false
	}

	filter := domain.DocumentFilter{OwnerLogin: login, Key: key, Value: value}
	if rawLimit, present := queryValue(query, "limit"); present {
		limit, err := strconv.Atoi(rawLimit)
		if err != nil || limit <= 0 || limit > h.deps.Limits.MaxListLimit {
			return domain.DocumentFilter{}, false
		}
		filter.Limit = limit
	}
	return filter, true
}

func validDocumentListFilterValue(key, value string) bool {
	switch key {
	case "id":
		_, err := uuid.Parse(value)
		return err == nil
	case "name", "mime":
		return true
	case "file", "public":
		return value == "true" || value == "false"
	case "created":
		if _, err := time.ParseInLocation(responseDocumentTimeFormat, value, time.UTC); err == nil {
			return true
		}
		_, err := time.Parse(time.RFC3339, value)
		return err == nil
	default:
		return false
	}
}

func singleQueryValue(query url.Values, name string) (string, bool) {
	values, present := query[name]
	if !present || len(values) != 1 {
		return "", false
	}
	return values[0], true
}

func queryValue(query url.Values, name string) (string, bool) {
	values, present := query[name]
	if !present || len(values) == 0 {
		return "", false
	}
	return values[0], true
}
