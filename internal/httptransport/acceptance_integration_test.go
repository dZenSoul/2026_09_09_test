package httptransport

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"documents/internal/auth"
	"documents/internal/blob"
	responsecache "documents/internal/cache"
	"documents/internal/document"
	"documents/internal/repository"
	postgresrepository "documents/internal/repository/postgres"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"golang.org/x/crypto/bcrypt"
)

const acceptanceMaxFileBytes int64 = 64

// TestAcceptanceLifecycle exercises the public API through all of its layers:
// HTTP parsing, authentication, use cases, PostgreSQL, filesystem blobs and
// response caching. TEST_POSTGRES_DSN keeps the normal unit-test run
// self-contained; CI supplies a disposable PostgreSQL instance.
func TestAcceptanceLifecycle(t *testing.T) {
	dsn := os.Getenv("TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("TEST_POSTGRES_DSN is not set")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	base, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect to postgres: %v", err)
	}
	defer base.Close(context.Background())
	schema := "acceptance_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	identifier := pgx.Identifier{schema}.Sanitize()
	if _, err := base.Exec(ctx, "CREATE SCHEMA "+identifier); err != nil {
		t.Fatalf("create schema: %v", err)
	}
	defer func() { _, _ = base.Exec(context.Background(), "DROP SCHEMA "+identifier+" CASCADE") }()

	store, err := postgresrepository.Open(ctx, postgresrepository.Config{
		DSN: acceptanceSearchPath(t, dsn, schema), MaxConns: 8, MinConns: 1,
	})
	if err != nil {
		t.Fatalf("open repository: %v", err)
	}
	defer store.Close()
	if err := store.MigrateUp(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	blobs, err := blob.NewFileStorage(t.TempDir())
	if err != nil {
		t.Fatalf("create blob storage: %v", err)
	}
	authService, err := auth.NewService(store.Users(), store.Sessions(), auth.Config{
		AdminToken: "acceptance-admin", SessionTTL: time.Hour, PasswordCost: bcrypt.MinCost,
	})
	if err != nil {
		t.Fatalf("create auth service: %v", err)
	}
	cache := responsecache.NewMemory(1<<20, 100)
	cacheEpoch := &atomic.Uint64{}
	documentService, err := document.NewService(store.Documents(), store.Users(), blobs, document.Config{
		MaxFileBytes: acceptanceMaxFileBytes, MaxGrantItems: 2, MaxListLimit: 2,
		Cleanup: store, InvalidateDelete: func(ctx context.Context, id string) error {
			cacheEpoch.Add(1)
			cache.Invalidate(ctx, "documents:list", "document:"+id)
			return ctx.Err()
		},
	})
	if err != nil {
		t.Fatalf("create document service: %v", err)
	}
	h := NewHandler(Dependencies{
		Auth: authService, Documents: documentService,
		Cache: cache, CacheEpoch: cacheEpoch, CacheTTL: time.Minute, ExposeCacheHeader: true,
		Limits: Limits{MaxRequestBytes: 4096, MaxFileBytes: acceptanceMaxFileBytes, MaxJSONBytes: 64, MaxGrantItems: 2, MaxListLimit: 2},
	})
	server := httptest.NewServer(h)
	defer server.Close()

	client := server.Client()
	for _, login := range []string{"alice001", "bob00001", "carol001"} {
		response := acceptanceForm(t, client, http.MethodPost, server.URL+"/api/register", url.Values{
			"token": {"acceptance-admin"}, "login": {login}, "pswd": {"Password1!"},
		})
		acceptanceCloseStatus(t, response, http.StatusOK)
	}
	acceptanceStatus(t, acceptanceForm(t, client, http.MethodPost, server.URL+"/api/register", url.Values{
		"token": {"acceptance-admin"}, "login": {"alice001"}, "pswd": {"Password1!"},
	}), http.StatusBadRequest)
	acceptanceStatus(t, acceptanceForm(t, client, http.MethodPost, server.URL+"/api/auth", url.Values{
		"login": {"alice001"}, "pswd": {"wrong"},
	}), http.StatusUnauthorized)

	tokens := make(map[string]string)
	for _, login := range []string{"alice001", "bob00001", "carol001"} {
		response := acceptanceForm(t, client, http.MethodPost, server.URL+"/api/auth", url.Values{
			"login": {login}, "pswd": {"Password1!"},
		})
		tokens[login] = acceptanceResponseString(t, response, "token")
	}

	privateJSON := []byte(`{"kind":"private","n":1}`)
	acceptanceCloseStatus(t, acceptanceUpload(t, client, server.URL+"/api/docs", map[string]any{
		"name": "a-private.json", "file": false, "public": false,
		"token": tokens["alice001"], "grant": []string{"bob00001", "bob00001"},
	}, privateJSON, nil), http.StatusOK)
	if docs := acceptanceList(t, client, server.URL, tokens["alice001"], ""); len(docs) != 1 {
		t.Fatalf("list after first upload has %d documents, want 1", len(docs))
	}
	fileBytes := []byte{0, 1, 2, 3, 0xff}
	acceptanceCloseStatus(t, acceptanceUpload(t, client, server.URL+"/api/docs", map[string]any{
		"name": "b-public.bin", "file": true, "public": true, "mime": "application/octet-stream",
		"token": tokens["alice001"], "grant": []string{},
	}, nil, fileBytes), http.StatusOK)

	aliceDocs := acceptanceList(t, client, server.URL, tokens["alice001"], "")
	if len(aliceDocs) != 2 || aliceDocs[0].Name != "a-private.json" || aliceDocs[1].Name != "b-public.bin" {
		t.Fatalf("owner list is not complete and stable: %#v", aliceDocs)
	}
	privateID, publicID := aliceDocs[0].ID, aliceDocs[1].ID
	bobDocs := acceptanceList(t, client, server.URL, tokens["bob00001"], "alice001")
	if len(bobDocs) != 2 {
		t.Fatalf("grant recipient sees %d documents, want 2", len(bobDocs))
	}
	carolDocs := acceptanceList(t, client, server.URL, tokens["carol001"], "alice001")
	if len(carolDocs) != 1 || carolDocs[0].ID != publicID {
		t.Fatalf("ungranted user view = %#v", carolDocs)
	}
	response := acceptanceGet(t, client, server.URL+"/api/docs?login=alice001", "")
	acceptanceStatus(t, response, http.StatusUnauthorized)

	response = acceptanceGet(t, client, server.URL+"/api/docs/"+privateID, tokens["bob00001"])
	acceptanceStatus(t, response, http.StatusOK)
	var jsonEnvelope struct {
		Data json.RawMessage `json:"data"`
	}
	acceptanceDecode(t, response, &jsonEnvelope)
	if !acceptanceJSONEqual(jsonEnvelope.Data, privateJSON) {
		t.Fatalf("JSON changed: got %s, want %s", jsonEnvelope.Data, privateJSON)
	}
	response = acceptanceGet(t, client, server.URL+"/api/docs/"+privateID, tokens["carol001"])
	acceptanceStatus(t, response, http.StatusForbidden)

	first := acceptanceGet(t, client, server.URL+"/api/docs/"+publicID, tokens["carol001"])
	acceptanceStatus(t, first, http.StatusOK)
	if got := first.Header.Get("X-Cache"); got != "MISS" {
		t.Fatalf("first cache status = %q, want MISS", got)
	}
	body, _ := io.ReadAll(first.Body)
	_ = first.Body.Close()
	if !bytes.Equal(body, fileBytes) || first.Header.Get("Content-Type") != "application/octet-stream" {
		t.Fatalf("file response = %v, %q", body, first.Header.Get("Content-Type"))
	}
	second := acceptanceGet(t, client, server.URL+"/api/docs/"+publicID, tokens["carol001"])
	acceptanceStatus(t, second, http.StatusOK)
	if got := second.Header.Get("X-Cache"); got != "HIT" {
		t.Fatalf("second cache status = %q, want HIT", got)
	}
	_ = second.Body.Close()
	headRequest, _ := http.NewRequest(http.MethodHead, server.URL+"/api/docs/"+publicID+"?token="+url.QueryEscape(tokens["carol001"]), nil)
	head, err := client.Do(headRequest)
	if err != nil {
		t.Fatalf("HEAD document: %v", err)
	}
	if head.StatusCode != http.StatusOK || head.ContentLength != int64(len(fileBytes)) {
		t.Fatalf("HEAD metadata = %d/%d", head.StatusCode, head.ContentLength)
	}
	headBody, _ := io.ReadAll(head.Body)
	_ = head.Body.Close()
	if len(headBody) != 0 {
		t.Fatalf("HEAD body length = %d", len(headBody))
	}

	for key, value := range map[string]string{
		"id": privateID, "name": "a-private.json", "mime": "application/octet-stream",
		"file": "true", "public": "false", "created": aliceDocs[0].Created,
	} {
		endpoint := fmt.Sprintf("%s/api/docs?token=%s&key=%s&value=%s", server.URL,
			url.QueryEscape(tokens["alice001"]), key, url.QueryEscape(value))
		response := acceptanceGetURL(t, client, endpoint)
		acceptanceStatus(t, response, http.StatusOK)
		if docs := acceptanceDecodeDocs(t, response); len(docs) == 0 {
			t.Fatalf("filter %s=%q returned no documents", key, value)
		}
	}
	for _, limit := range []string{"1", "2"} {
		response := acceptanceGetURL(t, client, server.URL+"/api/docs?token="+url.QueryEscape(tokens["alice001"])+"&limit="+limit)
		acceptanceStatus(t, response, http.StatusOK)
		_ = response.Body.Close()
	}
	response = acceptanceGetURL(t, client, server.URL+"/api/docs?token="+url.QueryEscape(tokens["alice001"])+"&limit=3")
	acceptanceStatus(t, response, http.StatusBadRequest)
	response = acceptanceGetURL(t, client, server.URL+"/api/docs?token="+url.QueryEscape(tokens["alice001"])+"&key=name&value=missing")
	acceptanceStatus(t, response, http.StatusOK)
	if docs := acceptanceDecodeDocs(t, response); len(docs) != 0 {
		t.Fatalf("empty filter returned %#v", docs)
	}

	acceptanceStatus(t, acceptanceForm(t, client, http.MethodDelete, server.URL+"/api/docs/"+privateID,
		url.Values{"token": {tokens["bob00001"]}}), http.StatusForbidden)
	acceptanceCloseStatus(t, acceptanceForm(t, client, http.MethodDelete, server.URL+"/api/docs/"+privateID,
		url.Values{"token": {tokens["alice001"]}}), http.StatusOK)
	response = acceptanceGet(t, client, server.URL+"/api/docs/"+privateID, tokens["alice001"])
	acceptanceStatus(t, response, http.StatusNotFound)
	if docs := acceptanceList(t, client, server.URL, tokens["alice001"], ""); len(docs) != 1 || docs[0].ID != publicID {
		t.Fatalf("list after delete is stale: %#v", docs)
	}
	acceptanceStatus(t, acceptanceForm(t, client, http.MethodDelete, server.URL+"/api/docs/"+privateID,
		url.Values{"token": {tokens["alice001"]}}), http.StatusNotFound)

	request, _ := http.NewRequest(http.MethodPut, server.URL+"/api/docs", nil)
	response, err = client.Do(request)
	if err != nil {
		t.Fatalf("unsupported method: %v", err)
	}
	if response.StatusCode != http.StatusMethodNotAllowed || response.Header.Get("Allow") != "GET, HEAD, POST" {
		t.Fatalf("method response = %d Allow=%q", response.StatusCode, response.Header.Get("Allow"))
	}
	_ = response.Body.Close()

	acceptanceCloseStatus(t, acceptanceForm(t, client, http.MethodDelete, server.URL+"/api/auth/"+tokens["carol001"], nil), http.StatusOK)
	response = acceptanceGet(t, client, server.URL+"/api/docs/"+publicID, tokens["carol001"])
	acceptanceStatus(t, response, http.StatusUnauthorized)
	acceptanceStatus(t, acceptanceForm(t, client, http.MethodDelete, server.URL+"/api/auth/"+tokens["carol001"], nil), http.StatusUnauthorized)

	// Concurrent authenticated reads exercise the shared cache and repository
	// pool under the race detector.
	var wg sync.WaitGroup
	errorsSeen := make(chan error, 12)
	for range 12 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			response := acceptanceGetNoFail(client, server.URL+"/api/docs/"+publicID, tokens["bob00001"])
			if response.err != nil {
				errorsSeen <- response.err
				return
			}
			defer response.response.Body.Close()
			if response.response.StatusCode != http.StatusOK {
				errorsSeen <- fmt.Errorf("status %d", response.response.StatusCode)
			}
		}()
	}
	wg.Wait()
	close(errorsSeen)
	for err := range errorsSeen {
		t.Errorf("concurrent read: %v", err)
	}

	// Upload and delete independent documents concurrently, then upload the
	// same logical names again. This covers mutation/cache coordination while
	// leaving document IDs immutable and non-reusable.
	mutationErrors := make(chan error, 6)
	for index := range 6 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			name := fmt.Sprintf("concurrent-%02d.json", index)
			response, err := acceptanceUploadNoFail(client, server.URL+"/api/docs", map[string]any{
				"name": name, "file": false, "public": false,
				"token": tokens["bob00001"], "grant": []string{},
			}, []byte(fmt.Sprintf(`{"index":%d}`, index)), nil)
			if err != nil {
				mutationErrors <- err
				return
			}
			defer response.Body.Close()
			if response.StatusCode != http.StatusOK {
				mutationErrors <- fmt.Errorf("upload %s status %d", name, response.StatusCode)
			}
		}()
	}
	wg.Wait()
	close(mutationErrors)
	for err := range mutationErrors {
		t.Fatalf("concurrent upload: %v", err)
	}

	bobOwned := acceptanceList(t, client, server.URL, tokens["bob00001"], "")
	if len(bobOwned) != 6 {
		t.Fatalf("concurrent uploads produced %d documents, want 6", len(bobOwned))
	}
	mutationErrors = make(chan error, len(bobOwned))
	for _, item := range bobOwned {
		item := item
		wg.Add(1)
		go func() {
			defer wg.Done()
			response, err := acceptanceFormNoFail(client, http.MethodDelete, server.URL+"/api/docs/"+item.ID,
				url.Values{"token": {tokens["bob00001"]}})
			if err != nil {
				mutationErrors <- err
				return
			}
			defer response.Body.Close()
			if response.StatusCode != http.StatusOK {
				mutationErrors <- fmt.Errorf("delete %s status %d", item.ID, response.StatusCode)
			}
		}()
	}
	wg.Wait()
	close(mutationErrors)
	for err := range mutationErrors {
		t.Fatalf("concurrent delete: %v", err)
	}
	acceptanceCloseStatus(t, acceptanceUpload(t, client, server.URL+"/api/docs", map[string]any{
		"name": "concurrent-00.json", "file": false, "public": false,
		"token": tokens["bob00001"], "grant": []string{},
	}, []byte(`{"reuploaded":true}`), nil), http.StatusOK)

	// Exercise both inclusive file-size boundaries through the complete public
	// API. The maximum payload is generated and verified incrementally so the
	// test never retains redundant full-size copies.
	acceptanceFileLifecycle(t, client, server.URL, tokens["alice001"], store, blobs,
		"boundary.bin", "application/x-empty", 0)
	acceptanceFileLifecycle(t, client, server.URL, tokens["alice001"], store, blobs,
		"boundary.bin", "application/octet-stream", acceptanceMaxFileBytes)
}

