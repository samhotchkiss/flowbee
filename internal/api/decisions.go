package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/samhotchkiss/flowbee/internal/auth"
	"github.com/samhotchkiss/flowbee/internal/store"
	"github.com/samhotchkiss/flowbee/internal/workintent"
)

const decisionSchemaVersion = "flowbee.decision/v1"

type decisionView struct {
	ID, ProjectID, EpicID, DeliveryID                   string                    `json:"-"`
	Kind                                                workintent.DecisionKind   `json:"kind"`
	Title, Prompt                                       string                    `json:"-"`
	Options                                             json.RawMessage           `json:"options"`
	ResponseSchema                                      json.RawMessage           `json:"response_schema"`
	ExpectedResponseKinds                               []workintent.ResponseKind `json:"expected_response_kinds"`
	Priority                                            int                       `json:"priority"`
	DueAt, DeferredUntil                                *time.Time                `json:"-"`
	DeferCondition, RequestedBy, RouteTo                string                    `json:"-"`
	SubjectArtifactRef, SubjectSHA256                   string                    `json:"-"`
	SubjectVersion, RequestVersion                      int                       `json:"-"`
	EvidenceRefs                                        json.RawMessage           `json:"evidence_refs"`
	Summary                                             string                    `json:"summary"`
	State                                               workintent.RequestState   `json:"state"`
	CurrentResponseID, SupersededBy, CancellationReason string                    `json:"-"`
	ResolvedAt                                          *time.Time                `json:"-"`
	CreatedAt, UpdatedAt                                time.Time                 `json:"-"`
}

// MarshalJSON keeps the public wire contract snake_case without coupling the
// durable store projection to one presentation format.
func (v decisionView) MarshalJSON() ([]byte, error) {
	type wire struct {
		ID                    string                    `json:"id"`
		ProjectID             string                    `json:"project_id"`
		EpicID                string                    `json:"epic_id,omitempty"`
		DeliveryID            string                    `json:"delivery_id,omitempty"`
		Kind                  workintent.DecisionKind   `json:"kind"`
		Title                 string                    `json:"title"`
		Prompt                string                    `json:"prompt"`
		Options               json.RawMessage           `json:"options"`
		ResponseSchema        json.RawMessage           `json:"response_schema"`
		ExpectedResponseKinds []workintent.ResponseKind `json:"expected_response_kinds"`
		Priority              int                       `json:"priority"`
		DueAt                 *time.Time                `json:"due_at,omitempty"`
		DeferredUntil         *time.Time                `json:"deferred_until,omitempty"`
		DeferCondition        string                    `json:"defer_condition,omitempty"`
		RequestedBy           string                    `json:"requested_by"`
		RouteTo               string                    `json:"route_to"`
		SubjectArtifactRef    string                    `json:"subject_artifact_ref"`
		SubjectSHA256         string                    `json:"subject_sha256"`
		SubjectVersion        int                       `json:"subject_version"`
		RequestVersion        int                       `json:"request_version"`
		EvidenceRefs          json.RawMessage           `json:"evidence_refs"`
		Summary               string                    `json:"summary,omitempty"`
		State                 workintent.RequestState   `json:"state"`
		CurrentResponseID     string                    `json:"current_response_id,omitempty"`
		SupersededBy          string                    `json:"superseded_by,omitempty"`
		CancellationReason    string                    `json:"cancellation_reason,omitempty"`
		ResolvedAt            *time.Time                `json:"resolved_at,omitempty"`
		CreatedAt             time.Time                 `json:"created_at"`
		UpdatedAt             time.Time                 `json:"updated_at"`
	}
	return json.Marshal(wire{
		ID: v.ID, ProjectID: v.ProjectID, EpicID: v.EpicID, DeliveryID: v.DeliveryID,
		Kind: v.Kind, Title: v.Title, Prompt: v.Prompt, Options: v.Options,
		ResponseSchema: v.ResponseSchema, ExpectedResponseKinds: v.ExpectedResponseKinds,
		Priority: v.Priority, DueAt: v.DueAt, DeferredUntil: v.DeferredUntil,
		DeferCondition: v.DeferCondition, RequestedBy: v.RequestedBy, RouteTo: v.RouteTo,
		SubjectArtifactRef: v.SubjectArtifactRef, SubjectSHA256: v.SubjectSHA256,
		SubjectVersion: v.SubjectVersion, RequestVersion: v.RequestVersion,
		EvidenceRefs: v.EvidenceRefs, Summary: v.Summary, State: v.State,
		CurrentResponseID: v.CurrentResponseID, SupersededBy: v.SupersededBy,
		CancellationReason: v.CancellationReason, ResolvedAt: v.ResolvedAt,
		CreatedAt: v.CreatedAt, UpdatedAt: v.UpdatedAt,
	})
}

