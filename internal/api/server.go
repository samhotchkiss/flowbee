// Package api hosts Flowbee's two HTTP servers (DESIGN §12.1): a health listener
// and the private worker API. M1 implements the full §7.2 worker surface
// (register / lease long-poll / heartbeat / result / release), the read-only SSE
// feed, and a minimal board. The handlers delegate every state decision to
// engine.Decide via internal/store — they never decide state themselves.
package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
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

	facts  store.FactSource
	policy job.Policy

	leaseTTL     time.Duration
	longPollWait time.Duration
	pollInterval time.Duration

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
	// authn is the worker-transport authenticator (§7.6). Nil = loopback-only dev
	// (no mutual auth); set for a non-loopback listener (bearer token / mTLS).
	authn auth.Authenticator
	// ui is the F12 web UI (internal/web): the productionized Fleet/Board/Dashboard
	// panes served off the same live store read-models, embedded via go:embed.
	ui *web.UI
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
	return &Server{
		store:              st,
		clock:              clk,
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
	}
}

// Broker exposes the SSE broker so the runtime can publish lifecycle events.
func (s *Server) Broker() *Broker { return s.broker }

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
	worker.HandleFunc("GET /v1/lease", s.lease)
	worker.HandleFunc("POST /v1/jobs/{job}/heartbeat", s.heartbeat)
	worker.HandleFunc("POST /v1/jobs/{job}/result", s.result)
	worker.HandleFunc("POST /v1/jobs/{job}/review", s.review)
	worker.HandleFunc("POST /v1/jobs/{job}/spec", s.specSubmit)
	worker.HandleFunc("POST /v1/jobs/{job}/spec-review", s.specReview)
	worker.HandleFunc("POST /v1/jobs/{job}/release", s.release)
	worker.HandleFunc("GET /v1/jobs/{job}/bundle", s.bundle)
	authed := auth.Middleware(s.authn, worker)

	mux := http.NewServeMux()
	// worker-protocol routes go through the authenticated surface.
	for _, p := range []string{
		"POST /v1/workers/register", "POST /v1/workers/usage", "GET /v1/lease",
		"POST /v1/jobs/{job}/heartbeat", "POST /v1/jobs/{job}/result",
		"POST /v1/jobs/{job}/review", "POST /v1/jobs/{job}/spec",
		"POST /v1/jobs/{job}/spec-review", "POST /v1/jobs/{job}/release",
		"GET /v1/jobs/{job}/bundle",
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
	mux.HandleFunc("GET /v1/needs-input", s.needsInputJSON)
	mux.HandleFunc("GET /v1/backlog", s.backlogJSON)
	mux.HandleFunc("GET /v1/fleet", s.fleetJSON)
	// F7 board-lifecycle write edges (operator / user-agent loop). These are the
	// deliberate human/agent decisions: answer a needs_design item (resume it),
	// promote a backlog item into its flow, opt a quiescent adopted item in.
	mux.HandleFunc("POST /v1/jobs/{job}/design", s.resolveDesign)
	mux.HandleFunc("POST /v1/jobs/{job}/promote", s.promoteBacklog)
	mux.HandleFunc("POST /v1/jobs/{job}/adopt", s.adoptOptIn)
	// operator retry: re-arm a job stranded in needs_human (escalated from a now-fixed
	// transient failure) back to ready. Same operator surface as promote/adopt.
	mux.HandleFunc("POST /v1/jobs/{job}/requeue", s.requeue)
	// the planner front-door (ingest): seed a spec-authoring job from a submitted
	// work item so the spec flow (author -> issue-review -> materialize) can run.
	mux.HandleFunc("POST /v1/specs", s.specCreate)
	mux.HandleFunc("POST /v1/epics", s.epicCreate)
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
	writeJSON(w, code, map[string]any{"status": status, "db": dbOK, "version": s.version})
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
	// Diff is the eng_worker's build patch, shipped to a code_reviewer so its agent
	// judges the actual change (the review harness writes .flowbee/diff.patch).
	Diff string `json:"diff,omitempty"`
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
	roleFilter := job.Role(r.URL.Query().Get("role"))

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
		for _, cand := range scheduler.Order(cands, attested, s.clock.Now()) {
			var ls *lease.Lease
			switch {
			case reviewing:
				ls, err = s.store.ClaimReviewJob(r.Context(), store.ClaimReviewParams{
					JobID: cand.JobID, LeaseID: s.minter.New(), Identity: identity,
					ModelFamily: family, Attested: attested, TTL: s.leaseTTL, Now: s.clock.Now(),
				})
			case specAuthoring:
				ls, err = s.store.ClaimSpecAuthor(r.Context(), store.ClaimSpecAuthorParams{
					JobID: cand.JobID, LeaseID: s.minter.New(), Identity: identity,
					ModelFamily: family, Attested: attested, TTL: s.leaseTTL, Now: s.clock.Now(),
				})
			case specReviewing:
				ls, err = s.store.ClaimSpecReview(r.Context(), store.ClaimSpecReviewParams{
					JobID: cand.JobID, LeaseID: s.minter.New(), Identity: identity,
					ModelFamily: family, Lens: lens, Attested: attested, TTL: s.leaseTTL, Now: s.clock.Now(),
				})
			case resolvingConflict:
				ls, err = s.store.ClaimConflictJob(r.Context(), store.ClaimConflictParams{
					JobID: cand.JobID, LeaseID: s.minter.New(), Identity: identity,
					ModelFamily: family, Attested: attested, TTL: s.leaseTTL, Now: s.clock.Now(),
				})
			default:
				ls, err = s.store.ClaimReadyJob(r.Context(), store.ClaimParams{
					JobID: cand.JobID, LeaseID: s.minter.New(), Identity: identity,
					ModelFamily: family, Role: role, Attested: attested, TTL: s.leaseTTL, Now: s.clock.Now(),
				})
			}
			if err == nil {
				j, _ := s.store.GetJob(r.Context(), cand.JobID)
				s.broker.Publish(LifeEvent{JobID: cand.JobID, State: string(j.State), Event: "lease_claimed", Epoch: ls.Epoch})
				grant := LeaseGrant{
					JobID: cand.JobID, Kind: string(j.Kind), Role: string(j.Role),
					BaseSHA: j.BaseSHA, LeaseID: ls.LeaseID, LeaseEpoch: ls.Epoch,
					LeaseTTLS: int(s.leaseTTL / time.Second), Deadline: ls.Deadline.Format(time.RFC3339Nano),
					SpecContentHash: j.SpecContentHash, SpecVersion: j.SpecVersion,
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
					BaseSHA:            j.BaseSHA,
					Task:               j.TaskText,
					Spec:               j.SpecText,
					AcceptanceCriteria: j.AcceptanceCriteria,
					PriorVerdict:       j.Verdict,
				}
				// the per-issue branch the node commits to (worker-push): builds + reviews
				// both target it. Resolved from the job's bound issue (adopted) or its spec
				// ancestor's materialized issue. Empty until an issue is bound.
				if reviewing || role == job.RoleEngWorker || resolvingConflict {
					grant.Context.IssueBranch = store.IssueBranch(s.store.ResolveIssueNum(r.Context(), cand.JobID), cand.JobID)
					// a build that has bounced is a re-attempt after a CI failure / changes
					// requested — tell the agent to fix what broke, not re-submit.
					if role == job.RoleEngWorker && j.Bounces > 0 {
						grant.Context.Rebuild = true
					}
					// F9: tell the (fungible) worker which repo this job belongs to so
					// worker-push targets the right remote. Resolve the job's repo scope to
					// its clone/push URL from the registry; the worker auths with its own
					// git credential. Empty repo => single-repo deployment (worker uses its
					// configured --repo-url).
					if j.Repo != "" {
						if rp, rerr := s.store.GetRepo(r.Context(), j.Repo); rerr == nil {
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
					if d, derr := s.store.JobPatchDiff(r.Context(), cand.JobID); derr == nil {
						grant.Context.Diff = d
					}
					grant.Context.Conflict = true
				}
				if reviewing {
					if d, derr := s.store.JobPatchDiff(r.Context(), cand.JobID); derr == nil {
						grant.Context.Diff = d
					}
					if f, _, ferr := s.facts.Facts(r.Context(), cand.JobID); ferr == nil {
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
						grant.PushTarget = gitops.EpochRef(cand.JobID, ls.Epoch)
					}
				}
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
}

// review runs the I-9 code-review gate for a fenced code_review result. The
// worker's claim is recorded as untrusted; the gate mints (or refuses) the
// tamper-evident verdict from reconciled facts. Stale epoch -> 409.
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
	resp, err := s.store.ReviewResult(r.Context(), s.facts, s.policy, store.ReviewResultParams{
		JobID: jobID, Epoch: epoch,
		Claim:          job.VerdictValue(req.Verdict),
		Disposition:    job.Disposition(req.Disposition),
		IdempotencyKey: r.Header.Get("Idempotency-Key"),
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
		Repo     string `json:"repo"`
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
	repo := body.Repo
	if repo == "" {
		repo = "default"
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
		Issues: issues, Priority: body.Priority, Now: s.clock.Now(),
	}); err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	s.broker.Publish(LifeEvent{JobID: epicID, State: "spec_review", Event: "epic_ingested"})
	writeJSON(w, http.StatusOK, map[string]any{"epic_id": epicID, "issue_ids": ids})
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
		Repo       string `json:"repo"`       // repos.id this work item belongs to (default "default")
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
	repo := body.Repo
	if repo == "" {
		repo = "default"
	}
	// the task the author specs: prefer an explicit `task`, fall back to the title so
	// a one-line "title only" ingest still gives the spec_author something to build.
	task := body.Task
	if strings.TrimSpace(task) == "" {
		task = body.Title
	}
	j, err := s.store.SeedSpecJob(r.Context(), store.SeedSpecParams{
		ID: id, ChatRef: body.Title, AuthorLens: lens, Priority: body.Priority, Repo: repo,
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
	var amendedHash string
	if job.VerdictValue(req.Decision) == job.VerdictAmended && req.AmendedSpecMarkdown != "" {
		amendedHash = spec.ContentHash([]byte(req.AmendedSpecMarkdown))
	}
	resp, err := s.store.SpecReviewResult(r.Context(), store.SpecReviewResultParams{
		JobID: jobID, Epoch: epoch,
		Claim:             job.VerdictValue(req.Decision),
		BindsTo:           req.BindsTo,
		MeetsStyle:        req.MeetsStyle,
		MeetsRequirements: req.MeetsRequirements,
		AmendedHash:       amendedHash,
		AmendedVersion:    req.AmendedVersion,
		IdempotencyKey:    r.Header.Get("Idempotency-Key"),
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
			DeclaredBlastRadius: string(body.BlastRadius), PushedRef: pushedRef, Now: s.clock.Now(),
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
	final, err := s.store.RequeueJob(r.Context(), jobID, s.clock.Now())
	if errors.Is(err, store.ErrJobNotFound) {
		http.Error(w, "requeue: no such job "+jobID+" (check the FULL job id, not a truncated one)", http.StatusNotFound)
		return
	}
	if err != nil {
		http.Error(w, "requeue: "+err.Error(), http.StatusInternalServerError)
		return
	}
	s.broker.Publish(LifeEvent{JobID: jobID, State: string(final), Event: "requeued"})
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
	// worker-push fast-forward race lost to a branch move). Default burns (a real
	// build abandon).
	noPenalty := r.URL.Query().Get("keep") == "1"
	if err := s.store.Release(r.Context(), store.ReleaseParams{JobID: jobID, Epoch: epoch, Now: s.clock.Now(), NoPenalty: noPenalty}); err != nil {
		s.writeFenceError(w, err)
		return
	}
	s.broker.Publish(LifeEvent{JobID: jobID, State: string(job.StateReady), Event: "lease_released", Epoch: epoch})
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