type acceptanceListItem struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Created string `json:"created"`
}

func acceptanceSearchPath(t *testing.T, dsn, schema string) string {
	t.Helper()
	parsed, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("parse TEST_POSTGRES_DSN: %v", err)
	}
	query := parsed.Query()
	query.Set("search_path", schema)
	parsed.RawQuery = query.Encode()
	return parsed.String()
}

func acceptanceForm(t *testing.T, client *http.Client, method, endpoint string, values url.Values) *http.Response {
	t.Helper()
	encoded := ""
	if values != nil {
		encoded = values.Encode()
	}
	request, _ := http.NewRequest(method, endpoint, strings.NewReader(encoded))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response, err := client.Do(request)
	if err != nil {
		t.Fatalf("%s %s: %v", method, endpoint, err)
	}
	return response
}

func acceptanceUpload(t *testing.T, client *http.Client, endpoint string, meta map[string]any, jsonPart, file []byte) *http.Response {
	t.Helper()
	response, err := acceptanceUploadNoFail(client, endpoint, meta, jsonPart, file)
	if err != nil {
		t.Fatalf("upload: %v", err)
	}
	return response
}

func acceptanceUploadNoFail(client *http.Client, endpoint string, meta map[string]any, jsonPart, file []byte) (*http.Response, error) {
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	encodedMeta, err := json.Marshal(meta)
	if err != nil {
		return nil, err
	}
	if err := writer.WriteField("meta", string(encodedMeta)); err != nil {
		return nil, err
	}
	if jsonPart != nil {
		if err := writer.WriteField("json", string(jsonPart)); err != nil {
			return nil, err
		}
	}
	if file != nil {
		part, err := writer.CreateFormFile("file", meta["name"].(string))
		if err != nil {
			return nil, err
		}
		if _, err := part.Write(file); err != nil {
			return nil, err
		}
	}
	if err := writer.Close(); err != nil {
		return nil, err
	}
	request, _ := http.NewRequest(http.MethodPost, endpoint, &body)
	request.Header.Set("Content-Type", writer.FormDataContentType())
	return client.Do(request)
}

