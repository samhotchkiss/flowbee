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
	"github.com/samhotchkiss/flowbee/internal/auth"
	"github.com/samhotchkiss/flowbee/internal/clock"
	"github.com/samhotchkiss/flowbee/internal/config"
	"github.com/samhotchkiss/flowbee/internal/github"
	"github.com/samhotchkiss/flowbee/internal/job"
	"github.com/samhotchkiss/flowbee/internal/reconcile"
	"github.com/samhotchkiss/flowbee/internal/store"
	"github.com/samhotchkiss/flowbee/internal/ulid"
	"github.com/samhotchkiss/flowbee/internal/webhook"
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

	// worker-transport mutual auth (§7.6): when a signing secret is configured the
	// private API requires a signed per-worker bearer token bound to an enrolled
	// identity — the trust boundary a non-loopback (Tailscale/LAN) listener needs.
	// Empty secret = loopback-only dev (no mutual auth). mTLS is the documented
	// alternative (auth.MTLSConfig); it needs a CA + per-worker certs (real infra),
	// so it is not the default in-env path.
	var authn auth.Authenticator
	if cfg.WorkerAuthSecret != "" {
		authn = auth.NewBearer([]byte(cfg.WorkerAuthSecret), cfg.EnrolledIdentities, cfg.AuthLoopbackBypass)
		logger.Info("worker mutual-auth enabled", "enrolled", len(cfg.EnrolledIdentities), "loopback_bypass", cfg.AuthLoopbackBypass)
	}

	srv := api.New(st, clock.Real{}, ulid.NewMinter(nil), api.Config{
		LeaseTTL:           cfg.LeaseTTL(),
		HeartbeatInterval:  cfg.HeartbeatInterval(),
		LongPollWait:       cfg.LongPollWait(),
		LeaseTTLS:          cfg.LeaseTTLS,
		HeartbeatIntervalS: cfg.HeartbeatIntervalS,
		// same-box `worktree` provisioning: hand workers the shared bare mirror so
		// they can add a per-lease worktree at base_sha and push to the epoch ref
		// (§7.4). Empty disables local provisioning hints.
		MirrorPath:    os.Getenv("FLOWBEE_MIRROR_PATH"),
		Authenticator: authn,
		// THE ONE DECISION (§14, F2): Branch B (autonomous merge) when
		// FLOWBEE_ALLOW_SELF_MERGE is set; default false = Branch A (handoff).
		Policy: job.Policy{AllowSelfMerge: cfg.AllowSelfMerge},
		// F2: the operator content-integrity posture (ceilings + extra denylist).
		ContentPolicy: cfg.ContentPolicy(),
	}, version)
	if cfg.AllowSelfMerge {
		logger.Info("autonomous merge enabled (Branch B): self_merge eligible jobs merge without a human gate")
	}
	healthSrv := &http.Server{Addr: cfg.HealthAddr, Handler: srv.HealthHandler()}
	privateSrv := &http.Server{Addr: cfg.PrivateAddr, Handler: srv.PrivateHandler()}

	go serveHTTP(logger, "health", healthSrv)
	go serveHTTP(logger, "private", privateSrv)

	// the single durable-timer polling goroutine (project override #2): drives the
	// no_eligible_worker alarm + the M8 liveness deadlines (Rung-3), epoch-guarded.
	livenessCfg := store.LivenessConfig{
		PhaseBudget:                   cfg.LeaseTTL() / 2, // soft deadline ~ half the TTL window
		AbsoluteCap:                   cfg.LeaseTTL(),     // the un-gameable Rung-3 floor
		Rung2Window:                   cfg.LeaseTTL() / 2,
		GovernorCeiling:               3, // Rung-4 anti-thrash (distinct from max_attempts)
		CircuitBreakerAbstainFraction: 0.8,
	}
	poller := alarm.New(st, clock.Real{}, time.Second, srv.Broker()).
		WithLiveness(livenessCfg, store.DBFactSource{DB: st.DB}, srv.Broker())
	go poller.Run(ctx)
	// the Rung-2 sweep + two-rung evaluation pass runs on the slower external-oracle
	// cadence (§10.2 — Rung-2 only updates on the reconcile sweep).
	go func() {
		t := time.NewTicker(30 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				poller.Rung2Tick(ctx)
			}
		}
	}()

	// reconcile-IN (M6, §8.1): Flowbee is the SINGLE GitHub caller (R4). Wired only
	// when GitHub App config is present (FLOWBEE_GITHUB_OWNER/REPO/TOKEN); there are
	// no creds in dev/CI, so this stays dormant by default (e2e_github off). When
	// configured: a boot sweep, then a low-frequency periodic sweep (the floor),
	// and a PUBLIC webhook listener (HMAC, deduped, write-ahead -> targeted refetch).
	if owner, repo := os.Getenv("FLOWBEE_GITHUB_OWNER"), os.Getenv("FLOWBEE_GITHUB_REPO"); owner != "" && repo != "" {
		tok := os.Getenv("FLOWBEE_GITHUB_TOKEN")
		ghClient := github.NewRealClient(owner, repo, func(context.Context) (string, error) { return tok, nil })
		rec := reconcile.New(st, ghClient, clock.Real{}, srv.Broker())
		// boot sweep + periodic floor sweep (every 2-5 min; default 3 min).
		go func() {
			if _, err := rec.Sweep(ctx); err != nil {
				logger.Error("boot reconcile sweep", "err", err)
			}
			t := time.NewTicker(3 * time.Minute)
			defer t.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-t.C:
					if _, err := rec.Sweep(ctx); err != nil {
						logger.Error("reconcile sweep", "err", err)
					}
				}
			}
		}()
		// the PUBLIC webhook listener (I-2): only started when a secret is set.
		if secret := os.Getenv("FLOWBEE_WEBHOOK_SECRET"); secret != "" {
			wh := webhook.New(secret, st, rec)
			webhookMux := http.NewServeMux()
			webhookMux.Handle("POST /webhooks", wh)
			webhookSrv := &http.Server{Addr: cfg.WebhookAddr, Handler: webhookMux}
			go serveHTTP(logger, "webhook", webhookSrv)
			defer func() {
				shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				_ = webhookSrv.Shutdown(shutdownCtx)
			}()
		}
		logger.Info("reconcile-IN wired", "owner", owner, "repo", repo)
	}

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
