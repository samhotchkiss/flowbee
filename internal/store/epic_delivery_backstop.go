package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/samhotchkiss/flowbee/internal/epicflow"
)

type EpicDeliveryBackstopResult struct{ Scanned, Alerted int }

// ReconcileEpicDeliveryBackstops is the delivery-agnostic safety net. Specialized
// reconcilers own automatic repair; this pass guarantees that an overdue state can
// never remain silent merely because its specialized reconciler crashed or missed a
// newly-added seam.
func (s *Store) ReconcileEpicDeliveryBackstops(ctx context.Context, now time.Time) (EpicDeliveryBackstopResult, error) {
	var out EpicDeliveryBackstopResult
	if !s.EnableEpicReviewHandoffV2 {
		return out, nil
	}
	rows, err := s.DB.QueryContext(ctx, `SELECT epic_id FROM epic_deliveries
		WHERE state NOT IN ('complete','abandoned') AND hold_reason=''`)
	if err != nil {
		return out, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return out, err
		}
		ids = append(ids, id)
	}
	if err := rows.Close(); err != nil {
		return out, err
	}
	for _, id := range ids {
		out.Scanned++
		alerted, err := s.reconcileOneEpicDeliveryBackstop(ctx, id, now)
		if err != nil {
			// Poison isolation remains visible and durable; another delivery still
			// surfaces while this exact row is quarantined for repair.
			_ = s.RecordReconcilerPoisonFact(ctx, "delivery_backstop", "epic:"+id, err.Error(), now)
			continue
		}
		_ = s.ResolveReconcilerPoisonFact(ctx, "delivery_backstop", "epic:"+id, now)
		if alerted {
			out.Alerted++
		}
	}
	return out, nil
}

func (s *Store) reconcileOneEpicDeliveryBackstop(ctx context.Context, epicID string, now time.Time) (bool, error) {
	alerted := false
	err := s.tx(ctx, func(tx *sql.Tx) error {
		var projectID, state, dueAt, enteredAt, progressAt, head, base, hold string
		var version int
		if err := tx.QueryRowContext(ctx, `SELECT project_id,state,state_version,state_due_at,
			state_entered_at,fact_progress_at,head_sha,base_sha,hold_reason FROM epic_deliveries WHERE epic_id=?`, epicID).
			Scan(&projectID, &state, &version, &dueAt, &enteredAt, &progressAt, &head, &base, &hold); err != nil {
			return err
		}
		policy, ok := epicflow.PolicyFor(state)
		if !ok || hold != "" {
			return nil
		}
		// Missing clocks are themselves a contract violation. Give a freshly-entered
		// legacy/backfilled row one default interval, then surface it rather than
		// silently skipping an empty due_at forever.
		deadline := time.Time{}
		if dueAt != "" {
			deadline, _ = time.Parse(rfc3339, dueAt)
		}
		if deadline.IsZero() {
			entered, _ := time.Parse(rfc3339, enteredAt)
			deadline = entered.Add(15 * time.Minute)
		}
		if deadline.IsZero() || now.Before(deadline) {
			return nil
		}
		dedup := fmt.Sprintf("delivery_overdue:%s:%s:%d", epicID, state, version)
		attentionID := "delivery-overdue-" + stableID(dedup)
		payload, _ := json.Marshal(map[string]any{
			"state": state, "state_version": version, "next_action": policy.NextAction,
			"progress_clock": policy.ProgressClock, "progress_at": progressAt,
			"head_sha": head, "base_sha": base,
		})
		_, err := tx.ExecContext(ctx, `INSERT INTO attention_items
			(id,kind,epic_id,repo,priority,state,dedup_key,blocking,leased_by,item_epoch,
			 lease_expires_at,awaiting_since,delivery_key,evidence_json,detail,resolution,
			 verdict,occurrences,first_seen_at,last_seen_at,resolved_at,created_at,updated_at)
			VALUES (?,?,?,'',10,'open',?,1,'',0,'','','',?,?,'','',1,?,?,'',?,?)`,
			attentionID, string(policy.AttentionKind), epicID, dedup, string(payload),
			"delivery state overdue; next action: "+policy.NextAction,
			now.UTC().Format(rfc3339), now.UTC().Format(rfc3339), now.UTC().Format(rfc3339), now.UTC().Format(rfc3339))
		if err != nil && !isUniqueConstraintErr(err) {
			return err
		}
		if err := ensureControlAlertTx(ctx, tx, projectID, epicID, string(policy.AttentionKind), dedup, string(payload), now); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE epic_deliveries SET alert_pending=1,
			last_error=?,updated_at=? WHERE epic_id=?`, "overdue:"+policy.NextAction,
			now.UTC().Format(rfc3339), epicID); err != nil {
			return err
		}
		alerted = true
		return nil
	})
	return alerted, err
}
