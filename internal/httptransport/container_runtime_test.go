package httptransport

import (
	"archive/tar"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"
)

const runtimeTemporaryDirectory = "/run/documents-tmp"

func TestRuntimeImageMultipartTemporaryStorage(t *testing.T) {
	if os.Getenv("RUN_DOCKER_MULTIPART_TEST") != "1" {
		t.Skip("set RUN_DOCKER_MULTIPART_TEST=1 to build and exercise the runtime image")
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Fatalf("docker is unavailable: %v", err)
	}
	if output, err := exec.Command("docker", "info", "--format", "{{.ServerVersion}}").CombinedOutput(); err != nil {
		t.Fatalf("Docker Engine is unavailable: %v: %s", err, output)
	}

	port := availableTCPPort(t)
	project := fmt.Sprintf("documents-temp-%d", time.Now().UnixNano())
	imageTag := fmt.Sprintf("multipart-%d", time.Now().UnixNano())
	composeEnv := append(os.Environ(), "APP_PORT="+port, "IMAGE_TAG="+imageTag)
	compose := func(arguments ...string) ([]byte, error) {
		command := exec.Command("docker", append([]string{"compose", "-p", project}, arguments...)...)
		command.Env = composeEnv
		return command.CombinedOutput()
	}
	t.Cleanup(func() {
		if output, err := compose("down", "--volumes", "--remove-orphans"); err != nil {
			t.Logf("compose cleanup: %v: %s", err, output)
		}
		if output, err := exec.Command("docker", "image", "rm", "documents:"+imageTag).CombinedOutput(); err != nil {
			t.Logf("image cleanup: %v: %s", err, output)
		}
	})

	if output, err := compose("up", "--build", "--detach"); err != nil {
		t.Fatalf("start runtime stack: %v\n%s", err, output)
	}
	baseURL := "http://127.0.0.1:" + port
	waitForRuntimeReady(t, baseURL)

	containerIDOutput, err := compose("ps", "--quiet", "app")
	if err != nil || strings.TrimSpace(string(containerIDOutput)) == "" {
		t.Fatalf("find app container: %v: %s", err, containerIDOutput)
	}
	containerID := strings.TrimSpace(string(containerIDOutput))
	assertRuntimeIdentityAndEnvironment(t, containerID)

	client := &http.Client{Timeout: 30 * time.Second}
	runtimeRegisterAndAuthenticate(t, client, baseURL)
	token := runtimeAuthenticate(t, client, baseURL)
	payload := bytes.Repeat([]byte("multipart-on-disk-"), 70_000) // > default MAX_JSON_BYTES.
	documentID := runtimeUploadAndFindDocument(t, client, baseURL, token, payload)

	response, err := client.Get(baseURL + "/api/docs/" + documentID + "?token=" + url.QueryEscape(token))
	if err != nil {
		t.Fatalf("download file: %v", err)
	}
	downloaded, readErr := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if readErr != nil || response.StatusCode != http.StatusOK {
		t.Fatalf("download status=%d read=%v", response.StatusCode, readErr)
	}
	if !bytes.Equal(downloaded, payload) {
		t.Fatalf("downloaded bytes differ: got %d bytes, want %d", len(downloaded), len(payload))
	}
	assertRuntimeTemporaryDirectory(t, containerID)
}

func availableTCPPort(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	return strconv.Itoa(port)
}

func waitForRuntimeReady(t *testing.T, baseURL string) {
	t.Helper()
	client := &http.Client{Timeout: 2 * time.Second}
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		response, err := client.Get(baseURL + "/health/ready")
		if err == nil {
			_ = response.Body.Close()
			if response.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatal("runtime application did not become ready")
}

func assertRuntimeIdentityAndEnvironment(t *testing.T, containerID string) {
	t.Helper()
	output, err := exec.Command("docker", "inspect", "--format", "{{.Config.User}}", containerID).CombinedOutput()
	if err != nil || strings.TrimSpace(string(output)) != "65532:65532" {
		t.Fatalf("runtime user = %q, want 65532:65532 (err=%v)", strings.TrimSpace(string(output)), err)
	}
	output, err = exec.Command("docker", "inspect", "--format", "{{json .Config.Env}}", containerID).CombinedOutput()
	if err != nil {
		t.Fatalf("inspect runtime environment: %v: %s", err, output)
	}
	var environment []string
	if err := json.Unmarshal(bytes.TrimSpace(output), &environment); err != nil {
		t.Fatalf("decode runtime environment: %v: %s", err, output)
	}
	want := "TMPDIR=" + runtimeTemporaryDirectory
	for _, item := range environment {
		if item == want {
			return
		}
	}
	t.Fatalf("runtime environment does not contain %q: %v", want, environment)
}

func runtimeRegisterAndAuthenticate(t *testing.T, client *http.Client, baseURL string) {
	t.Helper()
	response, err := client.PostForm(baseURL+"/api/register", url.Values{
		"token": {"local-admin-token-change-me"}, "login": {"runtime1"}, "pswd": {"Password1!"},
	})
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(response.Body)
		t.Fatalf("register status=%d body=%s", response.StatusCode, body)
	}
}

