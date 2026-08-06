// Command ciplatform is the control plane: webhook ingest, scheduling, status
// reporting, the services unmodified actions talk to, and the web UI.
//
// Everything it needs is required. There is no placeholder mode, no
// "configure it later" path, and nothing that starts up degraded and reports
// success anyway; a missing dependency fails here, naming what is missing.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/wow-look-at-my/ci-platform/internal/config"
)

// version is stamped at build time.
var version = "dev"

func main() {
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(log)

	if err := run(log); err != nil {
		log.Error("control plane stopped", "err", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	cfg, err := config.Load()
	if err != nil {
		// Configuration problems are reported all at once, so an operator
		// fixes them in one pass rather than one restart per variable.
		return err
	}
	log.Info("starting", "version", version, "public_url", cfg.PublicURL.String(), "listen", cfg.Listen)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	app, err := newApp(ctx, cfg, log)
	if err != nil {
		return err
	}
	defer app.Close()

	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           app.Handler(),
		ReadHeaderTimeout: 15 * time.Second,
		// No write timeout: the SSE log tail and the runner long-poll both hold
		// a response open far longer than any sane fixed limit.
	}

	errc := make(chan error, 1)
	go func() {
		log.Info("listening", "addr", cfg.Listen)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errc <- err
		}
	}()

	go app.RunBackground(ctx)

	select {
	case err := <-errc:
		return fmt.Errorf("http server: %w", err)
	case <-ctx.Done():
		log.Info("shutting down")
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("shutdown: %w", err)
	}
	return nil
}
