package httptransport

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"documents/internal/document"
	"documents/internal/domain"
)

func TestUploadDocumentHTTP(t *testing.T) {
	documents := &uploadDocuments{}
	handler := NewHandler(Dependencies{
		Auth: &uploadAuth{user: domain.User{ID: "owner-id", Login: "owner000"}}, Documents: documents,
		Limits: Limits{MaxRequestBytes: 4096, MaxFileBytes: 4, MaxJSONBytes: 64, MaxGrantItems: 2},
	})

	t.Run("json null remains present", func(t *testing.T) {
		response := performUpload(t, handler, `{"name":"value","file":false,"public":true,"token":"valid"}`, []byte("null"), nil)
		if response.Code != http.StatusOK {
			t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
		}
		var envelope map[string]json.RawMessage
		if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
			t.Fatal(err)
		}
		if string(envelope["data"]) != `{"json":null}` {
			t.Fatalf("data=%s", envelope["data"])
		}
		if documents.input.File != nil || string(documents.input.Document.JSON) != "null" {
			t.Fatalf("service input=%#v", documents.input)
		}
	})

	t.Run("file and json return only supplied fields", func(t *testing.T) {
		response := performUpload(t, handler, `{"name":"x.bin","file":true,"public":false,"token":"valid","mime":"application/octet-stream"}`, []byte(`[1,true]`), []byte("1234"))
		if response.Code != http.StatusOK || response.Body.String() != "{\"data\":{\"file\":\"x.bin\",\"json\":[1,true]}}\n" {
			t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
		}
		content, err := io.ReadAll(documents.input.File)
		if err != nil || string(content) != "1234" {
			t.Fatalf("file=%q err=%v", content, err)
		}
	})
}

func TestUploadDocumentHTTPRejectsInvalidRequests(t *testing.T) {
	documents := &uploadDocuments{}
	auth := &uploadAuth{user: domain.User{ID: "owner-id", Login: "owner000"}}
	handler := NewHandler(Dependencies{
		Auth: auth, Documents: documents,
		Limits: Limits{MaxRequestBytes: 4096, MaxFileBytes: 4, MaxJSONBytes: 64, MaxGrantItems: 1},
	})

	tests := []struct {
		name string
		meta string
		json []byte
		file []byte
	}{
		{"missing json", `{"name":"value","file":false,"public":false,"token":"valid"}`, nil, nil},
		{"file contradicts metadata", `{"name":"value","file":false,"public":false,"token":"valid"}`, []byte(`{}`), []byte("x")},
		{"missing mime", `{"name":"x.bin","file":true,"public":false,"token":"valid"}`, nil, []byte("x")},
		{"bad json", `{"name":"value","file":false,"public":false,"token":"valid"}`, []byte(`{`), nil},
		{"oversized file", `{"name":"x.bin","file":true,"public":false,"token":"valid","mime":"text/plain"}`, nil, []byte("12345")},
		{"too many grants", `{"name":"value","file":false,"public":false,"token":"valid","grant":["a","b"]}`, []byte(`{}`), nil},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			response := performUpload(t, handler, test.meta, test.json, test.file)
			if response.Code != http.StatusBadRequest {
				t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
			}
		})
	}
	if documents.calls != 0 {
		t.Fatalf("service called %d times", documents.calls)
	}

	auth.err = domain.ErrUnauthorized
	response := performUpload(t, handler, `{"name":"value","file":false,"public":false,"token":"invalid"}`, []byte(`{}`), nil)
	if response.Code != http.StatusUnauthorized || documents.calls != 0 {
		t.Fatalf("unauthorized status=%d calls=%d", response.Code, documents.calls)
	}
}

