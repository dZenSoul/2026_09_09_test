package httptransport

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"documents/internal/domain"
)

func TestAuthHTTPScenarios(t *testing.T) {
	fake := &fakeAuth{}
	h := NewHandler(Dependencies{Auth: fake})

	register := doForm(t, h, "/api/register", url.Values{"token": {"admin"}, "login": {"testuser"}, "pswd": {"Password1!"}})
	assertResponseValue(t, register, "login", "testuser")
	if fake.adminToken != "admin" || fake.password != "Password1!" {
		t.Fatal("register form was not passed to service")
	}

	authenticate := doForm(t, h, "/api/auth", url.Values{"login": {"testuser"}, "pswd": {"Password1!"}})
	assertResponseValue(t, authenticate, "token", "session-token")

	logout := httptest.NewRecorder()
	h.ServeHTTP(logout, httptest.NewRequest(http.MethodDelete, "/api/auth/session-token", nil))
	if logout.Code != http.StatusOK || fake.logoutToken != "session-token" {
		t.Fatalf("logout status=%d token=%q", logout.Code, fake.logoutToken)
	}
	var envelope struct {
		Response map[string]bool `json:"response"`
	}
	if err := json.Unmarshal(logout.Body.Bytes(), &envelope); err != nil || !envelope.Response["session-token"] {
		t.Fatalf("logout body = %q, %v", logout.Body, err)
	}
}

func TestAuthHTTPErrors(t *testing.T) {
	fake := &fakeAuth{err: domain.ErrUnauthorized}
	h := NewHandler(Dependencies{Auth: fake})
	response := doForm(t, h, "/api/auth", url.Values{"login": {"unknown"}, "pswd": {"bad"}})
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d", response.Code)
	}
	assertErrorEnvelope(t, response)

	wrongType := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/api/register", strings.NewReader("token=x"))
	request.Header.Set("Content-Type", "application/json")
	h.ServeHTTP(wrongType, request)
	if wrongType.Code != http.StatusBadRequest {
		t.Fatalf("wrong content type status = %d", wrongType.Code)
	}

	oversized := NewHandler(Dependencies{Auth: &fakeAuth{}, Limits: Limits{MaxRequestBytes: 2}})
	response = doForm(t, oversized, "/api/auth", url.Values{"login": {"too-large"}})
	if response.Code != http.StatusBadRequest {
		t.Fatalf("oversized status = %d", response.Code)
	}
}

func doForm(t *testing.T, h http.Handler, path string, values url.Values) *httptest.ResponseRecorder {
	t.Helper()
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(values.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded; charset=utf-8")
	h.ServeHTTP(recorder, request)
	return recorder
}
func assertResponseValue(t *testing.T, recorder *httptest.ResponseRecorder, key, want string) {
	t.Helper()
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body)
	}
	var body struct {
		Response map[string]string `json:"response"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil || body.Response[key] != want {
		t.Fatalf("body=%q err=%v", recorder.Body, err)
	}
}

type fakeAuth struct {
	adminToken, login, password, logoutToken string
	err                                      error
}

func (f *fakeAuth) Register(_ context.Context, adminToken, login, password string) (domain.User, error) {
	f.adminToken, f.login, f.password = adminToken, login, password
	if f.err != nil {
		return domain.User{}, f.err
	}
	return domain.User{Login: login}, nil
}
func (f *fakeAuth) Authenticate(_ context.Context, login, password string) (string, error) {
	f.login, f.password = login, password
	if f.err != nil {
		return "", f.err
	}
	return "session-token", nil
}
func (f *fakeAuth) Authorize(context.Context, string) (domain.User, error) {
	return domain.User{}, errors.New("unused")
}
func (f *fakeAuth) Logout(_ context.Context, token string) error { f.logoutToken = token; return f.err }