func acceptanceFileLifecycle(
	t *testing.T,
	client *http.Client,
	baseURL, token string,
	store *postgresrepository.Store,
	blobs blob.Storage,
	name, contentType string,
	size int64,
) {
	t.Helper()
	response, err := acceptanceUploadStream(client, baseURL+"/api/docs", map[string]any{
		"name": name, "file": true, "public": false, "token": token,
		"mime": contentType, "grant": []string{},
	}, &acceptancePatternReader{remaining: size})
	if err != nil {
		t.Fatalf("upload %s (%d bytes): %v", name, size, err)
	}
	acceptanceCloseStatus(t, response, http.StatusOK)

	var id string
	for _, item := range acceptanceList(t, client, baseURL, token, "") {
		if item.Name == name {
			id = item.ID
			break
		}
	}
	if id == "" {
		t.Fatalf("uploaded boundary file %q is absent from list", name)
	}
	metadata, err := store.Documents().ByID(context.Background(), id)
	if err != nil {
		t.Fatalf("read boundary metadata: %v", err)
	}
	object, err := blobs.Open(context.Background(), metadata.StorageKey)
	if err != nil || object.Size != size {
		t.Fatalf("stored boundary blob: size=%d error=%v", object.Size, err)
	}
	_ = object.Body.Close()

	response = acceptanceGet(t, client, baseURL+"/api/docs/"+id, token)
	acceptanceStatus(t, response, http.StatusOK)
	assertAcceptanceFileHeaders(t, response, name, contentType, size)
	acceptanceVerifyPattern(t, response.Body, size)
	_ = response.Body.Close()

	request, _ := http.NewRequest(http.MethodHead, baseURL+"/api/docs/"+id+"?token="+url.QueryEscape(token), nil)
	response, err = client.Do(request)
	if err != nil {
		t.Fatalf("HEAD boundary file: %v", err)
	}
	acceptanceStatus(t, response, http.StatusOK)
	assertAcceptanceFileHeaders(t, response, name, contentType, size)
	body, readErr := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if readErr != nil || len(body) != 0 {
		t.Fatalf("HEAD body: length=%d error=%v", len(body), readErr)
	}

	acceptanceCloseStatus(t, acceptanceForm(t, client, http.MethodDelete, baseURL+"/api/docs/"+id,
		url.Values{"token": {token}}), http.StatusOK)
	for _, method := range []string{http.MethodGet, http.MethodHead} {
		request, _ = http.NewRequest(method, baseURL+"/api/docs/"+id+"?token="+url.QueryEscape(token), nil)
		response, err = client.Do(request)
		if err != nil {
			t.Fatalf("%s deleted boundary file: %v", method, err)
		}
		acceptanceStatus(t, response, http.StatusNotFound)
	}
	for _, item := range acceptanceList(t, client, baseURL, token, "") {
		if item.ID == id {
			t.Fatalf("deleted boundary file remains in list: %#v", item)
		}
	}
	if _, err := store.Documents().ByID(context.Background(), id); !errors.Is(err, repository.ErrNotFound) {
		t.Fatalf("deleted boundary metadata error=%v, want not found", err)
	}
	if _, err := blobs.Open(context.Background(), metadata.StorageKey); !errors.Is(err, blob.ErrNotFound) {
		t.Fatalf("deleted boundary blob error=%v, want not found", err)
	}
	tasks, err := store.ClaimBlobCleanupTasks(context.Background(), 1, time.Now().Add(time.Minute))
	if err != nil || len(tasks) != 0 {
		t.Fatalf("completed boundary cleanup remains in outbox: tasks=%#v error=%v", tasks, err)
	}
}

