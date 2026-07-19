package store

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/samhotchkiss/flowbee/internal/workintent"
)

// DecisionAuditEvent is an immutable-ledger reference included in the human
// decision export. Seq is the stable global control-event cursor; EpicSeq is the
// stable per-epic cursor when the decision is attached to an epic.
type DecisionAuditEvent struct {
	Seq, EpicSeq, StateVersion int64
	Kind, FromState, ToState   string
	ActorKind, ActorID         string
	PayloadJSON                string
	CreatedAt                  time.Time
}

// DecisionAuditResponse projects one immutable response together with the
// exact request/artifact fence that authorized it and the resulting transition.
// AuthorityExact is deliberately derived fail-closed: malformed historical
// data can be exported, but can never be presented as valid authorization.
type DecisionAuditResponse struct {
	Response                 DecisionResponse
	RequestSHA256            string
	ExpectedScope            string
	AuthorityExact           bool
	AuthorityFailure         string
	AcknowledgementState     string
	ResultingTransition      *DecisionAuditEvent
	AcknowledgementEventRefs []DecisionAuditEvent
}

// DecisionAuditRecord is a project-scoped read model. DecisionRequest and
// DecisionResponse remain the durable sources of truth; this projection never
// mutates state and never upgrades an invalid response into authority.
type DecisionAuditRecord struct {
	Request   DecisionRequest
	Responses []DecisionAuditResponse
	Events    []DecisionAuditEvent
}

// ListDecisionAudit exports the full typed-decision trail for one exact project.
// decisionID may be empty to export every decision in that project. There is no
// cross-project fallback: portfolio callers must enumerate projects they are
// independently authorized to read.
func (s *Store) ListDecisionAudit(ctx context.Context, projectID, decisionID string) ([]DecisionAuditRecord, error) {
	if projectID == "" {
		return nil, errors.New("decision audit project is required")
	}
	requests, err := s.listDecisionRequests(ctx, ` WHERE project_id=? AND (?='' OR id=?)
		ORDER BY created_at,id`, projectID, decisionID, decisionID)
	if err != nil {
		return nil, err
	}
	if decisionID != "" && len(requests) == 0 {
		return nil, ErrDecisionNotFound
	}
	out := make([]DecisionAuditRecord, 0, len(requests))
	for _, request := range requests {
		responses, err := s.listDecisionAuditResponses(ctx, request)
		if err != nil {
			return nil, err
		}
		events, err := s.listDecisionAuditEvents(ctx, request.ProjectID, request.ID)
		if err != nil {
			return nil, err
		}
		projectDecisionAudit(request, responses, events)
		out = append(out, DecisionAuditRecord{Request: request, Responses: responses, Events: events})
	}
	return out, nil
}

func (s *Store) listDecisionAuditResponses(ctx context.Context, request DecisionRequest) ([]DecisionAuditResponse, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT id,project_id,request_id,request_version,subject_version,
		subject_sha256,kind,structured_value_json,comment,actor_id,authorization_scope,defer_until,
		defer_condition,downstream_ack_state,audit_ref,idempotency_key,created_at
		FROM decision_responses WHERE project_id=? AND request_id=? ORDER BY created_at,id`,
		request.ProjectID, request.ID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []DecisionAuditResponse
	for rows.Next() {
		var response DecisionResponse
		var kind, deferUntil, created string
		if err := rows.Scan(&response.ID, &response.ProjectID, &response.RequestID,
			&response.RequestVersion, &response.SubjectVersion, &response.SubjectSHA256, &kind,
			&response.StructuredValueJSON, &response.Comment, &response.ActorID,
			&response.AuthorizationScope, &deferUntil, &response.DeferCondition,
			&response.DownstreamAckState, &response.AuditRef, &response.IdempotencyKey,
			&created); err != nil {
			return nil, err
		}
		response.Kind = workintent.ResponseKind(kind)
		response.DeferUntil, response.CreatedAt = parseOptionalTime(deferUntil), parseOptionalTime(created)
		out = append(out, DecisionAuditResponse{Response: response,
			RequestSHA256: request.SubjectSHA256, ExpectedScope: "project:" + request.ProjectID,
			AcknowledgementState: response.DownstreamAckState})
	}
	return out, rows.Err()
}

func (s *Store) listDecisionAuditEvents(ctx context.Context, projectID, decisionID string) ([]DecisionAuditEvent, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT seq,epic_seq,state_version,kind,from_state,to_state,
		actor_kind,actor_id,payload_json,created_at FROM control_events
		WHERE project_id=? AND json_extract(payload_json,'$.decision_id')=? ORDER BY seq`,
		projectID, decisionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []DecisionAuditEvent
	for rows.Next() {
		var event DecisionAuditEvent
		var created string
		if err := rows.Scan(&event.Seq, &event.EpicSeq, &event.StateVersion, &event.Kind,
			&event.FromState, &event.ToState, &event.ActorKind, &event.ActorID,
			&event.PayloadJSON, &created); err != nil {
			return nil, err
		}
		event.CreatedAt = parseOptionalTime(created)
		out = append(out, event)
	}
	return out, rows.Err()
}

