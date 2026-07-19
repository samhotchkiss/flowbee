package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/samhotchkiss/flowbee/internal/auth"
	"github.com/samhotchkiss/flowbee/internal/store"
)

const workIntentSchemaVersion = "flowbee.work-intent/v1"

type workIntentView struct {
	ID                       string          `json:"id"`
	ProjectID                string          `json:"project_id"`
	SourceConversationID     string          `json:"source_conversation_id,omitempty"`
	SourceMessageID          string          `json:"source_message_id"`
	SourceMessageVersion     int             `json:"source_message_version"`
	InteractorIncarnationID  string          `json:"interactor_incarnation_id"`
	Title                    string          `json:"title"`
	Summary                  string          `json:"summary,omitempty"`
	ArtifactRef              string          `json:"artifact_ref"`
	ArtifactSHA256           string          `json:"artifact_sha256"`
	IntentVersion            int             `json:"intent_version"`
	StateVersion             int             `json:"state_version"`
	Priority                 int             `json:"priority"`
	DefinitionComplete       bool            `json:"definition_complete"`
	DefinitionEvidence       json.RawMessage `json:"definition_evidence"`
	DependencyRefs           json.RawMessage `json:"dependency_refs"`
	State                    string          `json:"state"`
	OwnerActorID             string          `json:"owner_actor_id"`
	RouteTo                  string          `json:"route_to"`
	OrchestratorRegistration string          `json:"orchestrator_registration,omitempty"`
	DeliveryActionID         string          `json:"delivery_action_id,omitempty"`
	RouteEpoch               int             `json:"route_epoch"`
	RouteAttempts            int             `json:"route_attempts"`
	RouteDueAt               *time.Time      `json:"route_due_at,omitempty"`
	RouteAcknowledgedAt      *time.Time      `json:"route_acknowledged_at,omitempty"`
	EpicContractRef          string          `json:"epic_contract_ref,omitempty"`
	EpicContractSHA256       string          `json:"epic_contract_sha256,omitempty"`
	SubmissionIdempotencyKey string          `json:"submission_idempotency_key"`
	AdmittedEpicID           string          `json:"admitted_epic_id,omitempty"`
	HoldKind                 string          `json:"hold_kind,omitempty"`
	HoldReason               string          `json:"hold_reason,omitempty"`
	NextRetryAt              *time.Time      `json:"next_retry_at,omitempty"`
	SupersededBy             string          `json:"superseded_by,omitempty"`
	CancellationReason       string          `json:"cancellation_reason,omitempty"`
	CreatedAt                time.Time       `json:"created_at"`
	UpdatedAt                time.Time       `json:"updated_at"`
}

func viewWorkIntent(item store.WorkIntent) workIntentView {
	return workIntentView{
		ID: item.ID, ProjectID: item.ProjectID, SourceConversationID: item.SourceConversationID,
		SourceMessageID: item.SourceMessageID, SourceMessageVersion: item.SourceMessageVersion,
		InteractorIncarnationID: item.InteractorIncarnationID, Title: item.Title, Summary: item.Summary,
		ArtifactRef: item.ArtifactRef, ArtifactSHA256: item.ArtifactSHA256,
		IntentVersion: item.IntentVersion, StateVersion: item.StateVersion, Priority: item.Priority,
		DefinitionComplete: item.DefinitionComplete, DefinitionEvidence: json.RawMessage(item.DefinitionEvidenceJSON),
		DependencyRefs: json.RawMessage(item.DependencyRefsJSON), State: string(item.State),
		OwnerActorID: item.OwnerActorID, RouteTo: item.RouteTo,
		OrchestratorRegistration: item.OrchestratorRegistration, DeliveryActionID: item.DeliveryActionID,
		RouteEpoch: item.RouteEpoch, RouteAttempts: item.RouteAttempts,
		RouteDueAt: optionalTime(item.RouteDueAt), RouteAcknowledgedAt: optionalTime(item.RouteAcknowledgedAt),
		EpicContractRef: item.EpicContractRef, EpicContractSHA256: item.EpicContractSHA256,
		SubmissionIdempotencyKey: item.SubmissionIdempotencyKey, AdmittedEpicID: item.AdmittedEpicID,
		HoldKind: item.HoldKind, HoldReason: item.HoldReason, NextRetryAt: optionalTime(item.NextRetryAt),
		SupersededBy: item.SupersededBy, CancellationReason: item.CancellationReason,
		CreatedAt: item.CreatedAt, UpdatedAt: item.UpdatedAt,
	}
}