func viewDecision(d store.DecisionRequest) decisionView {
	return decisionView{
		ID: d.ID, ProjectID: d.ProjectID, EpicID: d.EpicID, DeliveryID: d.DeliveryID,
		Kind: d.Kind, Title: d.Title, Prompt: d.Prompt,
		Options: json.RawMessage(d.OptionsJSON), ResponseSchema: json.RawMessage(d.ResponseSchemaJSON),
		ExpectedResponseKinds: d.ExpectedResponseKinds, Priority: d.Priority,
		DueAt: optionalTime(d.DueAt), DeferredUntil: optionalTime(d.DeferredUntil),
		DeferCondition: d.DeferCondition, RequestedBy: d.RequestedBy, RouteTo: d.RouteTo,
		SubjectArtifactRef: d.SubjectArtifactRef, SubjectSHA256: d.SubjectSHA256,
		SubjectVersion: d.SubjectVersion, RequestVersion: d.RequestVersion,
		EvidenceRefs: json.RawMessage(d.EvidenceRefsJSON), Summary: d.Summary, State: d.State,
		CurrentResponseID: d.CurrentResponseID, SupersededBy: d.SupersededBy,
		CancellationReason: d.CancellationReason, ResolvedAt: optionalTime(d.ResolvedAt),
		CreatedAt: d.CreatedAt, UpdatedAt: d.UpdatedAt,
	}
}

func optionalTime(value time.Time) *time.Time {
	if value.IsZero() {
		return nil
	}
	value = value.UTC()
	return &value
}

type createDecisionBody struct {
	ID                    string                    `json:"id"`
	ProjectID             string                    `json:"project_id"`
	EpicID                string                    `json:"epic_id"`
	DeliveryID            string                    `json:"delivery_id"`
	Kind                  workintent.DecisionKind   `json:"kind"`
	Title                 string                    `json:"title"`
	Prompt                string                    `json:"prompt"`
	Options               json.RawMessage           `json:"options"`
	ResponseSchema        json.RawMessage           `json:"response_schema"`
	EvidenceRefs          json.RawMessage           `json:"evidence_refs"`
	ExpectedResponseKinds []workintent.ResponseKind `json:"expected_response_kinds"`
	Priority              int                       `json:"priority"`
	DueAt                 time.Time                 `json:"due_at"`
	RequestedBy           string                    `json:"requested_by"`
	RouteTo               string                    `json:"route_to"`
	SubjectArtifactRef    string                    `json:"subject_artifact_ref"`
	SubjectSHA256         string                    `json:"subject_sha256"`
	SubjectVersion        int                       `json:"subject_version"`
	Summary               string                    `json:"summary"`
}

func (s *Server) decisionsList(w http.ResponseWriter, r *http.Request) {
	projectID := r.URL.Query().Get("project_id")
	if projectID == "" {
		if _, ok := s.requireHumanPortfolio(w, r, auth.HumanDecisionRead); !ok {
			return
		}
	} else if _, ok := s.requireHumanProject(w, r, projectID, auth.HumanDecisionRead); !ok {
		return
	}
	var rows []store.DecisionRequest
	var err error
	if projectID == "" {
		rows, err = s.store.ListCurrentDecisionRequestsAllProjects(r.Context())
	} else {
		rows, err = s.store.ListCurrentDecisionRequests(r.Context(), projectID)
	}
	if err != nil {
		http.Error(w, "decision list error", http.StatusInternalServerError)
		return
	}
	views := make([]decisionView, 0, len(rows))
	for _, row := range rows {
		views = append(views, viewDecision(row))
	}
	writeJSON(w, http.StatusOK, map[string]any{"schema_version": decisionSchemaVersion, "decisions": views})
}