func acceptanceUploadStream(client *http.Client, endpoint string, meta map[string]any, source io.Reader) (*http.Response, error) {
	pipeReader, pipeWriter := io.Pipe()
	writer := multipart.NewWriter(pipeWriter)
	request, err := http.NewRequest(http.MethodPost, endpoint, pipeReader)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Content-Type", writer.FormDataContentType())
	writeDone := make(chan error, 1)
	go func() {
		encodedMeta, marshalErr := json.Marshal(meta)
		if marshalErr == nil {
			marshalErr = writer.WriteField("meta", string(encodedMeta))
		}
		var part io.Writer
		if marshalErr == nil {
			part, marshalErr = writer.CreateFormFile("file", meta["name"].(string))
		}
		if marshalErr == nil {
			_, marshalErr = io.Copy(part, source)
		}
		if closeErr := writer.Close(); marshalErr == nil {
			marshalErr = closeErr
		}
		_ = pipeWriter.CloseWithError(marshalErr)
		writeDone <- marshalErr
	}()
	response, requestErr := client.Do(request)
	writeErr := <-writeDone
	if requestErr != nil {
		return nil, requestErr
	}
	if writeErr != nil {
		_ = response.Body.Close()
		return nil, writeErr
	}
	return response, nil
}

type acceptancePatternReader struct {
	offset, remaining int64
}

