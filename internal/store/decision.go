package store

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/samhotchkiss/flowbee/internal/ulid"
	"github.com/samhotchkiss/flowbee/internal/workintent"
)

const (
	maxDecisionTitle   = 200
	maxDecisionPrompt  = 20_000
	maxDecisionSummary = 2_000
	maxDecisionComment = 10_000
)

var (
	ErrDecisionNotFound            = errors.New("decision request not found")
	ErrDecisionIdempotencyConflict = errors.New("decision response idempotency key reused with different content")
	ErrDecisionDeferralActive      = errors.New("decision deferral is still active")
)

// DecisionRequest is the durable dashboard inbox projection. SubjectVersion and
// SubjectSHA256 identify exactly what was shown to the human; neither is mutable.
type DecisionRequest struct {
	ID, ProjectID, EpicID, DeliveryID                   string
	Kind                                                workintent.DecisionKind
	Title, Prompt, OptionsJSON, ResponseSchemaJSON      string
	ExpectedResponseKinds                               []workintent.ResponseKind
	Priority                                            int
	DueAt, DeferredUntil                                time.Time
	DeferCondition, RequestedBy, RouteTo                string
	SubjectArtifactRef, SubjectSHA256                   string
	SubjectVersion, RequestVersion                      int
	EvidenceRefsJSON, Summary                           string
	State                                               workintent.RequestState
	CurrentResponseID, SupersededBy, CancellationReason string
	ResolvedAt, CreatedAt, UpdatedAt                    time.Time
}

type CreateDecisionRequestInput struct {
	ID, ProjectID, EpicID, DeliveryID     string
	Kind                                  workintent.DecisionKind
	Title, Prompt                         string
	Options, ResponseSchema, EvidenceRefs json.RawMessage
	ExpectedResponseKinds                 []workintent.ResponseKind
	Priority                              int
	DueAt                                 time.Time
	RequestedBy, RouteTo                  string
	SubjectArtifactRef, SubjectSHA256     string
	SubjectVersion                        int
	Summary                               string
}

// DecisionResponse is immutable evidence. Current request state is projected on
// decision_requests, while every response remains available for audit/replay.
type DecisionResponse struct {
	ID, ProjectID, RequestID, SubjectSHA256      string
	RequestVersion, SubjectVersion               int
	Kind                                         workintent.ResponseKind
	StructuredValueJSON, Comment, ActorID        string
	AuthorizationScope, DeferCondition           string
	DeferUntil                                   time.Time
	DownstreamAckState, AuditRef, IdempotencyKey string
	CreatedAt                                    time.Time
}

type DecisionResponseInput struct {
	RequestID, SubjectSHA256             string
	RequestVersion, SubjectVersion       int
	Kind                                 workintent.ResponseKind
	StructuredValue                      json.RawMessage
	Comment, ActorID, AuthorizationScope string
	DeferUntil                           time.Time
	DeferCondition, IdempotencyKey       string
}

// DecisionInboxRow is the dashboard read projection for one typed human
// decision. DecisionRequest remains the durable authority; the extra fields are
// derived from its required work-intent edge and immutable current response so
// the UI can explain what is blocked and whether the response reached its next
// hop without trying to infer either fact in the browser.
type DecisionInboxRow struct {
	Request            DecisionRequest
	Blocking           bool
	ViewedAt           time.Time
	ResponseKind       workintent.ResponseKind
	ResponseActorID    string
	DownstreamAckState string
	ResponseCreatedAt  time.Time
}