func (s *Server) decisionOne(w http.ResponseWriter, r *http.Request) {
	projectID := r.URL.Query().Get("project_id")
	if projectID == "" {
		http.Error(w, "project_id is required", http.StatusBadRequest)
		return
	}
	if _, ok := s.requireHumanProject(w, r, projectID, auth.HumanDecisionRead); !ok {
		return
	}
	row, err := s.store.GetDecisionRequest(r.Context(), projectID, r.PathValue("id"))
	if err != nil {
		s.writeDecisionError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"schema_version": decisionSchemaVersion, "decision": viewDecision(row)})
}

func (s *Server) decisionCreate(w http.ResponseWriter, r *http.Request) {
	if !requireIdempotencyKey(w, r) {
		return
	}
	var body createDecisionBody
	if err := decodeBoundedJSON(r, &body); err != nil {
		http.Error(w, "invalid decision request", http.StatusBadRequest)
		return
	}
	principal, ok := s.requireHumanProject(w, r, body.ProjectID, auth.HumanDecisionCreate)
	if !ok {
		return
	}
	actor := principal.Identity
	if body.RequestedBy != "" && body.RequestedBy != actor {
		http.Error(w, "requested_by must match the authenticated identity", http.StatusForbidden)
		return
	}
	body.RequestedBy = actor
	row, err := s.store.CreateDecisionRequest(r.Context(), store.CreateDecisionRequestInput{
		ID: body.ID, ProjectID: body.ProjectID, EpicID: body.EpicID, DeliveryID: body.DeliveryID,
		Kind: body.Kind, Title: body.Title, Prompt: body.Prompt, Options: body.Options,
		ResponseSchema: body.ResponseSchema, ExpectedResponseKinds: body.ExpectedResponseKinds,
		Priority: body.Priority, DueAt: body.DueAt, RequestedBy: body.RequestedBy,
		RouteTo: body.RouteTo, SubjectArtifactRef: body.SubjectArtifactRef,
		SubjectSHA256: body.SubjectSHA256, SubjectVersion: body.SubjectVersion,
		EvidenceRefs: body.EvidenceRefs, Summary: body.Summary,
	}, s.clock.Now())
	if err != nil {
		s.writeDecisionError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"schema_version": decisionSchemaVersion, "decision": viewDecision(row)})
}

type decisionTransitionBody struct {
	ProjectID          string          `json:"project_id"`
	RequestVersion     int             `json:"request_version"`
	SubjectVersion     int             `json:"subject_version"`
	SubjectSHA256      string          `json:"subject_sha256"`
	StructuredValue    json.RawMessage `json:"value"`
	Comment            string          `json:"comment"`
	AuthorizationScope string          `json:"authorization_scope"`
	DeferUntil         time.Time       `json:"defer_until"`
	DeferCondition     string          `json:"defer_condition"`
}

func (s *Server) decisionView(w http.ResponseWriter, r *http.Request) {
	if !requireIdempotencyKey(w, r) {
		return
	}
	var body decisionTransitionBody
	if err := decodeBoundedJSON(r, &body); err != nil {
		http.Error(w, "invalid decision view", http.StatusBadRequest)
		return
	}
	if _, ok := s.requireHumanProject(w, r, body.ProjectID, auth.HumanDecisionView); !ok {
		return
	}
	if err := s.store.MarkDecisionRequestViewed(r.Context(), body.ProjectID, r.PathValue("id"),
		body.RequestVersion, decisionActor(r), s.clock.Now()); err != nil {
		s.writeDecisionError(w, err)
		return
	}
	row, err := s.store.GetDecisionRequest(r.Context(), body.ProjectID, r.PathValue("id"))
	if err != nil {
		s.writeDecisionError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"schema_version": decisionSchemaVersion, "decision": viewDecision(row)})
}

