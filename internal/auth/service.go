// Package auth defines authentication use cases consumed by transports.
package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"time"
	"unicode"
	"unicode/utf8"

	"documents/internal/domain"
	"documents/internal/repository"

	"golang.org/x/crypto/bcrypt"
)

type Service interface {
	Register(ctx context.Context, adminToken, login, password string) (domain.User, error)
	Authenticate(ctx context.Context, login, password string) (token string, err error)
	Authorize(ctx context.Context, token string) (domain.User, error)
	Logout(ctx context.Context, token string) error
}

const (
	defaultPasswordCost = 12
	tokenBytes          = 32
	maxTokenAttempts    = 5
)

// Config contains the security settings used by the authentication service.
// Rand and Now are optional test seams; production callers should leave them nil.
type Config struct {
	AdminToken   string
	SessionTTL   time.Duration
	PasswordCost int
	Rand         io.Reader
	Now          func() time.Time
}

type service struct {
	users        repository.UserRepository
	sessions     repository.SessionRepository
	adminDigest  [sha256.Size]byte
	sessionTTL   time.Duration
	passwordCost int
	rand         io.Reader
	now          func() time.Time
}

// NewService constructs the authentication use cases.
func NewService(users repository.UserRepository, sessions repository.SessionRepository, cfg Config) (Service, error) {
	if users == nil || sessions == nil || cfg.AdminToken == "" || cfg.SessionTTL <= 0 {
		return nil, fmt.Errorf("configure auth: %w", domain.ErrInvalidArgument)
	}
	cost := cfg.PasswordCost
	if cost == 0 {
		cost = defaultPasswordCost
	}
	if cost < bcrypt.MinCost || cost > bcrypt.MaxCost {
		return nil, fmt.Errorf("configure auth: %w", domain.ErrInvalidArgument)
	}
	random := cfg.Rand
	if random == nil {
		random = rand.Reader
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	return &service{
		users: users, sessions: sessions, adminDigest: sha256.Sum256([]byte(cfg.AdminToken)),
		sessionTTL: cfg.SessionTTL, passwordCost: cost, rand: random, now: now,
	}, nil
}

func (s *service) Register(ctx context.Context, adminToken, login, password string) (domain.User, error) {
	provided := sha256.Sum256([]byte(adminToken))
	if subtle.ConstantTimeCompare(provided[:], s.adminDigest[:]) != 1 {
		return domain.User{}, domain.ErrUnauthorized
	}
	if !validLogin(login) || !validPassword(password) {
		return domain.User{}, domain.ErrInvalidArgument
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), s.passwordCost)
	if err != nil {
		return domain.User{}, fmt.Errorf("register user: %w", domain.ErrInvalidArgument)
	}
	user, err := s.users.Create(ctx, login, hash)
	if err != nil {
		switch {
		case errors.Is(err, repository.ErrConflict), errors.Is(err, repository.ErrInvalidArgument):
			return domain.User{}, domain.ErrInvalidArgument
		default:
			return domain.User{}, fmt.Errorf("register user: %w", err)
		}
	}
	return user, nil
}

func (s *service) Authenticate(ctx context.Context, login, password string) (string, error) {
	if login == "" || password == "" {
		return "", domain.ErrUnauthorized
	}
	user, passwordHash, err := s.users.ByLogin(ctx, login)
	if err != nil {
		if errors.Is(err, repository.ErrNotFound) {
			// Keep the unknown-user path computationally similar to a bad password.
			_ = bcrypt.CompareHashAndPassword(dummyPasswordHash, []byte(password))
			return "", domain.ErrUnauthorized
		}
		return "", fmt.Errorf("authenticate user: %w", err)
	}
	if err := bcrypt.CompareHashAndPassword(passwordHash, []byte(password)); err != nil {
		if errors.Is(err, bcrypt.ErrMismatchedHashAndPassword) {
			return "", domain.ErrUnauthorized
		}
		return "", fmt.Errorf("authenticate user: invalid stored credential")
	}

	for attempt := 0; attempt < maxTokenAttempts; attempt++ {
		raw := make([]byte, tokenBytes)
		if _, err := io.ReadFull(s.rand, raw); err != nil {
			return "", fmt.Errorf("generate session token: %w", err)
		}
		token := base64.RawURLEncoding.EncodeToString(raw)
		digest := tokenDigest(token)
		now := s.now().UTC()
		_, err = s.sessions.Create(ctx, user.ID, digest, now.Add(s.sessionTTL))
		if err == nil {
			return token, nil
		}
		if !errors.Is(err, repository.ErrConflict) {
			return "", fmt.Errorf("create session: %w", err)
		}
	}
	return "", fmt.Errorf("create session: %w", repository.ErrConflict)
}

func (s *service) Authorize(ctx context.Context, token string) (domain.User, error) {
	if token == "" {
		return domain.User{}, domain.ErrUnauthorized
	}
	_, user, err := s.sessions.ByTokenHash(ctx, tokenDigest(token), s.now().UTC())
	if err != nil {
		if errors.Is(err, repository.ErrNotFound) || errors.Is(err, repository.ErrInvalidArgument) {
			return domain.User{}, domain.ErrUnauthorized
		}
		return domain.User{}, fmt.Errorf("authorize session: %w", err)
	}
	return user, nil
}

func (s *service) Logout(ctx context.Context, token string) error {
	if token == "" {
		return domain.ErrUnauthorized
	}
	if err := s.sessions.Revoke(ctx, tokenDigest(token), s.now().UTC()); err != nil {
		if errors.Is(err, repository.ErrNotFound) || errors.Is(err, repository.ErrInvalidArgument) {
			return domain.ErrUnauthorized
		}
		return fmt.Errorf("logout session: %w", err)
	}
	return nil
}

func validLogin(login string) bool {
	if len(login) < 8 {
		return false
	}
	for _, char := range login {
		if !((char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z') || (char >= '0' && char <= '9')) {
			return false
		}
	}
	return true
}

func validPassword(password string) bool {
	if utf8.RuneCountInString(password) < 8 {
		return false
	}
	var lower, upper, digit, special bool
	for _, char := range password {
		switch {
		case char >= 'a' && char <= 'z':
			lower = true
		case char >= 'A' && char <= 'Z':
			upper = true
		case char >= '0' && char <= '9':
			digit = true
		case !unicode.IsLetter(char) && !unicode.IsDigit(char):
			special = true
		}
	}
	return lower && upper && digit && special
}

func tokenDigest(token string) []byte {
	digest := sha256.Sum256([]byte(token))
	return digest[:]
}

// A valid bcrypt hash used only to equalize work for unknown user names.
var dummyPasswordHash = []byte("$2a$12$4pGEX9fXW/.nQI8dJmPKX.9c4XqQ0GJX8dM/hU.9mVvCbHfYpViX2")

var _ Service = (*service)(nil)
