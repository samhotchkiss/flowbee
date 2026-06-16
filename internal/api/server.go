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

	"github.com/samhotchkiss/flowbee/internal/clock"
	"github.com/samhotchkiss/flowbee/internal/gitops"
	"github.com/samhotchkiss/flowbee/internal/job"
	"github.com/samhotchkiss/flowbee/internal/lease"
	"github.com/samhotchkiss/flowbee/internal/scheduler"
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
	// staleHB is the roster's stale-heartbeat threshold (§12.6.2).
	staleHB time.Duration
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
	// workers (§7.4). Empty disables local provisioning hints.
	MirrorPath string
	// Allowlist is the enrolled-identity attestation policy (§9.4.1). The zero
	// value attests no role/family/tool; tests/dev use worker.OpenAllowlist().
	Allowlist worker.Allowlist
	// StaleHBThreshold badges a worker stale on the roster after this idle gap
	// (§12.6.2). Defaults to 3× the heartbeat interval.
	StaleHBThreshold time.Duration
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
		mirrorPath:   cfg.MirrorPath,
		staleHB:      staleHB,
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

// PrivateHandler serves the worker API + SSE + board (loopback / Tailscale only).
func (s *Server) PrivateHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/workers/register", s.register)
	mux.HandleFunc("GET /v1/lease", s.lease)
	mux.HandleFunc("POST /v1/jobs/{job}/heartbeat", s.heartbeat)
	mux.HandleFunc("POST /v1/jobs/{job}/result", s.result)
	mux.HandleFunc("POST /v1/jobs/{job}/review", s.review)
	mux.HandleFunc("POST /v1/jobs/{job}/release", s.release)
	mux.HandleFunc("GET /v1/events", s.eventsHandler)
	mux.HandleFunc("GET /v1/budget", s.budgetJSON)
	mux.HandleFunc("GET /v1/roster", s.rosterJSON)
	mux.HandleFunc("GET /roster", s.rosterPage)
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
}

// lease long-polls: rank `ready` candidates (scheduler: priority + aging +
// capability match), try them best-first via the atomic claim up to LongPollWait.
// On grant -> 200 + envelope; on timeout / persistent lost-race / no eligible
// candidate -> 204. A worker lacking a job's required capability is never offered
// it (the claim also rejects, belt-and-suspenders), so the job stays `ready` and
// its no_eligible_worker alarm can fire (I-6).
func (s *Server) lease(w http.ResponseWriter, r *http.Request) {
	identity := r.URL.Query().Get("identity")
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
	reviewing := role == job.RoleCodeReviewer

	deadline := time.Now().Add(s.longPollWait)
	for {
		// the gate stage (code_review) is claimed from review_pending jobs; every
		// other role claims `ready` jobs. The atomic claim is the correctness
		// backstop in both cases (§6.3.1).
		var cands []scheduler.Candidate
		var err error
		if reviewing {
			cands, err = s.store.ReviewPendingCandidates(r.Context())
		} else {
			cands, err = s.store.ReadyCandidates(r.Context())
		}
		if err != nil {
			http.Error(w, "lease error", http.StatusInternalServerError)
			return
		}
		for _, cand := range scheduler.Order(cands, attested, s.clock.Now()) {
			var ls *lease.Lease
			if reviewing {
				ls, err = s.store.ClaimReviewJob(r.Context(), store.ClaimReviewParams{
					JobID: cand.JobID, LeaseID: s.minter.New(), Identity: identity,
					ModelFamily: family, Attested: attested, TTL: s.leaseTTL, Now: s.clock.Now(),
				})
			} else {
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
				}
				// same-box `worktree` provisioning hints (§7.4): only for build jobs
				// that carry a base_sha and only when a local mirror is configured.
				if s.mirrorPath != "" && j.BaseSHA != "" {
					grant.Provisioning = "worktree"
					grant.MirrorPath = s.mirrorPath
					grant.PushTarget = gitops.EpochRef(cand.JobID, ls.Epoch)
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
		if final, derr := s.store.DispatchMerge(r.Context(), s.policy, store.DispatchMergeParams{JobID: jobID, Now: s.clock.Now()}); derr == nil {
			resp.JobState = string(final)
			s.broker.Publish(LifeEvent{JobID: jobID, State: string(final), Event: "merge_dispatched", Epoch: epoch})
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) heartbeat(w http.ResponseWriter, r *http.Request) {
	jobID := r.PathValue("job")
	epoch, ok := epochFromHeader(r)
	if !ok {
		http.Error(w, "missing X-Lease-Epoch", http.StatusBadRequest)
		return
	}
	dir, err := s.store.Heartbeat(r.Context(), store.HeartbeatParams{JobID: jobID, Epoch: epoch, Now: s.clock.Now()})
	if err != nil {
		s.writeFenceError(w, err)
		return
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
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	resp, err := s.store.Result(r.Context(), store.ResultParams{
		JobID: jobID, Epoch: epoch, IdempotencyKey: idemKey, Now: s.clock.Now(),
		PushedRef: body.PushedRef,
	})
	if err != nil {
		s.writeFenceError(w, err)
		return
	}
	s.broker.Publish(LifeEvent{JobID: jobID, State: resp.JobState, Event: "result_accepted", Epoch: epoch})
	writeJSON(w, http.StatusOK, resp)
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
