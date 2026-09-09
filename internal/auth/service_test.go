package auth

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"documents/internal/domain"
	"documents/internal/repository"

	"golang.org/x/crypto/bcrypt"
)

func TestValidationBoundaries(t *testing.T) {
	validLogins := []string{"abcdefgh", "Abc12345", "12345678"}
	for _, value := range validLogins {
		if !validLogin(value) {
			t.Errorf("validLogin(%q) = false", value)
		}
	}
	invalidLogins := []string{"abcdefg", "abcdefg_", "абвгдежз", "abcdefgh!"}
	for _, value := range invalidLogins {
		if validLogin(value) {
			t.Errorf("validLogin(%q) = true", value)
		}
	}

	validPasswords := []string{"Abcdef1!", "A1!abcde", "Ab1!cdeЖ"}
	for _, value := range validPasswords {
		if !validPassword(value) {
			t.Errorf("validPassword(%q) = false", value)
		}
	}
	invalidPasswords := []string{"Abcde1!", "ABCDEFG1!", "abcdefg1!", "Abcdefg!", "Abcdefg1", "Abcde1Жж"}
	for _, value := range invalidPasswords {
		if validPassword(value) {
			t.Errorf("validPassword(%q) = true", value)
		}
	}
}

func TestRegisterHashesPasswordAndMapsConflict(t *testing.T) {
	users := newMemoryUsers()
	svc := newTestService(t, users, newMemorySessions(), nil, time.Now)
	if _, err := svc.Register(context.Background(), "wrong", "testuser", "Password1!"); !errors.Is(err, domain.ErrUnauthorized) {
		t.Fatalf("wrong admin token error = %v", err)
	}
	user, err := svc.Register(context.Background(), "admin-secret", "testuser", "Password1!")
	if err != nil || user.Login != "testuser" {
		t.Fatalf("Register() = %#v, %v", user, err)
	}
	stored := users.hashes[user.Login]
	if bytes.Equal(stored, []byte("Password1!")) || bcrypt.CompareHashAndPassword(stored, []byte("Password1!")) != nil {
		t.Fatalf("password was not stored as a bcrypt hash: %q", stored)
	}
	if _, err := svc.Register(context.Background(), "admin-secret", "testuser", "Password1!"); !errors.Is(err, domain.ErrInvalidArgument) {
		t.Fatalf("duplicate error = %v", err)
	}
}

func TestConcurrentRegistrationHasOneWinner(t *testing.T) {
	users := newMemoryUsers()
	svc := newTestService(t, users, newMemorySessions(), nil, time.Now)
	const attempts = 8
	results := make(chan error, attempts)
	var start sync.WaitGroup
	start.Add(1)
	for range attempts {
		go func() {
			start.Wait()
			_, err := svc.Register(context.Background(), "admin-secret", "testuser", "Password1!")
			results <- err
		}()
	}
	start.Done()

	var successes, conflicts int
	for range attempts {
		err := <-results
		switch {
		case err == nil:
			successes++
		case errors.Is(err, domain.ErrInvalidArgument):
			conflicts++
		default:
			t.Fatalf("unexpected registration error: %v", err)
		}
	}
	if successes != 1 || conflicts != attempts-1 {
		t.Fatalf("successes=%d conflicts=%d", successes, conflicts)
	}
}

func TestAuthenticateAuthorizeLogoutLifecycle(t *testing.T) {
	now := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	users := newMemoryUsers()
	sessions := newMemorySessions()
	randomBytes := append(bytes.Repeat([]byte{0x5a}, tokenBytes), bytes.Repeat([]byte{0x5b}, tokenBytes)...)
	random := bytes.NewReader(randomBytes)
	svc := newTestService(t, users, sessions, random, func() time.Time { return now })
	user, err := svc.Register(context.Background(), "admin-secret", "testuser", "Password1!")
	if err != nil {
		t.Fatal(err)
	}
	for _, credentials := range [][2]string{{"missing1", "Password1!"}, {"testuser", "Password2!"}} {
		if _, err := svc.Authenticate(context.Background(), credentials[0], credentials[1]); !errors.Is(err, domain.ErrUnauthorized) {
			t.Fatalf("bad credentials error = %v", err)
		}
	}
	token, err := svc.Authenticate(context.Background(), "testuser", "Password1!")
	if err != nil || token == "" {
		t.Fatalf("Authenticate() token=%q err=%v", token, err)
	}
	if bytes.Contains(sessions.lastHash, []byte(token)) || bytes.Equal(sessions.lastHash, []byte(token)) {
		t.Fatal("plaintext token was stored")
	}
	got, err := svc.Authorize(context.Background(), token)
	if err != nil || got.ID != user.ID {
		t.Fatalf("Authorize() = %#v, %v", got, err)
	}
	secondToken, err := svc.Authenticate(context.Background(), "testuser", "Password1!")
	if err != nil || secondToken == token {
		t.Fatalf("second Authenticate() token=%q err=%v", secondToken, err)
	}
	if len(sessions.items) != 2 {
		t.Fatalf("session count = %d, want 2", len(sessions.items))
	}
	for _, item := range sessions.items {
		if !item.session.ExpiresAt.Equal(now.Add(time.Hour)) {
			t.Fatalf("expiry = %v", item.session.ExpiresAt)
		}
	}
	if err := svc.Logout(context.Background(), token); err != nil {
		t.Fatalf("Logout() = %v", err)
	}
	if _, err := svc.Authorize(context.Background(), token); !errors.Is(err, domain.ErrUnauthorized) {
		t.Fatalf("authorize after logout = %v", err)
	}
	if err := svc.Logout(context.Background(), token); !errors.Is(err, domain.ErrUnauthorized) {
		t.Fatalf("repeat logout = %v", err)
	}
	if _, err := svc.Authorize(context.Background(), secondToken); err != nil {
		t.Fatalf("independent session was revoked: %v", err)
	}
}