func (r *acceptancePatternReader) Read(dst []byte) (int, error) {
	if r.remaining == 0 {
		return 0, io.EOF
	}
	if int64(len(dst)) > r.remaining {
		dst = dst[:r.remaining]
	}
	for i := range dst {
		dst[i] = byte(((r.offset + int64(i)) * 31) % 251)
	}
	r.offset += int64(len(dst))
	r.remaining -= int64(len(dst))
	return len(dst), nil
}

func acceptanceVerifyPattern(t *testing.T, reader io.Reader, size int64) {
	t.Helper()
	buffer := make([]byte, 32)
	var offset int64
	for {
		n, err := reader.Read(buffer)
		for i, value := range buffer[:n] {
			want := byte(((offset + int64(i)) * 31) % 251)
			if value != want {
				t.Fatalf("download byte %d = %d, want %d", offset+int64(i), value, want)
			}
		}
		offset += int64(n)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("read boundary download: %v", err)
		}
	}
	if offset != size {
		t.Fatalf("download length=%d, want %d", offset, size)
	}
}

func assertAcceptanceFileHeaders(t *testing.T, response *http.Response, name, contentType string, size int64) {
	t.Helper()
	if response.StatusCode != http.StatusOK || response.ContentLength != size ||
		response.Header.Get("Content-Length") != strconv.FormatInt(size, 10) ||
		response.Header.Get("Content-Type") != contentType {
		t.Fatalf("file response: status=%d length=%d headers=%v", response.StatusCode, response.ContentLength, response.Header)
	}
	mediaType, parameters, err := mime.ParseMediaType(response.Header.Get("Content-Disposition"))
	if err != nil || mediaType != "inline" || parameters["filename"] != name {
		t.Fatalf("Content-Disposition=%q: media=%q params=%v error=%v",
			response.Header.Get("Content-Disposition"), mediaType, parameters, err)
	}
}

