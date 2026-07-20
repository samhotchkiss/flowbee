package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/samhotchkiss/flowbee/internal/job"
	"github.com/samhotchkiss/flowbee/internal/ledger"
)

type EpicReviewVerdictStallResult struct {
	Scanned, Requeued, Escalated int
}

// ReconcileEpicReviewVerdictStalls prevents a renewing-but-hung reviewer from
// holding an epic forever. A lease heartbeat proves process liveness, not verdict
// progress; only review_started_at/last_reviewer_fact_at advance this clock.
func (s *Store) ReconcileEpicReviewVerdictStalls(ctx context.Context, now time.Time, stallAfter time.Duration, maximumRecoveries int) (EpicReviewVerdictStallResult, error) {
	var out EpicReviewVerdictStallResult
	if !s.EnableEpicReviewHandoffV2 {
		return out, nil
	}
	if stallAfter <= 0 {
		stallAfter = 20 * time.Minute
	}
	if maximumRecoveries <= 0 {
		maximumRecoveries = 3
	}
	rows, err := s.DB.QueryContext(ctx, `SELECT epic_id,CASE WHEN last_reviewer_fact_at<>'' THEN last_reviewer_fact_at ELSE review_started_at END
		FROM epic_deliveries WHERE state='in_review' AND review_job_id<>''`)
	if err != nil {
		return out, err
	}
	type candidate struct{ epicID, progressAt string }
	var candidates []candidate
	for rows.Next() {
		var c candidate
		if err := rows.Scan(&c.epicID, &c.progressAt); err != nil {
			rows.Close()
			return out, err
		}
		out.Scanned++
		when, parseErr := time.Parse(rfc3339, c.progressAt)
		if parseErr == nil && !when.After(now.Add(-stallAfter)) {
			candidates = append(candidates, c)
		}
	}
	if err := rows.Close(); err != nil {
		return out, err
	}
	for _, c := range candidates {
		escalated, changed, err := s.reconcileOneEpicReviewVerdictStall(ctx, c.epicID, now, stallAfter, maximumRecoveries)
		if err != nil {
			// Poison isolation: one corrupt/stale delivery must not prevent recovery
			// for every other epic in the pass. The quarantine is durable and pushed;
			// it is not merely a swallowed log line.
			_ = s.RecordReconcilerPoisonFact(ctx, "review_verdict", "epic:"+c.epicID, err.Error(), now)
			continue
		}
		_ = s.ResolveReconcilerPoisonFact(ctx, "review_verdict", "epic:"+c.epicID, now)
		if changed {
			out.Requeued++
		}
		if escalated {
			out.Escalated++
		}
	}
	return out, nil
}

