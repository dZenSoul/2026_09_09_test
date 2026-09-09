package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"sort"
	"testing"
	"time"

	"documents/internal/domain"
	"documents/internal/repository"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

func TestPostgresRepositoriesIntegration(t *testing.T) {
	dsn := os.Getenv("TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("TEST_POSTGRES_DSN is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	base, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect to test postgres: %v", err)
	}
	defer base.Close(context.Background())
	schema := "documents_test_" + uuid.NewString()
	identifier := pgx.Identifier{schema}.Sanitize()
	if _, err := base.Exec(ctx, "CREATE SCHEMA "+identifier); err != nil {
		t.Fatalf("create test schema: %v", err)
	}
	defer func() { _, _ = base.Exec(context.Background(), "DROP SCHEMA "+identifier+" CASCADE") }()

	testDSN := withSearchPath(t, dsn, schema)
	store, err := Open(ctx, Config{DSN: testDSN, MaxConns: 4, MinConns: 1})
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer store.Close()
	if err := store.MigrateUp(ctx); err != nil {
		t.Fatalf("MigrateUp() error = %v", err)
	}
	if err := store.MigrateUp(ctx); err != nil {
		t.Fatalf("second MigrateUp() error = %v", err)
	}
	var versions int
	if err := store.pool.QueryRow(ctx, "SELECT count(*) FROM schema_migrations").Scan(&versions); err != nil || versions != 1 {
		t.Fatalf("migration versions = %d, %v; want 1", versions, err)
	}

	users := store.Users()
	alice := mustCreateUser(t, ctx, users, "alice000")
	bob := mustCreateUser(t, ctx, users, "bob00000")
	carol := mustCreateUser(t, ctx, users, "carol000")
	if _, err := users.Create(ctx, alice.Login, []byte("different")); !errors.Is(err, repository.ErrConflict) {
		t.Fatalf("duplicate login error = %v, want conflict", err)
	}

	rollbackMarker := errors.New("rollback")
	err = store.WithinTransaction(ctx, func(txCtx context.Context) error {
		if _, err := users.Create(txCtx, "rolledback", []byte("hash")); err != nil {
			return err
		}
		return rollbackMarker
	})
	if !errors.Is(err, rollbackMarker) {
		t.Fatalf("transaction error = %v", err)
	}
	if _, _, err := users.ByLogin(ctx, "rolledback"); !errors.Is(err, repository.ErrNotFound) {
		t.Fatalf("rolled-back user lookup = %v, want not found", err)
	}

	sessions := store.Sessions()
	now := time.Now().UTC().Truncate(time.Microsecond)
	tokenHash := []byte("token hash")
	if _, err := sessions.Create(ctx, alice.ID, tokenHash, now.Add(time.Hour)); err != nil {
		t.Fatalf("create session: %v", err)
	}
	if _, err := sessions.Create(ctx, bob.ID, tokenHash, now.Add(time.Hour)); !errors.Is(err, repository.ErrConflict) {
		t.Fatalf("duplicate token hash error = %v, want conflict", err)
	}
	if _, user, err := sessions.ByTokenHash(ctx, tokenHash, now); err != nil || user.ID != alice.ID {
		t.Fatalf("ByTokenHash() user = %#v, error = %v", user, err)
	}
	if err := sessions.Revoke(ctx, tokenHash, now.Add(time.Minute)); err != nil {
		t.Fatalf("Revoke() error = %v", err)
	}
	if _, _, err := sessions.ByTokenHash(ctx, tokenHash, now.Add(2*time.Minute)); !errors.Is(err, repository.ErrNotFound) {
		t.Fatalf("revoked session lookup error = %v", err)
	}

	documents := store.Documents()
	private := mustCreateDocument(t, ctx, documents, domain.Document{
		OwnerID: alice.ID, Name: "same", IsPublic: false,
		JSON: json.RawMessage(`{"kind":"private"}`), Grants: []string{bob.Login, bob.Login, alice.Login},
	})
	if _, err := store.pool.Exec(ctx,
		"INSERT INTO document_grants (document_id, user_id) VALUES ($1, $2)", private.ID, bob.ID,
	); !isPostgresCode(err, "23505") {
		t.Fatalf("duplicate grant error = %v, want PostgreSQL unique violation", err)
	}
	public := mustCreateDocument(t, ctx, documents, domain.Document{
		OwnerID: alice.ID, Name: "same", IsPublic: true,
		JSON: json.RawMessage(`{"kind":"public"}`),
	})
	_ = mustCreateDocument(t, ctx, documents, domain.Document{
		OwnerID: bob.ID, Name: "bob-owned", JSON: json.RawMessage(`null`),
	})

	aliceList, err := documents.List(ctx, alice.ID, domain.DocumentFilter{})
	if err != nil || len(aliceList) != 2 {
		t.Fatalf("alice list length = %d, error = %v", len(aliceList), err)
	}
	if aliceList[0].CreatedAt.Before(aliceList[1].CreatedAt) {
		t.Fatalf("same-name documents not sorted by created_at DESC: %#v", aliceList)
	}
	bobView, err := documents.List(ctx, bob.ID, domain.DocumentFilter{OwnerLogin: alice.Login})
	if err != nil || ids(bobView) != ids([]domain.Document{private, public}) {
		t.Fatalf("bob view = %v, error = %v", ids(bobView), err)
	}
	carolView, err := documents.List(ctx, carol.ID, domain.DocumentFilter{OwnerLogin: alice.Login})
	if err != nil || len(carolView) != 1 || carolView[0].ID != public.ID {
		t.Fatalf("carol view = %#v, error = %v", carolView, err)
	}
	filtered, err := documents.List(ctx, alice.ID, domain.DocumentFilter{Key: "public", Value: "true", Limit: 1})
	if err != nil || len(filtered) != 1 || filtered[0].ID != public.ID {
		t.Fatalf("filtered list = %#v, error = %v", filtered, err)
	}
	if _, err := documents.List(ctx, alice.ID, domain.DocumentFilter{Key: "public", Value: "yes"}); !errors.Is(err, repository.ErrInvalidArgument) {
		t.Fatalf("invalid filter error = %v", err)
	}
	if _, err := documents.ByIDAccessible(ctx, private.ID, bob.ID); err != nil {
		t.Fatalf("granted access error = %v", err)
	}
	if _, err := documents.ByIDAccessible(ctx, private.ID, carol.ID); !errors.Is(err, repository.ErrForbidden) {
		t.Fatalf("private access error = %v, want forbidden", err)
	}
	if _, err := documents.ByIDAccessible(ctx, uuid.NewString(), carol.ID); !errors.Is(err, repository.ErrNotFound) {
		t.Fatalf("missing access error = %v, want not found", err)
	}
	if _, err := documents.Delete(ctx, private.ID, bob.ID); !errors.Is(err, repository.ErrForbidden) {
		t.Fatalf("foreign delete error = %v, want forbidden", err)
	}
	if _, err := documents.Delete(ctx, private.ID, alice.ID); err != nil {
		t.Fatalf("owner delete error = %v", err)
	}
	var grants int
	if err := store.pool.QueryRow(ctx, "SELECT count(*) FROM document_grants WHERE document_id = $1", private.ID).Scan(&grants); err != nil || grants != 0 {
		t.Fatalf("remaining grants = %d, error = %v", grants, err)
	}
	if _, err := documents.Delete(ctx, private.ID, alice.ID); !errors.Is(err, repository.ErrNotFound) {
		t.Fatalf("repeat delete error = %v, want not found", err)
	}

	if err := store.MigrateDown(ctx); err != nil {
		t.Fatalf("MigrateDown() error = %v", err)
	}
	var usersTable *string
	if err := store.pool.QueryRow(ctx, "SELECT to_regclass('users')::text").Scan(&usersTable); err != nil || usersTable != nil {
		t.Fatalf("users table remains after down migration: %v, %v", usersTable, err)
	}
	if err := store.MigrateUp(ctx); err != nil {
		t.Fatalf("MigrateUp() after down error = %v", err)
	}
}

