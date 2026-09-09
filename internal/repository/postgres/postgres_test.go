package postgres

import (
	"errors"
	"testing"
	"time"

	"documents/internal/repository"

	"github.com/jackc/pgx/v5/pgconn"
)

func TestBuildDocumentFilterWhitelistAndTypes(t *testing.T) {
	tests := []struct {
		name    string
		key     string
		value   string
		wantSQL string
		wantErr bool
	}{
		{name: "none", wantSQL: ""},
		{name: "id", key: "id", value: "90fc1086-2083-4c4f-bdab-eb1691590b84", wantSQL: "d.id ="},
		{name: "name", key: "name", value: "invoice", wantSQL: "d.name ="},
		{name: "mime", key: "mime", value: "application/json", wantSQL: "d.mime ="},
		{name: "file", key: "file", value: "true", wantSQL: "d.is_file ="},
		{name: "public", key: "public", value: "false", wantSQL: "d.is_public ="},
		{name: "created", key: "created", value: "2026-09-09 10:30:56", wantSQL: "date_trunc('second', d.created_at) ="},
		{name: "unknown field", key: "name; DROP TABLE users", value: "x", wantErr: true},
		{name: "invalid uuid", key: "id", value: "not-an-id", wantErr: true},
		{name: "invalid bool", key: "public", value: "1", wantErr: true},
		{name: "invalid time", key: "created", value: "tomorrow", wantErr: true},
		{name: "missing value", key: "name", wantErr: true},
		{name: "value without key", value: "x", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotSQL, _, err := buildDocumentFilter(tt.key, tt.value)
			if tt.wantErr {
				if !errors.Is(err, repository.ErrInvalidArgument) {
					t.Fatalf("error = %v, want ErrInvalidArgument", err)
				}
				return
			}
			if err != nil || gotSQL != tt.wantSQL {
				t.Fatalf("buildDocumentFilter() = %q, %v; want %q, nil", gotSQL, err, tt.wantSQL)
			}
		})
	}
}

func TestLoadMigrationsHasUpAndDown(t *testing.T) {
	items, err := loadMigrations()
	if err != nil {
		t.Fatalf("loadMigrations() error = %v", err)
	}
	if len(items) == 0 {
		t.Fatal("expected embedded migrations")
	}
	for _, item := range items {
		if item.version <= 0 || item.up == "" || item.down == "" {
			t.Fatalf("invalid migration: %#v", item)
		}
	}
}

func TestClassifyDoesNotLeakPostgresDetails(t *testing.T) {
	driverError := &pgconn.PgError{Code: "23505", Message: "secret SQL detail"}
	err := classify("create user", driverError)
	if !errors.Is(err, repository.ErrConflict) {
		t.Fatalf("error = %v, want ErrConflict", err)
	}
	if errors.Is(err, driverError) || contains(err.Error(), driverError.Message) {
		t.Fatalf("driver detail leaked: %v", err)
	}

	internal := classify("query", errors.New("SELECT password_hash failed"))
	if !errors.Is(internal, repository.ErrInternal) || contains(internal.Error(), "password_hash") {
		t.Fatalf("internal detail leaked: %v", internal)
	}
}

func TestNormalizeSessionUsesUTC(t *testing.T) {
	zone := time.FixedZone("test", 3*60*60)
	value := time.Date(2026, 9, 9, 12, 0, 0, 0, zone)
	if got := normalizeTime(value); got.Location() != time.UTC || got.Hour() != 9 {
		t.Fatalf("normalizeTime() = %v", got)
	}
}

func contains(value, substring string) bool {
	for i := 0; i+len(substring) <= len(value); i++ {
		if value[i:i+len(substring)] == substring {
			return true
		}
	}
	return false
}
