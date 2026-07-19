package api

import (
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/samhotchkiss/flowbee/internal/auth"
	"github.com/samhotchkiss/flowbee/internal/store"
)

const decisionAuditSchemaVersion = "flowbee.decision-audit/v1"

type decisionAuditEventView struct {
	Ref          string          `json:"ref"`
	Seq          int64           `json:"seq"`
	EpicSeq      int64           `json:"epic_seq,omitempty"`
	Kind         string          `json:"kind"`
	FromState    string          `json:"from_state,omitempty"`
	ToState      string          `json:"to_state,omitempty"`
	StateVersion int64           `json:"state_version"`
	ActorKind    string          `json:"actor_kind"`
	ActorID      string          `json:"actor_id"`
	Payload      json.RawMessage `json:"payload"`
	CreatedAt    time.Time       `json:"created_at"`
}

type decisionAuditResponseView struct {
	ID                       string                   `json:"id"`
	RequestVersion           int                      `json:"request_version"`
	RequestSHA256            string                   `json:"request_sha256"`
	SubjectVersion           int                      `json:"subject_version"`
	SubjectSHA256            string                   `json:"subject_sha256"`
	Kind                     string                   `json:"kind"`
	Value                    json.RawMessage          `json:"value"`
	Comment                  string                   `json:"comment,omitempty"`
	ActorID                  string                   `json:"actor_id"`
	AuthorizationScope       string                   `json:"authorization_scope"`
	ExpectedScope            string                   `json:"expected_authorization_scope"`
	AuthorityExact           bool                     `json:"authority_exact"`
	AuthorityFailure         string                   `json:"authority_failure,omitempty"`
	AcknowledgementState     string                   `json:"acknowledgement_state"`
	ResultingTransition      *decisionAuditEventView  `json:"resulting_transition,omitempty"`
	AcknowledgementEventRefs []decisionAuditEventView `json:"acknowledgement_events"`
	AuditRef                 string                   `json:"audit_ref,omitempty"`
	IdempotencyKey           string                   `json:"idempotency_key"`
	CreatedAt                time.Time                `json:"created_at"`
}

type decisionAuditRecordView struct {
	Request   decisionView                `json:"request"`
	Responses []decisionAuditResponseView `json:"responses"`
	Events    []decisionAuditEventView    `json:"events"`
}

// HumanDecisionAuditHandler is the project-scoped authenticated handler to
// mount at GET /v1/decisions/audit. Keeping the wrapper here makes it difficult
// for a future route registration to accidentally expose the audit ledger on
// the anonymous dashboard surface.
func (s *Server) HumanDecisionAuditHandler() http.Handler {
	return auth.HumanMiddleware(s.human, http.HandlerFunc(s.decisionAuditExport))
}

func (s *Server) decisionAuditExport(w http.ResponseWriter, r *http.Request) {
	projectID := r.URL.Query().Get("project_id")
	if _, ok := s.requireHumanProject(w, r, projectID, auth.HumanDecisionRead); !ok {
		return
	}
	rows, err := s.store.ListDecisionAudit(r.Context(), projectID, r.URL.Query().Get("decision_id"))
	if err != nil {
		s.writeDecisionError(w, err)
		return
	}
	views := make([]decisionAuditRecordView, 0, len(rows))
	for _, row := range rows {
		view := decisionAuditRecordView{Request: viewDecision(row.Request),
			Responses: make([]decisionAuditResponseView, 0, len(row.Responses)),
			Events:    auditEventViews(row.Events)}
		for _, response := range row.Responses {
			entry := decisionAuditResponseView{
				ID: response.Response.ID, RequestVersion: response.Response.RequestVersion,
				RequestSHA256: response.RequestSHA256, SubjectVersion: response.Response.SubjectVersion,
				SubjectSHA256: response.Response.SubjectSHA256, Kind: string(response.Response.Kind),
				Value: json.RawMessage(response.Response.StructuredValueJSON), Comment: response.Response.Comment,
				ActorID: response.Response.ActorID, AuthorizationScope: response.Response.AuthorizationScope,
				ExpectedScope: response.ExpectedScope, AuthorityExact: response.AuthorityExact,
				AuthorityFailure: response.AuthorityFailure, AcknowledgementState: response.AcknowledgementState,
				AcknowledgementEventRefs: auditEventViews(response.AcknowledgementEventRefs),
				AuditRef:                 response.Response.AuditRef, IdempotencyKey: response.Response.IdempotencyKey,
				CreatedAt: response.Response.CreatedAt,
			}
			if response.ResultingTransition != nil {
				transition := auditEventView(*response.ResultingTransition)
				entry.ResultingTransition = &transition
			}
			view.Responses = append(view.Responses, entry)
		}
		views = append(views, view)
	}
	writeJSON(w, http.StatusOK, map[string]any{"schema_version": decisionAuditSchemaVersion,
		"project_id": projectID, "decisions": views})
}

func auditEventViews(events []store.DecisionAuditEvent) []decisionAuditEventView {
	out := make([]decisionAuditEventView, 0, len(events))
	for _, event := range events {
		out = append(out, auditEventView(event))
	}
	return out
}

func auditEventView(event store.DecisionAuditEvent) decisionAuditEventView {
	return decisionAuditEventView{Ref: "control-event:" + strconv.FormatInt(event.Seq, 10), Seq: event.Seq,
		EpicSeq: event.EpicSeq, Kind: event.Kind, FromState: event.FromState, ToState: event.ToState,
		StateVersion: event.StateVersion, ActorKind: event.ActorKind, ActorID: event.ActorID,
		Payload: json.RawMessage(event.PayloadJSON), CreatedAt: event.CreatedAt}
}
