package api

import (
	"errors"
	"net/http"
	"time"

	"github.com/samhotchkiss/flowbee/internal/auth"
	"github.com/samhotchkiss/flowbee/internal/lease"
	"github.com/samhotchkiss/flowbee/internal/store"
)

const projectBreakersSchemaVersion = "flowbee.project-circuit-breakers/v1"

type projectBreakerView struct {
	ProjectID           string `json:"project_id"`
	RepoID              string `json:"repo_id,omitempty"`
	State               string `json:"state"`
	StateVersion        int    `json:"state_version"`
	FailureKind         string `json:"failure_kind,omitempty"`
	Reason              string `json:"reason,omitempty"`
	FailureCount        int    `json:"failure_count"`
	OpenedAt            string `json:"opened_at,omitempty"`
	ProbeDueAt          string `json:"probe_due_at,omitempty"`
	ProbeOwner          string `json:"probe_owner,omitempty"`
	ProbeEpoch          int    `json:"probe_epoch"`
	ProbeLeaseExpiresAt string `json:"probe_lease_expires_at,omitempty"`
	LastRecoveryFact    string `json:"last_recovery_fact,omitempty"`
	UpdatedAt           string `json:"updated_at"`
}

func projectBreakerAPIView(in store.ProjectBreaker) projectBreakerView {
	return projectBreakerView{
		ProjectID: in.ProjectID, RepoID: in.RepoID, State: in.State, StateVersion: in.StateVersion,
		FailureKind: in.FailureKind, Reason: in.Reason, FailureCount: in.FailureCount,
		OpenedAt: apiTime(in.OpenedAt), ProbeDueAt: apiTime(in.ProbeDueAt), ProbeOwner: in.ProbeOwner,
		ProbeEpoch: in.ProbeEpoch, ProbeLeaseExpiresAt: apiTime(in.ProbeLeaseExpiresAt),
		LastRecoveryFact: in.LastRecoveryFact, UpdatedAt: apiTime(in.UpdatedAt),
	}
}

func apiTime(value time.Time) string {
	if value.IsZero() {
		return ""
	}
	return value.UTC().Format(time.RFC3339Nano)
}

// MountProjectCircuitBreakerRoutes mounts the complete, human-authenticated
// breaker API. Keeping auth inside this method prevents a future caller from
// accidentally exposing an operator control on the unauthenticated read mux.
func (s *Server) MountProjectCircuitBreakerRoutes(mux *http.ServeMux) {
	human := func(h http.HandlerFunc) http.Handler { return auth.HumanMiddleware(s.human, h) }
	mux.Handle("GET /v1/projects/{project_id}/circuit-breakers", human(s.projectBreakersList))
	mux.Handle("POST /v1/projects/{project_id}/circuit-breakers/open", human(s.projectBreakerOpen))
	mux.Handle("POST /v1/projects/{project_id}/circuit-breakers/probe-now", human(s.projectBreakerProbeNow))
}

// ProjectCircuitBreakerHandler is an isolated handler for contract tests and
// embedders. Production should call MountProjectCircuitBreakerRoutes on the
// private API mux.
func (s *Server) ProjectCircuitBreakerHandler() http.Handler {
	mux := http.NewServeMux()
	s.MountProjectCircuitBreakerRoutes(mux)
	return mux
}

func (s *Server) projectBreakersList(w http.ResponseWriter, r *http.Request) {
	projectID := r.PathValue("project_id")
	if _, ok := s.requireHumanProject(w, r, projectID, auth.HumanProjectRead); !ok {
		return
	}
	if _, err := s.store.GetPortfolioProject(r.Context(), projectID); errors.Is(err, store.ErrProjectNotFound) {
		http.Error(w, "project not found", http.StatusNotFound)
		return
	} else if err != nil {
		http.Error(w, "project read failed", http.StatusInternalServerError)
		return
	}
	breakers, err := s.store.ListProjectBreakers(r.Context(), projectID)
	if err != nil {
		http.Error(w, "project circuit breaker read failed", http.StatusInternalServerError)
		return
	}
	views := make([]projectBreakerView, 0, len(breakers))
	for _, breaker := range breakers {
		views = append(views, projectBreakerAPIView(breaker))
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"schema_version": projectBreakersSchemaVersion,
		"project_id":     projectID,
		"breakers":       views,
	})
}

type projectBreakerControlBody struct {
	RepoID               string `json:"repo_id"`
	ExpectedStateVersion int    `json:"expected_state_version"`
	Reason               string `json:"reason"`
	FailureKind          string `json:"failure_kind"`
	ProbeAfterSeconds    int64  `json:"probe_after_seconds"`
}

func (s *Server) projectBreakerOpen(w http.ResponseWriter, r *http.Request) {
	s.projectBreakerControl(w, r, "open")
}

func (s *Server) projectBreakerProbeNow(w http.ResponseWriter, r *http.Request) {
	s.projectBreakerControl(w, r, "probe_now")
}

func (s *Server) projectBreakerControl(w http.ResponseWriter, r *http.Request, action string) {
	projectID := r.PathValue("project_id")
	principal, ok := s.requireHumanProject(w, r, projectID, auth.HumanProjectManage)
	if !ok {
		return
	}
	key, ok := requireProjectIdempotencyKey(w, r)
	if !ok {
		return
	}
	var body projectBreakerControlBody
	if err := decodeBoundedJSON(r, &body); err != nil || body.ProbeAfterSeconds < 0 ||
		body.ProbeAfterSeconds > int64((30*24*time.Hour)/time.Second) ||
		(action == "probe_now" && (body.ExpectedStateVersion < 1 || body.FailureKind != "")) {
		http.Error(w, "invalid project circuit breaker control", http.StatusBadRequest)
		return
	}
	result, err := s.store.OverrideProjectBreaker(r.Context(), store.ProjectBreakerOverride{
		ProjectID: projectID, RepoID: body.RepoID, Action: action,
		ExpectedVersion: body.ExpectedStateVersion, ActorID: principal.Identity,
		IdempotencyKey: key, Reason: body.Reason, FailureKind: body.FailureKind,
		ProbeAfter: time.Duration(body.ProbeAfterSeconds) * time.Second,
	}, s.clock.Now())
	switch {
	case errors.Is(err, store.ErrProjectCommandConflict):
		http.Error(w, "Idempotency-Key conflicts with another project command", http.StatusConflict)
		return
	case errors.Is(err, lease.ErrStaleEpoch):
		http.Error(w, "stale project circuit breaker version", http.StatusPreconditionFailed)
		return
	case errors.Is(err, store.ErrProjectNotFound):
		http.Error(w, "project not found", http.StatusNotFound)
		return
	case errors.Is(err, store.ErrProjectBreakerInput):
		http.Error(w, "invalid project circuit breaker control", http.StatusUnprocessableEntity)
		return
	case err != nil:
		http.Error(w, "project circuit breaker control failed", http.StatusInternalServerError)
		return
	}
	s.broker.Publish(LifeEvent{ProjectID: projectID, State: "project_breaker", Event: "project_breaker_" + action})
	writeJSON(w, http.StatusOK, map[string]any{
		"schema_version": projectBreakersSchemaVersion,
		"breaker":        projectBreakerAPIView(result),
	})
}
