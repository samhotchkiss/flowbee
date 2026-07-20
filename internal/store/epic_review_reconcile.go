package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/samhotchkiss/flowbee/internal/job"
	"github.com/samhotchkiss/flowbee/internal/ledger"
)

// EpicReviewReconcileResult describes one pass of the v2 review-handoff
// reconciler. The pass is intentionally small and delivery-agnostic: it only
// materializes the durable native review job/obligation and attention row. The
// exact Driver wake action does not exist until a reviewer is atomically claimed.
type EpicReviewReconcileResult struct {
	Scanned    int
	Dispatched int
}

// ReconcileEpicReviewHandoffs repairs the build→review seam. A delivery that
// has an observed green CI result, is explicitly awaiting review dispatch, and
// has exceeded its handoff clock receives exactly one durable review job and one
// deduped attention item. The transaction makes an interrupted dispatcher
// harmless: rerunning the pass finds the same obligation and never double-sends.
//
// This method is flag-gated on the Store. The caller may still invoke it on
// every supervisor tick; disabled instances perform no database work.
func (s *Store) ReconcileEpicReviewHandoffs(ctx context.Context, now time.Time, stallAfter time.Duration) (EpicReviewReconcileResult, error) {
	var out EpicReviewReconcileResult
	if !s.EnableEpicReviewHandoffV2 {
		return out, nil
	}
	if stallAfter <= 0 {
		stallAfter = 5 * time.Minute
	}
	cutoff := now.Add(-stallAfter).Format(rfc3339)
	nowText := now.Format(rfc3339)
	err := s.tx(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `
			SELECT d.epic_id, d.project_id, d.delivery_repo, a.pr_number,
			       d.head_sha, d.base_sha, a.ci_green_observed_at,
			       d.builder_model_family, d.state_version
			  FROM epic_deliveries d
			  JOIN epic_artifacts a ON a.epic_id=d.epic_id
			 WHERE d.state='awaiting_review_dispatch'
			   AND d.review_required=1 AND d.ci_state='green'
			   AND d.review_job_id='' AND d.reviewer_identity='' AND d.verdict=''
			   AND a.pr_number IS NOT NULL AND a.pr_open=1 AND a.is_draft=0
			   AND a.ci_state='green' AND a.ci_has_real_success=1
			   AND a.check_contexts_truncated=0
			   AND a.head_sha=d.head_sha AND a.base_sha=d.base_sha
			   AND a.ci_green_observed_at <> '' AND a.ci_green_observed_at <= ?
			   AND d.hold_reason = ''`, cutoff)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var epicID, projectID, repo, head, base, greenAt, builderFamily string
			var prNumber, stateVersion int
			if err := rows.Scan(&epicID, &projectID, &repo, &prNumber, &head, &base, &greenAt, &builderFamily, &stateVersion); err != nil {
				return err
			}
			out.Scanned++
			if head == "" {
				// A green result without an immutable head cannot be dispatched
				// safely. Keep it visible to the artifact/CI backstop instead.
				continue
			}
			suffixHash := sha256.Sum256([]byte(head + "\x00" + base))
			suffix := hex.EncodeToString(suffixHash[:8])
			jobID := "epic-review-" + epicID + "-" + suffix
			var existingJobID, existingDomain, existingDeliveryID string
			var existingAdopted int
			err := tx.QueryRowContext(ctx, `SELECT id, COALESCE(adopted,0),
					COALESCE(workflow_domain,'legacy'), COALESCE(epic_delivery_id,'')
					FROM jobs
					WHERE (workflow_domain='epic_v2' AND epic_delivery_id=?)
					   OR (repo=? AND pr_number=? AND state<>'cancelled')
					ORDER BY CASE
						WHEN workflow_domain='epic_v2' AND epic_delivery_id=? THEN 0
						WHEN COALESCE(adopted,0)=0 THEN 1
						ELSE 2 END
					LIMIT 1`, epicID, repo, prNumber, epicID).Scan(
				&existingJobID, &existingAdopted, &existingDomain, &existingDeliveryID)
			switch {
			case err == nil && existingDomain == "epic_v2" && existingDeliveryID == epicID:
				// Artifact ingestion may already have absorbed a legacy adopted
				// review into the native domain. Reuse that exact durable job.
				jobID = existingJobID
				if _, err := tx.ExecContext(ctx, `UPDATE jobs SET
						state='review_pending', role='code_reviewer', stage='review', adopted=0,
						required_capabilities=?, repo=?, pr_number=?, head_sha=?, base_sha=?,
						builder_model_family=?, eng_worker_job=id, project_id=?, updated_at=? WHERE id=?`,
					marshalStrings([]string{"role:code_reviewer"}), repo, prNumber,
					head, base, builderFamily, projectID, nowText, jobID); err != nil {
					return err
				}
			case err == nil && existingAdopted == 0:
				// Another originated workflow owns the PR. Put this delivery on a
				// durable, board-visible hold rather than silently retrying forever or
				// creating a second reviewer.
				reason := fmt.Sprintf("PR #%d is already bound to non-adopted job %s", prNumber, existingJobID)
				if _, err := tx.ExecContext(ctx, `UPDATE epic_deliveries SET
						hold_kind='review_job_conflict', hold_reason=?, last_error=?, updated_at=?
					WHERE epic_id=?`, reason, reason, nowText, epicID); err != nil {
					return err
				}
				continue
			case err == nil:
				// Recovery for a database upgraded after legacy adoption but before
				// the next artifact sweep: fence it through the same primitive as
				// ingestion, then create a separate native execution.
				if err := supersedeLegacyAdoptedReviewsForPRTx(ctx, tx, repo, prNumber, epicID, now); err != nil {
					return err
				}
				if err := materializeNativeEpicReviewJobTx(ctx, tx, jobID, epicID, projectID, repo, prNumber, base, head, builderFamily, now); err != nil {
					return err
				}
			case err == sql.ErrNoRows:
				if err := materializeNativeEpicReviewJobTx(ctx, tx, jobID, epicID, projectID, repo, prNumber, base, head, builderFamily, now); err != nil {
					return err
				}
			case err != nil:
				return err
			}
			if err := upsertDomainBFactsTx(ctx, tx, jobID, ReconciledPR{Number: prNumber, UpdatedAt: now, HeadSHA: head, BaseSHA: base, CIGreen: true}); err != nil {
				return err
			}
			// Attention is independently deduped. Its evidence carries the
			// immutable head and observed-green timestamp for operators.
			attentionDedup := fmt.Sprintf("review_dispatch_stalled:%s:%s", epicID, head)
			attentionID := fmt.Sprintf("review-dispatch-stalled-%s-%s", epicID, head)
			_, err = tx.ExecContext(ctx, `
				INSERT INTO attention_items
				 (id, kind, epic_id, repo, priority, state, dedup_key, blocking,
				  leased_by, item_epoch, lease_expires_at, awaiting_since, delivery_key,
				  evidence_json, detail, resolution, verdict, occurrences, first_seen_at,
				  last_seen_at, resolved_at, created_at, updated_at)
				VALUES (?, 'review_dispatch_stalled', ?, '', 10, 'open', ?, 1, '', 0, '', '', ?,
				        ?, 'built CI-green PR has no active reviewer; durable native review obligation queued',
				        '', '', 1, ?, ?, '', ?, ?)`,
				attentionID, epicID, attentionDedup, jobID,
				fmt.Sprintf(`{"head_sha":%q,"base_sha":%q,"ci_green_observed_at":%q,"review_job_id":%q}`, head, base, greenAt, jobID), nowText, nowText, nowText, nowText)
			if err != nil && !isUniqueConstraintErr(err) {
				return fmt.Errorf("materialize review attention %s: %w", epicID, err)
			}
			alertPayload := fmt.Sprintf(`{"epic_id":%q,"job_id":%q,"head_sha":%q,"base_sha":%q,"kind":"review_dispatch_stalled"}`,
				epicID, jobID, head, base)
			if err := ensureControlAlertTx(ctx, tx, projectID, epicID, "review_dispatch_stalled",
				attentionDedup, alertPayload, now); err != nil {
				return fmt.Errorf("materialize review alert %s: %w", epicID, err)
			}
			_, err = tx.ExecContext(ctx, `UPDATE epic_deliveries
				SET state='review_queued', state_version=state_version+1,
				    review_job_id=?, dispatch_attempted_at=?, recovery_count=recovery_count+1,
				    last_recovered_at=?, alert_pending=1, state_entered_at=?,
				    state_due_at=?, fact_progress_at=?, updated_at=?
				WHERE epic_id=? AND state='awaiting_review_dispatch' AND state_version=?`,
				jobID, nowText, nowText, nowText, now.Add(10*time.Minute).Format(rfc3339), nowText, nowText, epicID, stateVersion)
			if err != nil {
				return err
			}
			var epicSeq int
			if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(epic_seq),0)+1 FROM control_events WHERE epic_id=?`, epicID).Scan(&epicSeq); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO control_events
				(project_id,epic_id,kind,from_state,to_state,state_version,epic_seq,actor_kind,payload_json,created_at)
				VALUES (?,?,'review_handoff_recovered','awaiting_review_dispatch','review_queued',?,?, 'reconciler',?,?)`,
				projectID, epicID, stateVersion+1, epicSeq, fmt.Sprintf(`{"job_id":%q}`, jobID), nowText); err != nil {
				return err
			}
			out.Dispatched++
		}
		return rows.Err()
	})
	return out, err
}

