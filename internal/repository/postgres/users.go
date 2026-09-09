package postgres

import (
	"context"
	"fmt"
	"time"

	"documents/internal/domain"
	"documents/internal/repository"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

func (s *Store) CreateUser(ctx context.Context, login string, passwordHash []byte) (domain.User, error) {
	if login == "" || len(passwordHash) == 0 {
		return domain.User{}, fmt.Errorf("create user: %w", repository.ErrInvalidArgument)
	}
	var user domain.User
	err := s.executor(ctx).QueryRow(ctx, `
        INSERT INTO users (id, login, password_hash)
        VALUES ($1, $2, $3)
        RETURNING id::text, login, created_at`, uuid.NewString(), login, passwordHash).
		Scan(&user.ID, &user.Login, &user.CreatedAt)
	if err != nil {
		return domain.User{}, classify("create user", err)
	}
	user.CreatedAt = user.CreatedAt.UTC()
	return user, nil
}

type userRepository struct{ *Store }

func (r userRepository) Create(ctx context.Context, login string, passwordHash []byte) (domain.User, error) {
	return r.CreateUser(ctx, login, passwordHash)
}

func (s *Store) Users() repository.UserRepository { return userRepository{s} }

func (s *Store) ByLogin(ctx context.Context, login string) (domain.User, []byte, error) {
	var user domain.User
	var passwordHash []byte
	err := s.executor(ctx).QueryRow(ctx, `
        SELECT id::text, login, password_hash, created_at
        FROM users
        WHERE login = $1`, login).
		Scan(&user.ID, &user.Login, &passwordHash, &user.CreatedAt)
	if err != nil {
		return domain.User{}, nil, classify("find user by login", err)
	}
	user.CreatedAt = user.CreatedAt.UTC()
	return user, passwordHash, nil
}

func (s *Store) ExistingLogins(ctx context.Context, logins []string) ([]string, error) {
	if len(logins) == 0 {
		return []string{}, nil
	}
	rows, err := s.executor(ctx).Query(ctx, `
        SELECT login
        FROM users
        WHERE login = ANY($1::text[])
        ORDER BY login`, logins)
	if err != nil {
		return nil, classify("find existing logins", err)
	}
	result, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, classify("read existing logins", err)
	}
	return result, nil
}

func normalizeTime(value time.Time) time.Time { return value.UTC() }