func (s *Store) CreateDecisionRequest(ctx context.Context, in CreateDecisionRequestInput, now time.Time) (DecisionRequest, error) {
	if in.ID == "" {
		in.ID = "decision-" + ulid.New()
	}
	if in.ProjectID == "" || in.RequestedBy == "" || in.RouteTo == "" {
		return DecisionRequest{}, errors.New("decision project, requester, and route are required")
	}
	if in.Title == "" || len(in.Title) > maxDecisionTitle || in.Prompt == "" || len(in.Prompt) > maxDecisionPrompt || len(in.Summary) > maxDecisionSummary {
		return DecisionRequest{}, errors.New("decision title, prompt, or summary is missing or exceeds its bound")
	}
	if !validDecisionKind(in.Kind) || in.SubjectArtifactRef == "" || in.SubjectVersion < 1 || !validSHA256(in.SubjectSHA256) {
		return DecisionRequest{}, errors.New("decision kind and exact subject artifact identity are required")
	}
	if in.Priority == 0 {
		in.Priority = 3
	}
	if in.Priority < 1 || in.Priority > 5 {
		return DecisionRequest{}, errors.New("decision priority must be between 1 and 5")
	}
	expected, err := normalizeResponseKinds(in.ExpectedResponseKinds)
	if err != nil {
		return DecisionRequest{}, err
	}
	options, err := normalizedJSON(in.Options, "[]")
	if err != nil {
		return DecisionRequest{}, fmt.Errorf("decision options: %w", err)
	}
	schema, err := normalizedJSON(in.ResponseSchema, "{}")
	if err != nil {
		return DecisionRequest{}, fmt.Errorf("decision response schema: %w", err)
	}
	evidence, err := normalizedJSON(in.EvidenceRefs, "[]")
	if err != nil {
		return DecisionRequest{}, fmt.Errorf("decision evidence references: %w", err)
	}
	nowText := now.UTC().Format(rfc3339)
	dueText := formatOptionalTime(in.DueAt)
	err = s.tx(ctx, func(tx *sql.Tx) error {
		if err := validateDecisionProjectRefsTx(ctx, tx, in.ProjectID, in.EpicID, in.DeliveryID); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO decision_requests
			(id,project_id,epic_id,delivery_id,kind,title,prompt,options_json,response_schema_json,
			 expected_response_kinds_json,priority,due_at,requested_by,route_to,subject_artifact_ref,
			 subject_version,subject_sha256,evidence_refs_json,summary,state,request_version,created_at,updated_at)
			VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,'open',1,?,?)`,
			in.ID, in.ProjectID, nullableText(in.EpicID), nullableText(in.DeliveryID), in.Kind,
			in.Title, in.Prompt, options, schema, expected, in.Priority, dueText, in.RequestedBy,
			in.RouteTo, in.SubjectArtifactRef, in.SubjectVersion, in.SubjectSHA256, evidence,
			in.Summary, nowText, nowText)
		if err != nil {
			return err
		}
		payload, _ := json.Marshal(map[string]any{"decision_id": in.ID, "kind": in.Kind,
			"request_version": 1, "subject_version": in.SubjectVersion, "subject_sha256": in.SubjectSHA256})
		return appendDecisionControlEventTx(ctx, tx, in.ProjectID, in.EpicID, "decision_request_created", "", "open", 1, "flowbee", in.RequestedBy, string(payload), now)
	})
	if err != nil {
		return DecisionRequest{}, err
	}
	return s.GetDecisionRequest(ctx, in.ProjectID, in.ID)
}

func (s *Store) GetDecisionRequest(ctx context.Context, projectID, id string) (DecisionRequest, error) {
	return scanDecisionRequest(s.DB.QueryRowContext(ctx, decisionRequestSelect+` WHERE project_id=? AND id=?`, projectID, id))
}

// ListCurrentDecisionRequests returns only actionable inbox rows for one project.
// Deferred requests remain visible but are not current until explicitly reopened.
func (s *Store) ListCurrentDecisionRequests(ctx context.Context, projectID string) ([]DecisionRequest, error) {
	return s.listDecisionRequests(ctx, `
		WHERE project_id=? AND state IN ('open','viewed','deferred')
		ORDER BY priority, CASE WHEN due_at='' THEN 1 ELSE 0 END, due_at, created_at, id`, projectID)

}

// ListCurrentDecisionRequestsAllProjects is the global Needs You projection.
// Ordering is deterministic across projects so one noisy project cannot reorder
// another project's equally urgent request between refreshes.
func (s *Store) ListCurrentDecisionRequestsAllProjects(ctx context.Context) ([]DecisionRequest, error) {
	return s.listDecisionRequests(ctx, `
		WHERE state IN ('open','viewed','deferred')
		ORDER BY priority, CASE WHEN due_at='' THEN 1 ELSE 0 END, due_at, created_at,project_id,id`)
}

// ListDecisionInboxAllProjects returns every current request plus a bounded
// recent terminal trail. Current and terminal rows are deliberately fetched
// separately: a burst of resolved history can never evict an actionable item.
// Presentation ordering is performed by the web projection because it includes
// derived blocking and wall-clock urgency; both result sets are nevertheless
// stable to make non-web consumers deterministic too.
func (s *Store) ListDecisionInboxAllProjects(ctx context.Context, resolvedLimit int) ([]DecisionInboxRow, error) {
	current, err := s.ListCurrentDecisionRequestsAllProjects(ctx)
	if err != nil {
		return nil, err
	}
	if resolvedLimit < 0 {
		resolvedLimit = 0
	}
	resolved := []DecisionRequest{}
	if resolvedLimit > 0 {
		resolved, err = s.listDecisionRequests(ctx, `
			WHERE state NOT IN ('open','viewed','deferred')
			ORDER BY updated_at DESC, project_id, id
			LIMIT ?`, resolvedLimit)
		if err != nil {
			return nil, err
		}
	}
	requests := append(current, resolved...)
	out := make([]DecisionInboxRow, 0, len(requests))
	for _, request := range requests {
		row := DecisionInboxRow{Request: request, Blocking: request.EpicID != "" || request.DeliveryID != ""}
		var required int
		if err := s.DB.QueryRowContext(ctx, `SELECT EXISTS(
			SELECT 1 FROM work_intent_decisions WHERE decision_id=? AND required=1
		)`, request.ID).Scan(&required); err != nil {
			return nil, err
		}
		row.Blocking = row.Blocking || required == 1
		var viewedAt string
		err := s.DB.QueryRowContext(ctx, `SELECT COALESCE(MAX(created_at),'') FROM control_events
			WHERE project_id=? AND kind='decision_request_viewed'
			AND json_extract(payload_json,'$.decision_id')=?`, request.ProjectID, request.ID).Scan(&viewedAt)
		if err != nil {
			return nil, err
		}
		row.ViewedAt = parseOptionalTime(viewedAt)
		if request.CurrentResponseID != "" {
			var kind, created, actionState string
			err := s.DB.QueryRowContext(ctx, `SELECT r.kind,r.actor_id,r.downstream_ack_state,r.created_at,
				COALESCE((SELECT a.state FROM decision_response_actions a
				 WHERE a.response_id=r.id AND a.state<>'fenced'
				 ORDER BY a.created_at DESC,a.id DESC LIMIT 1),'')
				FROM decision_responses r WHERE r.id=? AND r.project_id=? AND r.request_id=?`,
				request.CurrentResponseID, request.ProjectID, request.ID).
				Scan(&kind, &row.ResponseActorID, &row.DownstreamAckState, &created, &actionState)
			if err != nil {
				return nil, err
			}
			switch actionState {
			case "acknowledged":
				row.DownstreamAckState = "acknowledged"
			case "dead_letter":
				row.DownstreamAckState = "failed"
			}
			row.ResponseKind = workintent.ResponseKind(kind)
			row.ResponseCreatedAt = parseOptionalTime(created)
		}
		out = append(out, row)
	}
	return out, nil
}

func (s *Store) listDecisionRequests(ctx context.Context, suffix string, args ...any) ([]DecisionRequest, error) {
	rows, err := s.DB.QueryContext(ctx, decisionRequestSelect+suffix, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []DecisionRequest
	for rows.Next() {
		r, err := scanDecisionRequest(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// MarkDecisionRequestViewed records dashboard acknowledgement without resolving
// the request or changing the artifact fence. Refresh/retry is idempotent.
func (s *Store) MarkDecisionRequestViewed(ctx context.Context, projectID, id string, expectedVersion int, actorID string, now time.Time) error {
	if projectID == "" || id == "" || expectedVersion < 1 || actorID == "" {
		return errors.New("viewed decision identity, version, and actor are required")
	}
	return s.tx(ctx, func(tx *sql.Tx) error {
		var epicID, state string
		var version int
		if err := tx.QueryRowContext(ctx, `SELECT COALESCE(epic_id,''),state,request_version
			FROM decision_requests WHERE project_id=? AND id=?`, projectID, id).
			Scan(&epicID, &state, &version); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrDecisionNotFound
			}
			return err
		}
		if version != expectedVersion {
			return workintent.ErrRequestNotCurrent
		}
		if state == string(workintent.RequestViewed) {
			return nil
		}
		if state != string(workintent.RequestOpen) {
			return workintent.ErrRequestNotCurrent
		}
		nowText := now.UTC().Format(rfc3339)
		res, err := tx.ExecContext(ctx, `UPDATE decision_requests SET state='viewed',updated_at=?
			WHERE project_id=? AND id=? AND request_version=? AND state='open'`, nowText,
			projectID, id, expectedVersion)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return workintent.ErrRequestNotCurrent
		}
		payload, _ := json.Marshal(map[string]any{"decision_id": id, "request_version": expectedVersion})
		return appendDecisionControlEventTx(ctx, tx, projectID, epicID, "decision_request_viewed",
			"open", "viewed", expectedVersion, "human", actorID, string(payload), now)
	})
}

// RespondDecision atomically appends immutable human evidence and advances the
// request projection. Exact retries return the original row; reusing the same key
// with changed content is a conflict even after the request has resolved.
func (s *Store) RespondDecision(ctx context.Context, projectID string, in DecisionResponseInput, now time.Time) (DecisionResponse, error) {
	if projectID == "" || in.RequestID == "" || in.IdempotencyKey == "" || in.ActorID == "" {
		return DecisionResponse{}, errors.New("decision response project, request, actor, and idempotency key are required")
	}
	if len(in.Comment) > maxDecisionComment || !validSHA256(in.SubjectSHA256) {
		return DecisionResponse{}, errors.New("decision response comment or subject hash is invalid")
	}
	value, err := normalizedJSON(in.StructuredValue, "{}")
	if err != nil {
		return DecisionResponse{}, fmt.Errorf("decision response value: %w", err)
	}
	var out DecisionResponse
	err = s.tx(ctx, func(tx *sql.Tx) error {
		existing, found, err := getDecisionResponseByKeyTx(ctx, tx, projectID, in.RequestID, in.IdempotencyKey)
		if err != nil {
			return err
		}
		if found {
			if !sameDecisionResponse(existing, in, value) {
				return ErrDecisionIdempotencyConflict
			}
			out = existing
			return nil
		}
		req, err := scanDecisionRequest(tx.QueryRowContext(ctx, decisionRequestSelect+` WHERE project_id=? AND id=?`, projectID, in.RequestID))
		if err != nil {
			return err
		}
		coreReq := workintent.DecisionRequest{ID: req.ID, ProjectID: req.ProjectID, Kind: req.Kind,
			State: req.State, RequestVersion: req.RequestVersion, SubjectVersion: req.SubjectVersion,
			SubjectSHA256: req.SubjectSHA256, ExpectedResponseKind: req.ExpectedResponseKinds}
		coreResponse := workintent.DecisionResponse{RequestID: in.RequestID, RequestVersion: in.RequestVersion,
			SubjectVersion: in.SubjectVersion, SubjectSHA256: in.SubjectSHA256, Kind: in.Kind,
			IdempotencyKey: in.IdempotencyKey, ActorID: in.ActorID, Authorization: in.AuthorizationScope,
			StructuredValue: value, DeferUntil: in.DeferUntil, DeferCondition: in.DeferCondition}
		if err := workintent.ValidateResponse(coreReq, coreResponse); err != nil {
			return err
		}
		to := workintent.ResultingRequestState(in.Kind)
		if to == "" {
			return workintent.ErrResponseNotAllowed
		}
		responseID := "decision-response-" + stableID(projectID+":"+in.RequestID+":"+in.IdempotencyKey)
		nowText := now.UTC().Format(rfc3339)
		deferText := formatOptionalTime(in.DeferUntil)
		_, err = tx.ExecContext(ctx, `INSERT INTO decision_responses
			(id,project_id,request_id,request_version,subject_version,subject_sha256,kind,
			 structured_value_json,comment,actor_id,authorization_scope,defer_until,defer_condition,
			 downstream_ack_state,audit_ref,idempotency_key,created_at)
			VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,'pending','',?,?)`, responseID, projectID, in.RequestID,
			in.RequestVersion, in.SubjectVersion, in.SubjectSHA256, in.Kind, value, in.Comment,
			in.ActorID, in.AuthorizationScope, deferText, in.DeferCondition, in.IdempotencyKey, nowText)
		if err != nil {
			return err
		}
		res, err := tx.ExecContext(ctx, `UPDATE decision_requests SET state=?,current_response_id=?,
			deferred_until=?,defer_condition=?,resolved_at=?,updated_at=?
			WHERE project_id=? AND id=? AND request_version=? AND state IN ('open','viewed')`,
			to, responseID, deferText, in.DeferCondition, nowText, nowText, projectID, in.RequestID, in.RequestVersion)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return workintent.ErrRequestNotCurrent
		}
		payload, _ := json.Marshal(map[string]any{"decision_id": in.RequestID, "response_id": responseID,
			"response_kind": in.Kind, "request_version": in.RequestVersion, "subject_version": in.SubjectVersion,
			"subject_sha256": in.SubjectSHA256})
		if err := appendDecisionControlEventTx(ctx, tx, projectID, req.EpicID, "decision_response_recorded", string(req.State), string(to), in.RequestVersion, "human", in.ActorID, string(payload), now); err != nil {
			return err
		}
		// Commit the immutable Driver notification action in the same transaction
		// whenever the exact project Interactor is live. If it is temporarily
		// unbound, ensureDecisionResponseActionTx commits a visible durable hold;
		// the response itself is never discarded and the reconciler recovers it.
		if _, _, err := s.ensureDecisionResponseActionTx(ctx, tx, projectID, responseID, now); err != nil {
			return err
		}
		out = DecisionResponse{ID: responseID, ProjectID: projectID, RequestID: in.RequestID,
			RequestVersion: in.RequestVersion, SubjectVersion: in.SubjectVersion, SubjectSHA256: in.SubjectSHA256,
			Kind: in.Kind, StructuredValueJSON: value, Comment: in.Comment, ActorID: in.ActorID,
			AuthorizationScope: in.AuthorizationScope, DeferUntil: in.DeferUntil.UTC(), DeferCondition: in.DeferCondition,
			DownstreamAckState: "pending", IdempotencyKey: in.IdempotencyKey, CreatedAt: now.UTC()}
		return nil
	})
	return out, err
}

func (s *Store) CancelDecisionRequest(ctx context.Context, projectID, id string, expectedVersion int, actorID, reason string, now time.Time) error {
	if projectID == "" || id == "" || expectedVersion < 1 || actorID == "" || strings.TrimSpace(reason) == "" {
		return errors.New("decision cancellation identity, version, actor, and reason are required")
	}
	return s.transitionCurrentDecision(ctx, projectID, id, expectedVersion, workintent.RequestCancelled, "decision_request_cancelled", actorID, reason, "", now)
}

// SupersedeDecisionRequest closes an old request only after proving its replacement
// is a distinct request in the same project. The replacement is created first so
// the inbox can never lose the decision obligation between commits.
func (s *Store) SupersedeDecisionRequest(ctx context.Context, projectID, id string, expectedVersion int, replacementID, actorID, reason string, now time.Time) error {
	if replacementID == "" || replacementID == id {
		return errors.New("a distinct replacement decision is required")
	}
	return s.tx(ctx, func(tx *sql.Tx) error {
		var replacementProject string
		if err := tx.QueryRowContext(ctx, `SELECT project_id FROM decision_requests WHERE id=?`, replacementID).Scan(&replacementProject); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrDecisionNotFound
			}
			return err
		}
		if replacementProject != projectID {
			return ErrDecisionNotFound
		}
		return transitionCurrentDecisionTx(ctx, tx, projectID, id, expectedVersion, workintent.RequestSuperseded,
			"decision_request_superseded", actorID, reason, replacementID, now)
	})
}

// ReopenDeferredDecision advances request_version. Any response prepared against
// the earlier deferred incarnation is therefore rejected by the normal subject fence.
func (s *Store) ReopenDeferredDecision(ctx context.Context, projectID, id string, expectedVersion int, conditionSatisfied bool, actorID string, now time.Time) error {
	if projectID == "" || id == "" || expectedVersion < 1 || actorID == "" {
		return errors.New("deferred decision identity, version, and actor are required")
	}
	return s.tx(ctx, func(tx *sql.Tx) error {
		var epicID, state, deferredUntil, condition string
		var version int
		if err := tx.QueryRowContext(ctx, `SELECT COALESCE(epic_id,''),state,request_version,deferred_until,defer_condition
			FROM decision_requests WHERE project_id=? AND id=?`, projectID, id).
			Scan(&epicID, &state, &version, &deferredUntil, &condition); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrDecisionNotFound
			}
			return err
		}
		if state != string(workintent.RequestDeferred) || version != expectedVersion {
			return workintent.ErrRequestNotCurrent
		}
		if !conditionSatisfied {
			if deferredUntil == "" {
				return ErrDecisionDeferralActive
			}
			deadline, err := time.Parse(rfc3339, deferredUntil)
			if err != nil || now.Before(deadline) {
				return ErrDecisionDeferralActive
			}
		}
		nextVersion := version + 1
		nowText := now.UTC().Format(rfc3339)
		res, err := tx.ExecContext(ctx, `UPDATE decision_requests SET state='open',request_version=?,
			current_response_id='',deferred_until='',defer_condition='',resolved_at='',updated_at=?
			WHERE project_id=? AND id=? AND state='deferred' AND request_version=?`, nextVersion, nowText,
			projectID, id, version)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return workintent.ErrRequestNotCurrent
		}
		payload, _ := json.Marshal(map[string]any{"decision_id": id, "prior_request_version": version,
			"request_version": nextVersion, "defer_condition": condition})
		return appendDecisionControlEventTx(ctx, tx, projectID, epicID, "decision_request_reopened", "deferred", "open", nextVersion, "flowbee", actorID, string(payload), now)
	})
}

const decisionRequestSelect = `SELECT id,project_id,COALESCE(epic_id,''),COALESCE(delivery_id,''),kind,
	title,prompt,options_json,response_schema_json,expected_response_kinds_json,priority,due_at,
	deferred_until,defer_condition,requested_by,route_to,subject_artifact_ref,subject_version,
	subject_sha256,evidence_refs_json,summary,state,request_version,current_response_id,
	COALESCE(superseded_by,''),cancellation_reason,resolved_at,created_at,updated_at
	FROM decision_requests`

type decisionRowScanner interface{ Scan(...any) error }

func scanDecisionRequest(row decisionRowScanner) (DecisionRequest, error) {
	var out DecisionRequest
	var kind, state, expected, due, deferred, resolved, created, updated string
	err := row.Scan(&out.ID, &out.ProjectID, &out.EpicID, &out.DeliveryID, &kind, &out.Title,
		&out.Prompt, &out.OptionsJSON, &out.ResponseSchemaJSON, &expected, &out.Priority, &due,
		&deferred, &out.DeferCondition, &out.RequestedBy, &out.RouteTo, &out.SubjectArtifactRef,
		&out.SubjectVersion, &out.SubjectSHA256, &out.EvidenceRefsJSON, &out.Summary, &state,
		&out.RequestVersion, &out.CurrentResponseID, &out.SupersededBy, &out.CancellationReason,
		&resolved, &created, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return DecisionRequest{}, ErrDecisionNotFound
	}
	if err != nil {
		return DecisionRequest{}, err
	}
	out.Kind, out.State = workintent.DecisionKind(kind), workintent.RequestState(state)
	if err := json.Unmarshal([]byte(expected), &out.ExpectedResponseKinds); err != nil {
		return DecisionRequest{}, fmt.Errorf("decode expected response kinds: %w", err)
	}
	out.DueAt, out.DeferredUntil, out.ResolvedAt = parseOptionalTime(due), parseOptionalTime(deferred), parseOptionalTime(resolved)
	out.CreatedAt, out.UpdatedAt = parseOptionalTime(created), parseOptionalTime(updated)
	return out, nil
}

func getDecisionResponseByKeyTx(ctx context.Context, tx *sql.Tx, projectID, requestID, key string) (DecisionResponse, bool, error) {
	var out DecisionResponse
	var kind, deferUntil, created string
	err := tx.QueryRowContext(ctx, `SELECT id,project_id,request_id,request_version,subject_version,
		subject_sha256,kind,structured_value_json,comment,actor_id,authorization_scope,defer_until,
		defer_condition,downstream_ack_state,audit_ref,idempotency_key,created_at
		FROM decision_responses WHERE project_id=? AND request_id=? AND idempotency_key=?`,
		projectID, requestID, key).Scan(&out.ID, &out.ProjectID, &out.RequestID, &out.RequestVersion,
		&out.SubjectVersion, &out.SubjectSHA256, &kind, &out.StructuredValueJSON, &out.Comment,
		&out.ActorID, &out.AuthorizationScope, &deferUntil, &out.DeferCondition,
		&out.DownstreamAckState, &out.AuditRef, &out.IdempotencyKey, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return DecisionResponse{}, false, nil
	}
	if err != nil {
		return DecisionResponse{}, false, err
	}
	out.Kind = workintent.ResponseKind(kind)
	out.DeferUntil, out.CreatedAt = parseOptionalTime(deferUntil), parseOptionalTime(created)
	return out, true, nil
}

func sameDecisionResponse(got DecisionResponse, in DecisionResponseInput, normalizedValue string) bool {
	return got.RequestID == in.RequestID && got.RequestVersion == in.RequestVersion &&
		got.SubjectVersion == in.SubjectVersion && got.SubjectSHA256 == in.SubjectSHA256 &&
		got.Kind == in.Kind && got.StructuredValueJSON == normalizedValue && got.Comment == in.Comment &&
		got.ActorID == in.ActorID && got.AuthorizationScope == in.AuthorizationScope &&
		got.DeferUntil.Equal(in.DeferUntil.UTC()) && got.DeferCondition == in.DeferCondition
}

func (s *Store) transitionCurrentDecision(ctx context.Context, projectID, id string, expectedVersion int, to workintent.RequestState, kind, actorID, reason, replacementID string, now time.Time) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		return transitionCurrentDecisionTx(ctx, tx, projectID, id, expectedVersion, to, kind, actorID, reason, replacementID, now)
	})
}

func transitionCurrentDecisionTx(ctx context.Context, tx *sql.Tx, projectID, id string, expectedVersion int, to workintent.RequestState, kind, actorID, reason, replacementID string, now time.Time) error {
	var epicID, from string
	var version int
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(epic_id,''),state,request_version FROM decision_requests
		WHERE project_id=? AND id=?`, projectID, id).Scan(&epicID, &from, &version); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrDecisionNotFound
		}
		return err
	}
	if version != expectedVersion || !workintent.RequestState(from).Current() {
		return workintent.ErrRequestNotCurrent
	}
	nowText := now.UTC().Format(rfc3339)
	res, err := tx.ExecContext(ctx, `UPDATE decision_requests SET state=?,superseded_by=?,
		cancellation_reason=?,resolved_at=?,updated_at=? WHERE project_id=? AND id=?
		AND request_version=? AND state IN ('open','viewed')`, to, nullableText(replacementID),
		reason, nowText, nowText, projectID, id, expectedVersion)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return workintent.ErrRequestNotCurrent
	}
	payload, _ := json.Marshal(map[string]any{"decision_id": id, "request_version": expectedVersion,
		"reason": reason, "superseded_by": replacementID})
	return appendDecisionControlEventTx(ctx, tx, projectID, epicID, kind, from, string(to), expectedVersion, "human", actorID, string(payload), now)
}

