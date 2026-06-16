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
	"github.com/samhotchkiss/flowbee/internal/gitops"
	"github.com/samhotchkiss/flowbee/internal/job"
	"github.com/samhotchkiss/flowbee/internal/multirepo"
	"github.com/samhotchkiss/flowbee/internal/project"
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
		MirrorPath: os.Getenv("FLOWBEE_MIRROR_PATH"),
		// F3: cross-box, credential-less `bundle` provisioning when
		// FLOWBEE_BUNDLE_PROVISIONING is set. Workers then hold NO GitHub credential
		// and NO mirror path — they fetch a read-only bundle, return a diff, and
		// Flowbee performs every git write (apply + push + PR-open, R4/§8).
		BundleProvisioning: os.Getenv("FLOWBEE_BUNDLE_PROVISIONING") != "",
		Authenticator:      authn,
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

	// reconcile-IN + project-OUT (M6/M7, §8.1/§8.2): Flowbee is the SINGLE GitHub
	// caller (R4). F9 multi-repo: ONE control plane runs a per-repo reconcile-IN +
	// project-OUT loop over the repos registry, sharing a GLOBAL scheduler + fleet.
	// The registry is seeded from cfg.Repos (a structured list) or, for backward
	// compatibility, the single-repo FLOWBEE_GITHUB_OWNER/REPO env path. There are no
	// creds in dev/CI, so this stays dormant when nothing is configured.
	if mgr := wireMultiRepo(ctx, logger, cfg, st, srv); mgr != nil {
		// boot sweep + periodic floor sweep PER repo (every 2-5 min; default 3 min),
		// plus a periodic project-OUT drain PER repo.
		go func() {
			if _, err := mgr.SweepAll(ctx); err != nil {
				logger.Error("boot reconcile sweep", "err", err)
			}
			t := time.NewTicker(3 * time.Minute)
			defer t.Stop()
			drain := time.NewTicker(5 * time.Second)
			defer drain.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-t.C:
					if _, err := mgr.SweepAll(ctx); err != nil {
						logger.Error("reconcile sweep", "err", err)
					}
				case <-drain.C:
					if _, err := mgr.DrainAll(ctx); err != nil {
						logger.Error("project-out drain", "err", err)
					}
				}
			}
		}()
		// the PUBLIC webhook listener (I-2): only started when a secret is set. Hints
		// are routed to the right repo's reconciler via the X-Flowbee-Repo header
		// (default: the sole/first managed repo when unset).
		if secret := os.Getenv("FLOWBEE_WEBHOOK_SECRET"); secret != "" {
			wh := webhook.New(secret, st, repoRefetcher{mgr: mgr, defaultRepo: firstRepo(mgr)})
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
		logger.Info("multi-repo control plane wired", "repos", mgr.Repos())
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

// wireMultiRepo seeds the F9 repos registry from config (or the legacy single-repo
// env) and builds the per-repo reconcile/project Manager. Returns nil when no repo
// is configured (dev/CI with no creds), so the control plane runs without GitHub.
func wireMultiRepo(ctx context.Context, logger *slog.Logger, cfg config.Config, st *store.Store, srv *api.Server) *multirepo.Manager {
	// (1) seed the registry. cfg.Repos (structured list) wins; else fall back to the
	// single-repo FLOWBEE_GITHUB_OWNER/REPO env path (registered under id "default").
	repos := cfg.Repos
	tokenEnv := map[string]string{} // repo id -> token env-var name ("" = shared default)
	if len(repos) == 0 {
		owner, repo := os.Getenv("FLOWBEE_GITHUB_OWNER"), os.Getenv("FLOWBEE_GITHUB_REPO")
		if owner == "" || repo == "" {
			return nil // nothing configured: no GitHub loops
		}
		repos = []config.RepoConfig{{ID: "default", Owner: owner, Repo: repo, DefaultBranch: "main"}}
	}
	for _, rc := range repos {
		id := rc.ID
		if id == "" {
			id = rc.Repo
		}
		if err := st.RegisterRepo(ctx, store.Repo{
			ID: id, Owner: rc.Owner, Repo: rc.Repo,
			DefaultBranch: rc.DefaultBranch, Active: rc.IsActive(),
		}); err != nil {
			logger.Error("register repo", "id", id, "err", err)
			continue
		}
		tokenEnv[id] = rc.TokenEnv
	}

	// (2) per-repo GitHub factory: each repo gets its OWN RealClient bearing its
	// per-repo PAT (or the shared FLOWBEE_GITHUB_TOKEN). Workers hold NO creds (F3).
	sharedTok := os.Getenv("FLOWBEE_GITHUB_TOKEN")
	factory := func(r store.Repo) (github.Client, github.Writer, error) {
		tok := sharedTok
		if env := tokenEnv[r.ID]; env != "" {
			if v := os.Getenv(env); v != "" {
				tok = v
			}
		}
		client := github.NewRealClient(r.Owner, r.Repo, func(context.Context) (string, error) { return tok, nil })
		return client, client, nil
	}
	// (3) F11 (build-list §F): wire the LOCAL-git history writer so each repo's
	// project-OUT loop lands the dedicated post-merge issue-archive commit
	// (docs/history/<id>.md + the TOC) on the integration branch. The shared bare
	// mirror is the same one workers' worktrees/bundles come off; an unset mirror
	// path leaves history.write rows as audited no-ops (the ledger stays canonical).
	var historyOpt []multirepo.Option
	if mp := os.Getenv("FLOWBEE_MIRROR_PATH"); mp != "" {
		historyOpt = append(historyOpt, multirepo.WithHistory(func(store.Repo) project.HistoryWriter {
			return gitops.Open(mp)
		}))
	}
	mgr, err := multirepo.New(ctx, st, clock.Real{}, srv.Broker(), factory, historyOpt...)
	if err != nil {
		logger.Error("build multi-repo manager", "err", err)
		return nil
	}
	if len(mgr.Repos()) == 0 {
		return nil
	}
	return mgr
}

// repoRefetcher adapts the multi-repo Manager to the webhook.Refetcher interface:
// a hint is routed to the default repo's reconciler (a richer X-Flowbee-Repo router
// is a later enhancement; the floor sweep reconciles every repo regardless).
type repoRefetcher struct {
	mgr         *multirepo.Manager
	defaultRepo string
}

func (r repoRefetcher) RefetchHint(ctx context.Context, prNumber int) bool {
	return r.mgr.RefetchHint(ctx, r.defaultRepo, prNumber)
}

func firstRepo(mgr *multirepo.Manager) string {
	if rs := mgr.Repos(); len(rs) > 0 {
		return rs[0]
	}
	return ""
}

func serveHTTP(logger *slog.Logger, name string, srv *http.Server) {
	logger.Info("listening", "server", name, "addr", srv.Addr)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		logger.Error("http server", "server", name, "err", err)
	}
}