func mustCreateUser(t *testing.T, ctx context.Context, users repository.UserRepository, login string) domain.User {
	t.Helper()
	user, err := users.Create(ctx, login, []byte("password hash"))
	if err != nil {
		t.Fatalf("create user %q: %v", login, err)
	}
	if user.CreatedAt.Location() != time.UTC {
		t.Fatalf("created time is not UTC: %v", user.CreatedAt)
	}
	return user
}

func mustCreateDocument(t *testing.T, ctx context.Context, documents repository.DocumentRepository, input domain.Document) domain.Document {
	t.Helper()
	document, err := documents.Create(ctx, input)
	if err != nil {
		t.Fatalf("create document %q: %v", input.Name, err)
	}
	return document
}

func ids(documents []domain.Document) string {
	result := make([]string, 0, len(documents))
	for _, document := range documents {
		result = append(result, document.ID)
	}
	sort.Strings(result)
	return fmt.Sprint(result)
}

func withSearchPath(t *testing.T, dsn, schema string) string {
	t.Helper()
	parsed, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("parse TEST_POSTGRES_DSN: %v", err)
	}
	query := parsed.Query()
	query.Set("search_path", schema)
	parsed.RawQuery = query.Encode()
	return parsed.String()
}

func isPostgresCode(err error, code string) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == code
}
