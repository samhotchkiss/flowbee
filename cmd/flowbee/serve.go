package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/samhotchkiss/flowbee/internal/api"
	"github.com/samhotchkiss/flowbee/internal/config"
	fbriver "github.com/samhotchkiss/flowbee/internal/river"
	"github.com/samhotchkiss/flowbee/internal/store"
)

// runServe boots the control plane: load config -> open store -> migrate
// (River then Flowbee) -> start River -> open health + private listeners ->
// block until signal -> graceful shutdown.
func runServe(_ []string) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	st, err := store.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer st.Close()

	if err := fbriver.Migrate(ctx, st.Pool); err != nil {
		return err
	}
	if err := store.MigrateUp(ctx, st.Pool); err != nil {
		return err
	}
	logger.Info("migrations applied")

	riverClient, err := fbriver.NewClient(st.Pool, cfg.RiverMaxWorkers)
	if err != nil {
		return err
	}
	if err := riverClient.Start(ctx); err != nil {
		return err
	}
	var riverStarted atomic.Bool
	riverStarted.Store(true)

	srv := api.New(st, &riverStarted, version)
	healthSrv := &http.Server{Addr: cfg.HealthAddr, Handler: srv.HealthHandler()}
	privateSrv := &http.Server{Addr: cfg.PrivateAddr, Handler: srv.PrivateHandler()}

	go serveHTTP(logger, "health", healthSrv)
	go serveHTTP(logger, "private", privateSrv)

	logger.Info("flowbee serve started",
		"version", version, "health", cfg.HealthAddr, "private", cfg.PrivateAddr)

	<-ctx.Done()
	logger.Info("shutting down")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = healthSrv.Shutdown(shutdownCtx)
	_ = privateSrv.Shutdown(shutdownCtx)
	if err := riverClient.Stop(shutdownCtx); err != nil {
		logger.Error("river stop", "err", err)
	}
	return nil
}

func serveHTTP(logger *slog.Logger, name string, srv *http.Server) {
	logger.Info("listening", "server", name, "addr", srv.Addr)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		logger.Error("http server", "server", name, "err", err)
	}
}