func runtimeAuthenticate(t *testing.T, client *http.Client, baseURL string) string {
	t.Helper()
	response, err := client.PostForm(baseURL+"/api/auth", url.Values{
		"login": {"runtime1"}, "pswd": {"Password1!"},
	})
	if err != nil {
		t.Fatalf("authenticate: %v", err)
	}
	defer response.Body.Close()
	var envelope struct {
		Response map[string]string `json:"response"`
	}
	if response.StatusCode != http.StatusOK || json.NewDecoder(response.Body).Decode(&envelope) != nil || envelope.Response["token"] == "" {
		t.Fatalf("invalid authentication response: status=%d body=%v", response.StatusCode, envelope)
	}
	return envelope.Response["token"]
}

func runtimeUploadAndFindDocument(t *testing.T, client *http.Client, baseURL, token string, payload []byte) string {
	t.Helper()
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	meta, _ := json.Marshal(map[string]any{
		"name": "runtime-large.bin", "file": true, "public": false, "token": token,
		"mime": "application/octet-stream", "grant": []string{},
	})
	if err := writer.WriteField("meta", string(meta)); err != nil {
		t.Fatal(err)
	}
	part, err := writer.CreateFormFile("file", "runtime-large.bin")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write(payload); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	request, _ := http.NewRequest(http.MethodPost, baseURL+"/api/docs", &body)
	request.Header.Set("Content-Type", writer.FormDataContentType())
	response, err := client.Do(request)
	if err != nil {
		t.Fatalf("upload: %v", err)
	}
	if response.StatusCode != http.StatusOK {
		responseBody, _ := io.ReadAll(response.Body)
		_ = response.Body.Close()
		t.Fatalf("upload status=%d body=%s", response.StatusCode, responseBody)
	}
	_ = response.Body.Close()

	response, err = client.Get(baseURL + "/api/docs?token=" + url.QueryEscape(token))
	if err != nil {
		t.Fatalf("list documents: %v", err)
	}
	defer response.Body.Close()
	var envelope struct {
		Data struct {
			Docs []struct {
				ID   string `json:"id"`
				Name string `json:"name"`
			} `json:"docs"`
		} `json:"data"`
	}
	if response.StatusCode != http.StatusOK || json.NewDecoder(response.Body).Decode(&envelope) != nil {
		t.Fatalf("invalid list response: status=%d", response.StatusCode)
	}
	for _, item := range envelope.Data.Docs {
		if item.Name == "runtime-large.bin" {
			return item.ID
		}
	}
	t.Fatal("uploaded document is absent from the list")
	return ""
}

func assertRuntimeTemporaryDirectory(t *testing.T, containerID string) {
	t.Helper()
	command := exec.Command("docker", "export", containerID)
	stdout, err := command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	command.Stderr = &stderr
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	reader := tar.NewReader(stdout)
	found := false
	prefix := strings.TrimPrefix(runtimeTemporaryDirectory, "/")
	for {
		header, err := reader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("read exported filesystem: %v", err)
		}
		name := strings.TrimSuffix(header.Name, "/")
		if name == prefix {
			found = true
			permissions := header.FileInfo().Mode().Perm()
			if header.Typeflag != tar.TypeDir || header.Uid != 65532 || header.Gid != 65532 || permissions != 0o700 {
				t.Fatalf("temporary directory metadata: type=%d uid=%d gid=%d mode=%#o", header.Typeflag, header.Uid, header.Gid, permissions)
			}
		} else if strings.HasPrefix(name, prefix+"/") {
			t.Errorf("temporary file remains after request: %s", header.Name)
		}
		if _, err := io.Copy(io.Discard, reader); err != nil {
			t.Fatal(err)
		}
	}
	if err := command.Wait(); err != nil {
		t.Fatalf("export runtime filesystem: %v: %s", err, stderr.String())
	}
	if !found {
		t.Fatalf("%s is absent from runtime filesystem", runtimeTemporaryDirectory)
	}
}