func requireIdempotencyKey(w http.ResponseWriter, r *http.Request) bool {
	if strings.TrimSpace(r.Header.Get("Idempotency-Key")) == "" {
		http.Error(w, "Idempotency-Key is required", http.StatusBadRequest)
		return false
	}
	return true
}

func (s *Server) decisionRespond(kind workintent.ResponseKind) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var body decisionTransitionBody
		if err := decodeBoundedJSON(r, &body); err != nil {
			http.Error(w, "invalid decision response", http.StatusBadRequest)
			return
		}
		if _, ok := s.requireHumanProject(w, r, body.ProjectID, auth.HumanDecisionRespond); !ok {
			return
		}
		key := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
		if key == "" {
			http.Error(w, "Idempotency-Key is required", http.StatusBadRequest)
			return
		}
		scope, ok := exactHumanAuthorizationScope(body.ProjectID, body.AuthorizationScope)
		if !ok {
			http.Error(w, "authorization_scope is broader than the exact project artifact", http.StatusForbidden)
			return
		}
		response, err := s.store.RespondDecision(r.Context(), body.ProjectID, store.DecisionResponseInput{
			RequestID: r.PathValue("id"), RequestVersion: body.RequestVersion,
			SubjectVersion: body.SubjectVersion, SubjectSHA256: body.SubjectSHA256, Kind: kind,
			StructuredValue: body.StructuredValue, Comment: body.Comment, ActorID: decisionActor(r),
			AuthorizationScope: scope, DeferUntil: body.DeferUntil,
			DeferCondition: body.DeferCondition, IdempotencyKey: key,
		}, s.clock.Now())
		if err != nil {
			s.writeDecisionError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"schema_version": decisionSchemaVersion,
			"response": map[string]any{
				"id": response.ID, "project_id": response.ProjectID, "request_id": response.RequestID,
				"request_version": response.RequestVersion, "subject_version": response.SubjectVersion,
				"subject_sha256": response.SubjectSHA256, "kind": response.Kind,
				"value": json.RawMessage(response.StructuredValueJSON), "comment": response.Comment,
				"actor_id": response.ActorID, "authorization_scope": response.AuthorizationScope,
				"defer_until": optionalTime(response.DeferUntil), "defer_condition": response.DeferCondition,
				"downstream_ack_state": response.DownstreamAckState,
				"idempotency_key":      response.IdempotencyKey, "created_at": response.CreatedAt,
			},
		})
	}
}

func decisionActor(r *http.Request) string {
	if principal, ok := auth.HumanPrincipalFrom(r); ok {
		return principal.Identity
	}
	if actor, ok := auth.IdentityFrom(r); ok {
		return actor
	}
	return ""
}

func decodeBoundedJSON(r *http.Request, target any) error {
	decoder := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 1<<20))
	decoder.DisallowUnknownFields()
	return decoder.Decode(target)
}

func (s *Server) writeDecisionError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, store.ErrDecisionNotFound):
		http.Error(w, "decision not found", http.StatusNotFound)
	case errors.Is(err, workintent.ErrStaleSubject):
		http.Error(w, "decision subject changed", http.StatusPreconditionFailed)
	case errors.Is(err, workintent.ErrRequestNotCurrent),
		errors.Is(err, store.ErrDecisionIdempotencyConflict):
		http.Error(w, err.Error(), http.StatusConflict)
	case errors.Is(err, store.ErrDecisionDeferralActive):
		http.Error(w, err.Error(), http.StatusTooManyRequests)
	default:
		// Domain validation is safe to expose and is distinct from infrastructure
		// failure. SQLite/driver details remain hidden.
		if strings.Contains(err.Error(), "decision") || strings.Contains(err.Error(), "response") ||
			strings.Contains(err.Error(), "authorization") || strings.Contains(err.Error(), "defer") {
			http.Error(w, err.Error(), http.StatusUnprocessableEntity)
			return
		}
		http.Error(w, "decision error", http.StatusInternalServerError)
	}
}
