// Package api hosts Flowbee's two HTTP servers (DESIGN §12.1): a health listener
// and the private worker API. M1 implements the full §7.2 worker surface
// (register / lease long-poll / heartbeat / result / release), the read-only SSE
// feed, and a minimal board. The handlers delegate every state decision to
// engine.Decide via internal/store — they never decide state themselves.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/samhotchkiss/flowbee/internal/auth"
	"github.com/samhotchkiss/flowbee/internal/capacity"
	"github.com/samhotchkiss/flowbee/internal/clock"
	"github.com/samhotchkiss/flowbee/internal/content"
	"github.com/samhotchkiss/flowbee/internal/engine"
	"github.com/samhotchkiss/flowbee/internal/gitops"
	"github.com/samhotchkiss/flowbee/internal/job"
	"github.com/samhotchkiss/flowbee/internal/lease"
	"github.com/samhotchkiss/flowbee/internal/liveness"
	"github.com/samhotchkiss/flowbee/internal/scheduler"
	"github.com/samhotchkiss/flowbee/internal/spec"
	"github.com/samhotchkiss/flowbee/internal/store"
	"github.com/samhotchkiss/flowbee/internal/ulid"
	"github.com/samhotchkiss/flowbee/internal/web"
	"github.com/samhotchkiss/flowbee/internal/worker"
)

type Server struct {
	store    *store.Store
	clock    clock.Clock
	minter   *ulid.Minter
	registry *worker.Registry
	broker   *Broker
	version  string

	// GitHub reconcile health (the operator's "is the control plane still talking to
	// GitHub?" signal). A sustained BoardSweep failure — almost always an EXPIRED
	// token, but also a revoked PAT / rate-limit exhaustion / connectivity loss — would
	// otherwise just log every 45s and silently stop all progress. ghLastSuccess (unix
	// sec, seeded at startup) feeds flowbee_github_last_success_age_seconds so a scraper
	// can page when it grows; ghLastErr carries the latest error for /healthz.
	ghLastSuccess atomic.Int64
	ghLastErr     atomic.Pointer[string]

	// unstickTotal counts merge_handoff PRs the #214 un-stick sweep has fast-forwarded
	// (update-branch) since startup — the observable signal that the systemic merge-rot fix
	// is doing work (a behind PR was found + brought up to date). Surfaced as the
	// flowbee_unstick_total counter; the conditional 🔀 log line is the per-event detail.
	unstickTotal atomic.Int64

	// adopter imports a pre-existing PR into a repo's review pipeline (POST /v1/adopt).
	// Wired from the multi-repo Manager via SetAdopter; nil until then (single-repo /
	// test servers have no Manager, so adopt 503s there).
	adopter PRAdopter

	facts  store.FactSource
	policy job.Policy
	// reviewersByRepo overrides policy.RequiredReviewers per repo (F5 consensus panel): a
	// repo can run an N-reviewer panel while others stay single-reviewer. Empty => the global
	// policy applies everywhere. Set via SetRequiredReviewers from the per-repo config.
	reviewersByRepo map[string]int

	leaseTTL     time.Duration
	longPollWait time.Duration
	pollInterval time.Duration
	// liveness is the Rung-3 deadline config (soft phase budget + absolute cap). Armed
	// on every successful claim so the §10.2 soft-deadline ladder + the durable deadline
	// timers actually run in production. Zero-value (AbsoluteCap==0) = liveness off, so a
	// test server that never calls SetLiveness is unaffected (no premature soft deadline).
	liveness store.LivenessConfig

	// mirrorPath is the shared bare-repo mirror path handed to same-box workers
	// for `worktree` provisioning (§7.4). Empty when no local mirror is configured
	// (the worker then has no repo to provision and must supply its own).
	mirrorPath string
	// bundleProvisioning selects the CROSS-BOX, credential-less `bundle` mode (F3,
	// §7.4 mode (a)): the lease advertises `bundle`, the worker fetches a git bundle
	// of base_sha from GET /v1/bundle (read-only data, NO creds), returns only a
	// diff, and the CONTROL PLANE applies the patch + pushes the epoch ref + opens
	// the PR. When false, build leases default to same-box `worktree`.
	bundleProvisioning bool
	// pushRemoteURL is the credential-bearing GitHub remote (https with the token
	// baked in) the control plane publishes an eng_worker's build commit to as a
	// branch, so a PR can be opened (build-list F3/§7.3). Empty = no auto PR-open
	// (worker pushed only a local epoch ref). Single-repo for now; multi-repo routes
	// per job repo later.
	pushRemoteURL string
	workerGitSSH  bool
	// staleHB is the roster's stale-heartbeat threshold (§12.6.2).
	staleHB time.Duration
	// pauseMarkerPath is the filesystem path whose EXISTENCE signals a fleet
	// pause. When present, the lease endpoint returns no-work (204) without
	// attempting a claim — in-flight leases/heartbeats/results are unaffected.
	pauseMarkerPath string
	// reviewAccounts, when non-empty, is the allowlist of agent logins (FLOWBEE_ACCOUNT
	// values) permitted to claim code_review work — the operator "route all reviews to a
	// chosen (low-usage) account" lever (env FLOWBEE_REVIEW_ACCOUNTS, comma-separated).
	// Every reviewer NOT on the list is withheld review work (204) so reviews concentrate
	// on the pinned account(s); build / spec / conflict roles are unaffected. Empty
	// (default) = no restriction, every reviewer claims normally.
	reviewAccounts map[string]bool
	// buildAccounts is the symmetric allowlist for BUILD (eng_worker) leases — env
	// FLOWBEE_BUILD_ACCOUNTS. When set, only those agent logins may claim build work, so
	// builds concentrate on a chosen account (e.g. the codex login with headroom) and the
	// other builders sit idle for builds — the operator "run builds through codex, not
	// claude" lever. Review / spec / conflict roles are unaffected. Empty = no restriction.
	buildAccounts map[string]bool
	// resolverAccounts is the per-role pin for conflict_resolver (env
	// FLOWBEE_RESOLVER_ACCOUNTS): only these logins claim conflict-resolution work. Codex
	// stalls on the heavy 3-way-merge task, so this lets the operator route conflicts to a
	// claude login that has headroom while builds/reviews stay on codex. Empty = off.
	resolverAccounts map[string]bool
	// dispatchAccounts is the GLOBAL allowlist (env FLOWBEE_DISPATCH_ACCOUNTS): when set,
	// ONLY these logins get ANY work, of ANY role — the operator "park an entire agent"
	// lever (e.g. a maxed claude: pin every role to codex, claude idle until it recovers).
	// Broader than the per-role build/review pins; checked first. Empty = no restriction.
	dispatchAccounts map[string]bool
	// authn is the worker-transport authenticator (§7.6). Nil = loopback-only dev
	// (no mutual auth); set for a non-loopback listener (bearer token / mTLS).
	authn auth.Authenticator
	// ui is the F12 web UI (internal/web): the productionized Fleet/Board/Dashboard
	// panes served off the same live store read-models, embedded via go:embed.
	ui *web.UI
	// runningConfig is a redacted snapshot of the effective serve launch/config,
	// exposed read-only for operators who need to reproduce or audit the running
	// process without `ps eww` archaeology.
	runningConfig RunningConfig
}

// RunningConfig is the control plane's redacted effective runtime snapshot. It is
// deliberately limited to non-secret values and boolean "present" bits for secrets.
type RunningConfig struct {
	Version               string              `json:"version"`
	PID                   int                 `json:"pid"`
	SourceCommit          string              `json:"source_commit,omitempty"`
	TreeDirty             bool                `json:"tree_dirty"`
	TreeDirtyKnown        bool                `json:"tree_dirty_known"`
	OriginMainSHA         string              `json:"origin_main_sha,omitempty"`
	BehindOriginMainBy    int                 `json:"behind_origin_main_by,omitempty"`
	BehindOriginMainKnown bool                `json:"behind_origin_main_known"`
	SourceWarning         string              `json:"source_warning,omitempty"`
	ConfigPath            string              `json:"config_path,omitempty"`
	DatabaseURL           string              `json:"database_url"`
	PrivateAddr           string              `json:"private_addr"`
	HealthAddr            string              `json:"health_addr"`
	WebhookAddr           string              `json:"webhook_addr"`
	AllowSelfMerge        bool                `json:"allow_self_merge"`
	RequiredReviewers     int                 `json:"required_reviewers"`
	MirrorPath            string              `json:"mirror_path,omitempty"`
	GitRemote             string              `json:"git_remote,omitempty"`
	WorkerGitSSH          bool                `json:"worker_git_ssh"`
	BundleProvisioning    bool                `json:"bundle_provisioning"`
	GitHubTokenPresent    bool                `json:"github_token_present"`
	WebhookSecretPresent  bool                `json:"webhook_secret_present"`
	WorkerAuthConfigured  bool                `json:"worker_auth_configured"`
	InsecureWorkerAPI     bool                `json:"insecure_worker_api"`
	AuthLoopbackBypass    bool                `json:"auth_loopback_bypass"`
	Repos                 []RunningConfigRepo `json:"repos,omitempty"`
	LogPath               string              `json:"log_path,omitempty"`
	BackupDir             string              `json:"backup_dir,omitempty"`
	ReconcileIntervalEnv  string              `json:"reconcile_interval_s,omitempty"`
	UnstickIntervalEnv    string              `json:"unstick_interval_s,omitempty"`
	FlowbeeURL            string              `json:"flowbee_url,omitempty"`
}

type RunningConfigRepo struct {
	ID                string `json:"id"`
	Owner             string `json:"owner"`
	Repo              string `json:"repo"`
	DefaultBranch     string `json:"default_branch,omitempty"`
	Active            bool   `json:"active"`
	TokenEnv          string `json:"token_env,omitempty"`
	TokenPresent      bool   `json:"token_present"`
	ArchiveHistory    bool   `json:"archive_history"`
	RequiredReviewers int    `json:"required_reviewers,omitempty"`
}

// Config carries the timing knobs the worker API needs.
type Config struct {
	LeaseTTL           time.Duration
	HeartbeatInterval  time.Duration
	LongPollWait       time.Duration
	LeaseTTLS          int
	HeartbeatIntervalS int
	// Policy is THE ONE DECISION surface (§14): AllowSelfMerge default false =
	// Branch A (every approval -> handoff -> human).
	Policy job.Policy
	// RepoReviewers overrides Policy.RequiredReviewers (F5 consensus panel) per repo: a repo
	// can run an N-reviewer panel while others stay single-reviewer. Empty => Policy applies
	// everywhere.
	RepoReviewers map[string]int
	// Facts is the reconciled-fact source the I-9 gate consumes. If nil it
	// defaults to the DB-backed source (domain_b_facts), which reconcile-IN writes
	// (M3 tests seed it directly via store.UpsertDomainBFacts).
	Facts store.FactSource
	// MirrorPath is the shared bare-repo mirror handed to same-box `worktree`
	// workers (§7.4). Empty disables local provisioning hints. It is ALSO the source
	// the server bundles from for cross-box `bundle` provisioning (F3).
	MirrorPath string
	// BundleProvisioning selects the cross-box, credential-less `bundle` mode (F3,
	// §7.4 mode (a)) for build leases instead of same-box `worktree`. The worker
	// receives no GitHub credential and no local mirror path: it fetches a read-only
	// git bundle, returns a diff, and Flowbee does ALL git writes (apply + push +
	// PR-open). Requires MirrorPath (the bundle source). Default false (worktree).
	BundleProvisioning bool
	// Allowlist is the enrolled-identity attestation policy (§9.4.1). The zero
	// value attests no role/family/tool; tests/dev use worker.OpenAllowlist().
	Allowlist worker.Allowlist
	// StaleHBThreshold badges a worker stale on the roster after this idle gap
	// (§12.6.2). Defaults to 3× the heartbeat interval.
	StaleHBThreshold time.Duration
	// Authenticator is the worker-transport trust boundary (§7.6): every private
	// worker-API call is authenticated against the enrolled-identity allowlist
	// before it can lease job context. Nil = loopback-only dev (no mutual auth);
	// a non-loopback listener MUST set it (bearer token or mTLS). The bound,
	// unforgeable identity it returns overrides any self-asserted query param.
	Authenticator auth.Authenticator
	// ContentPolicy is the operator-configured content-integrity posture (F2): the
	// size ceilings + an EXTRA denylist that AUGMENT the shipped protected set the
	// content gate (§9.2, I-11) runs over a worker's untrusted diff. The zero value
	// is exactly the shipped defaults. New() installs it on the store.
	ContentPolicy content.Policy
	// PushRemoteURL is the credential-bearing GitHub remote (https with the token)
	// the control plane publishes a build commit to as a branch so a PR can open
	// (build-list F3). Empty disables auto PR-open after a build result.
	PushRemoteURL string
	// WorkerGitSSH makes the per-job repo URL the lease ships to workers an SSH
	// remote (git@github.com:owner/repo.git) instead of HTTPS — for fleets whose
	// boxes authenticate to GitHub with SSH keys (no HTTPS credential helper / no
	// token at rest). Default false (HTTPS).
	WorkerGitSSH bool
	// PauseMarkerPath is the filesystem path whose EXISTENCE pauses leasing.
	// `flowbee pause` creates it; `flowbee resume` removes it. Empty disables
	// the check (dev/test with no DB-backed file path).
	PauseMarkerPath string
	RunningConfig   RunningConfig
}

