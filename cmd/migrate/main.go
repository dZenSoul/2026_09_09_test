// Command migrate applies all pending embedded PostgreSQL migrations.
package main

import (
	"context"
	"log/slog"
	"os"

	"documents/internal/config"
	postgresrepository "documents/internal/repository/postgres"
)

func main() {
	if err := run(); err != nil {
		slog.Error("migration failed", "error", err)
		os.Exit(1)
	}
	slog.Info("migrations completed")
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), cfg.HTTPProcessingTimeout)
	defer cancel()
	store, err := postgresrepository.Open(ctx, postgresrepository.Config{
		DSN:               cfg.PostgresDSN,
		MaxConns:          int32(cfg.PostgresMaxConns),
		MinConns:          int32(cfg.PostgresMinConns),
		MaxConnLifetime:   cfg.PostgresMaxConnLifetime,
		MaxConnIdleTime:   cfg.PostgresMaxConnIdleTime,
		HealthCheckPeriod: cfg.PostgresHealthCheckPeriod,
	})
	if err != nil {
		return err
	}
	defer store.Close()

	return store.MigrateUp(ctx)
}
