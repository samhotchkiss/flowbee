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

	leaseTTL     time.Duration
	longPollWait time.Duration
	pollInterval time.Duration
}

// Config carries the timing knobs the worker API needs.
type Config struct {
	LeaseTTL           time.Duration
	HeartbeatInterval  time.Duration
	LongPollWait       time.Duration
	LeaseTTLS          int
	HeartbeatIntervalS int
}

func New(st *store.Store, clk clock.Clock, minter *ulid.Minter, cfg Config, version string) *Server {
	poll := 25 * time.Millisecond
	return &Server{
		store:        st,
		clock:        clk,
		minter:       minter,
		registry:     worker.NewRegistry(st, cfg.LeaseTTLS, cfg.HeartbeatIntervalS),
		broker:       NewBroker(),
		version:      version,
		leaseTTL:     cfg.LeaseTTL,
		longPollWait: cfg.LongPollWait,
		pollInterval: poll,
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
	mux.HandleFunc("POST /v1/jobs/{job}/release", s.release)
	mux.HandleFunc("GET /v1/events", s.eventsHandler)
	mux.HandleFunc("GET /", s.board)
	return mux
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

	deadline := time.Now().Add(s.longPollWait)
	for {
		cands, err := s.store.ReadyCandidates(r.Context())
		if err != nil {
			http.Error(w, "lease error", http.StatusInternalServerError)
			return
		}
		role := roleFilter
		if role == "" {
			role = job.RoleEngWorker
		}
		for _, cand := range scheduler.Order(cands, attested, s.clock.Now()) {
			ls, err := s.store.ClaimReadyJob(r.Context(), store.ClaimParams{
				JobID:       cand.JobID,
				LeaseID:     s.minter.New(),
				Identity:    identity,
				ModelFamily: family,
				Role:        role,
				Attested:    attested,
				TTL:         s.leaseTTL,
				Now:         s.clock.Now(),
			})
			if err == nil {
				j, _ := s.store.GetJob(r.Context(), cand.JobID)
				s.broker.Publish(LifeEvent{JobID: cand.JobID, State: string(j.State), Event: "lease_claimed", Epoch: ls.Epoch})
				writeJSON(w, http.StatusOK, LeaseGrant{
					JobID: cand.JobID, Kind: string(j.Kind), Role: string(j.Role),
					BaseSHA: j.BaseSHA, LeaseID: ls.LeaseID, LeaseEpoch: ls.Epoch,
					LeaseTTLS: int(s.leaseTTL / time.Second), Deadline: ls.Deadline.Format(time.RFC3339Nano),
				})
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
	resp, err := s.store.Result(r.Context(), store.ResultParams{
		JobID: jobID, Epoch: epoch, IdempotencyKey: idemKey, Now: s.clock.Now(),
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
