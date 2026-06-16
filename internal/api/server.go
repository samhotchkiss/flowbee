// Package api hosts Flowbee's two HTTP servers (DESIGN §12.1): a health listener
// and the private worker API. M1 implements the full §7.2 worker surface
// (register / lease long-poll / heartbeat / result / release), the read-only SSE
// feed, and a minimal board. The handlers delegate every state decision to
// engine.Decide via internal/store — they never decide state themselves.
package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
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
	// staleHB is the roster's stale-heartbeat threshold (§12.6.2).
	staleHB time.Duration
	// authn is the worker-transport authenticator (§7.6). Nil = loopback-only dev
	// (no mutual auth); set for a non-loopback listener (bearer token / mTLS).
	authn auth.Authenticator
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
	return &Server{
		store:        st,
		clock:        clk,
		minter:       minter,
		registry:     worker.NewRegistry(st, cfg.LeaseTTLS, cfg.HeartbeatIntervalS, allow),
		broker:       NewBroker(),
		version:      version,
		facts:        facts,
		policy:       cfg.Policy,
		leaseTTL:     cfg.LeaseTTL,
		longPollWait: cfg.LongPollWait,
		pollInterval: poll,
		mirrorPath:         cfg.MirrorPath,
		bundleProvisioning: cfg.BundleProvisioning,
		staleHB:            staleHB,
		authn:              cfg.Authenticator,
	}
}

// Broker exposes the SSE broker so the runtime can publish lifecycle events.
func (s *Server) Broker() *Broker { return s.broker }

// HealthHandler serves the health listener.
func (s *Server) HealthHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.healthz)
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
	mux.HandleFunc("GET /roster", s.rosterPage)
	mux.HandleFunc("GET /dashboard", s.dashboard)
	mux.HandleFunc("GET /", s.board)
	return mux
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

// dashboard renders the finished operator UI (§12.6): board + roster + budget +
// audit + cost in one live page, refreshed off the SSE feed.
func (s *Server) dashboard(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	board, err := s.store.BoardSnapshot(ctx)
	if err != nil {
		http.Error(w, "dashboard error", http.StatusInternalServerError)
		return
	}
	roster, err := s.store.Roster(ctx, s.clock.Now(), s.staleHB)
	if err != nil {
		http.Error(w, "dashboard error", http.StatusInternalServerError)
		return
	}
	budget, err := s.store.RateLimit(ctx)
	if err != nil {
		http.Error(w, "dashboard error", http.StatusInternalServerError)
		return
	}
	cost, err := s.store.AllJobCost(ctx)
	if err != nil {
		http.Error(w, "dashboard error", http.StatusInternalServerError)
		return
	}
	audit, err := s.store.AllAudit(ctx)
	if err != nil {
		http.Error(w, "dashboard error", http.StatusInternalServerError)
		return
	}
	needsHuman, err := s.store.NeedsHumanView(ctx)
	if err != nil {
		http.Error(w, "dashboard error", http.StatusInternalServerError)
		return
	}
	renderDashboard(w, dashboardData{
		Board: board, Roster: roster, Budget: budget,
		Cost: cost, Audit: audit, NeedsHuman: needsHuman,
	})
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

// rosterPage serves the HTML worker roster with a stale-hb badge (§12.6.2).
func (s *Server) rosterPage(w http.ResponseWriter, r *http.Request) {
	roster, err := s.store.Roster(r.Context(), s.clock.Now(), s.staleHB)
	if err != nil {
		http.Error(w, "roster error", http.StatusInternalServerError)
		return
	}
	renderRoster(w, roster)
}

func (s *Server) healthz(w http.ResponseWriter, r *http.Request) {
	dbOK := s.store.Ping(r.Context()) == nil
	status, code := "ok", http.StatusOK
	if !dbOK {
		status, code = "unavailable", http.StatusServiceUnavailable
	}
	writeJSON(w, code, map[string]any{"status": status, "db": dbOK, "version": s.version})
}

func (s *Server) register(w http.ResponseWriter, r *http.Request) {
	var reg worker.Registration
	if err := json.NewDecoder(r.Body).Decode(&reg); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if reg.WorkerID == "" {
		reg.WorkerID = s.minter.New()
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

// reviewRequest is the code-review result body: the reviewer's verdict CLAIM +
// requested disposition. Untrusted (I-9): the gate decides from reconciled facts.
type reviewRequest struct {
	Verdict     string `json:"verdict"`     // approved | changes_requested
	Disposition string `json:"disposition"` // self_merge | handoff (only meaningful on approved)
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

	// on a passing gate, advance the branch point (§5.4) so the job reaches its
	// terminal-for-M3 arm (merge_handoff by default; merging under self_merge).
	if resp.Minted {
		if final, derr := s.store.DispatchMerge(r.Context(), s.facts, s.policy, store.DispatchMergeParams{JobID: jobID, Now: s.clock.Now()}); derr == nil {
			resp.JobState = string(final)
			s.broker.Publish(LifeEvent{JobID: jobID, State: string(final), Event: "merge_dispatched", Epoch: epoch})
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
		JobID: jobID, ContentHash: hash, Version: version, Epoch: epoch, Now: s.clock.Now(),
	}); err != nil {
		s.writeFenceError(w, err)
		return
	}
	s.broker.Publish(LifeEvent{JobID: jobID, State: string(job.StateSpecReview), Event: "spec_authored", Epoch: epoch})
	writeJSON(w, http.StatusOK, map[string]any{"accepted": true, "spec_content_hash": hash, "spec_version": version})
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
	}
	_ = json.NewDecoder(r.Body).Decode(&body)

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
	if s.bundleProvisioning && pushedRef == "" && body.Diff != "" && s.mirrorPath != "" && body.BaseSHA != "" {
		_, ref, aerr := gitops.Open(s.mirrorPath).ApplyPatchAndPushEpoch(
			jobID, epoch, body.BaseSHA, body.Diff,
			"flowbee: applied bundle-worker patch "+jobID)
		if aerr != nil {
			// the returned patch did not apply cleanly: decline the result. The worker
			// re-leases (its lease is still live until it releases) or the job re-arms.
			http.Error(w, "patch did not apply: "+aerr.Error(), http.StatusUnprocessableEntity)
			return
		}
		pushedRef = ref
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
	s.broker.Publish(LifeEvent{JobID: jobID, State: resp.JobState, Event: "result_accepted", Epoch: epoch})
	writeJSON(w, http.StatusOK, resp)
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
	if err := s.store.Release(r.Context(), store.ReleaseParams{JobID: jobID, Epoch: epoch, Now: s.clock.Now()}); err != nil {
		s.writeFenceError(w, err)
		return
	}
	s.broker.Publish(LifeEvent{JobID: jobID, State: string(job.StateReady), Event: "lease_released", Epoch: epoch})
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) board(w http.ResponseWriter, r *http.Request) {
	jobs, err := s.store.BoardSnapshot(r.Context())
	if err != nil {
		http.Error(w, "board error", http.StatusInternalServerError)
		return
	}
	if r.Header.Get("Accept") == "application/json" {
		writeJSON(w, http.StatusOK, jobs)
		return
	}
	renderBoard(w, jobs)
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
