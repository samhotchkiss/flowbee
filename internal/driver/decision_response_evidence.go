package driver

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// DecisionResponseStageEvidence accepts only an exact provider-native user
// message hash for the live recipient incarnation after the committed cursor
// baseline. A Driver receipt alone is intentionally insufficient.
type DecisionResponseStageEvidence struct {
	DB  *sql.DB
	Now func() time.Time
}

func (s DecisionResponseStageEvidence) AwaitStage(ctx context.Context, action Action, receipt Receipt) (bool, error) {
	if s.DB == nil {
		return false, errors.New("decision response stage evidence: nil database")
	}
	if action.Kind != "notify_interactor" {
		return false, nil
	}
	if err := validateEvidenceReceipt(action, receipt); err != nil {
		return false, err
	}
	var target durableActionEvidenceTarget
	err := s.DB.QueryRowContext(ctx, `SELECT a.evidence_baseline_store_seq,
		a.evidence_baseline_uncertainty_epoch,a.payload_sha256,r.host_id,r.store_id,
		r.tmux_server_instance_id,r.session_id,r.pane_instance_id,r.agent_run_id
		FROM decision_response_actions a JOIN driver_session_bindings r
		  ON r.binding_id=a.target_binding_id
		WHERE a.id=? AND a.action_epoch=?`, action.ActionID, action.Epoch).
		Scan(&target.BaselineStoreSeq, &target.BaselineUncertaintyEpoch,
			&target.PayloadSHA256, &target.TargetHostID, &target.TargetStoreID,
			&target.TargetServerID, &target.RecipientSessionID,
			&target.RecipientPaneInstanceID, &target.RecipientAgentRunID)
	if errors.Is(err, sql.ErrNoRows) {
		return false, fmt.Errorf("decision response action epoch changed: %w", ErrStaleActionEpoch)
	}
	if err != nil {
		return false, err
	}
	if target.PayloadSHA256 != action.PayloadSHA256 || target.TargetStoreID != action.TargetStoreID ||
		target.RecipientSessionID != action.RecipientSessionID ||
		target.RecipientPaneInstanceID != action.RecipientPaneInstanceID ||
		target.RecipientAgentRunID != action.RecipientAgentRunID {
		return false, fmt.Errorf("decision response evidence target changed: %w", ErrIdentityMismatch)
	}
	var instanceState string
	var uncertaintyEpoch, highStoreSeq uint64
	err = s.DB.QueryRowContext(ctx, `SELECT i.state,c.uncertainty_epoch,c.high_store_seq
		FROM driver_instances i JOIN driver_observation_cursors c
		  ON c.store_id=i.store_id AND c.instance_ref=i.instance_ref
		WHERE i.host_id=? AND i.store_id=? AND c.active=1`, target.TargetHostID,
		target.TargetStoreID).Scan(&instanceState, &uncertaintyEpoch, &highStoreSeq)
	if errors.Is(err, sql.ErrNoRows) {
		return false, fmt.Errorf("Interactor evidence store is no longer authoritative: %w", ErrUncertain)
	}
	if err != nil {
		return false, err
	}
	if instanceState != "live" || uncertaintyEpoch != target.BaselineUncertaintyEpoch {
		return false, fmt.Errorf("Interactor evidence is gapped, reset, or resyncing: %w", ErrUncertain)
	}
	var current int
	err = s.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM driver_session_projections
		WHERE host_id=? AND store_id=? AND session_id=? AND pane_instance_id=?
		  AND agent_run_id=? AND tmux_server_instance_id=? AND lifecycle<>'ended'`,
		target.TargetHostID, target.TargetStoreID, target.RecipientSessionID,
		target.RecipientPaneInstanceID, target.RecipientAgentRunID, target.TargetServerID).Scan(&current)
	if err != nil {
		return false, err
	}
	if current != 1 {
		return false, fmt.Errorf("Interactor recipient incarnation is stale: %w", ErrUncertain)
	}
	if highStoreSeq <= target.BaselineStoreSeq {
		return false, nil
	}
	rows, err := s.DB.QueryContext(ctx, `SELECT event_id,store_seq,envelope_json
		FROM driver_observation_events WHERE store_id=? AND host_id=? AND session_id=?
		AND pane_instance_id=? AND store_seq>? AND historical=0 AND kind='message.completed'
		ORDER BY store_seq`, target.TargetStoreID, target.TargetHostID,
		target.RecipientSessionID, target.RecipientPaneInstanceID, target.BaselineStoreSeq)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	for rows.Next() {
		var eventID, envelope string
		var storeSeq uint64
		if err := rows.Scan(&eventID, &storeSeq, &envelope); err != nil {
			return false, err
		}
		matched, err := reviewWakeMessageMatches([]byte(envelope), target.PayloadSHA256)
		if err != nil {
			return false, fmt.Errorf("decode Interactor processing evidence %s: %w", eventID, err)
		}
		if !matched {
			continue
		}
		if err := rows.Close(); err != nil {
			return false, err
		}
		return true, s.persist(ctx, action, target, eventID, storeSeq)
	}
	if err := rows.Err(); err != nil {
		return false, err
	}
	return false, nil
}

func (s DecisionResponseStageEvidence) persist(ctx context.Context, action Action,
	target durableActionEvidenceTarget, eventID string, storeSeq uint64) error {
	now := time.Now().UTC()
	if s.Now != nil {
		now = s.Now().UTC()
	}
	stamp := now.Format(time.RFC3339Nano)
	_, err := s.DB.ExecContext(ctx, `INSERT INTO decision_response_action_evidence
		(action_id,action_epoch,store_id,event_id,store_seq,session_id,pane_instance_id,
		 agent_run_id,evidence_kind,payload_sha256,state,created_at,updated_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,'confirmed',?,?)
		ON CONFLICT(action_id,action_epoch) DO NOTHING`, action.ActionID, action.Epoch,
		target.TargetStoreID, eventID, storeSeq, target.RecipientSessionID,
		target.RecipientPaneInstanceID, target.RecipientAgentRunID,
		"provider_user_message_hash", target.PayloadSHA256, stamp, stamp)
	return err
}
