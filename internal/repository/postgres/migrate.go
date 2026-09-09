package postgres

import (
	"context"
	"embed"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"documents/internal/repository"

	"github.com/jackc/pgx/v5"
)

//go:embed migrations/*.sql
var migrationFiles embed.FS

const migrationLockID int64 = 742019663417

type migration struct {
	version int64
	up      string
	down    string
}

// MigrateUp applies every unapplied embedded migration exactly once.
func (s *Store) MigrateUp(ctx context.Context) error {
	migrations, err := loadMigrations()
	if err != nil {
		return err
	}
	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		return classify("acquire migration connection", err)
	}
	defer conn.Release()

	if _, err := conn.Exec(ctx, "SELECT pg_advisory_lock($1)", migrationLockID); err != nil {
		return classify("lock migrations", err)
	}
	defer func() { _, _ = conn.Exec(context.Background(), "SELECT pg_advisory_unlock($1)", migrationLockID) }()

	if _, err := conn.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
        version bigint PRIMARY KEY,
        applied_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP
    )`); err != nil {
		return classify("create migration ledger", err)
	}

	for _, item := range migrations {
		var applied bool
		if err := conn.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM schema_migrations WHERE version = $1)", item.version).Scan(&applied); err != nil {
			return classify("read migration ledger", err)
		}
		if applied {
			continue
		}
		if err := applyMigration(ctx, conn, item); err != nil {
			return err
		}
	}
	return nil
}

func applyMigration(ctx context.Context, conn interface {
	Begin(context.Context) (pgx.Tx, error)
}, item migration) error {
	tx, err := conn.Begin(ctx)
	if err != nil {
		return classify("begin migration", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if _, err := tx.Exec(ctx, item.up); err != nil {
		return classify(fmt.Sprintf("apply migration %d", item.version), err)
	}
	if _, err := tx.Exec(ctx, "INSERT INTO schema_migrations(version) VALUES ($1)", item.version); err != nil {
		return classify("record migration", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return classify("commit migration", err)
	}
	return nil
}

// MigrateDown reverts the most recently applied migration. It is intended for
// development and tests; production startup should call only MigrateUp.
func (s *Store) MigrateDown(ctx context.Context) error {
	migrations, err := loadMigrations()
	if err != nil {
		return err
	}
	byVersion := make(map[int64]migration, len(migrations))
	for _, item := range migrations {
		byVersion[item.version] = item
	}

	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		return classify("acquire migration connection", err)
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, "SELECT pg_advisory_lock($1)", migrationLockID); err != nil {
		return classify("lock migrations", err)
	}
	defer func() { _, _ = conn.Exec(context.Background(), "SELECT pg_advisory_unlock($1)", migrationLockID) }()

	var version int64
	if err := conn.QueryRow(ctx, "SELECT version FROM schema_migrations ORDER BY version DESC LIMIT 1").Scan(&version); err != nil {
		return classify("find migration to revert", err)
	}
	item, ok := byVersion[version]
	if !ok {
		return fmt.Errorf("revert migration: %w", repository.ErrInternal)
	}

	tx, err := conn.Begin(ctx)
	if err != nil {
		return classify("begin migration rollback", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if _, err := tx.Exec(ctx, item.down); err != nil {
		return classify("revert migration", err)
	}
	if _, err := tx.Exec(ctx, "DELETE FROM schema_migrations WHERE version = $1", version); err != nil {
		return classify("update migration ledger", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return classify("commit migration rollback", err)
	}
	return nil
}

func loadMigrations() ([]migration, error) {
	entries, err := migrationFiles.ReadDir("migrations")
	if err != nil {
		return nil, fmt.Errorf("load migrations: %w", repository.ErrInternal)
	}
	items := make(map[int64]*migration)
	for _, entry := range entries {
		name := entry.Name()
		parts := strings.Split(name, ".")
		if len(parts) != 3 || (parts[1] != "up" && parts[1] != "down") || parts[2] != "sql" {
			continue
		}
		prefix := strings.SplitN(parts[0], "_", 2)[0]
		version, err := strconv.ParseInt(prefix, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("load migrations: %w", repository.ErrInternal)
		}
		contents, err := migrationFiles.ReadFile("migrations/" + name)
		if err != nil {
			return nil, fmt.Errorf("load migrations: %w", repository.ErrInternal)
		}
		item := items[version]
		if item == nil {
			item = &migration{version: version}
			items[version] = item
		}
		if parts[1] == "up" {
			item.up = string(contents)
		} else {
			item.down = string(contents)
		}
	}
	result := make([]migration, 0, len(items))
	for _, item := range items {
		if item.up == "" || item.down == "" {
			return nil, fmt.Errorf("load migrations: %w", repository.ErrInternal)
		}
		result = append(result, *item)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].version < result[j].version })
	return result, nil
}
