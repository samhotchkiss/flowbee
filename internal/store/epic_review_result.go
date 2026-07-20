package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	"github.com/samhotchkiss/flowbee/internal/job"
)

// projectEpicReviewResultTx keeps the reusable jobs pipeline and the v2 delivery
// projection in one transaction. A reviewer result is not complete merely because
// jobs.state moved: the delivery must durably record the SHA-bound verdict and the
// next external effect before the lease is released.
func projectEpicReviewResultTx(ctx context.Context, tx *sql.Tx, jobID string, final job.State, minted *job.Verdict, notes string, now time.Time) error {
	var domain, epicID string
	if err := tx.QueryRowContext(ctx, `SELECT workflow_domain,epic_delivery_id FROM jobs WHERE id=?`, jobID).Scan(&domain, &epicID); err != nil {
		return err
	}
	if domain != "epic_v2" || epicID == "" {
		return nil
	}

	var projectID, fromState, head, base string
	var stateVersion, reviewRound int
	if err := tx.QueryRowContext(ctx, `SELECT project_id,state,state_version,head_sha,base_sha,review_round
		FROM epic_deliveries WHERE epic_id=? AND review_job_id=?`, epicID, jobID).
		Scan(&projectID, &fromState, &stateVersion, &head, &base, &reviewRound); err != nil {
		return fmt.Errorf("read epic review delivery: %w", err)
	}
	if fromState != "in_review" {
		return fmt.Errorf("epic review delivery %s is %s, want in_review", epicID, fromState)
	}
	// A result is independent stage evidence that this lease processed the
	// assignment. Cancel an executor action that has not started; an already
	// delivering/verifying effect remains in the uncertainty reconciler.
	if _, err := tx.ExecContext(ctx, `UPDATE epic_actions SET state='cancelled_superseded',
		last_error='review_stage_already_completed',updated_at=?
		WHERE epic_id=? AND kind='review_wake' AND state='pending'
		  AND lease_epoch=(SELECT lease_epoch FROM jobs WHERE id=?)`,
		now.UTC().Format(rfc3339), epicID, jobID); err != nil {
		return err
	}
	if minted != nil && (!minted.Verify(head, base) || minted.Value != job.VerdictApproved) {
		return fmt.Errorf("epic review verdict does not bind current artifact")
	}

	nowText := now.UTC().Format(rfc3339)
	toState, verdict := "", ""
	due := now.Add(10 * time.Minute)
	var actionKind, actionDedup, actionPayload string
	switch final {
	case job.StateMergeable:
		if minted == nil {
			return fmt.Errorf("epic review reached mergeable without minted verdict")
		}
		toState, verdict = "merge_queued", string(minted.Value)
		actionKind = "merge_dispatch"
		actionDedup = fmt.Sprintf("%s:%s:merge:%s:%s", projectID, epicID, head, base)
		actionPayload = marshalEpicActionPayload(epicID, head, base, "")
	case job.StateReady:
		toState, verdict = "changes_requested", string(job.VerdictChangesRequested)
		due = now.Add(30 * time.Minute)
		actionKind = "builder_rework"
		actionDedup = fmt.Sprintf("%s:%s:builder_rework:%s:%s", projectID, epicID, head, head)
		actionPayload = marshalEpicActionPayload(epicID, head, base, notes)
	case job.StateReviewPending:
		// A sub-threshold panel approval releases this reviewer and queues the same
		// artifact for the next distinct reviewer. It is not a final verdict.
		toState = "review_queued"
	case job.StateNeedsHuman:
		toState = "needs_human"
		due = now.Add(24 * time.Hour)
	default:
		return fmt.Errorf("unsupported epic review result state %s", final)
	}

	if actionKind != "" {
		var actionErr error
		if actionKind == "builder_rework" {
			actionErr = ensureBuilderReworkActionTx(ctx, tx, projectID, epicID,
				actionDedup, actionPayload, head, base, now)
		} else {
			actionErr = ensureEpicActionTx(ctx, tx, projectID, epicID, actionKind,
				actionDedup, actionPayload, head, base, now)
		}
		if actionErr != nil {
			return actionErr
		}
	}

	verdictHead, verdictBase := "", ""
	if verdict != "" {
		verdictHead, verdictBase = head, base
	}
	holdKind, holdReason, returnState := "", "", ""
	if toState == "needs_human" {
		holdKind, holdReason, returnState = "review", "review attempts exhausted", "in_review"
	}
	result, err := tx.ExecContext(ctx, `UPDATE epic_deliveries SET
		state=?, state_version=state_version+1, verdict=?, verdict_head_sha=?, verdict_base_sha=?,
		builder_affinity_state=CASE WHEN ?='changes_requested' THEN 'relaunching' ELSE builder_affinity_state END,
		reviewed_at=?, reviewer_identity='', reviewer_model_family='', review_started_at='',
		last_reviewer_fact_at=?, state_entered_at=?, state_due_at=?, fact_progress_at=?,
		hold_kind=?, hold_reason=?, return_state=?,
		review_round=review_round+CASE WHEN ?='changes_requested' THEN 1 ELSE 0 END,
		updated_at=?
		WHERE epic_id=? AND review_job_id=? AND state='in_review' AND state_version=?`,
		toState, verdict, verdictHead, verdictBase, toState, nowText, nowText, nowText,
		due.UTC().Format(rfc3339), nowText, holdKind, holdReason, returnState,
		toState, nowText, epicID, jobID, stateVersion)
	if err != nil {
		return err
	}
	if n, _ := result.RowsAffected(); n != 1 {
		return fmt.Errorf("epic review delivery changed concurrently")
	}

	payload, _ := json.Marshal(map[string]any{
		"job_id": jobID, "head_sha": head, "base_sha": base,
		"verdict": verdict, "review_round": reviewRound,
	})
	return appendEpicControlEventTx(ctx, tx, projectID, epicID, "review_result", fromState, toState,
		stateVersion+1, "reviewer", string(payload), now)
}

