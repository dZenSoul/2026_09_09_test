// Package config loads and validates all process configuration.
package config

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	StorageFilesystem = "filesystem"
	StorageS3         = "s3"
)

// Config contains settings that are fixed for the lifetime of a process.
// Secret values are deliberately kept as strings without String methods to
// reduce the chance that a whole configuration is accidentally logged.
type Config struct {
	HTTPHost string
	HTTPPort int

	PostgresDSN string
	AdminToken  string

	StorageDriver string
	FileStorage   FileStorageConfig
	S3Storage     S3StorageConfig

	SessionTTL    time.Duration
	CacheTTL      time.Duration
	CacheMaxBytes int64
	CacheMaxItems int

	MaxRequestBytes int64
	MaxFileBytes    int64
	MaxJSONBytes    int64
	MaxGrantItems   int
	MaxListLimit    int

	HTTPReadTimeout         time.Duration
	HTTPWriteTimeout        time.Duration
	HTTPProcessingTimeout   time.Duration
	GracefulShutdownTimeout time.Duration

	LogLevel          string
	ExposeCacheHeader bool
}

type FileStorageConfig struct {
	Root string
}

type S3StorageConfig struct {
	Endpoint  string
	Region    string
	Bucket    string
	AccessKey string
	SecretKey string
	UseTLS    bool
}

// HTTPAddress returns a correctly joined host and port suitable for net/http.
func (c Config) HTTPAddress() string {
	return net.JoinHostPort(c.HTTPHost, strconv.Itoa(c.HTTPPort))
}

// Load reads configuration from the process environment.
func Load() (Config, error) {
	return LoadFromLookup(os.LookupEnv)
}

// LoadFromLookup makes loading independently testable without mutating the
// process environment. The lookup function has the same contract as os.LookupEnv.
func LoadFromLookup(lookup func(string) (string, bool)) (Config, error) {
	c := Config{
		HTTPHost:                "0.0.0.0",
		HTTPPort:                8080,
		StorageDriver:           StorageFilesystem,
		FileStorage:             FileStorageConfig{Root: "./data/blobs"},
		S3Storage:               S3StorageConfig{Region: "us-east-1", UseTLS: true},
		SessionTTL:              24 * time.Hour,
		CacheTTL:                5 * time.Minute,
		CacheMaxBytes:           64 << 20,
		CacheMaxItems:           1000,
		MaxRequestBytes:         32 << 20,
		MaxFileBytes:            25 << 20,
		MaxJSONBytes:            1 << 20,
		MaxGrantItems:           100,
		MaxListLimit:            100,
		HTTPReadTimeout:         10 * time.Second,
		HTTPWriteTimeout:        30 * time.Second,
		HTTPProcessingTimeout:   30 * time.Second,
		GracefulShutdownTimeout: 10 * time.Second,
		LogLevel:                "info",
	}

	var problems []error
	setString(lookup, "HTTP_HOST", &c.HTTPHost)
	problems = appendParse(problems, parseInt(lookup, "HTTP_PORT", &c.HTTPPort))
	setString(lookup, "POSTGRES_DSN", &c.PostgresDSN)
	setString(lookup, "ADMIN_TOKEN", &c.AdminToken)

	setString(lookup, "STORAGE_DRIVER", &c.StorageDriver)
	c.StorageDriver = strings.ToLower(strings.TrimSpace(c.StorageDriver))
	setString(lookup, "FILE_STORAGE_ROOT", &c.FileStorage.Root)
	setString(lookup, "S3_ENDPOINT", &c.S3Storage.Endpoint)
	setString(lookup, "S3_REGION", &c.S3Storage.Region)
	setString(lookup, "S3_BUCKET", &c.S3Storage.Bucket)
	setString(lookup, "S3_ACCESS_KEY", &c.S3Storage.AccessKey)
	setString(lookup, "S3_SECRET_KEY", &c.S3Storage.SecretKey)
	problems = appendParse(problems, parseBool(lookup, "S3_USE_TLS", &c.S3Storage.UseTLS))

	for _, item := range []struct {
		name string
		dst  *time.Duration
	}{
		{"SESSION_TTL", &c.SessionTTL},
		{"CACHE_TTL", &c.CacheTTL},
		{"HTTP_READ_TIMEOUT", &c.HTTPReadTimeout},
		{"HTTP_WRITE_TIMEOUT", &c.HTTPWriteTimeout},
		{"HTTP_PROCESSING_TIMEOUT", &c.HTTPProcessingTimeout},
		{"GRACEFUL_SHUTDOWN_TIMEOUT", &c.GracefulShutdownTimeout},
	} {
		problems = appendParse(problems, parseDuration(lookup, item.name, item.dst))
	}

	for _, item := range []struct {
		name string
		dst  *int64
	}{
		{"CACHE_MAX_BYTES", &c.CacheMaxBytes},
		{"MAX_REQUEST_BYTES", &c.MaxRequestBytes},
		{"MAX_FILE_BYTES", &c.MaxFileBytes},
		{"MAX_JSON_BYTES", &c.MaxJSONBytes},
	} {
		problems = appendParse(problems, parseInt64(lookup, item.name, item.dst))
	}

	for _, item := range []struct {
		name string
		dst  *int
	}{
		{"CACHE_MAX_ITEMS", &c.CacheMaxItems},
		{"MAX_GRANT_ITEMS", &c.MaxGrantItems},
		{"MAX_LIST_LIMIT", &c.MaxListLimit},
	} {
		problems = appendParse(problems, parseInt(lookup, item.name, item.dst))
	}

	setString(lookup, "LOG_LEVEL", &c.LogLevel)
	c.LogLevel = strings.ToLower(strings.TrimSpace(c.LogLevel))
	problems = appendParse(problems, parseBool(lookup, "EXPOSE_CACHE_HEADER", &c.ExposeCacheHeader))
	problems = append(problems, c.validate()...)

	if len(problems) != 0 {
		return Config{}, fmt.Errorf("invalid configuration: %w", errors.Join(problems...))
	}
	return c, nil
}