func New(st *store.Store, clk clock.Clock, minter *ulid.Minter, cfg Config, version string) *Server {
	poll := 25 * time.Millisecond
	facts := cfg.Facts
	if facts == nil {
		facts = store.DBFactSource{DB: st.DB}
	}
	allow := cfg.Allowlist
	if !allow.Open && allow.Permit == nil {
		// unconfigured: default to the permissive dev/test allowlist (every
		// role/family/tool claim attested; arch/os still gated by the handshake).
		// Production sets an explicit strict allowlist via Config.Allowlist.
		allow = worker.OpenAllowlist()
	}
	staleHB := cfg.StaleHBThreshold
	if staleHB == 0 {
		hb := cfg.HeartbeatInterval
		if hb == 0 {
			hb = 30 * time.Second
		}
		staleHB = 3 * hb
	}
	// F2: install the operator content-integrity Policy on the store so the gate
	// (ReviewResult / DispatchMerge) runs the configured ceilings + extra denylist.
	st.ContentPolicy = cfg.ContentPolicy
	ui := web.New(st, clk, web.Config{StaleHB: staleHB})
	runningConfig := cfg.RunningConfig
	runningConfig.Version = version
	if runningConfig.PID == 0 {
		runningConfig.PID = os.Getpid()
	}
	srv := &Server{
		store:              st,
		clock:              clk,
		reviewersByRepo:    cfg.RepoReviewers,
		minter:             minter,
		registry:           worker.NewRegistry(st, cfg.LeaseTTLS, cfg.HeartbeatIntervalS, allow),
		broker:             NewBroker(),
		version:            version,
		facts:              facts,
		policy:             cfg.Policy,
		leaseTTL:           cfg.LeaseTTL,
		longPollWait:       cfg.LongPollWait,
		pollInterval:       poll,
		mirrorPath:         cfg.MirrorPath,
		bundleProvisioning: cfg.BundleProvisioning,
		pushRemoteURL:      cfg.PushRemoteURL,
		workerGitSSH:       cfg.WorkerGitSSH,
		staleHB:            staleHB,
		authn:              cfg.Authenticator,
		ui:                 ui,
		pauseMarkerPath:    cfg.PauseMarkerPath,
		reviewAccounts:     parseReviewAccounts(os.Getenv("FLOWBEE_REVIEW_ACCOUNTS")),
		buildAccounts:      parseReviewAccounts(os.Getenv("FLOWBEE_BUILD_ACCOUNTS")),
		resolverAccounts:   parseReviewAccounts(os.Getenv("FLOWBEE_RESOLVER_ACCOUNTS")),
		dispatchAccounts:   parseReviewAccounts(os.Getenv("FLOWBEE_DISPATCH_ACCOUNTS")),
		runningConfig:      runningConfig,
	}
	// seed GitHub health to "just succeeded" so the age metric starts ~0, not at the
	// unix epoch, before the first sweep runs.
	srv.ghLastSuccess.Store(clk.Now().Unix())
	return srv
}

// RecordGitHubSweep records the outcome of a reconcile BoardSweep so the operator can
// see a sustained GitHub failure (an expired/revoked token, exhausted rate limit, or
// connectivity loss) instead of it silently logging every 45s. On success it advances
// the last-success watermark; on failure it leaves the watermark (so the age grows)
// and stores the error for /healthz. The runtime calls this after every sweep.
func (s *Server) RecordGitHubSweep(err error) {
	if err == nil {
		s.ghLastSuccess.Store(s.clock.Now().Unix())
		s.ghLastErr.Store(nil)
		return
	}
	e := err.Error()
	s.ghLastErr.Store(&e)
}

// AddUnstick records that the #214 un-stick sweep fast-forwarded n behind merge_handoff PRs
// this pass (feeds flowbee_unstick_total). The runtime calls it after each UnstickAll.
func (s *Server) AddUnstick(n int) {
	if n > 0 {
		s.unstickTotal.Add(int64(n))
	}
}

// Broker exposes the SSE broker so the runtime can publish lifecycle events.
func (s *Server) Broker() *Broker { return s.broker }

// SetLiveness wires the Rung-3 deadline config so each successful claim arms the soft
// phase-budget + absolute-cap deadlines (the §10.2 ladder + durable timers). Called once
// at serve startup with the same config the alarm poller evaluates against.
func (s *Server) SetLiveness(cfg store.LivenessConfig) { s.liveness = cfg }

// PRAdopter imports a pre-existing PR (one Flowbee did not originate) into a repo's
// review pipeline. Satisfied by the multi-repo Manager (which owns the GitHub loops);
// nil when no repos are wired (single-repo/legacy or a test server), in which case
// POST /v1/adopt returns 503.
type PRAdopter interface {
	AdoptPR(ctx context.Context, repoID string, prNumber int) (string, bool, error)
}

// SetAdopter wires the PR-adoption backend (the multi-repo Manager). Called after
// wireMultiRepo builds the Manager, so adopt is only live once GitHub loops exist.
func (s *Server) SetAdopter(a PRAdopter) { s.adopter = a }

// HealthHandler serves the health listener.
func (s *Server) HealthHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.healthz)
	mux.HandleFunc("GET /metrics", s.metrics)
	return mux
}

// PrivateHandler serves the worker API + SSE + dashboard (loopback / Tailscale
// only). When an Authenticator is configured (§7.6), the mutating worker-protocol
// routes are wrapped in mutual-auth middleware: an unenrolled caller is rejected
// 401 and never reaches a handler — it cannot lease job context. The read-only
// dashboard/SSE views are served without auth (they expose no Domain-A write and
// bind to loopback/Tailscale).
func (s *Server) PrivateHandler() http.Handler {
	// the authenticated worker-protocol surface (every mutating call + lease).
	worker := http.NewServeMux()
	worker.HandleFunc("POST /v1/workers/register", s.register)
	worker.HandleFunc("POST /v1/workers/usage", s.usage)
	// dispatch control: a client (the russ worker, an operator) tells the dispatcher to
	// pause — globally ("pause everything") or for one repo ({"repo":"russ"}).
	worker.HandleFunc("POST /v1/control/pause", s.controlPause)
	worker.HandleFunc("POST /v1/control/resume", s.controlResume)
	worker.HandleFunc("GET /v1/control", s.controlStatus)
	worker.HandleFunc("GET /v1/lease", s.lease)
	worker.HandleFunc("POST /v1/jobs/{job}/heartbeat", s.heartbeat)
	worker.HandleFunc("POST /v1/jobs/{job}/result", s.result)
	worker.HandleFunc("POST /v1/jobs/{job}/review", s.review)
	worker.HandleFunc("POST /v1/jobs/{job}/spec", s.specSubmit)
	worker.HandleFunc("POST /v1/jobs/{job}/spec-review", s.specReview)
	worker.HandleFunc("POST /v1/jobs/{job}/release", s.release)
	worker.HandleFunc("POST /v1/jobs/{job}/rebase-conflict", s.rebaseConflict)
	worker.HandleFunc("GET /v1/jobs/{job}/bundle", s.bundle)
	worker.HandleFunc("GET /v1/config", s.configJSON)
	worker.HandleFunc("GET /configz", s.configJSON)
	authed := auth.Middleware(s.authn, worker)

	mux := http.NewServeMux()
	// worker-protocol routes go through the authenticated surface.
	for _, p := range []string{
		"POST /v1/workers/register", "POST /v1/workers/usage", "GET /v1/lease",
		"POST /v1/jobs/{job}/heartbeat", "POST /v1/jobs/{job}/result",
		"POST /v1/jobs/{job}/review", "POST /v1/jobs/{job}/spec",
		"POST /v1/jobs/{job}/spec-review", "POST /v1/jobs/{job}/release",
		"POST /v1/jobs/{job}/rebase-conflict",
		"GET /v1/jobs/{job}/bundle",
		"POST /v1/control/pause", "POST /v1/control/resume", "GET /v1/control",
		"GET /v1/config", "GET /configz",
	} {
		mux.Handle(p, authed)
	}
	// read-only dashboard + live feed (board / roster / budget / audit / cost, SSE).
	mux.HandleFunc("GET /v1/events", s.eventsHandler)
	mux.HandleFunc("GET /v1/budget", s.budgetJSON)
	mux.HandleFunc("GET /v1/roster", s.rosterJSON)
	mux.HandleFunc("GET /v1/fleet-health", s.fleetHealthJSON)
	mux.HandleFunc("GET /v1/audit", s.auditJSON)
	mux.HandleFunc("GET /v1/cost", s.costJSON)
	mux.HandleFunc("GET /v1/needs-human", s.needsHumanJSON)
	mux.HandleFunc("GET /v1/merge-handoff", s.mergeHandoffJSON)
	mux.HandleFunc("GET /v1/needs-input", s.needsInputJSON)
	mux.HandleFunc("GET /v1/backlog", s.backlogJSON)
	mux.HandleFunc("GET /v1/fleet", s.fleetJSON)
	// F7 board-lifecycle WRITE / intake edges (operator / user-agent / planner loop):
	// answer a needs_design item, promote a backlog item, opt a quiescent item in, retry
	// a needs_human job, cancel, or inject work via the spec/epic front door. These MUTATE
	// state / inject work, so they carry the SAME auth posture as the worker protocol —
	// loopback-bypass when enabled, a valid token required off-loopback — so a non-loopback
	// caller can't cancel/requeue a job or inject specs without a credential under the
	// secure posture. (They previously sat on the bare mux, unauthenticated even with
	// worker_auth_secret set; the read-only dashboard endpoints above stay open by design.)
	// auth.Middleware(nil, h) == h, so this is a no-op in the loopback-only dev default.
	op := func(h http.HandlerFunc) http.Handler { return auth.Middleware(s.authn, h) }
	mux.Handle("POST /v1/jobs/{job}/design", op(s.resolveDesign))
	mux.Handle("POST /v1/jobs/{job}/promote", op(s.promoteBacklog))
	mux.Handle("POST /v1/jobs/{job}/adopt", op(s.adoptOptIn))
	mux.Handle("POST /v1/jobs/{job}/requeue", op(s.requeue))
	mux.Handle("POST /v1/jobs/{job}/cancel", op(s.cancel))
	mux.Handle("POST /v1/specs", op(s.specCreate))
	mux.Handle("POST /v1/epics", op(s.epicCreate))
	mux.Handle("POST /v1/adopt", op(s.adoptPR))
	// the board's machine-readable snapshot (HTML clients hit the web UI's "/"; a
	// JSON client uses this stable endpoint instead of content-negotiating "/").
	mux.HandleFunc("GET /v1/board", s.boardJSON)
	// F12 web UI (internal/web): the rich Fleet + Board + Dashboard + Roster panes,
	// wired to the same live read-models the SSE feed refreshes. Embedded via
	// go:embed. It owns "/", "/board", "/board/detail", "/fleet", "/dashboard",
	// "/roster", and "/assets/". The read-only views bind to loopback/Tailscale.
	s.ui.Mount(mux)
	return mux
}

// configJSON exposes the RUNNING control plane's effective, redacted config. It is
// read-only and contains no secret material; token/secret fields are booleans only.
func (s *Server) configJSON(w http.ResponseWriter, r *http.Request) {
	if s.authn == nil && !requestFromLoopback(r) {
		http.Error(w, "running config is available only from loopback unless worker auth is configured", http.StatusForbidden)
		return
	}
	writeJSON(w, http.StatusOK, s.runningConfig)
}