func projectDecisionAudit(request DecisionRequest, responses []DecisionAuditResponse, events []DecisionAuditEvent) {
	for i := range responses {
		entry := &responses[i]
		response := entry.Response
		entry.AuthorityExact = true
		switch {
		case response.ProjectID != request.ProjectID || response.RequestID != request.ID:
			entry.AuthorityExact, entry.AuthorityFailure = false, "response is outside the exact request"
		case response.RequestVersion < 1 || response.RequestVersion > request.RequestVersion:
			entry.AuthorityExact, entry.AuthorityFailure = false, "request version is not an issued version"
		case response.SubjectVersion != request.SubjectVersion || response.SubjectSHA256 != request.SubjectSHA256:
			entry.AuthorityExact, entry.AuthorityFailure = false, "response artifact differs from the displayed artifact"
		case response.AuthorizationScope != entry.ExpectedScope:
			entry.AuthorityExact, entry.AuthorityFailure = false, "authorization scope is not the exact project"
		}

		for j := range events {
			event := events[j]
			var payload struct {
				ResponseID     string `json:"response_id"`
				RequestVersion int    `json:"request_version"`
				SubjectVersion int    `json:"subject_version"`
				SubjectSHA256  string `json:"subject_sha256"`
			}
			if json.Unmarshal([]byte(event.PayloadJSON), &payload) != nil || payload.ResponseID != response.ID {
				continue
			}
			switch event.Kind {
			case "decision_response_recorded":
				copyEvent := event
				entry.ResultingTransition = &copyEvent
				if event.StateVersion != int64(response.RequestVersion) ||
					payload.RequestVersion != response.RequestVersion ||
					payload.SubjectVersion != response.SubjectVersion ||
					payload.SubjectSHA256 != response.SubjectSHA256 {
					entry.AuthorityExact = false
					entry.AuthorityFailure = "resulting transition does not match the response fence"
				}
			case "decision_response_acknowledged":
				entry.AcknowledgementState = "acknowledged"
				entry.AcknowledgementEventRefs = append(entry.AcknowledgementEventRefs, event)
			case "decision_response_ack_failed":
				entry.AcknowledgementState = "failed"
				entry.AcknowledgementEventRefs = append(entry.AcknowledgementEventRefs, event)
			default:
				// Future acknowledgement-hop kinds remain inspectable in Events but
				// cannot silently upgrade the closed acknowledgement state machine.
			}
		}
		if entry.ResultingTransition == nil {
			entry.AuthorityExact = false
			entry.AuthorityFailure = "response has no matching resulting transition"
		}
		if entry.AcknowledgementState != "pending" && entry.AcknowledgementState != "acknowledged" && entry.AcknowledgementState != "failed" {
			entry.AcknowledgementState = "failed"
			entry.AuthorityExact = false
			entry.AuthorityFailure = "response has an invalid acknowledgement state"
		}
	}
}
