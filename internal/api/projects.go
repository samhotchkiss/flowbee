package api

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/samhotchkiss/flowbee/internal/auth"
	"github.com/samhotchkiss/flowbee/internal/store"
)

func requireProjectIdempotencyKey(w http.ResponseWriter, r *http.Request) (string, bool) {
	key := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if key == "" {
		http.Error(w, "Idempotency-Key is required", http.StatusBadRequest)
		return "", false
	}
	return key, true
}

func (s *Server) projectsList(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireHumanPortfolio(w, r, auth.HumanProjectRead); !ok {
		return
	}
	projects, err := s.store.ListPortfolioProjects(r.Context())
	if err != nil {
		http.Error(w, "project list failed", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"schema_version": "flowbee.projects/v1", "projects": projects})
}

// portfolio is the Phase-2 global read model. It exposes workload and actor
// route health together so a missing Interactor/Orchestrator incarnation cannot
// look like an idle, healthy project. ETag is shared with the dashboard digest;
// project, binding, breaker, and scheduler mutations all advance that digest.
func (s *Server) portfolio(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireHumanPortfolio(w, r, auth.HumanProjectRead); !ok {
		return
	}
	seq, err := s.store.EpicDigestSeq(r.Context())
	if err != nil {
		http.Error(w, "portfolio digest failed", http.StatusInternalServerError)
		return
	}
	projects, err := s.store.ProjectDashboard(r.Context())
	if err != nil {
		http.Error(w, "portfolio read failed", http.StatusInternalServerError)
		return
	}
	store.EvaluateProjectDashboardStarvation(projects, s.clock.Now(), store.ProjectStarvationBound)
	etag := portfolioETag(seq, projects)
	if r.Header.Get("If-None-Match") == etag {
		w.Header().Set("ETag", etag)
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.Header().Set("ETag", etag)
	writeJSON(w, http.StatusOK, map[string]any{
		"schema_version": "flowbee.portfolio/v1",
		"digest_seq":     seq,
		"projects":       projects,
	})
}

func portfolioETag(seq int64, projects []store.ProjectDashboardRow) string {
	payload, _ := json.Marshal(struct {
		Seq      int64                       `json:"seq"`
		Projects []store.ProjectDashboardRow `json:"projects"`
	}{Seq: seq, Projects: projects})
	digest := sha256.Sum256(payload)
	return fmt.Sprintf(`"%d-%x"`, seq, digest[:8])
}

func (s *Server) projectOne(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("project_id")
	if _, ok := s.requireHumanProject(w, r, id, auth.HumanProjectRead); !ok {
		return
	}
	project, err := s.store.GetPortfolioProject(r.Context(), id)
	if errors.Is(err, store.ErrProjectNotFound) {
		http.Error(w, "project not found", http.StatusNotFound)
		return
	}
	if err != nil {
		http.Error(w, "project read failed", http.StatusInternalServerError)
		return
	}
	repos, err := s.store.ProjectRepoIDs(r.Context(), id, false)
	if err != nil {
		http.Error(w, "project repository read failed", http.StatusInternalServerError)
		return
	}
	actors := map[string]store.ProjectActorRoute{}
	for _, role := range []string{store.DriverInteractorRole, store.DriverOrchestratorRole} {
		if actor, err := s.store.GetProjectActor(r.Context(), id, role); err == nil {
			actors[role] = actor
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"schema_version": "flowbee.project/v1",
		"project": project, "repository_ids": repos, "actors": actors})
}

func (s *Server) projectEpics(w http.ResponseWriter, r *http.Request) {
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
	epics, err := s.store.ListEpicRunsForProject(r.Context(), projectID)
	if err != nil {
		http.Error(w, "project epics read failed", http.StatusInternalServerError)
		return
	}
	seq, err := s.store.EpicDigestSeq(r.Context())
	if err != nil {
		http.Error(w, "project epics digest failed", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"schema_version": "flowbee.project-epics/v1",
		"digest_seq":     seq,
		"project_id":     projectID,
		"epics":          epics,
	})
}

func (s *Server) projectCreate(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireHumanPortfolio(w, r, auth.HumanProjectManage); !ok {
		return
	}
	key, ok := requireProjectIdempotencyKey(w, r)
	if !ok {
		return
	}
	var body store.PortfolioProject
	if err := decodeBoundedJSON(r, &body); err != nil {
		http.Error(w, "invalid project", http.StatusBadRequest)
		return
	}
	project, err := s.store.CreatePortfolioProjectCommand(r.Context(), body, key, s.clock.Now())
	if errors.Is(err, store.ErrProjectCommandConflict) {
		http.Error(w, "Idempotency-Key conflicts with another project command", http.StatusConflict)
		return
	}
	if errors.Is(err, store.ErrProjectConflict) {
		http.Error(w, "project conflicts with existing identity", http.StatusConflict)
		return
	}
	if err != nil {
		http.Error(w, "project create failed", http.StatusInternalServerError)
		return
	}
	s.broker.Publish(LifeEvent{ProjectID: project.ID, State: "projects", Event: "project_created"})
	writeJSON(w, http.StatusCreated, map[string]any{"schema_version": "flowbee.project/v1", "project": project})
}

func (s *Server) projectState(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("project_id")
	if _, ok := s.requireHumanProject(w, r, id, auth.HumanProjectManage); !ok {
		return
	}
	key, ok := requireProjectIdempotencyKey(w, r)
	if !ok {
		return
	}
	var body struct {
		State           string `json:"state"`
		Reason          string `json:"reason"`
		ExpectedVersion int    `json:"expected_state_version"`
	}
	if err := decodeBoundedJSON(r, &body); err != nil {
		http.Error(w, "invalid project state", http.StatusBadRequest)
		return
	}
	project, err := s.store.SetPortfolioProjectStateCommand(r.Context(), id, body.State, body.Reason,
		body.ExpectedVersion, key, s.clock.Now())
	if errors.Is(err, store.ErrProjectCommandConflict) {
		http.Error(w, "Idempotency-Key conflicts with another project command", http.StatusConflict)
		return
	}
	if errors.Is(err, store.ErrProjectConflict) {
		http.Error(w, "stale or invalid project state", http.StatusPreconditionFailed)
		return
	}
	if err != nil {
		http.Error(w, "project state failed", http.StatusInternalServerError)
		return
	}
	s.broker.Publish(LifeEvent{ProjectID: project.ID, State: "projects", Event: "project_state_changed"})
	writeJSON(w, http.StatusOK, map[string]any{"schema_version": "flowbee.project/v1", "project": project})
}

func (s *Server) projectRepoAdd(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("project_id")
	if _, ok := s.requireHumanProject(w, r, id, auth.HumanProjectManage); !ok {
		return
	}
	key, ok := requireProjectIdempotencyKey(w, r)
	if !ok {
		return
	}
	var body struct {
		RepoID string `json:"repo_id"`
	}
	if err := decodeBoundedJSON(r, &body); err != nil || strings.TrimSpace(body.RepoID) == "" {
		http.Error(w, "repo_id is required", http.StatusBadRequest)
		return
	}
	if err := s.store.AddProjectRepoCommand(r.Context(), id, body.RepoID, key, s.clock.Now()); errors.Is(err, store.ErrProjectCommandConflict) {
		http.Error(w, "Idempotency-Key conflicts with another project command", http.StatusConflict)
		return
	} else if err != nil {
		http.Error(w, "project repository attach failed", http.StatusConflict)
		return
	}
	s.broker.Publish(LifeEvent{ProjectID: id, State: "projects", Event: "project_repository_attached"})
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) projectActorRegister(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("project_id")
	if _, ok := s.requireHumanProject(w, r, id, auth.HumanProjectManage); !ok {
		return
	}
	key, ok := requireProjectIdempotencyKey(w, r)
	if !ok {
		return
	}
	var body struct {
		Role    string `json:"role"`
		ActorID string `json:"actor_id"`
	}
	if err := decodeBoundedJSON(r, &body); err != nil {
		http.Error(w, "invalid project actor", http.StatusBadRequest)
		return
	}
	actor, err := s.store.RegisterProjectActorCommand(r.Context(), store.ProjectActorRoute{
		ProjectID: id, Role: body.Role, ActorID: body.ActorID,
	}, key, s.clock.Now())
	if errors.Is(err, store.ErrProjectCommandConflict) {
		http.Error(w, "Idempotency-Key conflicts with another project command", http.StatusConflict)
		return
	}
	if errors.Is(err, store.ErrProjectConflict) {
		http.Error(w, "invalid project actor", http.StatusUnprocessableEntity)
		return
	}
	if err != nil {
		http.Error(w, "project actor registration failed", http.StatusInternalServerError)
		return
	}
	s.broker.Publish(LifeEvent{ProjectID: id, State: "projects", Event: "project_actor_registered"})
	writeJSON(w, http.StatusOK, map[string]any{"schema_version": "flowbee.project-actor/v1", "actor": actor})
}
