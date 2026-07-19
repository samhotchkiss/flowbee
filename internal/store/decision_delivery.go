package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

var ErrDecisionInteractorRouteUnavailable = errors.New("exact project Interactor Driver route is unavailable")

// DecisionDeliveryReconcileReport describes only durable projection work. It
// does not imply a Driver send or Interactor processing acknowledgement.
type DecisionDeliveryReconcileReport struct {
	ActionsCreated int
	RoutesHeld     int
}

type decisionNotificationPayload struct {
	Kind               string          `json:"kind"`
	ProjectID          string          `json:"project_id"`
	DecisionRequestID  string          `json:"decision_request_id"`
	DecisionResponseID string          `json:"decision_response_id"`
	RequestVersion     int             `json:"request_version"`
	SubjectVersion     int             `json:"subject_version"`
	SubjectSHA256      string          `json:"subject_sha256"`
	ResponseKind       string          `json:"response_kind"`
	StructuredValue    json.RawMessage `json:"structured_value"`
	Comment            string          `json:"comment,omitempty"`
	ActorID            string          `json:"actor_id"`
	AuthorizationScope string          `json:"authorization_scope,omitempty"`
	DeferUntil         string          `json:"defer_until,omitempty"`
	DeferCondition     string          `json:"defer_condition,omitempty"`
}

// ensureDecisionResponseActionTx runs in the same transaction that commits the
// append-only human response. With live exact bindings, that transaction also
// commits the immutable action body/hash and route. If the Interactor is absent,
// the human evidence still commits and a durable visible hold is inserted; the
// reconciler materializes the exact action as soon as a binding becomes live.
func (s *Store) ensureDecisionResponseActionTx(ctx context.Context, tx *sql.Tx, projectID, responseID string,
	now time.Time) (bool, bool, error) {
	created, err := s.materializeDecisionResponseActionTx(ctx, tx, projectID, responseID, now)
	if err == nil {
		return created, false, nil
	}
	if !errors.Is(err, ErrDecisionInteractorRouteUnavailable) &&
		!errors.Is(err, ErrDriverControlOriginUnavailable) {
		return false, false, err
	}
	stamp := now.UTC().Format(rfc3339)
	dedup := "decision_response_interactor_route_unavailable:" + responseID
	_, insertErr := tx.ExecContext(ctx, `INSERT OR IGNORE INTO control_alerts
		(id,project_id,epic_id,kind,dedup_key,payload_json,state,created_at,updated_at)
		VALUES (?,?,NULL,'decision_response_interactor_route_unavailable',?,
		json_object('decision_response_id',?,'reason',?),'pending',?,?)`,
		"decision-route-"+stableID(dedup), projectID, dedup, responseID, err.Error(), stamp, stamp)
	return false, true, insertErr
}

// ReconcileDecisionResponseActions drains responses committed while the exact
// Interactor route was unavailable and rebinds only by creating a new immutable
// action. Historical fenced actions are never retargeted.
func (s *Store) ReconcileDecisionResponseActions(ctx context.Context, now time.Time) (DecisionDeliveryReconcileReport, error) {
	var out DecisionDeliveryReconcileReport
	err := s.tx(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `SELECT r.project_id,r.id
			FROM decision_responses r
			WHERE NOT EXISTS (SELECT 1 FROM decision_response_actions a
			  WHERE a.response_id=r.id AND a.state<>'fenced')
			ORDER BY r.created_at,r.id`)
		if err != nil {
			return err
		}
		type item struct{ projectID, responseID string }
		var items []item
		for rows.Next() {
			var i item
			if err := rows.Scan(&i.projectID, &i.responseID); err != nil {
				rows.Close()
				return err
			}
			items = append(items, i)
		}
		if err := rows.Close(); err != nil {
			return err
		}
		for _, i := range items {
			created, held, err := s.ensureDecisionResponseActionTx(ctx, tx, i.projectID, i.responseID, now)
			if err != nil {
				return err
			}
			if created {
				out.ActionsCreated++
			}
			if held {
				out.RoutesHeld++
			}
		}
		return nil
	})
	return out, err
}