func appendDecisionControlEventTx(ctx context.Context, tx *sql.Tx, projectID, epicID, kind, from, to string, version int, actorKind, actorID, payload string, now time.Time) error {
	epicsSeq := 0
	if epicID != "" {
		if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(epic_seq),0)+1 FROM control_events WHERE epic_id=?`, epicID).Scan(&epicsSeq); err != nil {
			return err
		}
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO control_events
		(project_id,epic_id,kind,from_state,to_state,state_version,epic_seq,actor_kind,actor_id,payload_json,created_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?)`, projectID, epicID, kind, from, to, version, epicsSeq,
		actorKind, actorID, payload, now.UTC().Format(rfc3339))
	return err
}

func validateDecisionProjectRefsTx(ctx context.Context, tx *sql.Tx, projectID, epicID, deliveryID string) error {
	var exists int
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM projects WHERE id=?)`, projectID).Scan(&exists); err != nil || exists != 1 {
		if err != nil {
			return err
		}
		return fmt.Errorf("project %q does not exist", projectID)
	}
	if epicID != "" {
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM epics WHERE id=? AND project_id=?)`, epicID, projectID).Scan(&exists); err != nil || exists != 1 {
			if err != nil {
				return err
			}
			return errors.New("decision epic does not belong to project")
		}
	}
	if deliveryID != "" {
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM epic_deliveries WHERE epic_id=? AND project_id=?)`, deliveryID, projectID).Scan(&exists); err != nil || exists != 1 {
			if err != nil {
				return err
			}
			return errors.New("decision delivery does not belong to project")
		}
		if epicID != "" && epicID != deliveryID {
			return errors.New("decision epic and delivery do not match")
		}
	}
	return nil
}

func normalizedJSON(raw json.RawMessage, fallback string) (string, error) {
	if len(raw) == 0 {
		return fallback, nil
	}
	if !json.Valid(raw) {
		return "", errors.New("invalid JSON")
	}
	var dst bytes.Buffer
	if err := json.Compact(&dst, raw); err != nil {
		return "", err
	}
	return dst.String(), nil
}

func normalizeResponseKinds(kinds []workintent.ResponseKind) (string, error) {
	if len(kinds) == 0 {
		return "", errors.New("at least one expected response kind is required")
	}
	seen := make(map[workintent.ResponseKind]bool, len(kinds))
	for _, kind := range kinds {
		if !validResponseKind(kind) {
			return "", fmt.Errorf("invalid expected response kind %q", kind)
		}
		if seen[kind] {
			return "", fmt.Errorf("duplicate expected response kind %q", kind)
		}
		seen[kind] = true
	}
	blob, _ := json.Marshal(kinds)
	return string(blob), nil
}

func validDecisionKind(kind workintent.DecisionKind) bool {
	switch kind {
	case workintent.DecisionQuestion, workintent.DecisionPlanReview, workintent.DecisionDesignReview,
		workintent.DecisionAuthorization, workintent.DecisionException:
		return true
	default:
		return false
	}
}

func validResponseKind(kind workintent.ResponseKind) bool {
	switch kind {
	case workintent.ResponseAnswer, workintent.ResponseApprove, workintent.ResponseRequestChanges,
		workintent.ResponseDefer, workintent.ResponseDeny:
		return true
	default:
		return false
	}
}

func validSHA256(value string) bool {
	if !strings.HasPrefix(value, "sha256:") || len(value) != len("sha256:")+64 {
		return false
	}
	_, err := hex.DecodeString(strings.TrimPrefix(value, "sha256:"))
	return err == nil
}

func nullableText(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func parseOptionalTime(value string) time.Time {
	if value == "" {
		return time.Time{}
	}
	for _, layout := range []string{rfc3339, time.RFC3339, "2006-01-02 15:04:05"} {
		if parsed, err := time.Parse(layout, value); err == nil {
			return parsed.UTC()
		}
	}
	return time.Time{}
}
