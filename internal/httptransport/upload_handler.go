package httptransport

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"strings"

	"documents/internal/document"
	"documents/internal/domain"
)

type uploadMeta struct {
	Name   *string  `json:"name"`
	File   *bool    `json:"file"`
	Public *bool    `json:"public"`
	Token  *string  `json:"token"`
	MIME   *string  `json:"mime"`
	Grant  []string `json:"grant"`
}

func (h *handler) uploadDocument(w http.ResponseWriter, r *http.Request) {
	if h.deps.Auth == nil || h.deps.Documents == nil {
		writeAPIError(w, http.StatusNotImplemented, errorCodeNotImplemented, "operation not implemented")
		return
	}
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "multipart/form-data" {
		writeAPIError(w, http.StatusBadRequest, errorCodeBadRequest, "bad request")
		return
	}
	// ParseMultipartForm retains ordinary fields in memory and streams file
	// parts above this allowance to temporary files. The outer MaxBytesReader
	// still enforces the complete HTTP request limit.
	if err := r.ParseMultipartForm(h.deps.Limits.MaxJSONBytes); err != nil {
		writeAPIError(w, http.StatusBadRequest, errorCodeBadRequest, "bad request")
		return
	}
	defer r.MultipartForm.RemoveAll()
	if !knownUploadParts(r.MultipartForm) {
		writeAPIError(w, http.StatusBadRequest, errorCodeBadRequest, "bad request")
		return
	}

	metaBody, metaPresent, err := readMultipartValue(r.MultipartForm, "meta", h.deps.Limits.MaxRequestBytes)
	if err != nil || !metaPresent {
		writeAPIError(w, http.StatusBadRequest, errorCodeBadRequest, "bad request")
		return
	}
	meta, err := decodeUploadMeta(metaBody)
	if err != nil || len(meta.Grant) > h.deps.Limits.MaxGrantItems {
		writeAPIError(w, http.StatusBadRequest, errorCodeBadRequest, "bad request")
		return
	}

	requester, err := h.deps.Auth.Authorize(r.Context(), *meta.Token)
	if err != nil {
		writeDomainError(w, err)
		return
	}

	jsonBody, jsonPresent, err := readMultipartValue(r.MultipartForm, "json", h.deps.Limits.MaxJSONBytes)
	if err != nil || (jsonPresent && !json.Valid(jsonBody)) {
		writeAPIError(w, http.StatusBadRequest, errorCodeBadRequest, "bad request")
		return
	}
	file, fileSize, filePresent, err := openMultipartFile(r.MultipartForm, "file")
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, errorCodeBadRequest, "bad request")
		return
	}
	if file != nil {
		defer file.Close()
	}
	if fileSize > h.deps.Limits.MaxFileBytes || !validUploadShape(meta, jsonPresent, filePresent) {
		writeAPIError(w, http.StatusBadRequest, errorCodeBadRequest, "bad request")
		return
	}

	input := document.Upload{Document: domain.Document{
		Name: *meta.Name, IsFile: *meta.File, IsPublic: *meta.Public,
		JSON: jsonBody, Grants: meta.Grant,
	}}
	if meta.MIME != nil {
		input.Document.MIME = *meta.MIME
	}
	if filePresent {
		input.File = file
	}
	if _, err := h.deps.Documents.Upload(r.Context(), requester, input); err != nil {
		writeDomainError(w, err)
		return
	}
	if filePresent && h.deps.Metrics != nil {
		h.deps.Metrics.ObserveFile("upload", fileSize)
	}
	if h.deps.Cache != nil {
		h.cacheEpoch.Add(1)
		h.deps.Cache.Invalidate(context.WithoutCancel(r.Context()), listCacheTag)
		h.observeCacheSize()
	}

	data := make(map[string]any, 2)
	if jsonPresent {
		data["json"] = json.RawMessage(jsonBody)
	}
	if filePresent {
		data["file"] = *meta.Name
	}
	writeData(w, http.StatusOK, data)
}

func decodeUploadMeta(raw []byte) (uploadMeta, error) {
	var meta uploadMeta
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	if err := decoder.Decode(&meta); err != nil {
		return uploadMeta{}, err
	}
	if meta.Name == nil || meta.File == nil || meta.Public == nil || meta.Token == nil {
		return uploadMeta{}, errors.New("required upload metadata is missing")
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return uploadMeta{}, errors.New("upload metadata has trailing content")
	}
	return meta, nil
}

func validUploadShape(meta uploadMeta, jsonPresent, filePresent bool) bool {
	if *meta.File {
		return filePresent && meta.MIME != nil && *meta.MIME != ""
	}
	if filePresent || !jsonPresent {
		return false
	}
	return meta.MIME == nil || *meta.MIME != ""
}

func knownUploadParts(form *multipart.Form) bool {
	for name := range form.Value {
		if name != "meta" && name != "json" && name != "file" {
			return false
		}
	}
	for name := range form.File {
		if name != "meta" && name != "json" && name != "file" {
			return false
		}
	}
	return true
}

func readMultipartValue(form *multipart.Form, name string, limit int64) ([]byte, bool, error) {
	values := form.Value[name]
	files := form.File[name]
	if len(values)+len(files) == 0 {
		return nil, false, nil
	}
	if len(values)+len(files) != 1 {
		return nil, true, errors.New("multipart part occurs more than once")
	}
	if len(values) == 1 {
		if int64(len(values[0])) > limit {
			return nil, true, errors.New("multipart part is too large")
		}
		return []byte(values[0]), true, nil
	}
	reader, err := files[0].Open()
	if err != nil {
		return nil, true, err
	}
	defer reader.Close()
	return readAtMost(reader, limit)
}

func readAtMost(reader io.Reader, limit int64) ([]byte, bool, error) {
	content, err := io.ReadAll(io.LimitReader(reader, limit+1))
	if err != nil {
		return nil, true, err
	}
	if int64(len(content)) > limit {
		return nil, true, fmt.Errorf("multipart part exceeds %d bytes", limit)
	}
	return content, true, nil
}

func openMultipartFile(form *multipart.Form, name string) (io.ReadCloser, int64, bool, error) {
	values := form.Value[name]
	files := form.File[name]
	if len(values)+len(files) == 0 {
		return nil, 0, false, nil
	}
	if len(values)+len(files) != 1 {
		return nil, 0, true, errors.New("multipart part occurs more than once")
	}
	if len(files) == 1 {
		reader, err := files[0].Open()
		return reader, files[0].Size, true, err
	}
	return io.NopCloser(strings.NewReader(values[0])), int64(len(values[0])), true, nil
}