func TestUploadDocumentLimitBoundaries(t *testing.T) {
	t.Run("file and JSON", func(t *testing.T) {
		for _, test := range []struct {
			name   string
			json   []byte
			file   []byte
			isFile bool
			status int
		}{
			{"file below", nil, []byte("123"), true, http.StatusOK},
			{"file at", nil, []byte("1234"), true, http.StatusOK},
			{"file above", nil, []byte("12345"), true, http.StatusBadRequest},
			{"JSON below", []byte(`{}`), nil, false, http.StatusOK},
			{"JSON at", []byte(`null`), nil, false, http.StatusOK},
			{"JSON above", []byte(`[123]`), nil, false, http.StatusBadRequest},
		} {
			t.Run(test.name, func(t *testing.T) {
				documents := &uploadDocuments{}
				handler := NewHandler(Dependencies{
					Auth: &uploadAuth{user: domain.User{ID: "owner-id", Login: "owner000"}}, Documents: documents,
					Limits: Limits{MaxRequestBytes: 4096, MaxFileBytes: 4, MaxJSONBytes: 4, MaxGrantItems: 2},
				})
				meta := `{"name":"value","file":false,"public":false,"token":"valid"}`
				if test.isFile {
					meta = `{"name":"value","file":true,"public":false,"token":"valid","mime":"application/octet-stream"}`
				}
				response := performUpload(t, handler, meta, test.json, test.file)
				if response.Code != test.status {
					t.Fatalf("status=%d, want %d; body=%s", response.Code, test.status, response.Body)
				}
			})
		}
	})

	t.Run("grant", func(t *testing.T) {
		for count := 1; count <= 3; count++ {
			documents := &uploadDocuments{}
			handler := NewHandler(Dependencies{
				Auth: &uploadAuth{user: domain.User{ID: "owner-id", Login: "owner000"}}, Documents: documents,
				Limits: Limits{MaxRequestBytes: 4096, MaxFileBytes: 4, MaxJSONBytes: 4, MaxGrantItems: 2},
			})
			grants := make([]string, count)
			for index := range grants {
				grants[index] = string(rune('a' + index))
			}
			encodedGrants, err := json.Marshal(grants)
			if err != nil {
				t.Fatal(err)
			}
			meta := `{"name":"value","file":false,"public":false,"token":"valid","grant":` + string(encodedGrants) + `}`
			response := performUpload(t, handler, meta, []byte(`{}`), nil)
			want := http.StatusOK
			if count > 2 {
				want = http.StatusBadRequest
			}
			if response.Code != want {
				t.Fatalf("grant count %d status=%d, want %d", count, response.Code, want)
			}
		}
	})
}

func TestUploadDocumentRemovesMultipartTemporaryFiles(t *testing.T) {
	temporaryRoot := t.TempDir()
	for _, name := range []string{"TMPDIR", "TMP", "TEMP"} {
		t.Setenv(name, temporaryRoot)
	}
	if actual, err := filepath.Abs(os.TempDir()); err != nil || actual != temporaryRoot {
		t.Fatalf("os.TempDir() = %q, want %q (err=%v)", actual, temporaryRoot, err)
	}

	tests := []struct {
		name       string
		meta       string
		file       []byte
		maxFile    int64
		cancel     bool
		wantStatus int
	}{
		{
			name:       "success",
			meta:       `{"name":"x.bin","file":true,"public":false,"token":"valid","mime":"application/octet-stream"}`,
			file:       bytes.Repeat([]byte{0x5a}, 32),
			maxFile:    64,
			wantStatus: http.StatusOK,
		},
		{
			name:       "validation error",
			meta:       `{"name":"x.bin","file":true,"public":false,"token":"valid"}`,
			file:       bytes.Repeat([]byte{0x5a}, 32),
			maxFile:    64,
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "file limit",
			meta:       `{"name":"x.bin","file":true,"public":false,"token":"valid","mime":"application/octet-stream"}`,
			file:       bytes.Repeat([]byte{0x5a}, 32),
			maxFile:    16,
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "cancelled context",
			meta:       `{"name":"x.bin","file":true,"public":false,"token":"valid","mime":"application/octet-stream"}`,
			file:       bytes.Repeat([]byte{0x5a}, 32),
			maxFile:    64,
			cancel:     true,
			wantStatus: http.StatusInternalServerError,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			documents := &uploadDocuments{}
			auth := &uploadAuth{
				user: domain.User{ID: "owner-id", Login: "owner000"}, respectContext: test.cancel,
				temporaryDirectory: temporaryRoot,
			}
			handler := NewHandler(Dependencies{
				Auth:      auth,
				Documents: documents,
				Limits: Limits{
					MaxRequestBytes: 4096, MaxFileBytes: test.maxFile, MaxJSONBytes: 1, MaxGrantItems: 2,
				},
			})
			request := newUploadRequest(t, test.meta, nil, test.file)
			if test.cancel {
				ctx, cancel := context.WithCancel(request.Context())
				cancel()
				request = request.WithContext(ctx)
			}
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != test.wantStatus {
				t.Fatalf("status=%d, want %d; body=%s", response.Code, test.wantStatus, response.Body)
			}
			if !auth.sawTemporaryFile {
				t.Fatal("multipart file did not spill to temporary storage")
			}
			assertDirectoryEmpty(t, temporaryRoot)
		})
	}

	t.Run("request limit while parsing", func(t *testing.T) {
		var body bytes.Buffer
		writer := multipart.NewWriter(&body)
		filePart, err := writer.CreateFormFile("file", "x.bin")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := filePart.Write(bytes.Repeat([]byte{0x5a}, 32)); err != nil {
			t.Fatal(err)
		}
		if err := writer.WriteField("padding", string(bytes.Repeat([]byte{'p'}, 256))); err != nil {
			t.Fatal(err)
		}
		if err := writer.Close(); err != nil {
			t.Fatal(err)
		}
		handler := NewHandler(Dependencies{
			Auth:      &uploadAuth{},
			Documents: &uploadDocuments{},
			Limits:    Limits{MaxRequestBytes: 192, MaxFileBytes: 64, MaxJSONBytes: 1, MaxGrantItems: 2},
		})
		request := httptest.NewRequest(http.MethodPost, "/api/docs", &body)
		request.ContentLength = -1 // Exercise MaxBytesReader during multipart parsing.
		request.Header.Set("Content-Type", writer.FormDataContentType())
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusBadRequest {
			t.Fatalf("status=%d body=%s", response.Code, response.Body)
		}
		assertDirectoryEmpty(t, temporaryRoot)
	})
}

