package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"documents/internal/config"
	"documents/internal/httptransport"
	"documents/internal/observability"
	postgresrepository "documents/internal/repository/postgres"
)

func main() {
	if err := run(); err != nil {
		slog.Error("application stopped", "error", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	logger := observability.NewLogger(os.Stdout, cfg.LogLevel)
	slog.SetDefault(logger)

	databaseCtx, cancelDatabase := context.WithTimeout(context.Background(), cfg.HTTPProcessingTimeout)
	defer cancelDatabase()
	store, err := postgresrepository.Open(databaseCtx, postgresrepository.Config{DSN: cfg.PostgresDSN})
	if err != nil {
		return err
	}
	defer store.Close()
	if err := store.MigrateUp(databaseCtx); err != nil {
		return err
	}

	handler := httptransport.NewHandler(httptransport.Dependencies{
		Logger: logger,
		Limits: httptransport.Limits{
			MaxRequestBytes: cfg.MaxRequestBytes,
			MaxFileBytes:    cfg.MaxFileBytes,
			MaxJSONBytes:    cfg.MaxJSONBytes,
			MaxGrantItems:   cfg.MaxGrantItems,
			MaxListLimit:    cfg.MaxListLimit,
		},
	})
	server := &http.Server{
		Addr:              cfg.HTTPAddress(),
		Handler:           http.TimeoutHandler(handler, cfg.HTTPProcessingTimeout, "request timed out"),
		ReadTimeout:       cfg.HTTPReadTimeout,
		ReadHeaderTimeout: cfg.HTTPReadTimeout,
		WriteTimeout:      cfg.HTTPWriteTimeout,
	}

	serveErr := make(chan error, 1)
	go func() {
		logger.Info("HTTP server starting", "address", server.Addr)
		serveErr <- server.ListenAndServe()
	}()

	signalCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	select {
	case err := <-serveErr:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-signalCtx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.GracefulShutdownTimeout)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			return err
		}
		return nil
	}
}
