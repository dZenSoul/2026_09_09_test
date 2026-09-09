// Package postgres implements the repository ports with PostgreSQL.
package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"documents/internal/repository"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	defaultMaxConns          = int32(10)
	defaultMinConns          = int32(1)
	defaultMaxConnLifetime   = time.Hour
	defaultMaxConnIdleTime   = 30 * time.Minute
	defaultHealthCheckPeriod = time.Minute
)

// Config controls a deliberately bounded connection pool.
type Config struct {
	DSN               string
	MaxConns          int32
	MinConns          int32
	MaxConnLifetime   time.Duration
	MaxConnIdleTime   time.Duration
	HealthCheckPeriod time.Duration
}

// Store owns the pool and implements all repository interfaces.
type Store struct {
	pool *pgxpool.Pool
}

// Open creates and verifies a bounded pool. The caller must Close the store.
func Open(ctx context.Context, cfg Config) (*Store, error) {
	poolConfig, err := pgxpool.ParseConfig(cfg.DSN)
	if err != nil {
		return nil, fmt.Errorf("open postgres: %w", repository.ErrInternal)
	}

	applyPoolDefaults(&cfg)
	if cfg.MaxConns < 1 || cfg.MinConns < 0 || cfg.MinConns > cfg.MaxConns ||
		cfg.MaxConnLifetime < 0 || cfg.MaxConnIdleTime < 0 || cfg.HealthCheckPeriod <= 0 {
		return nil, fmt.Errorf("open postgres: %w", repository.ErrInvalidArgument)
	}
	poolConfig.MaxConns = cfg.MaxConns
	poolConfig.MinConns = cfg.MinConns
	poolConfig.MaxConnLifetime = cfg.MaxConnLifetime
	poolConfig.MaxConnIdleTime = cfg.MaxConnIdleTime
	poolConfig.HealthCheckPeriod = cfg.HealthCheckPeriod
	poolConfig.ConnConfig.RuntimeParams["timezone"] = "UTC"

	pool, err := pgxpool.NewWithConfig(ctx, poolConfig)
	if err != nil {
		return nil, fmt.Errorf("open postgres: %w", repository.ErrInternal)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping postgres: %w", repository.ErrInternal)
	}
	return New(pool), nil
}

func applyPoolDefaults(cfg *Config) {
	if cfg.MaxConns == 0 {
		cfg.MaxConns = defaultMaxConns
	}
	if cfg.MinConns == 0 {
		cfg.MinConns = defaultMinConns
	}
	if cfg.MaxConnLifetime == 0 {
		cfg.MaxConnLifetime = defaultMaxConnLifetime
	}
	if cfg.MaxConnIdleTime == 0 {
		cfg.MaxConnIdleTime = defaultMaxConnIdleTime
	}
	if cfg.HealthCheckPeriod == 0 {
		cfg.HealthCheckPeriod = defaultHealthCheckPeriod
	}
}

// New wraps an existing pool. Ownership is transferred to Store.
func New(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

func (s *Store) Close() {
	if s != nil && s.pool != nil {
		s.pool.Close()
	}
}

type txContextKey struct{}

type dbtx interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
	Query(context.Context, string, ...any) (pgx.Rows, error)
	QueryRow(context.Context, string, ...any) pgx.Row
}

func (s *Store) executor(ctx context.Context) dbtx {
	if tx, ok := ctx.Value(txContextKey{}).(pgx.Tx); ok {
		return tx
	}
	return s.pool
}

// WithinTransaction runs fn atomically. Repository calls made with the supplied
// context automatically use the transaction. Nested calls reuse the outer one.
func (s *Store) WithinTransaction(ctx context.Context, fn func(context.Context) error) (err error) {
	if fn == nil {
		return fmt.Errorf("transaction: %w", repository.ErrInvalidArgument)
	}
	if _, ok := ctx.Value(txContextKey{}).(pgx.Tx); ok {
		return fn(ctx)
	}

	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return classify("begin transaction", err)
	}
	defer func() {
		if recovered := recover(); recovered != nil {
			_ = tx.Rollback(context.Background())
			panic(recovered)
		}
	}()

	txCtx := context.WithValue(ctx, txContextKey{}, tx)
	if err = fn(txCtx); err != nil {
		_ = tx.Rollback(ctx)
		return err
	}
	if err = tx.Commit(ctx); err != nil {
		return classify("commit transaction", err)
	}
	return nil
}

func classify(operation string, err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("%s: %w", operation, repository.ErrNotFound)
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("%s: %w", operation, err)
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case "23505":
			return fmt.Errorf("%s: %w", operation, repository.ErrConflict)
		case "23503":
			return fmt.Errorf("%s: %w", operation, repository.ErrNotFound)
		}
	}
	return fmt.Errorf("%s: %w", operation, repository.ErrInternal)
}

var (
	_ repository.UserRepository     = userRepository{}
	_ repository.SessionRepository  = sessionRepository{}
	_ repository.DocumentRepository = documentRepository{}
	_ repository.Transactor         = (*Store)(nil)
)