func (s *Store) materializeDecisionResponseActionTx(ctx context.Context, tx *sql.Tx, projectID, responseID string, now time.Time) (bool, error) {
	var requestID, requestedBy, kind, structured, comment, actorID, authScope string
	var subjectSHA, deferUntil, deferCondition string
	var requestVersion, subjectVersion int
	err := tx.QueryRowContext(ctx, `SELECT r.request_id,q.requested_by,r.request_version,
		r.subject_version,r.subject_sha256,r.kind,r.structured_value_json,r.comment,r.actor_id,
		r.authorization_scope,r.defer_until,r.defer_condition
		FROM decision_responses r JOIN decision_requests q ON q.id=r.request_id
		WHERE r.project_id=? AND r.id=?`, projectID, responseID).
		Scan(&requestID, &requestedBy, &requestVersion, &subjectVersion, &subjectSHA,
			&kind, &structured, &comment, &actorID, &authScope, &deferUntil, &deferCondition)
	if err != nil {
		return false, err
	}
	recipient, err := exactProjectInteractorBindingTx(ctx, tx, projectID, requestedBy)
	if err != nil {
		return false, err
	}
	if !s.HasDriverControlOriginForBinding(recipient) {
		return false, fmt.Errorf("%w: Interactor endpoint %s/%s/%s is not control-origin ready",
			ErrDriverControlOriginUnavailable, recipient.HostID, recipient.StoreID, recipient.TmuxServerDomainID)
	}
	var baselineSeq, uncertaintyEpoch uint64
	var instanceState string
	err = tx.QueryRowContext(ctx, `SELECT c.high_store_seq,c.uncertainty_epoch,i.state
		FROM driver_observation_cursors c JOIN driver_instances i
		  ON i.instance_ref=c.instance_ref AND i.store_id=c.store_id
		WHERE c.store_id=? AND c.active=1 AND i.host_id=?`, recipient.StoreID, recipient.HostID).
		Scan(&baselineSeq, &uncertaintyEpoch, &instanceState)
	if errors.Is(err, sql.ErrNoRows) || err == nil && instanceState != "live" {
		return false, fmt.Errorf("%w: Interactor observation store is not live", ErrDecisionInteractorRouteUnavailable)
	}
	if err != nil {
		return false, err
	}
	payload, err := json.Marshal(decisionNotificationPayload{
		Kind: "human_decision_response", ProjectID: projectID, DecisionRequestID: requestID,
		DecisionResponseID: responseID, RequestVersion: requestVersion, SubjectVersion: subjectVersion,
		SubjectSHA256: subjectSHA, ResponseKind: kind, StructuredValue: json.RawMessage(structured),
		Comment: comment, ActorID: actorID, AuthorizationScope: authScope,
		DeferUntil: deferUntil, DeferCondition: deferCondition,
	})
	if err != nil {
		return false, err
	}
	hash := sha256.Sum256(payload)
	payloadHash := "sha256:" + hex.EncodeToString(hash[:])
	dedup := fmt.Sprintf("decision-response:%s:notify:%s:%s", responseID, DriverControlIdentity, recipient.BindingID)
	actionID := "decision-action-" + stableID(dedup)
	grantID := stableUUID("driver-decision-response-grant/v1", dedup)
	stamp := now.UTC().Format(rfc3339)
	res, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO decision_response_actions
		(id,project_id,request_id,response_id,kind,state,action_epoch,dedup_key,payload_json,
		 payload_sha256,target_actor_id,sender_principal_id,sender_binding_id,target_binding_id,
		 evidence_baseline_store_seq,evidence_baseline_uncertainty_epoch,grant_id,created_at,updated_at)
		VALUES (?,?,?,?,'notify_interactor','pending',0,?,?,?,?,?,NULL,?,?,?,?,?,?)`, actionID,
		projectID, requestID, responseID, dedup, string(payload), payloadHash,
		recipient.WorkerIdentity, DriverControlIdentity, recipient.BindingID, baselineSeq,
		uncertaintyEpoch, grantID, stamp, stamp)
	if err != nil {
		return false, err
	}
	created, _ := res.RowsAffected()
	var gotHash, gotPrincipal, gotSender, gotTarget string
	if err := tx.QueryRowContext(ctx, `SELECT payload_sha256,sender_principal_id,
		COALESCE(sender_binding_id,''),target_binding_id
		FROM decision_response_actions WHERE dedup_key=?`, dedup).
		Scan(&gotHash, &gotPrincipal, &gotSender, &gotTarget); err != nil {
		return false, err
	}
	if gotHash != payloadHash || gotPrincipal != DriverControlIdentity || gotSender != "" || gotTarget != recipient.BindingID {
		return false, errors.New("decision response notification idempotency conflict")
	}
	if created == 1 {
		_, err = tx.ExecContext(ctx, `INSERT INTO control_events
			(project_id,epic_id,kind,actor_kind,actor_id,payload_json,created_at)
			VALUES (?,'','decision_response_action_committed','flowbee','decision_delivery',
			json_object('decision_response_id',?,'action_id',?,'payload_sha256',?,
			'sender_principal_id',?,'target_binding_id',?),?)`, projectID, responseID,
			actionID, payloadHash, DriverControlIdentity, recipient.BindingID, stamp)
		if err != nil {
			return false, err
		}
	}
	_, err = tx.ExecContext(ctx, `UPDATE control_alerts SET state='acknowledged',
		acknowledged_at=?,updated_at=? WHERE dedup_key=? AND state IN ('pending','delivering')`,
		stamp, stamp, "decision_response_interactor_route_unavailable:"+responseID)
	return created == 1, err
}

func exactProjectInteractorBindingTx(ctx context.Context, tx *sql.Tx, projectID, requestedBy string) (DriverSessionBinding, error) {
	if requestedBy != "" {
		if b, err := activeDriverSessionBindingTx(ctx, tx, projectID, requestedBy, DriverInteractorRole); err == nil {
			return b, nil
		} else if !errors.Is(err, sql.ErrNoRows) {
			return DriverSessionBinding{}, err
		}
	}
	rows, err := tx.QueryContext(ctx, `SELECT binding_id,project_id,worker_identity,role,seat_id,
		binding_epoch,host_id,store_id,tmux_server_domain_id,tmux_server_instance_id,lifecycle_ownership,external_watch_id,lifecycle_key,target_epoch,
		profile_id,workspace_root_id,workspace_relative_path,session_id,pane_instance_id,
		agent_run_id,provider,conversation_id,observed_at
		FROM driver_session_bindings WHERE project_id=? AND role=? AND state='active'
		ORDER BY worker_identity,binding_epoch`, projectID, DriverInteractorRole)
	if err != nil {
		return DriverSessionBinding{}, err
	}
	defer rows.Close()
	var matches []DriverSessionBinding
	for rows.Next() {
		b, err := activeDriverSessionBindingRow(rows)
		if err != nil {
			return DriverSessionBinding{}, err
		}
		matches = append(matches, b)
	}
	if err := rows.Err(); err != nil {
		return DriverSessionBinding{}, err
	}
	if len(matches) != 1 {
		return DriverSessionBinding{}, fmt.Errorf("%w: expected one active Interactor, found %d", ErrDecisionInteractorRouteUnavailable, len(matches))
	}
	return matches[0], nil
}
