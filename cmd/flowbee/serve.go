package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
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

	// self-provision the control-plane mirror from the GitHub token if it's
	// configured but absent — so the operator need not pre-clone it, and a PRIVATE
	// repo works (a plain `git clone` would fail for lack of credentials).
	ensureControlMirror(logger)

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
	} else if !isLoopbackAddr(cfg.PrivateAddr) {
		// no auth + a non-loopback bind = the worker API is reachable by any host on
		// the network with NO token. With autonomous merge on, that means a peer could
		// register a worker, claim a lease, and push a diff that auto-merges to main.
		// Refuse to start unless the operator explicitly accepts that (a trusted
		// tailnet), so "open" is always a conscious choice, never a silent default.
		if os.Getenv("FLOWBEE_INSECURE") == "" {
			return fmt.Errorf("REFUSING TO START: worker API binds %s (non-loopback) with NO auth — any host that can reach it could inject code"+
				"%s set FLOWBEE_WORKER_AUTH_SECRET + FLOWBEE_ENROLLED_IDENTITIES (run `flowbee token --identity X` per worker),"+
				"%s or, on a trusted private tailnet, set FLOWBEE_INSECURE=1 to accept an open API", cfg.PrivateAddr, "\n   ", "\n   ")
		}
		logger.Warn("⚠️  worker API is OPEN (no auth) on a non-loopback bind — relying on the network (e.g. Tailscale) as the only trust boundary",
			"addr", cfg.PrivateAddr, "self_merge", cfg.AllowSelfMerge)
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
		// build-list §7.3: the credential-bearing GitHub remote the control plane
		// publishes a build commit to (as a branch) so a PR can open after a build
		// result. Built from the single-repo GitHub creds; empty disables auto PR-open.
		PushRemoteURL: githubPushURL(),
		// FLOWBEE_GIT_REMOTE=ssh makes the lease ship SSH repo URLs to workers
		// (git@github.com:owner/repo.git) — for fleets whose boxes auth with SSH keys
		// (no HTTPS credential helper / no token at rest). Default HTTPS.
		WorkerGitSSH: strings.EqualFold(os.Getenv("FLOWBEE_GIT_REMOTE"), "ssh"),
	}, buildVersion())
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

	// base_sha refresh + rebase-before-review, PER REPO (F9): each managed repo's bare
	// mirror tracks its OWN integration branch, so a build is cut from / rebased onto
	// the correct repo's tip — never another repo's tree. Queries the registry at tick
	// time (repos are registered at startup by wireMultiRepo). Single-repo "default"
	// keeps using FLOWBEE_MIRROR_PATH directly (controlMirrorFor), so this is backward
	// compatible. Cheap public fetch + a rebase pass every 45s.
	if os.Getenv("FLOWBEE_MIRROR_PATH") != "" {
		go func() {
			t := time.NewTicker(45 * time.Second)
			defer t.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-t.C:
					repos, err := st.ListRepos(ctx, true)
					if err != nil {
						continue
					}
					for _, r := range repos {
						mp := controlMirrorFor(r)
						if mp == "" {
							continue
						}
						url := repoTokenURL(r)
						ensureRepoMirror(logger, mp, url)
						branch := r.DefaultBranch
						if branch == "" {
							branch = "main"
						}
						if err := gitops.Open(mp).FetchBranch(branch); err != nil {
							logger.Warn("mirror refresh", "repo", r.ID, "branch", branch, "err", err)
							continue
						}
						rebaseStaleReviews(ctx, logger, st, r.ID, mp, url, branch)
					}
				}
			}
		}()
	}
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

	// fleet-health watchdog: a `ready` job with NO live worker sits silently (the
	// scheduler has nothing to assign, no alarm fires for a build stage). Warn loudly
	// + repeatedly so a down/disconnected fleet is impossible to miss — the operator
	// sees it in the log even without looking at the dashboard.
	staleHB := 3 * cfg.HeartbeatInterval()
	if staleHB <= 0 {
		staleHB = 90 * time.Second
	}
	go func() {
		t := time.NewTicker(60 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if h, err := st.FleetHealth(ctx, time.Now(), staleHB); err == nil && h.Stranded() {
					logger.Warn("⚠️  jobs WAITING but NO live worker — is the fleet up? start `flowbee fleet` on a box",
						"waiting_jobs", h.WaitingJobs, "stale_workers", h.StaleWorkers, "live_workers", h.LiveWorkers)
				}
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
		// reconcile cadence drives how fast Flowbee REACTS to GitHub facts — chiefly
		// CI going green (the reviewer can't mint until it's reconciled) and a PR
		// merging. A slow sweep means a green build sits unreviewed until the next tick,
		// so this is the dominant pipeline latency. Default 45s (public repos have ample
		// rate limit — one GraphQL board read per repo per tick); tune via
		// FLOWBEE_RECONCILE_INTERVAL_S. Webhooks make it event-driven when wired.
		reconcileEvery := 45 * time.Second
		if v := os.Getenv("FLOWBEE_RECONCILE_INTERVAL_S"); v != "" {
			if n, perr := strconv.Atoi(v); perr == nil && n > 0 {
				reconcileEvery = time.Duration(n) * time.Second
			}
		}
		logger.Info("reconcile cadence", "interval", reconcileEvery.String())
		go func() {
			if _, err := mgr.SweepAll(ctx); err != nil {
				logger.Error("boot reconcile sweep", "err", err)
			}
			t := time.NewTicker(reconcileEvery)
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
		"version", buildVersion(), "health", cfg.HealthAddr, "private", cfg.PrivateAddr)

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
// isLoopbackAddr reports whether a listen address is loopback-only (so an open,
// no-auth worker API is not exposed to the network). The default ":7070" binds ALL
// interfaces and is therefore NOT loopback-only.
func isLoopbackAddr(addr string) bool {
	host := addr
	if i := strings.LastIndex(addr, ":"); i >= 0 {
		host = addr[:i]
	}
	switch host {
	case "127.0.0.1", "::1", "localhost":
		return true
	default:
		return false
	}
}

// ensureControlMirror clones the control-plane bare mirror from the GitHub token
// when FLOWBEE_MIRROR_PATH is set but the directory is absent. The token is baked
// into the mirror's origin so the periodic fetch and branch pushes work against a
// private repo; the mirror lives on the control plane, which holds the token anyway.
func ensureControlMirror(logger *slog.Logger) {
	mp := os.Getenv("FLOWBEE_MIRROR_PATH")
	if mp == "" {
		return
	}
	if _, err := os.Stat(mp); err == nil {
		return // already provisioned
	}
	url := githubPushURL()
	if url == "" {
		logger.Warn("FLOWBEE_MIRROR_PATH set but absent and no FLOWBEE_GITHUB_OWNER/REPO/TOKEN to clone it", "path", mp)
		return
	}
	logger.Info("provisioning control-plane mirror from GitHub", "path", mp)
	if out, err := exec.Command("git", "clone", "--bare", "--quiet", url, mp).CombinedOutput(); err != nil {
		logger.Error("clone control-plane mirror", "err", err, "out", strings.TrimSpace(string(out)))
		return
	}
	// the mirror's origin URL embeds the token (needed for private-repo fetch/push);
	// lock the mirror down so that credential isn't world/group-readable on the box.
	_ = os.Chmod(mp, 0o700)
	_ = os.Chmod(filepath.Join(mp, "config"), 0o600)
}

// githubPushURL builds the credential-bearing https remote the control plane
// pushes build branches to (so a PR can open), from the single-repo GitHub env.
// Empty when any of owner/repo/token is unset — auto PR-open is then disabled.
func githubPushURL() string {
	owner, repo, tok := os.Getenv("FLOWBEE_GITHUB_OWNER"), os.Getenv("FLOWBEE_GITHUB_REPO"), os.Getenv("FLOWBEE_GITHUB_TOKEN")
	if owner == "" || repo == "" || tok == "" {
		return ""
	}
	return "https://x-access-token:" + tok + "@github.com/" + owner + "/" + repo + ".git"
}

// rebaseStaleReviews replays every review_pending build that is behind the current
// integration tip onto it, BEFORE a reviewer leases it (build-list rebase-before-
// review). It reuses the F8 machinery (store.RebaseOnto): a clean rebase re-arms
// review + CI at the integrated head and the rebased commit is force-pushed to the
// issue branch so the PR + CI track it; a real conflict diverts the job to a
// conflict_resolver. Best-effort: a single job's failure never blocks the others.
func rebaseStaleReviews(ctx context.Context, logger *slog.Logger, st *store.Store, repoID, mirrorPath, pushURL, branch string) {
	mainTip, err := gitops.Open(mirrorPath).HeadSHA("refs/heads/" + branch)
	if err != nil {
		return
	}
	stale, err := st.StaleReviewBuilds(ctx, repoID, mainTip)
	if err != nil || len(stale) == 0 {
		return
	}
	mirror := gitops.Open(mirrorPath)
	for _, jobID := range stale {
		res, rerr := st.RebaseOnto(ctx, mirror, store.RebaseOntoParams{
			JobID: jobID, NewBaseSHA: mainTip, Now: time.Now(),
		})
		if rerr != nil {
			logger.Warn("rebase-before-review", "job", jobID, "err", rerr)
			continue
		}
		switch {
		case res.Clean && res.NewSHA != "" && pushURL != "":
			// force-push the rebased head to the issue branch so the PR + CI track it.
			brName := store.IssueBranch(st.ResolveIssueNum(ctx, jobID), jobID)
			if perr := mirror.PushCommit(pushURL, res.NewSHA, brName); perr != nil {
				logger.Warn("rebase-before-review push", "job", jobID, "branch", brName, "err", perr)
			} else {
				logger.Info("rebased review onto tip", "job", jobID, "branch", brName, "head", res.NewSHA[:min(7, len(res.NewSHA))])
			}
		case res.ResolverNeeded:
			logger.Info("review diverted to conflict_resolver before review", "job", jobID)
		}
	}
}

// controlMirrorFor resolves the control-plane bare mirror for a repo (F9 per-repo
// mirror). Backward-compatible: the legacy single-repo "default" keeps using
// FLOWBEE_MIRROR_PATH directly; additional repos get sibling mirrors <dir>/<id>.git,
// so a non-default repo's build NEVER resolves base_sha from another repo's tree.
// Empty when no mirror is configured.
func controlMirrorFor(r store.Repo) string {
	base := os.Getenv("FLOWBEE_MIRROR_PATH")
	if base == "" {
		return ""
	}
	if r.ID == "" || r.ID == "default" {
		return base
	}
	return filepath.Join(filepath.Dir(base), r.ID+".git")
}

// repoTokenURL builds the credential-bearing clone/push URL for a repo from the
// shared FLOWBEE_GITHUB_TOKEN (the local mirror sweep's auth; per-repo token_env is
// honored for the GitHub-API writer in wireMultiRepo). Empty if coords/token missing.
func repoTokenURL(r store.Repo) string {
	tok := os.Getenv("FLOWBEE_GITHUB_TOKEN")
	if r.Owner == "" || r.Repo == "" || tok == "" {
		return ""
	}
	return "https://x-access-token:" + tok + "@github.com/" + r.Owner + "/" + r.Repo + ".git"
}

// ensureRepoMirror clones a repo's bare mirror if absent (F9 per-repo provisioning),
// locking it down so the baked-in token isn't world/group-readable. A no-op when the
// mirror is already present or coords are missing.
func ensureRepoMirror(logger *slog.Logger, mp, url string) {
	if mp == "" || url == "" {
		return
	}
	if _, err := os.Stat(mp); err == nil {
		return // already provisioned
	}
	if out, err := exec.Command("git", "clone", "--bare", "--quiet", url, mp).CombinedOutput(); err != nil {
		logger.Error("clone repo mirror", "path", mp, "err", err, "out", strings.TrimSpace(string(out)))
		return
	}
	_ = os.Chmod(mp, 0o700)
	_ = os.Chmod(filepath.Join(mp, "config"), 0o600)
}

func wireMultiRepo(ctx context.Context, logger *slog.Logger, cfg config.Config, st *store.Store, srv *api.Server) *multirepo.Manager {
	// (1) seed the registry. cfg.Repos (structured list) wins; else fall back to the
	// single-repo FLOWBEE_GITHUB_OWNER/REPO env path (registered under id "default").
	repos := cfg.Repos
	tokenEnv := map[string]string{} // repo id -> token env-var name ("" = shared default)
	if len(repos) == 0 {
		// env wins (legacy path); else fall back to the flowbee.yaml coords
		// `flowbee init` prefills (F13).
		owner, repo := os.Getenv("FLOWBEE_GITHUB_OWNER"), os.Getenv("FLOWBEE_GITHUB_REPO")
		if owner == "" {
			owner = cfg.GithubOwner
		}
		if repo == "" {
			repo = cfg.GithubRepo
		}
		if owner == "" || repo == "" {
			return nil // nothing configured: no GitHub loops
		}
		branch := cfg.GithubDefaultBranch
		if branch == "" {
			branch = "main"
		}
		repos = []config.RepoConfig{{ID: "default", Owner: owner, Repo: repo, DefaultBranch: branch}}
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
	if os.Getenv("FLOWBEE_MIRROR_PATH") != "" {
		// F9: each repo's history archive + base_sha resolution come off ITS OWN bare
		// mirror (provisioned lazily), not one shared mirror — so a non-default repo
		// never reads another repo's tree.
		historyOpt = append(historyOpt, multirepo.WithHistory(func(r store.Repo) project.HistoryWriter {
			mp := controlMirrorFor(r)
			if mp == "" {
				return nil
			}
			ensureRepoMirror(logger, mp, repoTokenURL(r))
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