func materializeNativeEpicReviewJobTx(ctx context.Context, tx *sql.Tx, jobID, epicID, projectID, repo string, prNumber int, base, head, builderFamily string, now time.Time) error {
	nowText := now.UTC().Format(rfc3339)
	if _, err := tx.ExecContext(ctx, `INSERT INTO jobs
		(id,project_id,kind,flow,stage,state,role,repo,pr_number,base_sha,head_sha,
		 blocked_by,required_capabilities,enqueued_at,lease_epoch,attempts,
		 max_attempts,bounces,max_bounces,job_seq,adopted,opted_in,priority,
		 workflow_domain,epic_delivery_id,builder_model_family,eng_worker_job)
		VALUES (?,?,'build','build','review','review_pending','code_reviewer',?,?,?,?,
		 '[]',?, ?,0,0,5,0,4,1,0,1,5,'epic_v2',?,?,?)`,
		jobID, projectID, repo, prNumber, base, head, marshalStrings([]string{"role:code_reviewer"}),
		nowText, epicID, builderFamily, jobID); err != nil {
		return fmt.Errorf("materialize native epic review job: %w", err)
	}
	ev := ledger.Event{JobID: jobID, JobSeq: 1, Kind: ledger.KindJobCreated,
		ToState: job.StateReviewPending, Actor: "epic-review-reconciler", CreatedAt: now,
		Payload: ledger.Payload{
			Kind: job.KindBuild, Flow: "build", Stage: "review", Role: job.RoleCodeReviewer,
			BaseSHA: base, HeadSHA: head, PRNumber: prNumber,
			RequiredCapabilities: []string{"role:code_reviewer"},
		}}
	return appendEvent(ctx, tx, ev)
}