func (s *Server) workIntentsList(w http.ResponseWriter, r *http.Request) {
	projectID := r.PathValue("project_id")
	if _, ok := s.requireHumanProject(w, r, projectID, auth.HumanWorkIntentRead); !ok {
		return
	}
	rows, err := s.store.ListWorkIntents(r.Context(), projectID)
	if err != nil {
		http.Error(w, "work intent list error", http.StatusInternalServerError)
		return
	}
	views := make([]workIntentView, 0, len(rows))
	for _, row := range rows {
		views = append(views, viewWorkIntent(row))
	}
	writeJSON(w, http.StatusOK, map[string]any{"schema_version": workIntentSchemaVersion, "work_intents": views})
}

func (s *Server) workIntentOne(w http.ResponseWriter, r *http.Request) {
	projectID := r.URL.Query().Get("project_id")
	if projectID == "" {
		http.Error(w, "project_id is required", http.StatusBadRequest)
		return
	}
	if _, ok := s.requireHumanProject(w, r, projectID, auth.HumanWorkIntentRead); !ok {
		return
	}
	item, err := s.store.GetWorkIntent(r.Context(), projectID, r.PathValue("id"))
	if err != nil {
		s.writeWorkIntentError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"schema_version": workIntentSchemaVersion, "work_intent": viewWorkIntent(item)})
}

type createWorkIntentBody struct {
	ID                       string          `json:"id"`
	SourceConversationID     string          `json:"source_conversation_id"`
	SourceMessageID          string          `json:"source_message_id"`
	SourceMessageVersion     int             `json:"source_message_version"`
	InteractorIncarnationID  string          `json:"interactor_incarnation_id"`
	Title                    string          `json:"title"`
	Summary                  string          `json:"summary"`
	ArtifactRef              string          `json:"artifact_ref"`
	ArtifactSHA256           string          `json:"artifact_sha256"`
	IntentVersion            int             `json:"intent_version"`
	Priority                 int             `json:"priority"`
	DefinitionComplete       bool            `json:"definition_complete"`
	DefinitionEvidence       json.RawMessage `json:"definition_evidence"`
	DependencyRefs           json.RawMessage `json:"dependency_refs"`
	OrchestratorRegistration string          `json:"orchestrator_registration"`
	RequiredDecisionIDs      []string        `json:"required_decision_ids"`
}

func (s *Server) workIntentCreate(w http.ResponseWriter, r *http.Request) {
	if !requireIdempotencyKey(w, r) {
		return
	}
	var body createWorkIntentBody
	if err := decodeBoundedJSON(r, &body); err != nil {
		http.Error(w, "invalid work intent", http.StatusBadRequest)
		return
	}
	projectID := r.PathValue("project_id")
	principal, ok := s.requireHumanProject(w, r, projectID, auth.HumanWorkIntentCreate)
	if !ok {
		return
	}
	if projectID != "default" {
		thread, err := s.store.GetConversationThread(r.Context(), projectID, body.SourceConversationID)
		if err != nil || thread.InteractorIncarnationID != body.InteractorIncarnationID {
			http.Error(w, "work intent is not bound to the current project conversation", http.StatusPreconditionFailed)
			return
		}
		route, err := s.store.GetProjectActor(r.Context(), projectID, store.DriverOrchestratorRole)
		if err != nil || route.State != "active" {
			http.Error(w, "project has no active Orchestrator route", http.StatusConflict)
			return
		}
		body.OrchestratorRegistration = route.ActorID
	}
	item, err := s.store.CreateWorkIntent(r.Context(), store.CreateWorkIntentInput{
		ID: body.ID, ProjectID: projectID, SourceConversationID: body.SourceConversationID,
		SourceMessageID: body.SourceMessageID, SourceMessageVersion: body.SourceMessageVersion,
		InteractorIncarnationID: body.InteractorIncarnationID, Title: body.Title, Summary: body.Summary,
		ArtifactRef: body.ArtifactRef, ArtifactSHA256: body.ArtifactSHA256,
		IntentVersion: body.IntentVersion, Priority: body.Priority,
		DefinitionComplete: body.DefinitionComplete, DefinitionEvidence: body.DefinitionEvidence,
		DependencyRefs: body.DependencyRefs, OwnerActorID: principal.Identity,
		OrchestratorRegistration: body.OrchestratorRegistration,
		RequiredDecisionIDs:      body.RequiredDecisionIDs,
	}, s.clock.Now())
	if err != nil {
		s.writeWorkIntentError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"schema_version": workIntentSchemaVersion, "work_intent": viewWorkIntent(item)})
}

type workIntentTransitionBody struct {
	ProjectID      string          `json:"project_id"`
	StateVersion   int             `json:"state_version"`
	IntentVersion  int             `json:"intent_version"`
	ArtifactSHA256 string          `json:"artifact_sha256"`
	Complete       bool            `json:"complete"`
	Evidence       json.RawMessage `json:"evidence"`
	Reason         string          `json:"reason"`
	Registration   string          `json:"registration"`
}

