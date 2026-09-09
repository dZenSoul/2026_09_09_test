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

	handler := httptransport.NewHandler(httptransport.Dependencies{Logger: logger})
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
