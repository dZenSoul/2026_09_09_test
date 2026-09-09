package postgres

import (
	"context"
	"fmt"
	"time"

	"documents/internal/domain"
	"documents/internal/repository"

	"github.com/google/uuid"
)

func (s *Store) CreateSession(ctx context.Context, userID string, tokenHash []byte, expiresAt time.Time) (domain.Session, error) {
	if _, err := uuid.Parse(userID); err != nil || len(tokenHash) == 0 || expiresAt.IsZero() {
		return domain.Session{}, fmt.Errorf("create session: %w", repository.ErrInvalidArgument)
	}
	var session domain.Session
	err := s.executor(ctx).QueryRow(ctx, `
        INSERT INTO sessions (id, user_id, token_hash, expires_at)
        VALUES ($1, $2, $3, $4)
        RETURNING id::text, user_id::text, token_hash, created_at, expires_at, revoked_at`,
		uuid.NewString(), userID, tokenHash, normalizeTime(expiresAt)).Scan(
		&session.ID, &session.UserID, &session.TokenHash, &session.CreatedAt,
		&session.ExpiresAt, &session.RevokedAt,
	)
	if err != nil {
		return domain.Session{}, classify("create session", err)
	}
	normalizeSession(&session)
	return session, nil
}

// Create is named CreateSession to avoid Go's lack of method overloading: a
// Store exposes repository-specific views below when interfaces are requested.
func (s *Store) ByTokenHash(ctx context.Context, tokenHash []byte, now time.Time) (domain.Session, domain.User, error) {
	if len(tokenHash) == 0 || now.IsZero() {
		return domain.Session{}, domain.User{}, fmt.Errorf("find session: %w", repository.ErrInvalidArgument)
	}
	var session domain.Session
	var user domain.User
	err := s.executor(ctx).QueryRow(ctx, `
        SELECT s.id::text, s.user_id::text, s.token_hash, s.created_at,
               s.expires_at, s.revoked_at, u.id::text, u.login, u.created_at
        FROM sessions s
        JOIN users u ON u.id = s.user_id
        WHERE s.token_hash = $1 AND s.revoked_at IS NULL AND s.expires_at > $2`,
		tokenHash, normalizeTime(now)).Scan(
		&session.ID, &session.UserID, &session.TokenHash, &session.CreatedAt,
		&session.ExpiresAt, &session.RevokedAt,
		&user.ID, &user.Login, &user.CreatedAt,
	)
	if err != nil {
		return domain.Session{}, domain.User{}, classify("find session", err)
	}
	normalizeSession(&session)
	user.CreatedAt = user.CreatedAt.UTC()
	return session, user, nil
}

func (s *Store) Revoke(ctx context.Context, tokenHash []byte, now time.Time) error {
	if len(tokenHash) == 0 || now.IsZero() {
		return fmt.Errorf("revoke session: %w", repository.ErrInvalidArgument)
	}
	tag, err := s.executor(ctx).Exec(ctx, `
        UPDATE sessions
        SET revoked_at = $2
        WHERE token_hash = $1 AND revoked_at IS NULL AND expires_at > $2`, tokenHash, normalizeTime(now))
	if err != nil {
		return classify("revoke session", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("revoke session: %w", repository.ErrNotFound)
	}
	return nil
}

func normalizeSession(session *domain.Session) {
	session.CreatedAt = session.CreatedAt.UTC()
	session.ExpiresAt = session.ExpiresAt.UTC()
	if session.RevokedAt != nil {
		value := session.RevokedAt.UTC()
		session.RevokedAt = &value
	}
}

// sessionRepository avoids ambiguous Create methods on a combined Store.
type sessionRepository struct{ *Store }

func (r sessionRepository) Create(ctx context.Context, userID string, tokenHash []byte, expiresAt time.Time) (domain.Session, error) {
	return r.CreateSession(ctx, userID, tokenHash, expiresAt)
}

func (s *Store) Sessions() repository.SessionRepository { return sessionRepository{s} }
