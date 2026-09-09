// Package repository defines persistence ports used by business services.
// Implementations (for example PostgreSQL) live outside the domain packages.
package repository

import (
	"context"
	"time"

	"documents/internal/domain"
)

type UserRepository interface {
	Create(ctx context.Context, login string, passwordHash []byte) (domain.User, error)
	ByLogin(ctx context.Context, login string) (domain.User, []byte, error)
	ExistingLogins(ctx context.Context, logins []string) ([]string, error)
}

type SessionRepository interface {
	Create(ctx context.Context, userID string, tokenHash []byte, expiresAt time.Time) (domain.Session, error)
	ByTokenHash(ctx context.Context, tokenHash []byte, now time.Time) (domain.Session, domain.User, error)
	Revoke(ctx context.Context, tokenHash []byte, now time.Time) error
}

type DocumentRepository interface {
	Create(ctx context.Context, document domain.Document) (domain.Document, error)
	ByID(ctx context.Context, id string) (domain.Document, error)
	List(ctx context.Context, requesterID string, filter domain.DocumentFilter) ([]domain.Document, error)
	Delete(ctx context.Context, id, ownerID string) (domain.Document, error)
}

// Transactor lets services keep multi-step metadata changes atomic without
// depending on a database driver or concrete transaction type.
type Transactor interface {
	WithinTransaction(ctx context.Context, fn func(context.Context) error) error
}