func acceptanceGet(t *testing.T, client *http.Client, endpoint, token string) *http.Response {
	t.Helper()
	separator := "?"
	if strings.Contains(endpoint, "?") {
		separator = "&"
	}
	return acceptanceGetURL(t, client, endpoint+separator+"token="+url.QueryEscape(token))
}

func acceptanceGetURL(t *testing.T, client *http.Client, endpoint string) *http.Response {
	t.Helper()
	response, err := client.Get(endpoint)
	if err != nil {
		t.Fatalf("GET %s: %v", endpoint, err)
	}
	return response
}

type acceptanceResult struct {
	response *http.Response
	err      error
}

func acceptanceGetNoFail(client *http.Client, endpoint, token string) acceptanceResult {
	response, err := client.Get(endpoint + "?token=" + url.QueryEscape(token))
	return acceptanceResult{response: response, err: err}
}

func acceptanceFormNoFail(client *http.Client, method, endpoint string, values url.Values) (*http.Response, error) {
	request, err := http.NewRequest(method, endpoint, strings.NewReader(values.Encode()))
	if err != nil {
		return nil, err
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return client.Do(request)
}

func acceptanceStatus(t *testing.T, response *http.Response, want int) {
	t.Helper()
	if response.StatusCode != want {
		body, _ := io.ReadAll(response.Body)
		_ = response.Body.Close()
		t.Fatalf("status = %d, want %d; body=%s", response.StatusCode, want, body)
	}
	if want != http.StatusOK {
		defer response.Body.Close()
		if response.Request != nil && response.Request.Method == http.MethodHead {
			body, err := io.ReadAll(response.Body)
			if err != nil || len(body) != 0 ||
				response.Header.Get("Content-Type") != "application/json; charset=utf-8" ||
				response.Header.Get("Content-Length") == "" {
				t.Fatalf("invalid HEAD error response: read=%v headers=%v body=%q", err, response.Header, body)
			}
			return
		}
		var envelope map[string]json.RawMessage
		if err := json.NewDecoder(response.Body).Decode(&envelope); err != nil || len(envelope) != 1 || envelope["error"] == nil {
			t.Fatalf("invalid error envelope: %v %#v", err, envelope)
		}
	}
}

func acceptanceCloseStatus(t *testing.T, response *http.Response, want int) {
	t.Helper()
	acceptanceStatus(t, response, want)
	if want == http.StatusOK {
		_, _ = io.Copy(io.Discard, response.Body)
		_ = response.Body.Close()
	}
}

func acceptanceResponseString(t *testing.T, response *http.Response, key string) string {
	t.Helper()
	defer response.Body.Close()
	acceptanceStatus(t, response, http.StatusOK)
	var envelope struct {
		Response map[string]string `json:"response"`
	}
	acceptanceDecode(t, response, &envelope)
	if envelope.Response[key] == "" {
		t.Fatalf("response %q is empty", key)
	}
	return envelope.Response[key]
}

func acceptanceDecode(t *testing.T, response *http.Response, target any) {
	t.Helper()
	defer response.Body.Close()
	if err := json.NewDecoder(response.Body).Decode(target); err != nil {
		t.Fatalf("decode response: %v", err)
	}
}

func acceptanceList(t *testing.T, client *http.Client, baseURL, token, owner string) []acceptanceListItem {
	t.Helper()
	values := url.Values{"token": {token}}
	if owner != "" {
		values.Set("login", owner)
	}
	response := acceptanceGetURL(t, client, baseURL+"/api/docs?"+values.Encode())
	acceptanceStatus(t, response, http.StatusOK)
	return acceptanceDecodeDocs(t, response)
}

func acceptanceDecodeDocs(t *testing.T, response *http.Response) []acceptanceListItem {
	t.Helper()
	var envelope struct {
		Data struct {
			Docs []acceptanceListItem `json:"docs"`
		} `json:"data"`
	}
	acceptanceDecode(t, response, &envelope)
	return envelope.Data.Docs
}

func acceptanceJSONEqual(left, right []byte) bool {
	var leftValue, rightValue any
	if err := json.Unmarshal(left, &leftValue); err != nil {
		return false
	}
	if err := json.Unmarshal(right, &rightValue); err != nil {
		return false
	}
	return reflect.DeepEqual(leftValue, rightValue)
}
