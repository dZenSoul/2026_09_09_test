// Package auth defines authentication use cases consumed by transports.
package auth

import (
	"context"

	"documents/internal/domain"
)

type Service interface {
	Register(ctx context.Context, adminToken, login, password string) (domain.User, error)
	Authenticate(ctx context.Context, login, password string) (token string, err error)
	Authorize(ctx context.Context, token string) (domain.User, error)
	Logout(ctx context.Context, token string) error
}