func (s *Store) reconcileOneEpicReviewVerdictStall(ctx context.Context, epicID string, now time.Time, stallAfter time.Duration, maximumRecoveries int) (bool, bool, error) {
	escalated, changed := false, false
	err := s.tx(ctx, func(tx *sql.Tx) error {
		var projectID, state, jobID, reviewer, progressAt string
		var stateVersion, recoveryCount int
		if err := tx.QueryRowContext(ctx, `SELECT project_id,state,state_version,review_job_id,reviewer_identity,
			CASE WHEN last_reviewer_fact_at<>'' THEN last_reviewer_fact_at ELSE review_started_at END,recovery_count
			FROM epic_deliveries WHERE epic_id=?`, epicID).
			Scan(&projectID, &state, &stateVersion, &jobID, &reviewer, &progressAt, &recoveryCount); err != nil {
			return err
		}
		if state != "in_review" || jobID == "" || reviewer == "" {
			return nil
		}
		progress, err := time.Parse(rfc3339, progressAt)
		if err != nil || progress.After(now.Add(-stallAfter)) {
			return nil
		}
		j, seq, err := loadJobTx(ctx, tx, jobID)
		if err != nil {
			return err
		}
		if j.State != job.StateCodeReview || j.BoundIdentity != reviewer || j.LeaseID == "" {
			return nil
		}
		// This backstop is intentionally for a LIVE lease. Expired leases are owned
		// by the ordinary liveness reaper and must not be double-revoked here.
		var leaseDeadlineText string
		if err := tx.QueryRowContext(ctx, `SELECT COALESCE(lease_deadline,'') FROM jobs WHERE id=?`, jobID).Scan(&leaseDeadlineText); err != nil {
			return err
		}
		leaseDeadline, err := time.Parse(rfc3339, leaseDeadlineText)
		if err != nil || !leaseDeadline.After(now) {
			return nil
		}
		to := job.StateReviewPending
		kind := ledger.KindLeaseRevoked
		if recoveryCount+1 >= maximumRecoveries {
			to, kind, escalated = job.StateNeedsHuman, ledger.KindStallEscalated, true
		}
		newEpoch := j.LeaseEpoch + 1
		ev := ledger.Event{JobID: jobID, JobSeq: seq + 1, Kind: kind, FromState: j.State,
			ToState: to, LeaseEpoch: newEpoch, Actor: "review-verdict-reconciler", CreatedAt: now,
			Payload: ledger.Payload{RevokeReason: "review_verdict_overdue", StallRevocationsDelta: 1}}
		if err := appendEvent(ctx, tx, ev); err != nil {
			return err
		}
		res, err := tx.ExecContext(ctx, `UPDATE jobs SET state=?,lease_epoch=?,lease_id=NULL,
			bound_identity=NULL,bound_model_family=NULL,lease_hb_due=NULL,lease_deadline=NULL,
			stall_revocations=stall_revocations+1,updated_at=?
			WHERE id=? AND state='code_review' AND lease_epoch=?`, string(to), newEpoch,
			now.UTC().Format(rfc3339), jobID, j.LeaseEpoch)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return nil
		}
		if err := setJobSeq(ctx, tx, jobID, seq+1); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE leases SET ended_at=?,end_reason='review_verdict_overdue'
			WHERE job_id=? AND lease_epoch=? AND ended_at IS NULL`, now.UTC().Format(rfc3339), jobID, j.LeaseEpoch); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE timers SET fired=1 WHERE job_id=? AND expected_epoch=? AND fired=0`, jobID, j.LeaseEpoch); err != nil {
			return err
		}
		if err := projectEpicReviewLeaseEndTx(ctx, tx, jobID, to, "review_verdict_overdue", now); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE epic_deliveries SET recovery_count=recovery_count+1,
			last_recovered_at=?,alert_pending=1,last_error='review_verdict_overdue',updated_at=?
			WHERE epic_id=?`, now.UTC().Format(rfc3339), now.UTC().Format(rfc3339), epicID); err != nil {
			return err
		}
		attentionID := fmt.Sprintf("review-verdict-overdue-%s-%d", epicID, stateVersion)
		dedup := fmt.Sprintf("review_verdict_overdue:%s:%d", epicID, stateVersion)
		_, err = tx.ExecContext(ctx, `INSERT INTO attention_items
			(id,kind,epic_id,repo,priority,state,dedup_key,blocking,leased_by,item_epoch,
			 lease_expires_at,awaiting_since,delivery_key,evidence_json,detail,resolution,
			 verdict,occurrences,first_seen_at,last_seen_at,resolved_at,created_at,updated_at)
			VALUES (?,'review_verdict_overdue',?,'',10,'open',?,1,'',0,'','','',?,
			 'live reviewer lease made no durable verdict progress','','',1,?,?,'',?,?)`,
			attentionID, epicID, dedup,
			fmt.Sprintf(`{"job_id":%q,"reviewer":%q,"progress_at":%q}`, jobID, reviewer, progressAt),
			now.UTC().Format(rfc3339), now.UTC().Format(rfc3339), now.UTC().Format(rfc3339), now.UTC().Format(rfc3339))
		if err != nil && !isUniqueConstraintErr(err) {
			return err
		}
		alertPayload := fmt.Sprintf(`{"epic_id":%q,"job_id":%q,"reviewer":%q,"kind":"review_verdict_overdue"}`,
			epicID, jobID, reviewer)
		if err := ensureControlAlertTx(ctx, tx, projectID, epicID, "review_verdict_overdue", dedup, alertPayload, now); err != nil {
			return err
		}
		changed = true
		return nil
	})
	if errors.Is(err, sql.ErrNoRows) {
		return false, false, nil
	}
	return escalated, changed, err
}