func (s *Server) workIntentDefinition(w http.ResponseWriter, r *http.Request) {
	if !requireIdempotencyKey(w, r) {
		return
	}
	var body workIntentTransitionBody
	if err := decodeBoundedJSON(r, &body); err != nil {
		http.Error(w, "invalid work intent definition", http.StatusBadRequest)
		return
	}
	if _, ok := s.requireHumanProject(w, r, body.ProjectID, auth.HumanWorkIntentDefine); !ok {
		return
	}
	err := s.store.SetWorkIntentDefinition(r.Context(), body.ProjectID, r.PathValue("id"),
		body.StateVersion, body.IntentVersion, body.ArtifactSHA256, body.Complete, body.Evidence,
		decisionActor(r), s.clock.Now())
	s.finishWorkIntentMutation(w, r, body.ProjectID, err)
}

func (s *Server) workIntentRegisterOrchestrator(w http.ResponseWriter, r *http.Request) {
	if !requireIdempotencyKey(w, r) {
		return
	}
	var body workIntentTransitionBody
	if err := decodeBoundedJSON(r, &body); err != nil {
		http.Error(w, "invalid orchestrator registration", http.StatusBadRequest)
		return
	}
	if _, ok := s.requireHumanProject(w, r, body.ProjectID, auth.HumanWorkIntentRegister); !ok {
		return
	}
	err := s.store.RegisterWorkIntentOrchestrator(r.Context(), body.ProjectID, r.PathValue("id"),
		body.StateVersion, body.Registration, decisionActor(r), s.clock.Now())
	s.finishWorkIntentMutation(w, r, body.ProjectID, err)
}

func (s *Server) workIntentPause(w http.ResponseWriter, r *http.Request) {
	s.workIntentHoldMutation(w, r, true)
}

func (s *Server) workIntentResume(w http.ResponseWriter, r *http.Request) {
	s.workIntentHoldMutation(w, r, false)
}

func (s *Server) workIntentHoldMutation(w http.ResponseWriter, r *http.Request, pause bool) {
	if !requireIdempotencyKey(w, r) {
		return
	}
	var body workIntentTransitionBody
	if err := decodeBoundedJSON(r, &body); err != nil {
		http.Error(w, "invalid work intent transition", http.StatusBadRequest)
		return
	}
	action := auth.HumanWorkIntentResume
	if pause {
		action = auth.HumanWorkIntentPause
	}
	if _, ok := s.requireHumanProject(w, r, body.ProjectID, action); !ok {
		return
	}
	var err error
	if pause {
		err = s.store.PauseWorkIntent(r.Context(), body.ProjectID, r.PathValue("id"), body.StateVersion,
			decisionActor(r), body.Reason, s.clock.Now())
	} else {
		err = s.store.ResumeWorkIntent(r.Context(), body.ProjectID, r.PathValue("id"), body.StateVersion,
			decisionActor(r), s.clock.Now())
	}
	s.finishWorkIntentMutation(w, r, body.ProjectID, err)
}

func (s *Server) workIntentCancel(w http.ResponseWriter, r *http.Request) {
	if !requireIdempotencyKey(w, r) {
		return
	}
	var body workIntentTransitionBody
	if err := decodeBoundedJSON(r, &body); err != nil {
		http.Error(w, "invalid work intent cancellation", http.StatusBadRequest)
		return
	}
	if _, ok := s.requireHumanProject(w, r, body.ProjectID, auth.HumanWorkIntentCancel); !ok {
		return
	}
	err := s.store.CancelWorkIntent(r.Context(), body.ProjectID, r.PathValue("id"), body.StateVersion,
		decisionActor(r), body.Reason, s.clock.Now())
	s.finishWorkIntentMutation(w, r, body.ProjectID, err)
}

func (s *Server) finishWorkIntentMutation(w http.ResponseWriter, r *http.Request, projectID string, err error) {
	if err != nil {
		s.writeWorkIntentError(w, err)
		return
	}
	item, err := s.store.GetWorkIntent(r.Context(), projectID, r.PathValue("id"))
	if err != nil {
		s.writeWorkIntentError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"schema_version": workIntentSchemaVersion, "work_intent": viewWorkIntent(item)})
}

func (s *Server) writeWorkIntentError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, store.ErrWorkIntentNotFound):
		http.Error(w, "work intent not found", http.StatusNotFound)
	case errors.Is(err, store.ErrWorkIntentFenced):
		http.Error(w, err.Error(), http.StatusPreconditionFailed)
	default:
		if strings.Contains(err.Error(), "work intent") || strings.Contains(err.Error(), "required decision") {
			http.Error(w, err.Error(), http.StatusUnprocessableEntity)
			return
		}
		http.Error(w, "work intent error", http.StatusInternalServerError)
	}
}