func assertDirectoryEmpty(t *testing.T, path string) {
	t.Helper()
	entries, err := os.ReadDir(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("temporary directory contains %v", entries)
	}
}

func performUpload(t *testing.T, handler http.Handler, meta string, jsonPart, file []byte) *httptest.ResponseRecorder {
	t.Helper()
	request := newUploadRequest(t, meta, jsonPart, file)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func newUploadRequest(t *testing.T, meta string, jsonPart, file []byte) *http.Request {
	t.Helper()
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	if err := writer.WriteField("meta", meta); err != nil {
		t.Fatal(err)
	}
	if jsonPart != nil {
		part, err := writer.CreateFormField("json")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := part.Write(jsonPart); err != nil {
			t.Fatal(err)
		}
	}
	if file != nil {
		part, err := writer.CreateFormFile("file", "ignored-user-name")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := part.Write(file); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/api/docs", &body)
	request.Header.Set("Content-Type", writer.FormDataContentType())
	return request
}

type uploadAuth struct {
	user               domain.User
	err                error
	respectContext     bool
	temporaryDirectory string
	sawTemporaryFile   bool
}

func (*uploadAuth) Register(context.Context, string, string, string) (domain.User, error) {
	return domain.User{}, errors.New("unused")
}
func (*uploadAuth) Authenticate(context.Context, string, string) (string, error) {
	return "", errors.New("unused")
}
func (f *uploadAuth) Authorize(ctx context.Context, _ string) (domain.User, error) {
	if f.temporaryDirectory != "" {
		entries, _ := os.ReadDir(f.temporaryDirectory)
		f.sawTemporaryFile = len(entries) > 0
	}
	if f.respectContext {
		select {
		case <-ctx.Done():
			return domain.User{}, ctx.Err()
		default:
		}
	}
	return f.user, f.err
}
func (*uploadAuth) Logout(context.Context, string) error { return errors.New("unused") }

type uploadDocuments struct {
	input document.Upload
	calls int
	err   error
}

func (f *uploadDocuments) Upload(_ context.Context, _ domain.User, input document.Upload) (domain.Document, error) {
	f.calls++
	// The production service consumes the reader synchronously. Preserve that
	// behavior in this fake because multipart temporary files are request-scoped.
	if input.File != nil {
		content, err := io.ReadAll(input.File)
		if err != nil {
			return domain.Document{}, err
		}
		input.File = bytes.NewReader(content)
	}
	f.input = input
	return input.Document, f.err
}
func (*uploadDocuments) List(context.Context, domain.User, domain.DocumentFilter) ([]domain.Document, error) {
	return nil, errors.New("unused")
}
func (*uploadDocuments) Get(context.Context, domain.User, string) (document.Content, error) {
	return document.Content{}, errors.New("unused")
}
func (*uploadDocuments) Delete(context.Context, domain.User, string) error {
	return errors.New("unused")
}
