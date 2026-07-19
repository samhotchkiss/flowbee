package driver

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// SQLStageEvidence verifies processing from Flowbee's durable Driver event
// ledger.  It never reads terminal prose and never treats a send receipt as
// stage success. Review/rework wakes and Phase-1 Orchestrator delivery use the
// same deliberately strict processing acknowledgement: an exact, live
// provider-native user message with the immutable action payload hash.
type SQLStageEvidence struct {
	DB  *sql.DB
	Now func() time.Time
}

type durableActionEvidenceTarget struct {
	BaselineStoreSeq         uint64
	BaselineUncertaintyEpoch uint64
	PayloadSHA256            string
	TargetHostID             string
	TargetStoreID            string
	TargetServerID           string
	RecipientSessionID       string
	RecipientPaneInstanceID  string
	RecipientAgentRunID      string
}

// AwaitStage implements StageEvidence. ErrUncertain is fail-closed evidence:
// the action remains in verifying and the original body must not be resent.
func (s SQLStageEvidence) AwaitStage(ctx context.Context, action Action, receipt Receipt) (bool, error) {
	if s.DB == nil {
		return false, errors.New("driver stage evidence: nil database")
	}
	intentAction := action.Kind == "deliver_to_orchestrator"
	if action.Kind != "review_wake" && action.Kind != "builder_rework_wake" &&
		action.Kind != "builder_launch_contract" && !intentAction {
		return false, nil
	}
	if err := validateEvidenceReceipt(action, receipt); err != nil {
		return false, err
	}

	var target durableActionEvidenceTarget
	var err error
	if intentAction {
		err = s.DB.QueryRowContext(ctx, `SELECT a.evidence_baseline_store_seq,
			a.evidence_baseline_uncertainty_epoch,a.payload_sha256,r.host_id,r.store_id,
			r.tmux_server_instance_id,r.session_id,r.pane_instance_id,r.agent_run_id
			FROM work_intent_actions a JOIN driver_session_bindings r
			  ON r.binding_id=a.target_incarnation
			WHERE a.id=? AND a.action_epoch=?`, action.ActionID, action.Epoch).
			Scan(&target.BaselineStoreSeq, &target.BaselineUncertaintyEpoch,
				&target.PayloadSHA256, &target.TargetHostID, &target.TargetStoreID,
				&target.TargetServerID, &target.RecipientSessionID,
				&target.RecipientPaneInstanceID, &target.RecipientAgentRunID)
	} else {
		err = s.DB.QueryRowContext(ctx, `SELECT evidence_baseline_store_seq,
			evidence_baseline_uncertainty_epoch,payload_sha256,target_host_id,target_store_id,
			target_server_id,recipient_session_id,recipient_pane_instance_id,recipient_agent_run_id
			FROM epic_actions WHERE id=? AND action_epoch=? AND executor_kind='driver'`,
			action.ActionID, action.Epoch).Scan(&target.BaselineStoreSeq,
			&target.BaselineUncertaintyEpoch, &target.PayloadSHA256, &target.TargetHostID,
			&target.TargetStoreID, &target.TargetServerID, &target.RecipientSessionID,
			&target.RecipientPaneInstanceID, &target.RecipientAgentRunID)
	}
	if errors.Is(err, sql.ErrNoRows) {
		return false, fmt.Errorf("driver stage evidence action epoch changed: %w", ErrStaleActionEpoch)
	}
	if err != nil {
		return false, err
	}
	if target.PayloadSHA256 != action.PayloadSHA256 || target.TargetStoreID != action.TargetStoreID ||
		target.RecipientSessionID != action.RecipientSessionID ||
		target.RecipientPaneInstanceID != action.RecipientPaneInstanceID ||
		target.RecipientAgentRunID != action.RecipientAgentRunID {
		return false, fmt.Errorf("driver stage evidence immutable target changed: %w", ErrIdentityMismatch)
	}

	// The store must still be the active cursor domain and caught up, with the
	// same uncertainty generation captured when the action was created. A cursor
	// gap, store reset, source gap/reset, or invalidation advances/fences one of
	// these predicates and can never be interpreted as absence or success.
	var instanceState string
	var currentUncertaintyEpoch, highStoreSeq uint64
	err = s.DB.QueryRowContext(ctx, `SELECT i.state,c.uncertainty_epoch,c.high_store_seq
		FROM driver_instances i JOIN driver_observation_cursors c
		  ON c.store_id=i.store_id AND c.instance_ref=i.instance_ref
		WHERE i.host_id=? AND i.store_id=? AND c.active=1`,
		target.TargetHostID, target.TargetStoreID).Scan(&instanceState,
		&currentUncertaintyEpoch, &highStoreSeq)
	if errors.Is(err, sql.ErrNoRows) {
		return false, fmt.Errorf("Driver evidence store reset or no longer authoritative: %w", ErrUncertain)
	}
	if err != nil {
		return false, err
	}
	if instanceState != "live" || currentUncertaintyEpoch != target.BaselineUncertaintyEpoch {
		return false, fmt.Errorf("Driver evidence history is gapped, reset, invalidated, or resyncing: %w", ErrUncertain)
	}

	// Recheck the exact recipient incarnation from the authoritative current
	// projection. Same-CWD, same provider, similar names, and wall-clock proximity
	// are intentionally absent from this join.
	var current int
	err = s.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM driver_session_projections
		WHERE host_id=? AND store_id=? AND session_id=? AND pane_instance_id=?
		  AND agent_run_id=? AND tmux_server_instance_id=? AND lifecycle<>'ended'`,
		target.TargetHostID, target.TargetStoreID, target.RecipientSessionID,
		target.RecipientPaneInstanceID, target.RecipientAgentRunID,
		target.TargetServerID).Scan(&current)
	if err != nil {
		return false, err
	}
	if current != 1 {
		return false, fmt.Errorf("Driver recipient incarnation is stale: %w", ErrUncertain)
	}
	if highStoreSeq <= target.BaselineStoreSeq {
		return false, nil
	}

	rows, err := s.DB.QueryContext(ctx, `SELECT event_id,store_seq,envelope_json
		FROM driver_observation_events
		WHERE store_id=? AND host_id=? AND session_id=? AND pane_instance_id=?
		  AND store_seq>? AND historical=0 AND kind='message.completed'
		ORDER BY store_seq`, target.TargetStoreID, target.TargetHostID,
		target.RecipientSessionID, target.RecipientPaneInstanceID,
		target.BaselineStoreSeq)
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
			return false, fmt.Errorf("decode Driver review acknowledgement %s: %w", eventID, err)
		}
		if !matched {
			continue
		}
		// Store intentionally uses one SQLite connection. Release the evidence
		// query before the idempotent evidence insert to avoid self-deadlock.
		if err := rows.Close(); err != nil {
			return false, err
		}
		if err := s.persist(ctx, action, target, eventID, storeSeq, intentAction); err != nil {
			return false, err
		}
		return true, nil
	}
	if err := rows.Err(); err != nil {
		return false, err
	}
	return false, nil
}

func validateEvidenceReceipt(a Action, r Receipt) error {
	if r.ActionID != a.ActionID || r.GrantID != a.GrantID || r.GrantEpoch != a.Epoch ||
		r.PayloadSHA256 != a.PayloadSHA256 || r.Recipient.SessionID != a.RecipientSessionID ||
		r.Recipient.PaneInstanceID != a.RecipientPaneInstanceID {
		return fmt.Errorf("Driver receipt does not bind the immutable action: %w", ErrIdentityMismatch)
	}
	return nil
}

func reviewWakeMessageMatches(envelope []byte, wantHash string) (bool, error) {
	var event struct {
		Kind       string `json:"kind"`
		Historical bool   `json:"historical"`
		Source     struct {
			Kind         string `json:"kind"`
			Fidelity     string `json:"fidelity"`
			BindingEpoch *int64 `json:"binding_epoch"`
		} `json:"source"`
		Payload struct {
			Role          string `json:"role"`
			Status        string `json:"status"`
			ContentSHA256 string `json:"content_sha256"`
		} `json:"payload"`
	}
	if err := json.Unmarshal(envelope, &event); err != nil {
		return false, err
	}
	return event.Kind == "message.completed" && !event.Historical &&
		event.Source.Kind == "provider_log" && event.Source.Fidelity == "replayable" &&
		event.Source.BindingEpoch != nil && *event.Source.BindingEpoch > 0 &&
		event.Payload.Role == "user" && event.Payload.Status == "completed" &&
		event.Payload.ContentSHA256 == wantHash, nil
}

func (s SQLStageEvidence) persist(ctx context.Context, action Action,
	target durableActionEvidenceTarget, eventID string, storeSeq uint64, intentAction bool) error {
	now := time.Now().UTC()
	if s.Now != nil {
		now = s.Now().UTC()
	}
	stamp := now.Format(time.RFC3339Nano)
	table := "driver_action_evidence"
	if intentAction {
		table = "work_intent_action_evidence"
	}
	_, err := s.DB.ExecContext(ctx, `INSERT INTO `+table+`
		(action_id,action_epoch,store_id,event_id,store_seq,session_id,pane_instance_id,
		 agent_run_id,evidence_kind,payload_sha256,state,created_at,updated_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,'confirmed',?,?)
		ON CONFLICT(action_id,action_epoch) DO NOTHING`, action.ActionID, action.Epoch,
		target.TargetStoreID, eventID, storeSeq, target.RecipientSessionID,
		target.RecipientPaneInstanceID, target.RecipientAgentRunID,
		"provider_user_message_hash", target.PayloadSHA256, stamp, stamp)
	if err != nil {
		return err
	}
	var gotStore, gotEvent, gotSession, gotPane, gotRun, gotKind, gotHash, gotState string
	var gotSeq uint64
	err = s.DB.QueryRowContext(ctx, `SELECT store_id,event_id,store_seq,session_id,pane_instance_id,
		agent_run_id,evidence_kind,payload_sha256,state FROM `+table+`
		WHERE action_id=? AND action_epoch=?`, action.ActionID, action.Epoch).Scan(&gotStore,
		&gotEvent, &gotSeq, &gotSession, &gotPane, &gotRun, &gotKind, &gotHash, &gotState)
	if err != nil {
		return err
	}
	if gotStore != target.TargetStoreID || gotEvent != eventID || gotSeq != storeSeq ||
		gotSession != target.RecipientSessionID || gotPane != target.RecipientPaneInstanceID ||
		gotRun != target.RecipientAgentRunID || gotKind != "provider_user_message_hash" ||
		gotHash != target.PayloadSHA256 || gotState != "confirmed" {
		return fmt.Errorf("Driver stage evidence replay changed: %w", ErrIdempotencyBody)
	}
	return nil
}
