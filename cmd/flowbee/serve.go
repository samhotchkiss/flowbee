package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/samhotchkiss/flowbee/internal/alarm"
	"github.com/samhotchkiss/flowbee/internal/api"
	"github.com/samhotchkiss/flowbee/internal/clock"
	"github.com/samhotchkiss/flowbee/internal/config"
	"github.com/samhotchkiss/flowbee/internal/store"
	"github.com/samhotchkiss/flowbee/internal/ulid"
)

// runServe boots the control plane: load config -> open store -> migrate ->
// open health + private listeners -> block until signal -> graceful shutdown.
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

	if err := store.MigrateUp(ctx, st.DB); err != nil {
		return err
	}
	logger.Info("migrations applied")

	st.NoEligibleWorkerDelay = cfg.NoEligibleWorker()

	srv := api.New(st, clock.Real{}, ulid.NewMinter(nil), api.Config{
		LeaseTTL:           cfg.LeaseTTL(),
		HeartbeatInterval:  cfg.HeartbeatInterval(),
		LongPollWait:       cfg.LongPollWait(),
		LeaseTTLS:          cfg.LeaseTTLS,
		HeartbeatIntervalS: cfg.HeartbeatIntervalS,
		// same-box `worktree` provisioning: hand workers the shared bare mirror so
		// they can add a per-lease worktree at base_sha and push to the epoch ref
		// (§7.4). Empty disables local provisioning hints.
		MirrorPath: os.Getenv("FLOWBEE_MIRROR_PATH"),
	}, version)
	healthSrv := &http.Server{Addr: cfg.HealthAddr, Handler: srv.HealthHandler()}
	privateSrv := &http.Server{Addr: cfg.PrivateAddr, Handler: srv.PrivateHandler()}

	go serveHTTP(logger, "health", healthSrv)
	go serveHTTP(logger, "private", privateSrv)

	// the single durable-timer polling goroutine (project override #2): drives the
	// no_eligible_worker alarm, epoch-guarded.
	poller := alarm.New(st, clock.Real{}, time.Second, srv.Broker())
	go poller.Run(ctx)

	logger.Info("flowbee serve started",
		"version", version, "health", cfg.HealthAddr, "private", cfg.PrivateAddr)

	<-ctx.Done()
	logger.Info("shutting down")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = healthSrv.Shutdown(shutdownCtx)
	_ = privateSrv.Shutdown(shutdownCtx)
	return nil
}

func serveHTTP(logger *slog.Logger, name string, srv *http.Server) {
	logger.Info("listening", "server", name, "addr", srv.Addr)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		logger.Error("http server", "server", name, "err", err)
	}
}