func TestExpiredSessionIsUnauthorized(t *testing.T) {
	now := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	users := newMemoryUsers()
	sessions := newMemorySessions()
	svc := newTestService(t, users, sessions, bytes.NewReader(bytes.Repeat([]byte{1}, tokenBytes)), func() time.Time { return now })
	_, _ = svc.Register(context.Background(), "admin-secret", "testuser", "Password1!")
	token, _ := svc.Authenticate(context.Background(), "testuser", "Password1!")
	now = now.Add(time.Hour + time.Nanosecond)
	if _, err := svc.Authorize(context.Background(), token); !errors.Is(err, domain.ErrUnauthorized) {
		t.Fatalf("expired authorize = %v", err)
	}
	if err := svc.Logout(context.Background(), token); !errors.Is(err, domain.ErrUnauthorized) {
		t.Fatalf("expired logout = %v", err)
	}
}

func newTestService(t *testing.T, users repository.UserRepository, sessions repository.SessionRepository, random *bytes.Reader, now func() time.Time) Service {
	t.Helper()
	cfg := Config{AdminToken: "admin-secret", SessionTTL: time.Hour, PasswordCost: bcrypt.MinCost, Now: now}
	if random != nil {
		cfg.Rand = random
	}
	svc, err := NewService(users, sessions, cfg)
	if err != nil {
		t.Fatal(err)
	}
	return svc
}

type memoryUsers struct {
	mu     sync.Mutex
	users  map[string]domain.User
	hashes map[string][]byte
}

func newMemoryUsers() *memoryUsers {
	return &memoryUsers{users: map[string]domain.User{}, hashes: map[string][]byte{}}
}
func (m *memoryUsers) Create(_ context.Context, login string, hash []byte) (domain.User, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, exists := m.users[login]; exists {
		return domain.User{}, repository.ErrConflict
	}
	u := domain.User{ID: login + "-id", Login: login, CreatedAt: time.Now().UTC()}
	m.users[login] = u
	m.hashes[login] = append([]byte(nil), hash...)
	return u, nil
}
func (m *memoryUsers) ByLogin(_ context.Context, login string) (domain.User, []byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	u, ok := m.users[login]
	if !ok {
		return domain.User{}, nil, repository.ErrNotFound
	}
	return u, append([]byte(nil), m.hashes[login]...), nil
}
func (m *memoryUsers) ExistingLogins(_ context.Context, _ []string) ([]string, error) {
	return nil, nil
}

type memorySession struct {
	session domain.Session
	user    domain.User
	revoked bool
}
type memorySessions struct {
	mu       sync.Mutex
	items    map[string]*memorySession
	lastHash []byte
}

func newMemorySessions() *memorySessions { return &memorySessions{items: map[string]*memorySession{}} }
func (m *memorySessions) Create(_ context.Context, userID string, hash []byte, expires time.Time) (domain.Session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	key := string(hash)
	if _, ok := m.items[key]; ok {
		return domain.Session{}, repository.ErrConflict
	}
	s := domain.Session{ID: "session-id", UserID: userID, TokenHash: append([]byte(nil), hash...), ExpiresAt: expires}
	m.items[key] = &memorySession{session: s, user: domain.User{ID: userID}}
	m.lastHash = append([]byte(nil), hash...)
	return s, nil
}
func (m *memorySessions) ByTokenHash(_ context.Context, hash []byte, now time.Time) (domain.Session, domain.User, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	item, ok := m.items[string(hash)]
	if !ok || item.revoked || !item.session.ExpiresAt.After(now) {
		return domain.Session{}, domain.User{}, repository.ErrNotFound
	}
	return item.session, item.user, nil
}
func (m *memorySessions) Revoke(_ context.Context, hash []byte, now time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	item, ok := m.items[string(hash)]
	if !ok || item.revoked || !item.session.ExpiresAt.After(now) {
		return repository.ErrNotFound
	}
	item.revoked = true
	return nil
}