func marshalEpicActionPayload(epicID, head, base, notes string) string {
	b, _ := json.Marshal(map[string]string{
		"epic_id": epicID, "head_sha": head, "base_sha": base, "review_notes": notes,
	})
	return string(b)
}

func ensureEpicActionTx(ctx context.Context, tx *sql.Tx, projectID, epicID, kind, dedup, payload, head, base string, now time.Time) error {
	h := sha256.Sum256([]byte(payload))
	payloadHash := "sha256:" + hex.EncodeToString(h[:])
	idHash := sha256.Sum256([]byte(dedup))
	baseActionID := kind + "-" + hex.EncodeToString(idHash[:12])
	actionID := baseActionID
	nowText := now.UTC().Format(rfc3339)
	for revisit := 0; ; revisit++ {
		if revisit > 0 {
			actionID = fmt.Sprintf("%s-r%d", baseActionID, revisit)
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO epic_actions
			(id,project_id,epic_id,kind,state,action_epoch,dedup_key,payload_json,payload_sha256,
			 head_sha,base_sha,next_attempt_at,created_at,updated_at)
			VALUES (?,?,?,?,'pending',0,?,?,?,?,?,?,?,?)`, actionID, projectID, epicID, kind,
			dedup, payload, payloadHash, head, base, nowText, nowText, nowText)
		if err == nil {
			return nil
		}
		if !isUniqueConstraintErr(err) {
			return err
		}
		var gotKind, gotPayloadHash, gotHead, gotBase string
		qerr := tx.QueryRowContext(ctx, `SELECT kind,payload_sha256,head_sha,base_sha FROM epic_actions
			WHERE dedup_key=? AND state<>'cancelled_superseded'`, dedup).
			Scan(&gotKind, &gotPayloadHash, &gotHead, &gotBase)
		if qerr == nil {
			if gotKind != kind || gotPayloadHash != payloadHash || gotHead != head || gotBase != base {
				return fmt.Errorf("epic action dedup collision for %s", dedup)
			}
			return nil
		}
		if qerr != sql.ErrNoRows {
			return qerr
		}
		// A prior incarnation with this semantic key was cancelled because its
		// artifact was superseded. The partial uniqueness rule intentionally permits
		// H1 -> H2 -> H1; retain the cancelled row as audit history and allocate a
		// deterministic revisit suffix rather than colliding on the historical PK.
	}
}

func appendEpicControlEventTx(ctx context.Context, tx *sql.Tx, projectID, epicID, kind, from, to string, stateVersion int, actor, payload string, now time.Time) error {
	var epicSeq int
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(epic_seq),0)+1 FROM control_events WHERE epic_id=?`, epicID).Scan(&epicSeq); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO control_events
		(project_id,epic_id,kind,from_state,to_state,state_version,epic_seq,actor_kind,payload_json,created_at)
		VALUES (?,?,?,?,?,?,?,?,?,?)`, projectID, epicID, kind, from, to, stateVersion, epicSeq, actor, payload, now.UTC().Format(rfc3339))
	return err
}

// projectEpicReviewLeaseEndTx mirrors a review lease release/revocation into the
// delivery. Without this, jobs.state can be claimable again while the board and
// delivery reconciler remain stuck forever in in_review with a dead reviewer.
func projectEpicReviewLeaseEndTx(ctx context.Context, tx *sql.Tx, jobID string, to job.State, reason string, now time.Time) error {
	var domain, epicID string
	if err := tx.QueryRowContext(ctx, `SELECT workflow_domain,epic_delivery_id FROM jobs WHERE id=?`, jobID).Scan(&domain, &epicID); err != nil {
		return err
	}
	if domain != "epic_v2" || epicID == "" {
		return nil
	}
	var projectID, fromState string
	var stateVersion int
	if err := tx.QueryRowContext(ctx, `SELECT project_id,state,state_version FROM epic_deliveries
		WHERE epic_id=? AND review_job_id=?`, epicID, jobID).Scan(&projectID, &fromState, &stateVersion); err != nil {
		return err
	}
	if fromState != "in_review" {
		return fmt.Errorf("epic review lease ended while delivery is %s", fromState)
	}
	// A released/revoked lease must never be woken after its authority ended.
	// Only a not-yet-started effect is cancelled; uncertain external mutation is
	// retained for receipt/evidence reconciliation under the old epoch.
	if _, err := tx.ExecContext(ctx, `UPDATE epic_actions SET state='cancelled_superseded',
		last_error='review_lease_ended',updated_at=?
		WHERE epic_id=? AND kind='review_wake' AND state='pending'
		  AND lease_epoch=(SELECT lease_epoch FROM jobs WHERE id=?)`,
		now.UTC().Format(rfc3339), epicID, jobID); err != nil {
		return err
	}
	deliveryState, holdKind, holdReason, returnState := "review_queued", "", "", ""
	if to == job.StateNeedsHuman {
		deliveryState, holdKind, holdReason, returnState = "needs_human", "review", reason, "in_review"
	}
	nowText := now.UTC().Format(rfc3339)
	due := now.Add(10 * time.Minute)
	if deliveryState == "needs_human" {
		due = now.Add(24 * time.Hour)
	}
	res, err := tx.ExecContext(ctx, `UPDATE epic_deliveries SET
		state=?,state_version=state_version+1,reviewer_identity='',reviewer_model_family='',
		review_started_at='',last_reviewer_fact_at=?,state_entered_at=?,state_due_at=?,
		fact_progress_at=?,hold_kind=?,hold_reason=?,return_state=?,updated_at=?
		WHERE epic_id=? AND state='in_review' AND state_version=?`, deliveryState, nowText,
		nowText, due.UTC().Format(rfc3339), nowText, holdKind, holdReason, returnState,
		nowText, epicID, stateVersion)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return fmt.Errorf("epic review lease projection changed concurrently")
	}
	payload, _ := json.Marshal(map[string]string{"job_id": jobID, "reason": reason})
	return appendEpicControlEventTx(ctx, tx, projectID, epicID, "review_lease_ended", fromState,
		deliveryState, stateVersion+1, "reconciler", string(payload), now)
}
