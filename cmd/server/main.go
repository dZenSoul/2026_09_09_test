package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"documents/internal/auth"
	"documents/internal/blob"
	responsecache "documents/internal/cache"
	"documents/internal/config"
	"documents/internal/document"
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
	metrics := observability.NewMetrics()
	slog.SetDefault(logger)

	databaseCtx, cancelDatabase := context.WithTimeout(context.Background(), cfg.HTTPProcessingTimeout)
	defer cancelDatabase()
	store, err := postgresrepository.Open(databaseCtx, postgresrepository.Config{
		DSN:      cfg.PostgresDSN,
		MaxConns: int32(cfg.PostgresMaxConns), MinConns: int32(cfg.PostgresMinConns),
		MaxConnLifetime: cfg.PostgresMaxConnLifetime, MaxConnIdleTime: cfg.PostgresMaxConnIdleTime,
		HealthCheckPeriod: cfg.PostgresHealthCheckPeriod, ObserveQuery: metrics.ObserveDatabase,
	})
	if err != nil {
		return err
	}
	defer store.Close()
	authService, err := auth.NewService(store.Users(), store.Sessions(), auth.Config{
		AdminToken: cfg.AdminToken,
		SessionTTL: cfg.SessionTTL,
	})
	if err != nil {
		return err
	}
	if cfg.StorageDriver != config.StorageFilesystem {
		return errors.New("configured blob storage driver is not implemented")
	}
	blobStorage, err := blob.NewFileStorage(cfg.FileStorage.Root)
	if err != nil {
		return err
	}
	defer blobStorage.Close()
	documentService, err := document.NewService(store.Documents(), store.Users(), blobStorage, document.Config{
		MaxFileBytes:  cfg.MaxFileBytes,
		MaxGrantItems: cfg.MaxGrantItems,
		MaxListLimit:  cfg.MaxListLimit,
	})
	if err != nil {
		return err
	}

	cache := responsecache.NewMemory(cfg.CacheMaxBytes, cfg.CacheMaxItems)
	defer cache.Close()
	handler := httptransport.NewHandler(httptransport.Dependencies{
		Auth:              authService,
		Documents:         documentService,
		Cache:             cache,
		CacheTTL:          cfg.CacheTTL,
		ExposeCacheHeader: cfg.ExposeCacheHeader,
		Logger:            logger,
		Metrics:           metrics,
		Readiness: func(ctx context.Context) error {
			if err := store.Ping(ctx); err != nil {
				return err
			}
			return blobStorage.Ping(ctx)
		},
		ProcessingTimeout: cfg.HTTPProcessingTimeout,
		Limits: httptransport.Limits{
			MaxRequestBytes: cfg.MaxRequestBytes,
			MaxFileBytes:    cfg.MaxFileBytes,
			MaxJSONBytes:    cfg.MaxJSONBytes,
			MaxGrantItems:   cfg.MaxGrantItems,
			MaxListLimit:    cfg.MaxListLimit,
		},
	})
	mux := http.NewServeMux()
	mux.Handle("/metrics", metrics)
	mux.Handle("/", handler)
	server := &http.Server{
		Addr:              cfg.HTTPAddress(),
		Handler:           mux,
		ReadTimeout:       cfg.HTTPReadTimeout,
		ReadHeaderTimeout: cfg.HTTPReadTimeout,
		WriteTimeout:      cfg.HTTPWriteTimeout,
		IdleTimeout:       cfg.HTTPIdleTimeout,
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
			closeErr := server.Close()
			if closeErr != nil {
				return errors.Join(err, closeErr)
			}
			return err
		}
		return nil
	}
}