func requestFromLoopback(r *http.Request) bool {
	host := r.RemoteAddr
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// boardJSON serves the live board snapshot as JSON (the machine-readable board the
// user-agent loop and any non-browser client consume; the HTML board is the web UI).
func (s *Server) boardJSON(w http.ResponseWriter, r *http.Request) {
	jobs, err := s.store.BoardSnapshot(r.Context())
	if err != nil {
		http.Error(w, "board error", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, jobs)
}

// budgetJSON serves the single installation token's rate-limit gauge (I-14,
// §12.6) — one bucket to watch. Driven by every reconcile sweep.
func (s *Server) budgetJSON(w http.ResponseWriter, r *http.Request) {
	g, err := s.store.RateLimit(r.Context())
	if err != nil {
		http.Error(w, "budget error", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, g)
}

// auditJSON serves the GitHub audit log (§3.3): every project-OUT action keyed
// (job_id, action, head_sha). The dashboard's audit pane reads this.
func (s *Server) auditJSON(w http.ResponseWriter, r *http.Request) {
	rows, err := s.store.AllAudit(r.Context())
	if err != nil {
		http.Error(w, "audit error", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, rows)
}

// costJSON serves the per-job cost meter (§6.7, I-15): tokens + micro-USD + over
// budget, for the dashboard cost pane.
func (s *Server) costJSON(w http.ResponseWriter, r *http.Request) {
	rows, err := s.store.AllJobCost(r.Context())
	if err != nil {
		http.Error(w, "cost error", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, rows)
}

// needsHumanJSON serves the unified needs_human chokepoint (§12.6.1): every job
// that escalated, tagged with the trigger (attempts | bounces | cost | stall).
func (s *Server) needsHumanJSON(w http.ResponseWriter, r *http.Request) {
	rows, err := s.store.NeedsHumanView(r.Context())
	if err != nil {
		http.Error(w, "needs_human error", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, rows)
}

// mergeHandoffJSON serves the merge_handoff lane: every change Flowbee approved but
// reserves for a human to merge (self-merge off, or a protected change). With
// AllowSelfMerge off this is the operator's entire merge queue — the PRs to merge.
func (s *Server) mergeHandoffJSON(w http.ResponseWriter, r *http.Request) {
	rows, err := s.store.MergeHandoffView(r.Context())
	if err != nil {
		http.Error(w, "merge_handoff error", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, rows)
}

// needsInputJSON serves the F4 needs-input surface (flow-pass §D): every job
// parked in needs_design awaiting a human DESIGN decision (a design fork issue-review
// could not resolve by amending). The user's board-check loop reads this, walks the
// human through, posts the answer, and Flowbee resumes the spec_review gate.
func (s *Server) needsInputJSON(w http.ResponseWriter, r *http.Request) {
	rows, err := s.store.NeedsInput(r.Context())
	if err != nil {
		http.Error(w, "needs_input error", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, rows)
}

// backlogJSON serves the F7 Backlog lane (flow-pass §D): every tracked-but-NOT-
// scheduled job, with its "needs full spec" flag. The user-agent's board-check
// loop reads this (+ /v1/needs-input) to decide what to promote.
func (s *Server) backlogJSON(w http.ResponseWriter, r *http.Request) {
	rows, err := s.store.Backlog(r.Context())
	if err != nil {
		http.Error(w, "backlog error", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, rows)
}

// resolveDesignRequest is the user-agent loop's answer to a needs_design item
// (F7): the human's design decision, optionally encoded as an edited spec. With
// an AmendedSpecMarkdown, Flowbee commits it (computes the BLAKE3 hash — the human
// never self-addresses the artifact) and the re-armed gate judges the new bytes;
// without it, the SAME bytes are re-reviewed (the answer rode in the chat).
type resolveDesignRequest struct {
	AmendedSpecMarkdown string `json:"amended_spec_markdown,omitempty"`
	AmendedVersion      int    `json:"amended_version,omitempty"`
}

// resolveDesign is the resume edge of the user-agent board-check loop (F7): a
// human (via their agent) supplies the design decision for a job parked in
// needs_design, and Flowbee resumes it to spec_review (a fresh issue-review judges
// the now-resolved spec). This closes the loop "post an answer -> needs_design ->
// issue_review".
func (s *Server) resolveDesign(w http.ResponseWriter, r *http.Request) {
	jobID := r.PathValue("job")
	var req resolveDesignRequest
	if r.Body != nil {
		_ = json.NewDecoder(r.Body).Decode(&req) // an empty body = re-review same bytes
	}
	var newHash string
	if req.AmendedSpecMarkdown != "" {
		newHash = spec.ContentHash([]byte(req.AmendedSpecMarkdown))
	}
	if err := s.store.ResolveDesign(r.Context(), jobID, newHash, req.AmendedVersion, s.clock.Now()); err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	s.broker.Publish(LifeEvent{JobID: jobID, State: string(job.StateSpecReview), Event: "design_resolved"})
	writeJSON(w, http.StatusOK, map[string]any{"resumed": true, "state": string(job.StateSpecReview)})
}

// promoteBacklog is the deliberate "promote when ready" edge (F7): an operator /
// user-agent releases a tracked-but-not-scheduled backlog item into its flow (a
// needs-full-spec item into spec_authoring, a ready-to-build item into ready).
// Before this the item was never leasable.
func (s *Server) promoteBacklog(w http.ResponseWriter, r *http.Request) {
	jobID := r.PathValue("job")
	to, err := s.store.PromoteBacklog(r.Context(), jobID, s.clock.Now())
	if err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	s.broker.Publish(LifeEvent{JobID: jobID, State: string(to), Event: "promoted"})
	writeJSON(w, http.StatusOK, map[string]any{"promoted": true, "state": string(to)})
}

// adoptOptIn is the deliberate opt-in edge (F7 / §12.7): an operator promotes a
// quiescent adopted item (an issue or PR) into Flowbee's control. An adopted issue
// enters a standalone single-issue flow at issue-review; an adopted PR enters
// review_pending. The flowbee:adopt label opts an issue in automatically on the
// adopt sweep; this is the manual edge.
func (s *Server) adoptOptIn(w http.ResponseWriter, r *http.Request) {
	jobID := r.PathValue("job")
	if err := s.store.OptIn(r.Context(), jobID, s.clock.Now()); err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"opted_in": true})
}

// adoptPR is the targeted import edge (`flowbee adopt <pr>`): it pulls a pre-existing
// PR — one Flowbee did not originate, e.g. an external agent-pool branch — into the
// named repo's review pipeline. Flowbee reads the PR's REAL state from GitHub, binds
// it to an opted-in adopted code_reviewer job in review_pending, and the normal
// review/merge machinery takes over (self-merge on approval + green CI, or needs_human
// on changes_requested). Idempotent: an unchanged PR Flowbee already tracks returns
// already_tracked=true with no new job. A tracked adopted PR whose head/base moved
// returns rearmed=true. 503 when no repos are wired (no adopter).
func (s *Server) adoptPR(w http.ResponseWriter, r *http.Request) {
	if s.adopter == nil {
		http.Error(w, "adoption unavailable: no repos wired (single-repo/legacy control plane)", http.StatusServiceUnavailable)
		return
	}
	var body struct {
		Repo string `json:"repo"`
		PR   int    `json:"pr"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if body.PR <= 0 {
		http.Error(w, `"pr" is required (a positive PR number)`, http.StatusBadRequest)
		return
	}
	repo, err := s.resolveIngestRepo(r.Context(), body.Repo)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	id, rearmed, err := s.adopter.AdoptPR(r.Context(), repo, body.PR)
	if err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	if rearmed {
		writeJSON(w, http.StatusOK, map[string]any{"job_id": id, "rearmed": true, "pr": body.PR, "repo": repo})
		return
	}
	if id == "" {
		writeJSON(w, http.StatusOK, map[string]any{"already_tracked": true, "pr": body.PR, "repo": repo})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"job_id": id, "pr": body.PR, "repo": repo})
}

// rosterJSON serves the worker roster as JSON (§12.6.2).
func (s *Server) rosterJSON(w http.ResponseWriter, r *http.Request) {
	roster, err := s.store.Roster(r.Context(), s.clock.Now(), s.staleHB)
	if err != nil {
		http.Error(w, "roster error", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, roster)
}

// fleetHealthJSON answers "is anyone home?": live vs stale workers + jobs waiting for
// one. `stranded: true` (work to do, zero live workers) is the down-fleet signature.
func (s *Server) fleetHealthJSON(w http.ResponseWriter, r *http.Request) {
	h, err := s.store.FleetHealth(r.Context(), s.clock.Now(), s.staleHB)
	if err != nil {
		http.Error(w, "fleet health error", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"live_workers": h.LiveWorkers, "stale_workers": h.StaleWorkers,
		"waiting_jobs": h.WaitingJobs, "stranded": h.Stranded(),
	})
}

func (s *Server) healthz(w http.ResponseWriter, r *http.Request) {
	dbOK := s.store.Ping(r.Context()) == nil
	status, code := "ok", http.StatusOK
	if !dbOK {
		status, code = "unavailable", http.StatusServiceUnavailable
	}
	resp := map[string]any{"status": status, "db": dbOK, "version": s.version,
		"github_last_success_age_seconds": s.clock.Now().Unix() - s.ghLastSuccess.Load()}
	if e := s.ghLastErr.Load(); e != nil {
		resp["github_last_error"] = *e
	}
	writeJSON(w, code, resp)
}

// metrics serves Prometheus text-format gauges on the unauthenticated health
// listener (alongside /healthz) so a scraper on the private network can alert on
// the operational signals that actually page someone: jobs wedged in needs_human,
// a dead fleet, work piling up unclaimed, cost ceilings breached. Everything here
// is a cheap aggregate read; it never touches Domain-A write paths.
func (s *Server) metrics(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var b strings.Builder

	fmt.Fprintf(&b, "# HELP flowbee_build_info Build version (always 1).\n")
	fmt.Fprintf(&b, "# TYPE flowbee_build_info gauge\n")
	fmt.Fprintf(&b, "flowbee_build_info{version=%q} 1\n", s.version)

	// GitHub reconcile health: seconds since the last successful BoardSweep. Grows
	// without bound when the control plane can't reach GitHub (expired/revoked token,
	// rate-limit exhaustion, connectivity loss) — the signal that ALL progress has
	// silently stalled. Alert on flowbee_github_last_success_age_seconds > a few min.
	fmt.Fprintf(&b, "# HELP flowbee_github_last_success_age_seconds Seconds since the last successful GitHub reconcile sweep.\n")
	fmt.Fprintf(&b, "# TYPE flowbee_github_last_success_age_seconds gauge\n")
	fmt.Fprintf(&b, "flowbee_github_last_success_age_seconds %d\n", s.clock.Now().Unix()-s.ghLastSuccess.Load())

	// DB on-disk size: the ledger (job_events) is append-only, so this grows with
	// throughput over months. Litestream-backed + SQLite handles multi-GB, but surface
	// it so an operator can watch the one unbounded table rather than be surprised.
	fmt.Fprintf(&b, "# HELP flowbee_db_size_bytes On-disk size of the SQLite database (main + WAL + SHM).\n")
	fmt.Fprintf(&b, "# TYPE flowbee_db_size_bytes gauge\n")
	fmt.Fprintf(&b, "flowbee_db_size_bytes %d\n", s.store.DBSizeBytes())

	// Jobs by repo+state: the core liveness signal. A state with no jobs emits no
	// series (Prometheus treats absent == 0), which is the correct semantics for
	// alerting on `flowbee_jobs{state="needs_human"} > 0`.
	if jobs, err := s.store.BoardSnapshot(ctx); err == nil {
		counts := map[[2]string]int{}
		for _, j := range jobs {
			counts[[2]string{j.Repo, j.State}]++
		}
		fmt.Fprintf(&b, "# HELP flowbee_jobs Jobs by repo and state.\n")
		fmt.Fprintf(&b, "# TYPE flowbee_jobs gauge\n")
		for k, n := range counts {
			fmt.Fprintf(&b, "flowbee_jobs{repo=%q,state=%q} %d\n", k[0], k[1], n)
		}

		// Pending-merge STALL AGE: the oldest job parked awaiting merge (merge_handoff =
		// Flowbee approved it but a human/policy must merge; merging = mid-merge). A COUNT
		// of these is normal (handoffs happen); a large AGE is the page — a change Flowbee
		// approved that NOBODY merged. This is the signal that was missing when a handoff sat
		// 15h+ SILENTLY: the count gauge fires on any handoff (noisy), but only the age
		// distinguishes a fresh handoff from a wedged one. updated_at is stable for a parked
		// handoff (reconcile does not touch it), so now-updated_at is the true stall age.
		// Alert on flowbee_oldest_pending_merge_age_seconds > a few hours.
		oldestMerge := map[string]time.Time{}
		for _, j := range jobs {
			if j.State != string(job.StateMergeHandoff) && j.State != string(job.StateMerging) {
				continue
			}
			if cur, ok := oldestMerge[j.Repo]; !ok || j.UpdatedAt.Before(cur) {
				oldestMerge[j.Repo] = j.UpdatedAt
			}
		}
		if len(oldestMerge) > 0 {
			now := s.clock.Now()
			fmt.Fprintf(&b, "# HELP flowbee_oldest_pending_merge_age_seconds Age of the oldest job parked awaiting merge (merge_handoff/merging), per repo.\n")
			fmt.Fprintf(&b, "# TYPE flowbee_oldest_pending_merge_age_seconds gauge\n")
			for repo, ts := range oldestMerge {
				age := int64(0)
				if !ts.IsZero() {
					if d := now.Sub(ts); d > 0 {
						age = int64(d.Seconds())
					}
				}
				fmt.Fprintf(&b, "flowbee_oldest_pending_merge_age_seconds{repo=%q} %d\n", repo, age)
			}
		}
	}

	// #214 un-stick: how many behind merge_handoff PRs the sweep has fast-forwarded since
	// startup. A rising count means the systemic merge-rot fix is doing work (PRs were
	// falling behind a moving base and got brought up to date); flat-at-0 is the steady state.
	fmt.Fprintf(&b, "# HELP flowbee_unstick_total merge_handoff PRs fast-forwarded (update-branch) by the #214 un-stick sweep since startup.\n")
	fmt.Fprintf(&b, "# TYPE flowbee_unstick_total counter\n")
	fmt.Fprintf(&b, "flowbee_unstick_total %d\n", s.unstickTotal.Load())

	// Dispatch pause state: a paused dispatcher (global) or a parked repo (per-repo) hands
	// out NO work — make it OBSERVABLE so a pause is never silently forgotten ("why has russ
	// been idle for days?"). Alert on flowbee_dispatch_paused == 1 or flowbee_repo_parked == 1
	// lasting longer than intended.
	if paused, perr := s.store.DispatchPaused(ctx); perr == nil {
		v := 0
		if paused {
			v = 1
		}
		fmt.Fprintf(&b, "# HELP flowbee_dispatch_paused 1 when global dispatch is paused (no leases issued to any worker).\n")
		fmt.Fprintf(&b, "# TYPE flowbee_dispatch_paused gauge\n")
		fmt.Fprintf(&b, "flowbee_dispatch_paused %d\n", v)
	}
	if repos, rerr := s.store.ListRepos(ctx, false); rerr == nil && len(repos) > 0 {
		fmt.Fprintf(&b, "# HELP flowbee_repo_parked 1 when a repo is parked (its jobs are withheld from leasing).\n")
		fmt.Fprintf(&b, "# TYPE flowbee_repo_parked gauge\n")
		for _, rp := range repos {
			parked := 0
			if !rp.Active {
				parked = 1
			}
			fmt.Fprintf(&b, "flowbee_repo_parked{repo=%q} %d\n", rp.ID, parked)
		}
		// Red main (green-main stop-the-line, russ #214): the integration branch's CI is
		// definitively red — feature PRs can't be fairly judged and pile up un-mergeable.
		// Alert on flowbee_main_ci_red == 1; the fix should be filed flowbee:p1 to jump the queue.
		fmt.Fprintf(&b, "# HELP flowbee_main_ci_red 1 when a repo's integration branch CI is red (stop-the-line: fix main first).\n")
		fmt.Fprintf(&b, "# TYPE flowbee_main_ci_red gauge\n")
		for _, rp := range repos {
			red := 0
			if r, _ := s.store.RepoMainCIRed(ctx, rp.ID); r {
				red = 1
			}
			fmt.Fprintf(&b, "flowbee_main_ci_red{repo=%q} %d\n", rp.ID, red)
		}
	}

	// Fleet liveness + backlog: a fleet of zero live workers with waiting jobs is
	// the "nothing is getting done" page.
	if fh, err := s.store.FleetHealth(ctx, s.clock.Now(), s.staleHB); err == nil {
		fmt.Fprintf(&b, "# HELP flowbee_fleet_workers Registered workers by liveness.\n")
		fmt.Fprintf(&b, "# TYPE flowbee_fleet_workers gauge\n")
		fmt.Fprintf(&b, "flowbee_fleet_workers{status=\"live\"} %d\n", fh.LiveWorkers)
		fmt.Fprintf(&b, "flowbee_fleet_workers{status=\"stale\"} %d\n", fh.StaleWorkers)
		fmt.Fprintf(&b, "# HELP flowbee_fleet_waiting_jobs Ready jobs with no worker yet.\n")
		fmt.Fprintf(&b, "# TYPE flowbee_fleet_waiting_jobs gauge\n")
		fmt.Fprintf(&b, "flowbee_fleet_waiting_jobs %d\n", fh.WaitingJobs)
	}

	// Cost: cumulative metered spend + how many jobs tripped their ceiling.
	if costs, err := s.store.AllJobCost(ctx); err == nil {
		var totalMicro int64
		var overBudget int
		for _, c := range costs {
			totalMicro += c.MicroUSD
			if c.OverBudget {
				overBudget++
			}
		}
		fmt.Fprintf(&b, "# HELP flowbee_cost_micro_usd_total Cumulative metered agent spend (micro-USD).\n")
		fmt.Fprintf(&b, "# TYPE flowbee_cost_micro_usd_total counter\n")
		fmt.Fprintf(&b, "flowbee_cost_micro_usd_total %d\n", totalMicro)
		fmt.Fprintf(&b, "# HELP flowbee_jobs_over_budget Jobs that breached their cost ceiling.\n")
		fmt.Fprintf(&b, "# TYPE flowbee_jobs_over_budget gauge\n")
		fmt.Fprintf(&b, "flowbee_jobs_over_budget %d\n", overBudget)
	}

	// Abandoned (dead-lettered) GitHub writes: work that never took effect. Critical ones
	// (create-issue/merge) also escalate to needs_human, but cosmetic ones (comments, the §F
	// archive) are otherwise silent — alert on any growth here.
	if ab, err := s.store.OutboxAbandonedByAction(ctx); err == nil && len(ab) > 0 {
		fmt.Fprintf(&b, "# HELP flowbee_outbox_abandoned Dead-lettered GitHub writes by action (never took effect).\n")
		fmt.Fprintf(&b, "# TYPE flowbee_outbox_abandoned gauge\n")
		for action, n := range ab {
			fmt.Fprintf(&b, "flowbee_outbox_abandoned{action=%q} %d\n", action, n)
		}
	}

	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	_, _ = w.Write([]byte(b.String()))
}

func (s *Server) register(w http.ResponseWriter, r *http.Request) {
	var reg worker.Registration
	if err := json.NewDecoder(r.Body).Decode(&reg); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if reg.WorkerID == "" {
		// Reuse the worker_id already registered under this identity so a RE-registration
		// (a worker that restarted, possibly with a changed model_family/role) UPDATES its
		// existing row instead of minting a fresh worker_id that collides with the
		// UNIQUE(identity) constraint and fails — which would freeze the worker's stored
		// capabilities at its first registration (the stale-roster bug). Mint only for a
		// genuinely new identity.
		if existing, err := s.store.WorkerIDForIdentity(r.Context(), reg.Identity); err == nil && existing != "" {
			reg.WorkerID = existing
		} else {
			reg.WorkerID = s.minter.New()
		}
	}
	resp, err := s.registry.Register(r.Context(), reg, s.clock.Now())
	if err != nil {
		http.Error(w, "register failed", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// usage folds per-account usage reports (F6, POST /v1/workers/usage): a box reports
// its accounts' usage best-effort (~15 min) or IMMEDIATELY on a 429. Usage is
// PER ACCOUNT (shared across boxes on the same login), so the report updates the
// canonical bucket the ceiling gate reads at dispatch. The response echoes which
// accounts are now at/over ceiling (a capacity-alarm surface).
func (s *Server) usage(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Reports []capacity.UsageReport `json:"reports"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	atCeiling, err := s.store.RecordUsage(r.Context(), body.Reports, s.clock.Now())
	if err != nil {
		http.Error(w, "usage error", http.StatusInternalServerError)
		return
	}
	for _, id := range atCeiling {
		s.broker.Publish(LifeEvent{JobID: "", State: "capacity", Event: "account_at_ceiling:" + id})
	}
	writeJSON(w, http.StatusOK, map[string]any{"accepted": true, "at_ceiling": atCeiling})
}

// controlPause (POST /v1/control/pause) lets a client tell the dispatcher to stop handing
// out work. Body {"repo":"<id>"} parks just that repo — its jobs drop out of the lease
// queue while every other repo keeps flowing; an empty body (no repo) pauses dispatch
// GLOBALLY ("pause everything"). Running jobs are never interrupted — only NEW leasing
// stops; heartbeats/results still flow. Idempotent + DB-backed (survives a CP redeploy).
func (s *Server) controlPause(w http.ResponseWriter, r *http.Request) { s.setPause(w, r, true) }

// controlResume (POST /v1/control/resume) is the inverse — resume global dispatch or a
// single repo (same body shape).
func (s *Server) controlResume(w http.ResponseWriter, r *http.Request) { s.setPause(w, r, false) }

func (s *Server) setPause(w http.ResponseWriter, r *http.Request, pause bool) {
	var body struct {
		Repo string `json:"repo,omitempty"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body) // body optional: no repo => global
	scope, err := "all", error(nil)
	if repo := strings.TrimSpace(body.Repo); repo != "" {
		scope = repo
		err = s.store.SetRepoActive(r.Context(), repo, !pause) // pause => active=false
	} else {
		err = s.store.SetDispatchPaused(r.Context(), pause)
		if err == nil {
			// Keep the CP-local marker file in lock-step with the DB flag so the two
			// sources of truth can't disagree. Both the lease gate (isPaused) and
			// `flowbee status` read `marker OR db-flag`; if `resume` cleared only the
			// DB flag, a marker left by any source (an old binary, a manual touch)
			// would wedge the fleet with no documented recovery (#216). This file op
			// is safe here because the API runs ON the control-plane box, so a REMOTE
			// client's resume still reaches the marker.
			err = s.syncPauseMarker(pause)
		}
	}
	if err != nil {
		http.Error(w, "control: "+err.Error(), http.StatusBadRequest)
		return
	}
	verb := "resumed"
	if pause {
		verb = "paused"
	}
	s.broker.Publish(LifeEvent{State: "control", Event: "dispatch_" + verb + ":" + scope})
	writeJSON(w, http.StatusOK, map[string]any{"paused": pause, "scope": scope})
}

// controlStatus (GET /v1/control) reports the current dispatch state: the global pause flag
// + the list of parked repos, so a client can see whether (and where) work is flowing.
func (s *Server) controlStatus(w http.ResponseWriter, r *http.Request) {
	global, err := s.store.DispatchPaused(r.Context())
	if err != nil {
		http.Error(w, "control error", http.StatusInternalServerError)
		return
	}
	repos, _ := s.store.ListRepos(r.Context(), false)
	parked := []string{}
	for _, rp := range repos {
		if !rp.Active {
			parked = append(parked, rp.ID)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"dispatch_paused": global, "parked_repos": parked})
}

// fleetJSON serves the §G fleet view source: per-account usage gauges with their
// ceiling line + whether each account is currently gated out (the rollover state).
func (s *Server) fleetJSON(w http.ResponseWriter, r *http.Request) {
	rows, err := s.store.AllAccountUsage(r.Context())
	if err != nil {
		http.Error(w, "fleet error", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, rows)
}

// LeaseGrant is the §7.2 lease envelope returned to a worker on GET /v1/lease.
type LeaseGrant struct {
	JobID      string `json:"job_id"`
	Kind       string `json:"kind"`
	Role       string `json:"role"`
	BaseSHA    string `json:"base_sha"`
	LeaseID    string `json:"lease_id"`
	LeaseEpoch int    `json:"lease_epoch"`
	LeaseTTLS  int    `json:"lease_ttl_s"`
	Deadline   string `json:"lease_deadline"`
	DryRun     bool   `json:"dry_run,omitempty"`
	// Repo provisioning (§7.4). For same-box `worktree`, MirrorPath is the shared
	// bare mirror the worker adds a per-lease worktree off; PushTarget is the
	// epoch-namespaced ref the worker pushes its build to (Flowbee promotes it).
	Provisioning string `json:"provisioning"`
	MirrorPath   string `json:"mirror_path"`
	PushTarget   string `json:"push_target"`
	// Spec-flow inputs (§11): the content hash the reviewer must bind its verdict
	// to. The spec_review lease carries it so the reviewer judges the EXACT bytes
	// (a stale binding is rejected as superseded, §11.5).
	SpecContentHash string `json:"spec_content_hash,omitempty"`
	SpecVersion     int    `json:"spec_version,omitempty"`
	// Context is the F1 self-contained context block (§B): the resolved identity
	// (who-to-be) + task/spec/acceptance + base_sha + prior verdicts that make the
	// lease JSON self-contained. An untrusted worker reads it; it can never choose
	// its own identity or task (both are resolved by Flowbee and fenced here).
	Context *LeaseContext `json:"context,omitempty"`
}

// LeaseContext is the F1 resolved context block carried in a LeaseGrant (§B
// "self-contained lease JSON" = resolved identity + context). Every field is a
// RESOLVED fact: the worker does not negotiate any of it. The Mode-A harness
// writes Task/Spec/Acceptance into the worktree (.flowbee/task.md) and exports
// them as env so any agent CLI reads the task without knowing Flowbee.
type LeaseContext struct {
	// Identity is who-to-be: the resolved actor the worker acts AS (fenced; the
	// worker cannot pick its own, §B). Lens is the resolved persona/lens file.
	Identity    string `json:"identity"`
	ModelFamily string `json:"model_family,omitempty"`
	Lens        string `json:"lens,omitempty"`
	Role        string `json:"role"`
	// BaseSHA is the SHA the task applies to (echoed for the worktree checkout).
	BaseSHA string `json:"base_sha,omitempty"`
	// Task / Spec / Acceptance are the human intent the agent must satisfy.
	Task               string `json:"task,omitempty"`
	Spec               string `json:"spec,omitempty"`
	AcceptanceCriteria string `json:"acceptance_criteria,omitempty"`
	// PriorVerdict is the last minted gate verdict on this job, if any (e.g. a
	// build re-armed after a bounce sees the reviewer's prior changes-requested).
	PriorVerdict *job.Verdict `json:"prior_verdict,omitempty"`
	// PriorReviewFindings is the most recent code-review's changes-requested findings
	// (the reviewer's "fix X, Y, Z"), carried to a rebuild so the agent addresses what
	// was flagged instead of re-submitting a patch that already failed review (§F).
	PriorReviewFindings string `json:"prior_review_findings,omitempty"`
	// CIFailures names the checks that failed CI on the prior attempt (newline-separated),
	// carried to a rebuild so the agent re-runs the named gate + fixes the real violation
	// instead of rebuilding blind and re-failing the same check (§F compounding memory).
	CIFailures string `json:"ci_failures,omitempty"`
	// StuckHint is the Rung-E advisor's note (0024) when this job was re-armed out of the
	// needs_human sink by the advisor: "here is what was tried / try this", so the rebuild
	// re-enters with fresh direction instead of blindly repeating the stalled attempt.
	StuckHint string `json:"stuck_hint,omitempty"`
	// Diff is the eng_worker's build patch, shipped to a code_reviewer so its agent
	// judges the actual change (the review harness writes .flowbee/diff.patch).
	Diff string `json:"diff,omitempty"`
	// DiffEmpty marks an authoritative empty adopted-PR diff; missing legacy diffs
	// keep this false so review tooling can distinguish absence from no-op changes.
	DiffEmpty bool `json:"diff_empty,omitempty"`
	// CIReady is true when reconciled facts are green; a code_reviewer harness skips
	// (releases) until then, so it never approves a not-green PR and thrashes.
	CIReady bool `json:"ci_ready,omitempty"`
	// IssueBranch is the per-issue branch the node commits to (flowbee/issue-N): the
	// worker-push harness fetches it, commits its work on top, and pushes it back.
	IssueBranch string `json:"issue_branch,omitempty"`
	// RepoURL is the job's repo clone/push URL (F9 multi-repo): the control plane tells
	// a fungible worker which repo each job belongs to so worker-push targets the right
	// remote. Empty in single-repo deployments.
	RepoURL string `json:"repo_url,omitempty"`
	// Rebuild signals a re-attempt after a bounce (prior CI failure / changes
	// requested) so the build brief tells the agent to FIX what broke, not re-submit.
	Rebuild bool `json:"rebuild,omitempty"`
	// Conflict signals a conflict_resolver lease: the worktree is at the CURRENT main
	// (a sibling merged into the same area) and Diff carries this job's ORIGINAL intended
	// change. The brief tells the agent to re-apply that intent on the current code,
	// reconciling it with the sibling change — not to re-run the original task.
	Conflict bool `json:"conflict,omitempty"`
}

// syncPauseMarker mirrors the global pause flag onto the on-disk marker that the
// lease gate (isPaused) and `flowbee status` read. pause => ensure the marker
// exists; resume => ensure it's gone. A no-op when no marker path is configured.
// This is what makes `flowbee resume` a reliable recovery: it clears the marker,
// not just the DB flag (#216).
func (s *Server) syncPauseMarker(pause bool) error {
	if s.pauseMarkerPath == "" {
		return nil
	}
	if pause {
		f, err := os.OpenFile(s.pauseMarkerPath, os.O_CREATE|os.O_WRONLY, 0o644)
		if err != nil {
			return err
		}
		return f.Close()
	}
	if err := os.Remove(s.pauseMarkerPath); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// isPaused reports whether the fleet is currently paused. It checks for the
// marker file on every call so pause/resume take effect immediately without a
// server restart. A missing marker path (empty or stat error) means not paused.
func (s *Server) isPaused() bool {
	if s.pauseMarkerPath == "" {
		return false
	}
	_, err := os.Stat(s.pauseMarkerPath)
	return err == nil
}

// lease long-polls: rank `ready` candidates (scheduler: priority + aging +
// capability match), try them best-first via the atomic claim up to LongPollWait.
// On grant -> 200 + envelope; on timeout / persistent lost-race / no eligible
// candidate -> 204. A worker lacking a job's required capability is never offered
// it (the claim also rejects, belt-and-suspenders), so the job stays `ready` and
// its no_eligible_worker alarm can fire (I-6).
func (s *Server) lease(w http.ResponseWriter, r *http.Request) {
	// The identity is the CREDENTIAL-BOUND one (§7.6: unforgeable), not the
	// self-asserted query param, whenever the request was authenticated. This is
	// the boundary that stops an enrolled box from leasing as someone else.
	identity := r.URL.Query().Get("identity")
	if bound, ok := auth.IdentityFrom(r); ok {
		identity = bound
	}
	family := r.URL.Query().Get("model_family")
	// CLAMP model_family to the credential-bound value when the operator declared one for
	// this enrolled identity. The self-asserted param feeds the §5.5 anti-affinity
	// exclusion (a same-family reviewer can't approve a build); without this clamp a
	// worker could lie about its family to win the review of a same-family (or its own
	// model's) build — defeating the uncorrelated-review guarantee (I-10). Identity is
	// already unforgeable above; this grounds family in the same credential.
	if bound, ok := auth.FamilyFrom(r); ok {
		family = bound
	}
	// model is the ACTUAL backend/model the worker runs (e.g. "codex", "sonnet") — a
	// display label recorded on the bound event so the §F card shows which model did each
	// node, since model_family is now only the anti-affinity tag (codex tags sonnet/opus).
	// Self-asserted is fine: it's display-only, never a gate (unlike identity/family).
	model := r.URL.Query().Get("model")
	roleFilter := job.Role(r.URL.Query().Get("role"))
	dryRun := truthyQuery(r.URL.Query().Get("dry_run")) || truthyQuery(r.URL.Query().Get("dry-run"))

	// polling for work is proof of liveness: bump last_seen so an idle worker (no
	// active lease to heartbeat) isn't badged stale on the roster / by the watchdog.
	if identity != "" {
		_ = s.store.RecordWorkerSeen(r.Context(), identity, s.clock.Now())
	}

	attested, err := s.registry.AttestedFor(r.Context(), identity)
	if err != nil {
		http.Error(w, "lease error", http.StatusInternalServerError)
		return
	}

	role := roleFilter
	if role == "" {
		role = job.RoleEngWorker
	}
	lens := r.URL.Query().Get("lens")
	reviewing := role == job.RoleCodeReviewer
	specAuthoring := role == job.RoleSpecAuthor
	specReviewing := role == job.RoleSpecReviewer
	resolvingConflict := role == job.RoleConflictResolver

	// Fleet pause: stop issuing new leases while letting in-flight jobs finish.
	// Heartbeats and result submissions are NOT affected (separate handlers). Two
	// sources: the operator's filesystem marker (isPaused) AND the client-triggerable,
	// DB-backed global flag (a worker/operator POSTing /v1/control/pause — "pause
	// everything"). Either pauses dispatch for the whole fleet.
	if s.isPaused() {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if paused, perr := s.store.DispatchPaused(r.Context()); perr == nil && paused {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	// F6 per-account ceiling: if this worker's agent login is rate-limited (within the
	// cooldown), withhold work so dispatch rolls over to boxes/accounts that aren't maxed.
	// Fail-open (a gate error never blocks dispatch). Mirrors the pause short-circuit.
	if acct := r.URL.Query().Get("account_id"); acct != "" {
		if gated, gerr := s.store.IsAccountGated(r.Context(), acct, s.clock.Now()); gerr == nil && gated {
			w.WriteHeader(http.StatusNoContent)
			return
		}
	}
	// HARD LINE: global dispatch-account allowlist (FLOWBEE_DISPATCH_ACCOUNTS). When set,
	// ONLY the listed logins get ANY work of ANY role — every worker on a non-listed
	// account (e.g. a maxed claude) OR advertising no account at all is withheld here,
	// before any role/claim logic. This keys on the account_id the worker authenticates
	// as, NOT the (misleading) sonnet/opus family tag, so a codex worker that labels
	// itself sonnet still passes and a claude worker never does. Empty = no restriction.
	if len(s.dispatchAccounts) > 0 {
		if acct := r.URL.Query().Get("account_id"); !s.dispatchAccounts[acct] {
			w.WriteHeader(http.StatusNoContent)
			return
		}
	}
	// Operator review-account pin (FLOWBEE_REVIEW_ACCOUNTS): concentrate ALL code_review
	// work on a chosen low-usage login. When the allowlist is set, a reviewer whose
	// advertised account_id is not on it gets no review work (204), so reviews roll onto
	// the pinned account only — other reviewers (claude OR codex) stay idle for reviews
	// while their build / spec / conflict roles keep flowing. Empty allowlist = no
	// restriction. The pinned worker advertises its account_id, so this never starves it.
	if reviewing && len(s.reviewAccounts) > 0 {
		if acct := r.URL.Query().Get("account_id"); !s.reviewAccounts[acct] {
			w.WriteHeader(http.StatusNoContent)
			return
		}
	}
	// Symmetric build-account pin (FLOWBEE_BUILD_ACCOUNTS): only the listed logins may
	// claim BUILD (eng_worker) work, so builds concentrate on a chosen agent (e.g. codex)
	// while a rate-limited agent's builders (e.g. a maxed claude) sit idle for builds. The
	// anti-affinity (§5.5) then routes those codex-built jobs to NON-codex (claude)
	// reviewers automatically. Review / spec / conflict roles are unaffected. Empty = off.
	if role == job.RoleEngWorker && len(s.buildAccounts) > 0 {
		if acct := r.URL.Query().Get("account_id"); !s.buildAccounts[acct] {
			w.WriteHeader(http.StatusNoContent)
			return
		}
	}
	// Conflict-resolver pin (FLOWBEE_RESOLVER_ACCOUNTS): route the heavy 3-way-merge task
	// to a login that handles it (codex stalls), e.g. a rolled-over claude — while builds
	// stay on codex. Only listed logins claim conflict-resolution work. Empty = off.
	if resolvingConflict && len(s.resolverAccounts) > 0 {
		if acct := r.URL.Query().Get("account_id"); !s.resolverAccounts[acct] {
			w.WriteHeader(http.StatusNoContent)
			return
		}
	}

	deadline := time.Now().Add(s.longPollWait)
	for {
		// each role claims from its own source state: code_review from review_pending,
		// spec_author from spec_authoring, spec_review from spec_review, every other
		// (eng_worker) from `ready`. The atomic claim is the correctness backstop in
		// every case (§6.3.1).
		var cands []scheduler.Candidate
		var err error
		switch {
		case reviewing:
			cands, err = s.store.ReviewPendingCandidates(r.Context())
		case specAuthoring:
			cands, err = s.store.SpecAuthoringCandidates(r.Context())
		case specReviewing:
			cands, err = s.store.SpecReviewCandidates(r.Context())
		case resolvingConflict:
			cands, err = s.store.ResolvingConflictCandidates(r.Context())
		default:
			// eng_worker: F8 blast-radius reservations withhold a ready candidate whose
			// declared write-set overlaps an in-flight build (avoid the conflict first).
			cands, err = s.store.ReadyCandidatesReserved(r.Context())
		}
		if err != nil {
			http.Error(w, "lease error", http.StatusInternalServerError)
			return
		}
		// per-repo pause ("pause russ"): drop candidates whose repo is parked (active=0)
		// so a paused repo's jobs stay put while every other repo keeps flowing. One batch
		// lookup at this single chokepoint; a no-op fast path when no repo is parked.
		if parked, perr := s.store.ParkedRepoJobIDs(r.Context()); perr == nil && len(parked) > 0 {
			kept := cands[:0]
			for _, c := range cands {
				if _, isParked := parked[c.JobID]; !isParked {
					kept = append(kept, c)
				}
			}
			cands = kept
		}
		for _, cand := range scheduler.Order(cands, attested, s.clock.Now()) {
			if dryRun {
				j, gerr := s.store.GetJob(r.Context(), cand.JobID)
				if gerr != nil {
					http.Error(w, "lease error", http.StatusInternalServerError)
					return
				}
				grant := s.leaseGrantForJob(r.Context(), cand.JobID, j, identity, family, lens, role, reviewing, resolvingConflict,
					"dry-run", j.LeaseEpoch+1, s.clock.Now().Add(s.leaseTTL), true)
				writeJSON(w, http.StatusOK, grant)
				return
			}
			var ls *lease.Lease
			switch {
			case reviewing:
				ls, err = s.store.ClaimReviewJob(r.Context(), store.ClaimReviewParams{
					JobID: cand.JobID, LeaseID: s.minter.New(), Identity: identity,
					ModelFamily: family, Model: model, Attested: attested, TTL: s.leaseTTL, Now: s.clock.Now(),
				})
			case specAuthoring:
				ls, err = s.store.ClaimSpecAuthor(r.Context(), store.ClaimSpecAuthorParams{
					JobID: cand.JobID, LeaseID: s.minter.New(), Identity: identity,
					ModelFamily: family, Model: model, Attested: attested, TTL: s.leaseTTL, Now: s.clock.Now(),
				})
			case specReviewing:
				ls, err = s.store.ClaimSpecReview(r.Context(), store.ClaimSpecReviewParams{
					JobID: cand.JobID, LeaseID: s.minter.New(), Identity: identity,
					ModelFamily: family, Model: model, Lens: lens, Attested: attested, TTL: s.leaseTTL, Now: s.clock.Now(),
				})
			case resolvingConflict:
				ls, err = s.store.ClaimConflictJob(r.Context(), store.ClaimConflictParams{
					JobID: cand.JobID, LeaseID: s.minter.New(), Identity: identity,
					ModelFamily: family, Model: model, Attested: attested, TTL: s.leaseTTL, Now: s.clock.Now(),
				})
			default:
				ls, err = s.store.ClaimReadyJob(r.Context(), store.ClaimParams{
					JobID: cand.JobID, LeaseID: s.minter.New(), Identity: identity,
					ModelFamily: family, Model: model, Role: role, Attested: attested, TTL: s.leaseTTL, Now: s.clock.Now(),
				})
			}
			if err == nil {
				// Arm the Rung-3 deadlines (soft phase budget + absolute cap) for this lease
				// so the §10.2 ladder + durable deadline timers run in production — the claim
				// itself only sets lease_deadline, never phase_deadline_at, so without this the
				// soft-deadline early-escalation rung is silently inert. Best-effort + fenced
				// to ls.Epoch: a failure here only skips soft early-escalation for this one
				// lease (the absolute cap is already on the row). Guarded so a liveness-less
				// test server (AbsoluteCap==0) never arms a now-due soft deadline.
				if s.liveness.AbsoluteCap > 0 {
					_ = s.store.ArmLeaseLivenessTimers(r.Context(), cand.JobID, ls.Epoch, s.clock.Now(), s.liveness)
				}
				j, _ := s.store.GetJob(r.Context(), cand.JobID)
				s.broker.Publish(LifeEvent{JobID: cand.JobID, State: string(j.State), Event: "lease_claimed", Epoch: ls.Epoch})
				grant := s.leaseGrantForJob(r.Context(), cand.JobID, j, identity, family, lens, role, reviewing, resolvingConflict,
					ls.LeaseID, ls.Epoch, ls.Deadline, false)
				writeJSON(w, http.StatusOK, grant)
				return
			}
			if !errors.Is(err, lease.ErrLostRace) {
				http.Error(w, "claim error", http.StatusInternalServerError)
				return
			}
			// lost race / lost on capability: try the next candidate.
		}
		if time.Now().After(deadline) {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		select {
		case <-r.Context().Done():
			return
		case <-time.After(s.pollInterval):
		}
	}
}

// parseReviewAccounts parses a comma-separated FLOWBEE_REVIEW_ACCOUNTS allowlist into a
// set of agent logins permitted to claim code_review work. Empty/whitespace entries are
// dropped; an all-empty value yields nil (no restriction — every reviewer claims). The
// lease handler consults it only for code_review claims, so build/spec/conflict roles are
// never affected.
func parseReviewAccounts(v string) map[string]bool {
	m := map[string]bool{}
	for _, p := range strings.Split(v, ",") {
		if s := strings.TrimSpace(p); s != "" {
			m[s] = true
		}
	}
	if len(m) == 0 {
		return nil
	}
	return m
}

func truthyQuery(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "t", "yes", "y", "on":
		return true
	default:
		return false
	}
}

func (s *Server) leaseGrantForJob(ctx context.Context, jobID string, j job.Job, identity, family, lens string, role job.Role, reviewing, resolvingConflict bool, leaseID string, leaseEpoch int, leaseDeadline time.Time, dryRun bool) LeaseGrant {
	grant := LeaseGrant{
		JobID: jobID, Kind: string(j.Kind), Role: string(j.Role),
		BaseSHA: j.BaseSHA, LeaseID: leaseID, LeaseEpoch: leaseEpoch,
		LeaseTTLS: int(s.leaseTTL / time.Second), Deadline: leaseDeadline.Format(time.RFC3339Nano),
		DryRun: dryRun, SpecContentHash: j.SpecContentHash, SpecVersion: j.SpecVersion,
	}
	// F1: ship the self-contained context block (§B). Identity/lens are the
	// CREDENTIAL-BOUND resolved values (the fence): the worker acts AS this
	// identity and cannot choose another. The bound_lens persisted at claim
	// time wins over the self-asserted query param when present.
	ctxLens := lens
	if j.BoundLens != "" {
		ctxLens = j.BoundLens
	}
	grant.Context = &LeaseContext{
		Identity: identity, ModelFamily: family, Lens: ctxLens, Role: string(j.Role),
		BaseSHA:             j.BaseSHA,
		Task:                j.TaskText,
		Spec:                j.SpecText,
		AcceptanceCriteria:  j.AcceptanceCriteria,
		PriorVerdict:        j.Verdict,
		PriorReviewFindings: j.LastReviewNotes,
		StuckHint:           j.StuckHint,
	}
	// the per-issue branch the node commits to (worker-push): builds + reviews
	// both target it. Resolved from the job's bound issue (adopted) or its spec
	// ancestor's materialized issue. Empty until an issue is bound.
	if reviewing || role == job.RoleEngWorker || resolvingConflict {
		grant.Context.IssueBranch = store.IssueBranch(s.store.ResolveIssueNum(ctx, jobID), jobID)
		// a build that has bounced — OR that carries recorded CI failures from a prior
		// attempt — is a re-attempt: tell the agent to fix what broke, not re-submit.
		// (A manual `requeue` zeroes the bounce counter but preserves last_ci_failures,
		// so the recorded failures alone still mark this a rebuild.) Carry the NAMES of
		// the checks that failed so the brief points the agent at the exact gate to fix.
		if role == job.RoleEngWorker && (j.Bounces > 0 || j.LastCIFailures != "") {
			grant.Context.Rebuild = true
			grant.Context.CIFailures = j.LastCIFailures
			if d, ok, derr := s.store.AdoptedPatchForRebuild(ctx, jobID); derr == nil && ok {
				grant.Context.Diff = d
			}
		}
		// F9: tell the (fungible) worker which repo this job belongs to so
		// worker-push targets the right remote. Resolve the job's repo scope to
		// its clone/push URL from the registry; the worker auths with its own
		// git credential. Empty repo => single-repo deployment (worker uses its
		// configured --repo-url).
		if j.Repo != "" {
			if rp, rerr := s.store.GetRepo(ctx, j.Repo); rerr == nil {
				grant.Context.RepoURL = workerRepoURL(rp.Owner, rp.Repo, s.workerGitSSH)
			}
		}
	}
	// a code_reviewer judges the actual change: ship the build patch so its
	// agent reads the diff (the review harness writes .flowbee/diff.patch),
	// and ship CIReady so the harness skips until reconciled CI is green
	// (an approval before then can't mint — it would bounce + rebuild-thrash).
	// a conflict_resolver re-applies the job's ORIGINAL change on the CURRENT
	// main (a sibling merged into the same area). Ship that original build patch
	// as the Diff and flag Conflict so the resolver brief tells the agent to
	// reconcile its intent with the current code — NOT to re-run the original
	// task, whose target may no longer exist (which is the conflict).
	if resolvingConflict {
		if d, derr := s.store.JobPatchDiff(ctx, jobID); derr == nil {
			grant.Context.Diff = d
		}
		grant.Context.Conflict = true
	}
	if reviewing {
		if d, derr := s.store.JobPatchDiff(ctx, jobID); derr == nil {
			grant.Context.Diff = d
		}
		grant.Context.DiffEmpty = j.DiffEmpty
		if f, _, ferr := s.facts.Facts(ctx, jobID); ferr == nil {
			grant.Context.CIReady = f.PRExists && f.CIGreen && !f.Merged
		}
	}
	// Repo provisioning hints (§7.4): only for build jobs that carry a
	// base_sha and only when a local mirror is configured.
	if s.mirrorPath != "" && j.BaseSHA != "" {
		if s.bundleProvisioning {
			// F3 cross-box `bundle`: the worker gets NO mirror path and NO push
			// target — it fetches a read-only bundle from /v1/bundle, returns a
			// diff, and Flowbee applies the patch + pushes the epoch ref itself.
			// The worker holds no GitHub credential and never touches git writes.
			grant.Provisioning = "bundle"
		} else {
			// same-box `worktree`: the worker adds a per-lease worktree off the
			// shared mirror and pushes the epoch ref locally (no creds either).
			grant.Provisioning = "worktree"
			grant.MirrorPath = s.mirrorPath
			grant.PushTarget = gitops.EpochRef(jobID, leaseEpoch)
		}
	}
	return grant
}

// workerRepoURL builds the per-job clone/push URL the lease ships to a worker. SSH
// (git@github.com:owner/repo.git) for fleets that auth with SSH keys (no token at
// rest); else HTTPS. The worker uses its OWN credential — the control plane never
// embeds a token in this URL.
func workerRepoURL(owner, repo string, ssh bool) string {
	if ssh {
		return "git@github.com:" + owner + "/" + repo + ".git"
	}
	return "https://github.com/" + owner + "/" + repo + ".git"
}

// renderReviewComment renders the reviewer's verdict + findings as the markdown
// body Flowbee posts into the GitHub issue (build-list §F). It mirrors the empty
// findings-commit message the reviewer node lands on the branch, so the issue and
// the git history tell the same story. Findings are the reviewer's own words; the
// header is Flowbee's deterministic framing.
func renderReviewComment(verdictLabel, notes string, epoch int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "### 🐝 Flowbee code review — %s\n\n", verdictLabel)
	if n := strings.TrimSpace(notes); n != "" {
		b.WriteString(n)
		b.WriteString("\n\n")
	} else {
		b.WriteString("_No findings recorded._\n\n")
	}
	fmt.Fprintf(&b, "---\n_Review epoch %d. Posted by Flowbee from the reviewer's verdict (R4: the control plane is the sole GitHub writer)._", epoch)
	return b.String()
}

// reviewRequest is the code-review result body: the reviewer's verdict CLAIM +
// requested disposition. Untrusted (I-9): the gate decides from reconciled facts.
type reviewRequest struct {
	Verdict     string `json:"verdict"`     // approved | changes_requested
	Disposition string `json:"disposition"` // self_merge | handoff (only meaningful on approved)
	Notes       string `json:"notes"`       // the reviewer's findings — posted into the GitHub issue + the §F record
	HeadSHA     string `json:"head_sha"`    // the issue-branch HEAD the reviewer advanced (empty findings-commit); tracked on a panel accumulate
}

// review runs the I-9 code-review gate for a fenced code_review result. The
// worker's claim is recorded as untrusted; the gate mints (or refuses) the
// tamper-evident verdict from reconciled facts. Stale epoch -> 409.
// policyForRepo returns the global policy with RequiredReviewers overridden by the repo's
// F5 panel size when one is configured — so a repo can run an N-reviewer consensus panel
// while others stay single-reviewer. A 0/absent override inherits the global policy.
func (s *Server) policyForRepo(repo string) job.Policy {
	p := s.policy
	if n, ok := s.reviewersByRepo[repo]; ok && n > 0 {
		p.RequiredReviewers = n
	}
	return p
}

func (s *Server) review(w http.ResponseWriter, r *http.Request) {
	jobID := r.PathValue("job")
	epoch, ok := epochFromHeader(r)
	if !ok {
		http.Error(w, "missing X-Lease-Epoch", http.StatusBadRequest)
		return
	}
	var req reviewRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	// resolve the F5 consensus size for THIS job's repo (no override => the global policy).
	policy := s.policy
	if len(s.reviewersByRepo) > 0 {
		if j, e := s.store.GetJob(r.Context(), jobID); e == nil {
			policy = s.policyForRepo(j.Repo)
		}
	}
	resp, err := s.store.ReviewResult(r.Context(), s.facts, policy, store.ReviewResultParams{
		JobID: jobID, Epoch: epoch,
		Claim:          job.VerdictValue(req.Verdict),
		Disposition:    job.Disposition(req.Disposition),
		IdempotencyKey: r.Header.Get("Idempotency-Key"),
		Notes:          req.Notes, // carried forward to the rebuild on a bounce (§F read)
		ReviewerHead:   req.HeadSHA,
		Now:            s.clock.Now(),
	})
	if err != nil {
		s.writeFenceError(w, err)
		return
	}
	ev := "review_bounced"
	if resp.Minted {
		ev = "verdict_minted"
	}
	s.broker.Publish(LifeEvent{JobID: jobID, State: resp.JobState, Event: ev, Epoch: epoch})

	// the GitHub issue is the durable record of the review (build-list §F): post the
	// reviewer's verdict + findings into the issue as a comment. Both arms record —
	// an approval and a changes-requested are equally part of the history. Dedupe per
	// review epoch so a retried submission posts exactly once. Control plane is the
	// sole GitHub writer (R4): the worker only declared the claim, Flowbee writes it.
	verdictLabel := "CHANGES REQUESTED 🔁"
	if resp.Minted {
		verdictLabel = "APPROVED ✅"
	}
	body := renderReviewComment(verdictLabel, req.Notes, epoch)
	if _, cerr := s.store.EnqueueIssueComment(r.Context(), jobID, body, fmt.Sprintf("review-e%d", epoch)); cerr == nil {
		s.broker.Publish(LifeEvent{JobID: jobID, State: resp.JobState, Event: "review_comment_enqueued", Epoch: epoch})
	}

	// on a passing gate, advance the branch point (§5.4) so the job reaches its
	// terminal-for-M3 arm (merge_handoff by default; merging under self_merge).
	if resp.Minted {
		if final, derr := s.store.DispatchMerge(r.Context(), s.facts, s.policy, store.DispatchMergeParams{JobID: jobID, Now: s.clock.Now()}); derr == nil {
			resp.JobState = string(final)
			s.broker.Publish(LifeEvent{JobID: jobID, State: string(final), Event: "merge_dispatched", Epoch: epoch})
			// Branch B autonomous merge (build-list §8.5): once the gate dispatches to
			// `merging`, enqueue the GitHub merge so project-OUT integrates the PR. No
			// human gate — this is the magic. handoff (Branch A) enqueues nothing.
			if final == job.StateMerging {
				if _, merr := s.store.EnqueueMergeForJob(r.Context(), jobID, s.clock.Now()); merr == nil {
					s.broker.Publish(LifeEvent{JobID: jobID, State: string(final), Event: "merge_enqueued", Epoch: epoch})
				}
			}
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

// specSubmit is the spec_author's work-product submission (§11.6): the author
// POSTs the spec_doc prose; FLOWBEE — never the worker — commits it (computes the
// BLAKE3 content hash) and opens the spec_review gate. The author cannot
// self-address its artifact (§11.1). Fenced by the author's lease epoch.
func (s *Server) specSubmit(w http.ResponseWriter, r *http.Request) {
	jobID := r.PathValue("job")
	epoch, ok := epochFromHeader(r)
	if !ok {
		http.Error(w, "missing X-Lease-Epoch", http.StatusBadRequest)
		return
	}
	var body struct {
		SpecMarkdown string `json:"spec_markdown"`
		Version      int    `json:"version"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	version := body.Version
	if version == 0 {
		version = 1
	}
	// Flowbee owns the hash (§11.1): the author submits bytes, Flowbee addresses them.
	hash := spec.ContentHash([]byte(body.SpecMarkdown))
	if err := s.store.MaterializeSpec(r.Context(), store.MaterializeSpecParams{
		JobID: jobID, ContentHash: hash, Version: version, Markdown: body.SpecMarkdown, Epoch: epoch, Now: s.clock.Now(),
	}); err != nil {
		s.writeFenceError(w, err)
		return
	}
	s.broker.Publish(LifeEvent{JobID: jobID, State: string(job.StateSpecReview), Event: "spec_authored", Epoch: epoch})
	writeJSON(w, http.StatusOK, map[string]any{"accepted": true, "spec_content_hash": hash, "spec_version": version})
}

// epicCreate is the planner front-door for a DECOMPOSED goal (F4): it seeds the
// epic-barrier job (the ONE issue-review over the whole decomposition) plus a child
// issue per submitted work item. The barrier review (scope · coverage · dep-graph)
// runs once; on pass the fan-out drain releases the children, each into its own spec
// flow (author -> review -> materialize -> build -> review -> merge). Loopback admin.
func (s *Server) epicCreate(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Title    string `json:"title"` // the epic goal / chat ref
		Lens     string `json:"lens"`  // the lens that authored the decomposition (anti-affinity)
		Repo     string `json:"repo"`  // repos.id — REQUIRED whenever more than one repo is registered; see resolveIngestRepo
		Priority int    `json:"priority"`
		Issues   []struct {
			Task       string `json:"task"`
			Acceptance string `json:"acceptance"`
		} `json:"issues"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if len(body.Issues) == 0 {
		http.Error(w, "epic needs at least one issue", http.StatusBadRequest)
		return
	}
	lens := body.Lens
	if lens == "" {
		lens = "product_speccer"
	}
	repo, err := s.resolveIngestRepo(r.Context(), body.Repo)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	epicID := ulid.New()
	issues := make([]store.EpicIssue, len(body.Issues))
	ids := make([]string, len(body.Issues))
	for i, it := range body.Issues {
		id := ulid.New()
		ids[i] = id
		issues[i] = store.EpicIssue{ID: id, Task: it.Task, Acceptance: it.Acceptance}
	}
	if err := s.store.SeedEpic(r.Context(), store.SeedEpicParams{
		EpicID: epicID, ChatRef: body.Title, AuthorLens: lens, Repo: repo,
		Issues: issues, Priority: job.NormalizePriority(body.Priority), Now: s.clock.Now(),
	}); err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	s.broker.Publish(LifeEvent{JobID: epicID, State: "spec_review", Event: "epic_ingested"})
	writeJSON(w, http.StatusOK, map[string]any{"epic_id": epicID, "issue_ids": ids})
}

// resolveIngestRepo resolves the repo a POST /v1/specs or /v1/epics ingest belongs
// to. An explicit, non-empty repo is returned as-is (the caller's intent is
// unambiguous). A repo-less ingest is resolved ONLY when there is no real
// ambiguity to guess through: zero registered repos falls back to "" (the legacy
// single-repo scope the non-repo-scoped sender drains), and exactly one registered
// repo is used as the sole sensible default.
//
// With TWO OR MORE registered repos, a repo-less ingest is a HARD ERROR instead of
// a silent guess. This replaced a prior silent default to "the primary registered
// repo (first by id)", which caused a real incident: three raw context-dump specs
// about the `russ` mail product (issues #254, #257, #258 — "Sam's mail surface
// today shows...", "Sam is dialing in mail quality...") were POSTed to /v1/specs
// without a `repo` field, silently landed in `flowbee`'s OWN pipeline (the
// alphabetically/config-order-first registered repo) instead of `russ`, and were
// built, reviewed, and bounced there for days before anyone noticed — every
// eng_worker/reviewer correctly found nothing in flowbee's repo to fix, since the
// spec described russ's `backend/internal/email/...` and
// `frontend/src/features/mail/...` paths. A silent guess is never recoverable
// after the fact as cheaply as an immediate, explicit rejection at ingest time.
func (s *Server) resolveIngestRepo(ctx context.Context, explicit string) (string, error) {
	if explicit != "" {
		return explicit, nil
	}
	repos, err := s.store.ListRepos(ctx, true)
	if err != nil || len(repos) == 0 {
		return "", nil
	}
	if len(repos) == 1 {
		return repos[0].ID, nil
	}
	ids := make([]string, len(repos))
	for i, r := range repos {
		ids[i] = r.ID
	}
	return "", fmt.Errorf("multiple repos registered (%s) — \"repo\" is required and must name one of them explicitly; a repo-less ingest cannot be safely guessed", strings.Join(ids, ", "))
}

// specCreate is the planner front-door (ingest): it seeds a spec-authoring job
// from a submitted work item so the spec flow can run (author -> issue-review ->
// materialize -> GitHub issue). The planner names the work; an author worker
// writes the spec.md and a distinct reviewer signs it off. Loopback admin route.
func (s *Server) specCreate(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ID         string `json:"id"`
		Title      string `json:"title"`      // short human label / chat ref for the work item
		Task       string `json:"task"`       // the work item the spec_author must spec ($FLOWBEE_TASK)
		Acceptance string `json:"acceptance"` // optional done-when
		Lens       string `json:"lens"`       // author lens (default product_speccer; distinct from the issue-reviewer lens)
		Repo       string `json:"repo"`       // repos.id this work item belongs to — REQUIRED whenever more than one repo is registered; see resolveIngestRepo
		Priority   int    `json:"priority"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	id := body.ID
	if id == "" {
		id = ulid.New()
	}
	lens := body.Lens
	if lens == "" {
		lens = "product_speccer"
	}
	repo, err := s.resolveIngestRepo(r.Context(), body.Repo)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	// the task the author specs: prefer an explicit `task`, fall back to the title so
	// a one-line "title only" ingest still gives the spec_author something to build.
	task := body.Task
	if strings.TrimSpace(task) == "" {
		task = body.Title
	}
	j, err := s.store.SeedSpecJob(r.Context(), store.SeedSpecParams{
		ID: id, ChatRef: body.Title, AuthorLens: lens, Priority: job.NormalizePriority(body.Priority), Repo: repo,
		TaskText: task, AcceptanceCriteria: body.Acceptance, Now: s.clock.Now(),
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	s.broker.Publish(LifeEvent{JobID: j.ID, State: string(j.State), Event: "spec_ingested"})
	writeJSON(w, http.StatusOK, map[string]any{"job_id": j.ID, "state": string(j.State)})
}

// specReviewRequest is the spec reviewer's verdict CLAIM + the two §11.3 sub-checks
// + the content hash it judged. Untrusted (I-9): the gate decides from the bytes.
type specReviewRequest struct {
	Decision          string `json:"decision"`           // signed_off | amended | needs_design | changes_requested
	BindsTo           string `json:"binds_to"`           // the spec_content_hash the worker judged
	MeetsStyle        bool   `json:"meets_style"`        // Q1
	MeetsRequirements bool   `json:"meets_requirements"` // Q2
	// F4 amend-in-place: on decision=="amended" the issue-reviewer supplies the
	// AMENDED spec prose. FLOWBEE — never the worker — commits it: the server computes
	// the BLAKE3 content hash and the new version, and the gate mints a sign-off bound
	// to the amended hash. Issue-review amends in place; it never bounces to the author.
	AmendedSpecMarkdown string `json:"amended_spec_markdown,omitempty"`
	AmendedVersion      int    `json:"amended_version,omitempty"`
	// Notes are the issue-reviewer's findings; carried to the spec-author rebuild on a
	// changes-requested bounce (§F read side).
	Notes string `json:"notes,omitempty"`
}

// specReview runs the I-9 spec gate for a fenced spec-review result (§11.5). The
// worker's claim is recorded untrusted; the gate mints (or refuses) the
// content-hash-bound sign-off from the CURRENT spec bytes. On a sign-off it
// enqueues materialize_issues (project-OUT renders the issue). Stale epoch -> 409.
func (s *Server) specReview(w http.ResponseWriter, r *http.Request) {
	jobID := r.PathValue("job")
	epoch, ok := epochFromHeader(r)
	if !ok {
		http.Error(w, "missing X-Lease-Epoch", http.StatusBadRequest)
		return
	}
	var req specReviewRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	// F4 amend-in-place: Flowbee commits the reviewer's amended bytes (computes the
	// BLAKE3 hash — the worker never self-addresses its artifact, §11.1). The new
	// version defaults to one past the reviewed version if unspecified.
	var amendedHash, amendedMarkdown string
	if job.VerdictValue(req.Decision) == job.VerdictAmended && req.AmendedSpecMarkdown != "" {
		amendedHash = spec.ContentHash([]byte(req.AmendedSpecMarkdown))
		amendedMarkdown = req.AmendedSpecMarkdown // persisted as spec_text so the issue/build carry the amended content
	}
	resp, err := s.store.SpecReviewResult(r.Context(), store.SpecReviewResultParams{
		JobID: jobID, Epoch: epoch,
		Claim:             job.VerdictValue(req.Decision),
		BindsTo:           req.BindsTo,
		MeetsStyle:        req.MeetsStyle,
		MeetsRequirements: req.MeetsRequirements,
		AmendedHash:       amendedHash,
		AmendedMarkdown:   amendedMarkdown,
		AmendedVersion:    req.AmendedVersion,
		IdempotencyKey:    r.Header.Get("Idempotency-Key"),
		Notes:             req.Notes, // carried to the spec-author rebuild on a bounce (§F read)
		Now:               s.clock.Now(),
	})
	if err != nil {
		s.writeFenceError(w, err)
		return
	}
	ev := "spec_bounced"
	switch {
	case resp.Amended:
		ev = "spec_amended"
	case resp.Minted:
		ev = "spec_signoff_minted"
	case resp.NeedsDesign:
		ev = "spec_needs_design"
	case resp.Superseded:
		ev = "spec_superseded"
	}
	s.broker.Publish(LifeEvent{JobID: jobID, State: resp.JobState, Event: ev, Epoch: epoch})
	writeJSON(w, http.StatusOK, resp)
}

// heartbeatRequest carries the §10 liveness observations a worker reports on each
// heartbeat (all HINTS, never authoritative kills, I-13) plus the two fast-path
// flags (§10.6). The body is optional — an empty heartbeat is a bare liveness ping.
type heartbeatRequest struct {
	AgentHealth   string `json:"agent_health"`   // Rung-0 enum (ok|zombie|stdin_block|cpu_spin|oom|hung_child)
	Rung1Class    string `json:"rung1_class"`    // Rung-1 (working|frozen|spinning)
	AwaitingInput bool   `json:"awaiting_input"` // §10.6 fast-path -> cancel
	AgentExited   bool   `json:"agent_exited"`   // §10.6 fast-path -> failed
	// M10 cost report (§6.7, I-15): the {tokens_in, tokens_out, $} DELTA. $ is
	// MICRO-USD. A delta crossing the ceiling escalates -> needs_human + cancel.
	TokensInDelta  int64 `json:"tokens_in_delta"`
	TokensOutDelta int64 `json:"tokens_out_delta"`
	MicroUSDDelta  int64 `json:"micro_usd_delta"`
}

func (s *Server) heartbeat(w http.ResponseWriter, r *http.Request) {
	jobID := r.PathValue("job")
	epoch, ok := epochFromHeader(r)
	if !ok {
		http.Error(w, "missing X-Lease-Epoch", http.StatusBadRequest)
		return
	}
	var req heartbeatRequest
	_ = json.NewDecoder(r.Body).Decode(&req) // optional body
	// M10 (§6.7, I-15): fold any reported cost delta FIRST. If it crosses the ceiling
	// the lease is revoked to needs_human in this call and the worker is told to
	// `cancel` — there is nothing further to heartbeat (the next heartbeat would 409).
	if req.TokensInDelta != 0 || req.TokensOutDelta != 0 || req.MicroUSDDelta != 0 {
		cr, err := s.store.RecordCost(r.Context(), store.CostParams{
			JobID: jobID, Epoch: epoch, Now: s.clock.Now(),
			TokensInDelta:  req.TokensInDelta,
			TokensOutDelta: req.TokensOutDelta,
			MicroUSDDelta:  req.MicroUSDDelta,
		})
		if err != nil {
			s.writeFenceError(w, err)
			return
		}
		if cr.Escalated {
			j, _ := s.store.GetJob(r.Context(), jobID)
			s.broker.Publish(LifeEvent{JobID: jobID, State: string(j.State), Event: "cost_escalated", Epoch: epoch})
			writeJSON(w, http.StatusOK, map[string]string{"directive": string(cr.Directive)})
			return
		}
	}
	dir, err := s.store.Heartbeat(r.Context(), store.HeartbeatParams{
		JobID: jobID, Epoch: epoch, Now: s.clock.Now(),
		Health:        liveness.AgentHealth(req.AgentHealth),
		Rung1:         liveness.Rung1Class(req.Rung1Class),
		AwaitingInput: req.AwaitingInput,
		AgentExited:   req.AgentExited,
	})
	if err != nil {
		s.writeFenceError(w, err)
		return
	}
	if dir == engine.DirectiveCancel {
		j, _ := s.store.GetJob(r.Context(), jobID)
		s.broker.Publish(LifeEvent{JobID: jobID, State: string(j.State), Event: "fast_path_cancel", Epoch: epoch})
	}
	writeJSON(w, http.StatusOK, map[string]string{"directive": string(dir)})
}

func (s *Server) result(w http.ResponseWriter, r *http.Request) {
	jobID := r.PathValue("job")
	epoch, ok := epochFromHeader(r)
	if !ok {
		http.Error(w, "missing X-Lease-Epoch", http.StatusBadRequest)
		return
	}
	idemKey := r.Header.Get("Idempotency-Key")
	// the eng_worker's work-product (§7.3): a patch bound to base_sha + the epoch
	// ref it pushed. Untrusted hints; Flowbee records the pushed ref so it can
	// promote it (the canonical PR-open trigger lands fully in M7). There is NO pr
	// field — Domain B owns PR existence (§3.4).
	var body struct {
		PushedRef string `json:"pushed_ref"`
		BaseSHA   string `json:"base_sha"`
		// Diff is the eng_worker's returned unified diff — UNTRUSTED DATA the M9
		// content-integrity gate (§9.2, I-11) judges at the code_review stage.
		Diff string `json:"diff"`
		// BlastRadius is the worker's DECLARED scope (paths + scope), a commitment
		// Flowbee verifies against the actual diff (§9.2b).
		BlastRadius json.RawMessage `json:"blast_radius"`
		// CommitMessage is the node's OWN detailed commit message (the agent's
		// .flowbee/commit.md). When the control plane commits the patch on the worker's
		// behalf (credential-less bundle path), it commits WITH this message so the
		// issue-branch history carries the node author's account of the change.
		CommitMessage string `json:"commit_message"`
		// PushedBranch + HeadSHA are the WORKER-PUSH signal: a node that holds a key
		// committed its own work and pushed it to flowbee/issue-N on GitHub itself
		// (build-list: "each node commits"). The control plane then does NOT apply or
		// re-push anything — it just opens the PR (it stays the sole GitHub-API caller +
		// merger). HeadSHA is the commit the node pushed; the diff still rides along for
		// the content gate + the reviewer.
		PushedBranch string `json:"pushed_branch"`
		HeadSHA      string `json:"head_sha"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)

	// worker-push: the node already published its commit to the issue branch on
	// GitHub. Record the pushed SHA as the head marker; skip the control-plane apply
	// + branch-push entirely (the worker did the git write with its own key).
	workerPushed := body.PushedBranch != "" && body.HeadSHA != ""

	// F3 — credential-less cross-box write path (§7.4 bundle/scoped-read, R4/§8).
	// A worker that was provisioned read-only (a bundle) holds NO git write path and
	// NO GitHub credential: it returns ONLY a diff and pushes nothing. When the
	// result carries a diff but no epoch ref, THE CONTROL PLANE applies the untrusted
	// patch onto base_sha in a throwaway worktree, commits under the Flowbee identity,
	// and pushes the epoch-namespaced ref ITSELF — producing exactly the ref a
	// same-box worker would have pushed, so the downstream promote/PR-open path is
	// identical. Flowbee does ALL git writes; the worker touched neither git nor
	// GitHub. The patch is untrusted data: a malformed/hostile diff fails to apply in
	// the disposable worktree and the result is declined (it cannot corrupt the mirror).
	pushedRef := body.PushedRef
	if workerPushed {
		// the worker pushed the branch itself: head_ref marker = the pushed commit.
		pushedRef = body.HeadSHA
	} else if s.bundleProvisioning && pushedRef == "" && body.Diff != "" && s.mirrorPath != "" && body.BaseSHA != "" {
		applyMsg := body.CommitMessage
		if strings.TrimSpace(applyMsg) == "" {
			applyMsg = "flowbee: applied bundle-worker patch " + jobID
		}
		_, ref, aerr := gitops.Open(s.mirrorPath).ApplyPatchAndPushEpoch(
			jobID, epoch, body.BaseSHA, body.Diff, applyMsg)
		if aerr != nil {
			// the returned patch did not apply cleanly: decline the result. The worker
			// re-leases (its lease is still live until it releases) or the job re-arms.
			http.Error(w, "patch did not apply: "+aerr.Error(), http.StatusUnprocessableEntity)
			return
		}
		pushedRef = ref
	}

	// A conflict_resolver submits its RESOLUTION through this same endpoint, but a
	// resolving_conflict job is NOT a build state — store.Result would fence it (409).
	// Route it to the conflict handler, which re-gates the resolved diff and re-arms
	// review_pending; the resolver force-pushed the issue branch, so reconcile picks up
	// the updated PR head + re-runs CI, then the gate re-reviews + re-merges.
	if cj, jerr := s.store.GetJob(r.Context(), jobID); jerr == nil && cj.State == job.StateResolvingConflict {
		rr, rerr := s.store.ResolveConflictResult(r.Context(), store.ResolveConflictParams{
			JobID: jobID, Epoch: epoch, ResolvedDiff: body.Diff,
			DeclaredBlastRadius: string(body.BlastRadius), PushedRef: pushedRef,
			PushedSHA: body.HeadSHA, Now: s.clock.Now(),
		})
		if rerr != nil {
			s.writeFenceError(w, rerr)
			return
		}
		s.broker.Publish(LifeEvent{JobID: jobID, State: rr.JobState, Event: "conflict_resolved", Epoch: epoch})
		if workerPushed {
			// the resolution was force-pushed to the issue branch; re-enqueue PR-open so the
			// PR tracks the new head (idempotent — a no-op if the PR already exists).
			_, _ = s.store.EnqueuePROpen(r.Context(), jobID, body.HeadSHA, "main")
		}
		writeJSON(w, http.StatusOK, rr)
		return
	}

	resp, err := s.store.Result(r.Context(), store.ResultParams{
		JobID: jobID, Epoch: epoch, IdempotencyKey: idemKey, Now: s.clock.Now(),
		PushedRef:           pushedRef,
		PushedSHA:           body.HeadSHA,
		PatchDiff:           body.Diff,
		DeclaredBlastRadius: string(body.BlastRadius),
	})
	if err != nil {
		s.writeFenceError(w, err)
		return
	}
	// build result -> PR open (build-list §7.3, the EnqueuePROpen trigger): once a
	// build lands review_pending, the control plane PUBLISHES the commit the worker
	// produced (only a local epoch ref so far) to GitHub as a branch under its own
	// token, then enqueues the PR-open. The worker holds no credential; Flowbee does
	// the GitHub write. Best-effort: a publish failure leaves the job review_pending
	// for a retry and never fails the accepted result.
	if resp.JobState == string(job.StateReviewPending) {
		switch {
		case workerPushed:
			// worker-push: the node already pushed flowbee/issue-N to GitHub. The control
			// plane does NO git write — it just opens the PR (sole GitHub-API caller +
			// merger). Idempotent enqueue keyed on the pushed head.
			if _, err := s.store.EnqueuePROpen(r.Context(), jobID, body.HeadSHA, "main"); err == nil {
				s.broker.Publish(LifeEvent{JobID: jobID, State: resp.JobState, Event: "pr_open_enqueued", Epoch: epoch})
			}
		case s.pushRemoteURL != "" && s.mirrorPath != "" && pushedRef != "":
			// credential-less path: the worker pushed only a local epoch ref; the control
			// plane publishes that commit to GitHub as the branch, then opens the PR.
			if sha, ok := gitops.Open(s.mirrorPath).RefSHA(pushedRef); ok {
				branch := store.IssueBranch(s.store.ResolveIssueNum(r.Context(), jobID), jobID)
				if err := gitops.Open(s.mirrorPath).PushCommit(s.pushRemoteURL, sha, branch); err != nil {
					s.broker.Publish(LifeEvent{JobID: jobID, State: resp.JobState, Event: "pr_branch_push_failed", Epoch: epoch})
				} else if _, err := s.store.EnqueuePROpen(r.Context(), jobID, sha, "main"); err == nil {
					s.broker.Publish(LifeEvent{JobID: jobID, State: resp.JobState, Event: "pr_open_enqueued", Epoch: epoch})
				}
			}
		}
	}
	s.broker.Publish(LifeEvent{JobID: jobID, State: resp.JobState, Event: "result_accepted", Epoch: epoch})
	writeJSON(w, http.StatusOK, resp)
}

// requeue re-arms a stranded job (escalated to needs_human from a now-fixed
// transient failure) for a fresh attempt: reset attempts/bounces, clear the lease +
// verdict, bump the epoch, route back to ready. The operator's "retry" — no jobs-table
// surgery. (An ADMIN action; on a secured listener it needs a token, or run it from
// the control-plane box where the loopback bypass applies.)
func (s *Server) requeue(w http.ResponseWriter, r *http.Request) {
	jobID := r.PathValue("job")
	force := r.URL.Query().Get("force") == "true" || r.URL.Query().Get("force") == "1"
	final, err := s.store.RequeueJob(r.Context(), jobID, force, s.clock.Now())
	if errors.Is(err, store.ErrJobNotFound) {
		http.Error(w, "requeue: no such job "+jobID+" (check the FULL job id, not a truncated one)", http.StatusNotFound)
		return
	}
	if errors.Is(err, store.ErrJobActivelyLeased) {
		http.Error(w, "requeue: "+err.Error()+" — re-run with --force if you really mean to", http.StatusConflict)
		return
	}
	if err != nil {
		http.Error(w, "requeue: "+err.Error(), http.StatusInternalServerError)
		return
	}
	s.broker.Publish(LifeEvent{JobID: jobID, State: string(final), Event: "requeued"})
	writeJSON(w, http.StatusOK, map[string]string{"job_id": jobID, "state": string(final)})
}

// cancel terminally cancels a stranded job the operator has decided not to pursue (the
// complement to requeue). It clears the job from the needs_human triage view without
// jobs-table surgery. An ADMIN action (same trust posture as requeue).
func (s *Server) cancel(w http.ResponseWriter, r *http.Request) {
	jobID := r.PathValue("job")
	force := r.URL.Query().Get("force") == "true" || r.URL.Query().Get("force") == "1"
	final, err := s.store.CancelJob(r.Context(), jobID, force, s.clock.Now())
	if errors.Is(err, store.ErrJobNotFound) {
		http.Error(w, "cancel: no such job "+jobID+" (check the FULL job id, not a truncated one)", http.StatusNotFound)
		return
	}
	if errors.Is(err, store.ErrJobActivelyLeased) {
		http.Error(w, "cancel: "+err.Error()+" — re-run with --force if you really mean to", http.StatusConflict)
		return
	}
	if err != nil {
		http.Error(w, "cancel: "+err.Error(), http.StatusInternalServerError)
		return
	}
	s.broker.Publish(LifeEvent{JobID: jobID, State: string(final), Event: "cancelled"})
	writeJSON(w, http.StatusOK, map[string]string{"job_id": jobID, "state": string(final)})
}

// bundle serves a read-only `git bundle` of a job's base SHA (F3, §7.4 mode (a)):
// the credential-less, cross-box provisioning channel. A `bundle`-provisioned
// worker GETs this over the authenticated worker transport, clones a working tree
// from the returned bytes (no network to GitHub, NO credential), runs its agent,
// and returns ONLY a diff. The bytes are pure read-only data: the worst a hostile
// worker can do is read code it was already going to build (R4, I-14). It never
// receives a write path or a token — Flowbee performs every git write (§8).
func (s *Server) bundle(w http.ResponseWriter, r *http.Request) {
	jobID := r.PathValue("job")
	if s.mirrorPath == "" {
		http.Error(w, "no mirror configured", http.StatusNotFound)
		return
	}
	j, err := s.store.GetJob(r.Context(), jobID)
	if err != nil || j.BaseSHA == "" {
		http.Error(w, "job has no base sha", http.StatusNotFound)
		return
	}
	b, err := gitops.Open(s.mirrorPath).Bundle(j.BaseSHA)
	if err != nil {
		http.Error(w, "bundle error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/x-git-bundle")
	w.Header().Set("X-Flowbee-Base-SHA", j.BaseSHA)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(b)
}

func (s *Server) release(w http.ResponseWriter, r *http.Request) {
	jobID := r.PathValue("job")
	epoch, ok := epochFromHeader(r)
	if !ok {
		http.Error(w, "missing X-Lease-Epoch", http.StatusBadRequest)
		return
	}
	// ?keep=1 re-arms without burning an attempt (a non-failure abandon, e.g. a
	// worker-push fast-forward race lost to a branch move). ?fail=1 forces an attempt
	// to burn even on a penalty-free GATE release (a reviewer that produced no parseable
	// verdict), so a broken reviewer escalates after max_attempts instead of churning.
	// Default burns only for a build abandon (-> ready).
	noPenalty := r.URL.Query().Get("keep") == "1"
	failed := r.URL.Query().Get("fail") == "1"
	if err := s.store.Release(r.Context(), store.ReleaseParams{JobID: jobID, Epoch: epoch, Now: s.clock.Now(), NoPenalty: noPenalty, Failed: failed}); err != nil {
		s.writeFenceError(w, err)
		return
	}
	s.broker.Publish(LifeEvent{JobID: jobID, State: string(job.StateReady), Event: "lease_released", Epoch: epoch})
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// rebaseConflict diverts a build whose worker-side rebase hit a REAL conflict (its branch
// patch doesn't apply onto the granted base) into the conflict_resolver path — the worker
// reports the conflict + its branch change so resolution actually happens, instead of the
// build looping to needs_human where the conflict is never resolved. Fenced on lease epoch.
func (s *Server) rebaseConflict(w http.ResponseWriter, r *http.Request) {
	jobID := r.PathValue("job")
	epoch, ok := epochFromHeader(r)
	if !ok {
		http.Error(w, "missing X-Lease-Epoch", http.StatusBadRequest)
		return
	}
	var body struct {
		BaseSHA string `json:"base_sha"`
		Diff    string `json:"diff"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "bad body", http.StatusBadRequest)
		return
	}
	if err := s.store.RouteBuildConflict(r.Context(), store.RouteBuildConflictParams{
		JobID: jobID, Epoch: epoch, NewBaseSHA: body.BaseSHA, BranchDiff: body.Diff, Now: s.clock.Now(),
	}); err != nil {
		s.writeFenceError(w, err)
		return
	}
	s.broker.Publish(LifeEvent{JobID: jobID, State: string(job.StateResolvingConflict), Event: "conflict_detected", Epoch: epoch})
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// writeFenceError maps the lease sentinel errors to HTTP status codes.
func (s *Server) writeFenceError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, lease.ErrStaleEpoch):
		http.Error(w, "stale lease epoch", http.StatusConflict) // 409
	default:
		http.Error(w, "internal error", http.StatusInternalServerError)
	}
}

func epochFromHeader(r *http.Request) (int, bool) {
	v := r.Header.Get("X-Lease-Epoch")
	if v == "" {
		return 0, false
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, false
	}
	return n, true
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
