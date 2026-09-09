package config

import (
	"strings"
	"testing"
	"time"
)

func lookupFrom(values map[string]string) func(string) (string, bool) {
	return func(key string) (string, bool) {
		value, ok := values[key]
		return value, ok
	}
}

func requiredEnvironment() map[string]string {
	return map[string]string{
		"POSTGRES_DSN": "postgres://app:secret@db:5432/documents?sslmode=disable",
		"ADMIN_TOKEN":  "a-test-admin-token",
	}
}

func TestLoadDefaults(t *testing.T) {
	cfg, err := LoadFromLookup(lookupFrom(requiredEnvironment()))
	if err != nil {
		t.Fatalf("LoadFromLookup() error = %v", err)
	}

	if cfg.HTTPAddress() != "0.0.0.0:8080" {
		t.Errorf("HTTPAddress() = %q", cfg.HTTPAddress())
	}
	if cfg.StorageDriver != StorageFilesystem || cfg.FileStorage.Root != "./data/blobs" {
		t.Errorf("unexpected default storage: %#v", cfg)
	}
	if cfg.SessionTTL != 24*time.Hour || cfg.CacheTTL != 5*time.Minute {
		t.Errorf("unexpected TTL defaults: session=%s cache=%s", cfg.SessionTTL, cfg.CacheTTL)
	}
	if cfg.MaxRequestBytes != 32<<20 || cfg.MaxFileBytes != 25<<20 || cfg.MaxJSONBytes != 1<<20 {
		t.Errorf("unexpected size defaults")
	}
	if cfg.ExposeCacheHeader {
		t.Error("diagnostic cache header must be disabled by default")
	}
}

func TestLoadOverrides(t *testing.T) {
	env := requiredEnvironment()
	overrides := map[string]string{
		"HTTP_HOST": "127.0.0.1", "HTTP_PORT": "9090",
		"SESSION_TTL": "2h", "CACHE_TTL": "45s",
		"CACHE_MAX_BYTES": "2048", "CACHE_MAX_ITEMS": "7",
		"MAX_REQUEST_BYTES": "4096", "MAX_FILE_BYTES": "3072", "MAX_JSON_BYTES": "1024",
		"MAX_GRANT_ITEMS": "5", "MAX_LIST_LIMIT": "25",
		"HTTP_READ_TIMEOUT": "3s", "HTTP_WRITE_TIMEOUT": "9s",
		"HTTP_PROCESSING_TIMEOUT": "8s", "GRACEFUL_SHUTDOWN_TIMEOUT": "4s",
		"LOG_LEVEL": "debug", "EXPOSE_CACHE_HEADER": "true",
	}
	for key, value := range overrides {
		env[key] = value
	}

	cfg, err := LoadFromLookup(lookupFrom(env))
	if err != nil {
		t.Fatalf("LoadFromLookup() error = %v", err)
	}
	if cfg.HTTPAddress() != "127.0.0.1:9090" || cfg.SessionTTL != 2*time.Hour || cfg.CacheTTL != 45*time.Second {
		t.Errorf("overrides were not applied: %#v", cfg)
	}
	if cfg.CacheMaxBytes != 2048 || cfg.CacheMaxItems != 7 || cfg.MaxListLimit != 25 {
		t.Errorf("limit overrides were not applied: %#v", cfg)
	}
	if cfg.LogLevel != "debug" || !cfg.ExposeCacheHeader {
		t.Errorf("observability overrides were not applied: %#v", cfg)
	}
}

func TestLoadS3Storage(t *testing.T) {
	env := requiredEnvironment()
	env["STORAGE_DRIVER"] = "s3"
	env["S3_ENDPOINT"] = "minio:9000"
	env["S3_BUCKET"] = "documents"
	env["S3_ACCESS_KEY"] = "access"
	env["S3_SECRET_KEY"] = "secret"
	env["S3_USE_TLS"] = "false"

	cfg, err := LoadFromLookup(lookupFrom(env))
	if err != nil {
		t.Fatalf("LoadFromLookup() error = %v", err)
	}
	if cfg.S3Storage.Endpoint != "minio:9000" || cfg.S3Storage.UseTLS {
		t.Errorf("unexpected S3 config: %#v", cfg.S3Storage)
	}
}

func TestLoadRejectsMissingRequiredValues(t *testing.T) {
	_, err := LoadFromLookup(lookupFrom(nil))
	if err == nil {
		t.Fatal("expected an error")
	}
	for _, name := range []string{"POSTGRES_DSN", "ADMIN_TOKEN"} {
		if !strings.Contains(err.Error(), name) {
			t.Errorf("error %q does not name %s", err, name)
		}
	}
}

func TestLoadRejectsInvalidValues(t *testing.T) {
	tests := []struct {
		name  string
		key   string
		value string
	}{
		{"duration", "SESSION_TTL", "tomorrow"},
		{"negative size", "CACHE_MAX_BYTES", "-1"},
		{"port", "HTTP_PORT", "70000"},
		{"dsn", "POSTGRES_DSN", "mysql://db/documents"},
		{"boolean", "EXPOSE_CACHE_HEADER", "sometimes"},
		{"log level", "LOG_LEVEL", "verbose"},
		{"file larger than request", "MAX_FILE_BYTES", "999999999"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := requiredEnvironment()
			env[tt.key] = tt.value
			if _, err := LoadFromLookup(lookupFrom(env)); err == nil {
				t.Fatal("expected an error")
			}
		})
	}
}

func TestErrorsDoNotRevealSecrets(t *testing.T) {
	const adminSecret = "never-print-this-admin-secret"
	const databaseSecret = "never-print-this-db-secret"
	env := map[string]string{
		"POSTGRES_DSN": "postgres://app:" + databaseSecret + "@db:5432/documents",
		"ADMIN_TOKEN":  adminSecret,
		"SESSION_TTL":  "invalid",
	}
	_, err := LoadFromLookup(lookupFrom(env))
	if err == nil {
		t.Fatal("expected an error")
	}
	if strings.Contains(err.Error(), adminSecret) || strings.Contains(err.Error(), databaseSecret) {
		t.Fatalf("configuration error revealed a secret: %v", err)
	}
}

func TestS3ErrorsDoNotRevealCredentials(t *testing.T) {
	env := requiredEnvironment()
	env["STORAGE_DRIVER"] = "s3"
	env["S3_ACCESS_KEY"] = "sensitive-access-key"
	_, err := LoadFromLookup(lookupFrom(env))
	if err == nil {
		t.Fatal("expected an error")
	}
	if strings.Contains(err.Error(), env["S3_ACCESS_KEY"]) {
		t.Fatalf("configuration error revealed a credential: %v", err)
	}
}