func (c Config) validate() []error {
	var problems []error
	if strings.TrimSpace(c.HTTPHost) == "" {
		problems = append(problems, errors.New("HTTP_HOST must not be empty"))
	}
	if c.HTTPPort < 1 || c.HTTPPort > 65535 {
		problems = append(problems, errors.New("HTTP_PORT must be between 1 and 65535"))
	}
	if err := validatePostgresDSN(c.PostgresDSN); err != nil {
		problems = append(problems, err)
	}
	if strings.TrimSpace(c.AdminToken) == "" {
		problems = append(problems, errors.New("ADMIN_TOKEN is required"))
	}

	switch c.StorageDriver {
	case StorageFilesystem:
		if strings.TrimSpace(c.FileStorage.Root) == "" {
			problems = append(problems, errors.New("FILE_STORAGE_ROOT is required for filesystem storage"))
		}
	case StorageS3:
		for name, value := range map[string]string{
			"S3_ENDPOINT":   c.S3Storage.Endpoint,
			"S3_BUCKET":     c.S3Storage.Bucket,
			"S3_ACCESS_KEY": c.S3Storage.AccessKey,
			"S3_SECRET_KEY": c.S3Storage.SecretKey,
		} {
			if strings.TrimSpace(value) == "" {
				problems = append(problems, fmt.Errorf("%s is required for S3 storage", name))
			}
		}
	default:
		problems = append(problems, errors.New("STORAGE_DRIVER must be filesystem or s3"))
	}

	positiveDurations := []struct {
		name  string
		value time.Duration
	}{
		{"SESSION_TTL", c.SessionTTL}, {"CACHE_TTL", c.CacheTTL},
		{"HTTP_READ_TIMEOUT", c.HTTPReadTimeout}, {"HTTP_WRITE_TIMEOUT", c.HTTPWriteTimeout},
		{"HTTP_PROCESSING_TIMEOUT", c.HTTPProcessingTimeout}, {"GRACEFUL_SHUTDOWN_TIMEOUT", c.GracefulShutdownTimeout},
	}
	for _, item := range positiveDurations {
		if item.value <= 0 {
			problems = append(problems, fmt.Errorf("%s must be positive", item.name))
		}
	}
	positiveValues := []struct {
		name  string
		value int64
	}{
		{"CACHE_MAX_BYTES", c.CacheMaxBytes}, {"CACHE_MAX_ITEMS", int64(c.CacheMaxItems)},
		{"MAX_REQUEST_BYTES", c.MaxRequestBytes}, {"MAX_FILE_BYTES", c.MaxFileBytes},
		{"MAX_JSON_BYTES", c.MaxJSONBytes}, {"MAX_GRANT_ITEMS", int64(c.MaxGrantItems)},
		{"MAX_LIST_LIMIT", int64(c.MaxListLimit)},
	}
	for _, item := range positiveValues {
		if item.value <= 0 {
			problems = append(problems, fmt.Errorf("%s must be positive", item.name))
		}
	}
	if c.MaxFileBytes > c.MaxRequestBytes {
		problems = append(problems, errors.New("MAX_FILE_BYTES must not exceed MAX_REQUEST_BYTES"))
	}
	if c.MaxJSONBytes > c.MaxRequestBytes {
		problems = append(problems, errors.New("MAX_JSON_BYTES must not exceed MAX_REQUEST_BYTES"))
	}
	if c.HTTPWriteTimeout < c.HTTPProcessingTimeout {
		problems = append(problems, errors.New("HTTP_WRITE_TIMEOUT must be at least HTTP_PROCESSING_TIMEOUT"))
	}
	if !isLogLevel(c.LogLevel) {
		problems = append(problems, errors.New("LOG_LEVEL must be debug, info, warn, or error"))
	}
	return problems
}

func validatePostgresDSN(value string) error {
	value = strings.TrimSpace(value)
	if value == "" {
		return errors.New("POSTGRES_DSN is required")
	}
	u, err := url.Parse(value)
	if err != nil || (u.Scheme != "postgres" && u.Scheme != "postgresql") || u.Host == "" || strings.Trim(u.Path, "/") == "" {
		return errors.New("POSTGRES_DSN must be a valid postgres or postgresql URL with host and database")
	}
	return nil
}

func isLogLevel(value string) bool {
	return value == "debug" || value == "info" || value == "warn" || value == "error"
}

func setString(lookup func(string) (string, bool), name string, dst *string) {
	if value, ok := lookup(name); ok {
		*dst = value
	}
}

func parseDuration(lookup func(string) (string, bool), name string, dst *time.Duration) error {
	value, ok := lookup(name)
	if !ok {
		return nil
	}
	parsed, err := time.ParseDuration(value)
	if err != nil {
		return fmt.Errorf("%s must be a valid duration", name)
	}
	*dst = parsed
	return nil
}

func parseInt(lookup func(string) (string, bool), name string, dst *int) error {
	value, ok := lookup(name)
	if !ok {
		return nil
	}
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return fmt.Errorf("%s must be an integer", name)
	}
	*dst = parsed
	return nil
}

func parseInt64(lookup func(string) (string, bool), name string, dst *int64) error {
	value, ok := lookup(name)
	if !ok {
		return nil
	}
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return fmt.Errorf("%s must be an integer number of bytes", name)
	}
	*dst = parsed
	return nil
}

func parseBool(lookup func(string) (string, bool), name string, dst *bool) error {
	value, ok := lookup(name)
	if !ok {
		return nil
	}
	parsed, err := strconv.ParseBool(value)
	if err != nil {
		return fmt.Errorf("%s must be true or false", name)
	}
	*dst = parsed
	return nil
}

func appendParse(problems []error, err error) []error {
	if err != nil {
		return append(problems, err)
	}
	return problems
}
