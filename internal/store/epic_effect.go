package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// EpicDomainAction is a claimed, epoch-fenced GitHub/control-plane effect. It
// deliberately contains only admission-owned or reconciled identifiers; callers
// cannot substitute a PR, branch, or SHA at execution time.
type EpicDomainAction struct {
	ID, ProjectID, EpicID, Kind, State string
	Repo, Branch, HeadSHA, BaseSHA     string
	MergeCommitSHA                     string
	PRNumber                           int
	Epoch                              int64
}

type EpicEffectReconcileResult struct {
	Scanned, Ensured, Rearmed int
}

// ReconcileEpicEffectActions closes the merge/cleanup transaction seams and
// re-arms the SAME dead-lettered historical action within a bounded automatic
// recovery budget. It never manufactures a replacement dedup key.
func (s *Store) ReconcileEpicEffectActions(ctx context.Context, now time.Time, maximumRecoveries int) (EpicEffectReconcileResult, error) {
	var out EpicEffectReconcileResult
	if !s.EnableEpicReviewHandoffV2 {
		return out, nil
	}
	if maximumRecoveries <= 0 {
		maximumRecoveries = 2
	}
	err := s.tx(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `SELECT d.epic_id,d.project_id,d.state,d.head_sha,d.base_sha,
			d.state_version,d.delivery_repo,d.branch,COALESCE(a.merge_commit_sha,''),COALESCE(a.merged,0)
			FROM epic_deliveries d LEFT JOIN epic_artifacts a ON a.epic_id=d.epic_id
			WHERE d.state IN ('merge_queued','merged','cleanup_pending') AND d.hold_reason=''`)
		if err != nil {
			return err
		}
		type candidate struct {
			epicID, projectID, state, head, base, repo, branch, mergeCommit string
			version, merged                                                 int
		}
		var candidates []candidate
		for rows.Next() {
			var c candidate
			if err := rows.Scan(&c.epicID, &c.projectID, &c.state, &c.head, &c.base,
				&c.version, &c.repo, &c.branch, &c.mergeCommit, &c.merged); err != nil {
				rows.Close()
				return err
			}
			candidates = append(candidates, c)
		}
		if err := rows.Close(); err != nil {
			return err
		}
		for _, c := range candidates {
			out.Scanned++
			switch c.state {
			case "merge_queued":
				if c.head == "" || c.base == "" {
					continue
				}
				dedup := fmt.Sprintf("%s:%s:merge:%s:%s", c.projectID, c.epicID, c.head, c.base)
				before, err := countLiveEpicActionTx(ctx, tx, dedup)
				if err != nil {
					return err
				}
				if err := ensureEpicActionTx(ctx, tx, c.projectID, c.epicID, "merge_dispatch", dedup,
					marshalEpicActionPayload(c.epicID, c.head, c.base, ""), c.head, c.base, now); err != nil {
					return err
				}
				if before == 0 {
					out.Ensured++
				}
			case "merged", "cleanup_pending":
				if c.merged == 0 || c.mergeCommit == "" {
					continue
				}
				dedup := fmt.Sprintf("%s:%s:cleanup:%s", c.projectID, c.epicID, c.mergeCommit)
				before, err := countLiveEpicActionTx(ctx, tx, dedup)
				if err != nil {
					return err
				}
				payload, _ := json.Marshal(map[string]any{
					"epic_id": c.epicID, "repo": c.repo, "branch": c.branch,
					"merge_commit_sha": c.mergeCommit,
					"targets":          []string{"registered_branch", "delivery_reservations"},
				})
				if err := ensureEpicActionTx(ctx, tx, c.projectID, c.epicID, "cleanup", dedup,
					string(payload), c.mergeCommit, "", now); err != nil {
					return err
				}
				if before == 0 {
					out.Ensured++
				}
				if c.state == "merged" {
					nowText := now.UTC().Format(rfc3339)
					res, err := tx.ExecContext(ctx, `UPDATE epic_deliveries SET state='cleanup_pending',
						state_version=state_version+1,state_entered_at=?,state_due_at=?,fact_progress_at=?,updated_at=?
						WHERE epic_id=? AND state='merged' AND state_version=?`, nowText,
						now.Add(10*time.Minute).UTC().Format(rfc3339), nowText, nowText, c.epicID, c.version)
					if err != nil {
						return err
					}
					if n, _ := res.RowsAffected(); n == 1 {
						if err := appendEpicControlEventTx(ctx, tx, c.projectID, c.epicID,
							"cleanup_materialized", "merged", "cleanup_pending", c.version+1,
							"effect_reconciler", `{}`, now); err != nil {
							return err
						}
					}
				}
			}
		}

		// Only the current effect for the current delivery state may consume an
		// automatic recovery. Superseded/terminal actions stay immutable history.
		dead, err := tx.QueryContext(ctx, `SELECT x.id,x.project_id,x.epic_id,x.kind,x.head_sha,x.base_sha,x.recovery_count
			FROM epic_actions x JOIN epic_deliveries d ON d.epic_id=x.epic_id
			WHERE x.state='dead_letter' AND x.kind IN ('merge_dispatch','cleanup','builder_rework','conflict_resolution')
			  AND d.state NOT IN ('complete','abandoned','paused','needs_human')
			ORDER BY x.created_at,x.id`)
		if err != nil {
			return err
		}
		type deadRow struct {
			id, projectID, epicID, kind, head, base string
			recovery                                int
		}
		var deadRows []deadRow
		for dead.Next() {
			var row deadRow
			if err := dead.Scan(&row.id, &row.projectID, &row.epicID, &row.kind, &row.head, &row.base, &row.recovery); err != nil {
				dead.Close()
				return err
			}
			deadRows = append(deadRows, row)
		}
		if err := dead.Close(); err != nil {
			return err
		}
		for _, row := range deadRows {
			if row.recovery > 0 {
				payload, _ := json.Marshal(map[string]any{"action_id": row.id, "action_kind": row.kind,
					"head_sha": row.head, "recovery_count": row.recovery})
				if err := ensureControlAlertTx(ctx, tx, row.projectID, row.epicID, "action_dead_letter",
					fmt.Sprintf("action_dead_letter:%s:recovery:%d", row.id, row.recovery), string(payload), now); err != nil {
					return err
				}
			}
			var state, head, base string
			if err := tx.QueryRowContext(ctx, `SELECT state,head_sha,base_sha FROM epic_deliveries WHERE epic_id=?`, row.epicID).
				Scan(&state, &head, &base); err != nil {
				return err
			}
			current := false
			switch row.kind {
			case "merge_dispatch":
				current = state == "merge_queued" && row.head == head && row.base == base
			case "cleanup":
				current = state == "cleanup_pending"
			case "builder_rework":
				current = state == "changes_requested" || state == "rebuild_in_flight"
			case "conflict_resolution":
				current = state == "conflict_resolution"
			}
			if !current || row.recovery >= maximumRecoveries {
				continue
			}
			res, err := tx.ExecContext(ctx, `UPDATE epic_actions SET state='pending',recovery_count=recovery_count+1,
				next_attempt_at=?,claim_owner='',claim_deadline_at='',dead_lettered_at='',last_error='',updated_at=?
				WHERE id=? AND state='dead_letter' AND recovery_count<?`, now.UTC().Format(rfc3339),
				now.UTC().Format(rfc3339), row.id, maximumRecoveries)
			if err != nil {
				return err
			}
			if n, _ := res.RowsAffected(); n == 1 {
				out.Rearmed++
			}
		}
		return nil
	})
	return out, err
}

func countLiveEpicActionTx(ctx context.Context, tx *sql.Tx, dedup string) (int, error) {
	var count int
	err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM epic_actions WHERE dedup_key=? AND state<>'cancelled_superseded'`, dedup).Scan(&count)
	return count, err
}

// ClaimNextEpicDomainAction claims one GitHub/control-plane effect and validates
// its complete exact-head authorization in the same transaction. A stale action
// is cancelled instead of ever reaching a caller that can mutate GitHub.
func (s *Store) ClaimNextEpicDomainAction(ctx context.Context, owner string, now time.Time, ttl time.Duration) (EpicDomainAction, bool, error) {
	if owner == "" {
		return EpicDomainAction{}, false, errors.New("epic effect claim owner is required")
	}
	if ttl <= 0 {
		ttl = 2 * time.Minute
	}
	var out EpicDomainAction
	found := false
	err := s.tx(ctx, func(tx *sql.Tx) error {
		row := tx.QueryRowContext(ctx, `SELECT x.id,x.project_id,x.epic_id,x.kind,x.state,x.action_epoch,
			x.head_sha,x.base_sha,d.delivery_repo,d.branch,d.state,d.state_version,d.ci_state,
			d.verdict,d.verdict_head_sha,d.verdict_base_sha,d.hold_reason,
			COALESCE(a.pr_number,0),COALESCE(a.head_sha,''),COALESCE(a.base_sha,''),
			COALESCE(a.ci_state,'unknown'),COALESCE(a.ci_has_real_success,0),
			COALESCE(a.check_contexts_truncated,0),COALESCE(a.merged,0),COALESCE(a.merge_commit_sha,'')
			FROM epic_actions x JOIN epic_deliveries d ON d.epic_id=x.epic_id
			LEFT JOIN epic_artifacts a ON a.epic_id=x.epic_id
			WHERE x.state='pending' AND x.executor_kind='domain'
			  AND x.kind IN ('merge_dispatch','cleanup')
			  AND (x.next_attempt_at='' OR julianday(x.next_attempt_at)<=julianday(?))
			ORDER BY x.created_at,x.id LIMIT 1`, now.UTC().Format(rfc3339))
		var deliveryState, ciState, verdict, verdictHead, verdictBase, hold string
		var artifactHead, artifactBase, artifactCI string
		var deliveryVersion, realSuccess, truncated, merged int
		if err := row.Scan(&out.ID, &out.ProjectID, &out.EpicID, &out.Kind, &out.State, &out.Epoch,
			&out.HeadSHA, &out.BaseSHA, &out.Repo, &out.Branch, &deliveryState, &deliveryVersion,
			&ciState, &verdict, &verdictHead, &verdictBase, &hold, &out.PRNumber,
			&artifactHead, &artifactBase, &artifactCI, &realSuccess, &truncated, &merged,
			&out.MergeCommitSHA); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return nil
			}
			return err
		}
		valid := hold == ""
		switch out.Kind {
		case "merge_dispatch":
			valid = valid && deliveryState == "merge_queued" && out.PRNumber > 0 &&
				out.HeadSHA != "" && out.BaseSHA != "" && out.HeadSHA == artifactHead &&
				out.BaseSHA == artifactBase && ciState == "green" && artifactCI == "green" &&
				realSuccess == 1 && truncated == 0 && verdict == "approved" &&
				verdictHead == out.HeadSHA && verdictBase == out.BaseSHA
		case "cleanup":
			valid = valid && deliveryState == "cleanup_pending" && merged == 1 &&
				out.MergeCommitSHA != "" && out.HeadSHA == out.MergeCommitSHA
		}
		nowText := now.UTC().Format(rfc3339)
		if !valid {
			_, err := tx.ExecContext(ctx, `UPDATE epic_actions SET state='cancelled_superseded',
				last_error='effect_authorization_superseded',updated_at=? WHERE id=? AND state='pending'`, nowText, out.ID)
			return err
		}
		res, err := tx.ExecContext(ctx, `UPDATE epic_actions SET state='delivering',action_epoch=action_epoch+1,
			claim_owner=?,claim_deadline_at=?,delivery_started_at=?,attempts=attempts+1,updated_at=?
			WHERE id=? AND state='pending' AND action_epoch=?`, owner,
			now.Add(ttl).UTC().Format(rfc3339), nowText, nowText, out.ID, out.Epoch)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return nil
		}
		out.Epoch++
		out.State = "delivering"
		if out.Kind == "merge_dispatch" {
			res, err := tx.ExecContext(ctx, `UPDATE epic_deliveries SET state='merging',state_version=state_version+1,
				state_entered_at=?,state_due_at=?,fact_progress_at=?,updated_at=?
				WHERE epic_id=? AND state='merge_queued' AND state_version=?`, nowText,
				now.Add(10*time.Minute).UTC().Format(rfc3339), nowText, nowText, out.EpicID, deliveryVersion)
			if err != nil {
				return err
			}
			if n, _ := res.RowsAffected(); n != 1 {
				return errors.New("epic merge delivery changed while claiming effect")
			}
			if err := appendEpicControlEventTx(ctx, tx, out.ProjectID, out.EpicID,
				"merge_claimed", "merge_queued", "merging", deliveryVersion+1,
				"effect_executor", fmt.Sprintf(`{"action_id":%q}`, out.ID), now); err != nil {
				return err
			}
		}
		found = true
		return nil
	})
	return out, found, err
}

func (s *Store) MarkEpicDomainActionVerifying(ctx context.Context, action EpicDomainAction, owner, detail string, now time.Time) error {
	res, err := s.DB.ExecContext(ctx, `UPDATE epic_actions SET state='verifying',claim_owner='',claim_deadline_at='',
		last_error=?,updated_at=? WHERE id=? AND state='delivering' AND claim_owner=? AND action_epoch=?`,
		detail, now.UTC().Format(rfc3339), action.ID, owner, action.Epoch)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return errors.New("stale epic effect epoch")
	}
	return nil
}

func (s *Store) RetryEpicDomainAction(ctx context.Context, action EpicDomainAction, owner, detail string, next, now time.Time) error {
	return s.transitionEpicDomainAction(ctx, action, owner, "pending", detail, next, now)
}

func (s *Store) DeadLetterEpicDomainAction(ctx context.Context, action EpicDomainAction, owner, detail string, now time.Time) error {
	return s.transitionEpicDomainAction(ctx, action, owner, "dead_letter", detail, time.Time{}, now)
}

// HoldEpicMergeAuthorization consumes an unsafe merge attempt and parks the
// delivery at the typed human boundary. It is intentionally distinct from a
// dead letter: a deterministic content/scope/contract denial must not be
// automatically re-armed against the same immutable head.
func (s *Store) HoldEpicMergeAuthorization(ctx context.Context, action EpicDomainAction, owner, detail string, now time.Time) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `UPDATE epic_actions SET state='acknowledged',last_error=?,
			claim_owner='',claim_deadline_at='',updated_at=? WHERE id=? AND state='delivering'
			AND claim_owner=? AND action_epoch=?`, detail, now.UTC().Format(rfc3339), action.ID, owner, action.Epoch)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return errors.New("stale epic effect epoch")
		}
		var projectID string
		var version int
		if err := tx.QueryRowContext(ctx, `SELECT project_id,state_version FROM epic_deliveries
			WHERE epic_id=? AND state='merging'`, action.EpicID).Scan(&projectID, &version); err != nil {
			return err
		}
		nowText := now.UTC().Format(rfc3339)
		res, err = tx.ExecContext(ctx, `UPDATE epic_deliveries SET state='needs_human',state_version=state_version+1,
			hold_kind='merge_authorization_denied',hold_reason=?,return_state='merge_queued',
			state_entered_at=?,state_due_at='',fact_progress_at=?,updated_at=?
			WHERE epic_id=? AND state='merging' AND state_version=?`, detail, nowText, nowText, nowText,
			action.EpicID, version)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return errors.New("epic merge delivery changed while applying authorization hold")
		}
		payload, _ := json.Marshal(map[string]any{"action_id": action.ID, "head_sha": action.HeadSHA, "reason": detail})
		if err := ensureControlAlertTx(ctx, tx, projectID, action.EpicID, "merge_authorization_denied",
			"merge_authorization_denied:"+action.ID, string(payload), now); err != nil {
			return err
		}
		return appendEpicControlEventTx(ctx, tx, projectID, action.EpicID, "merge_authorization_denied",
			"merging", "needs_human", version+1, "github_effect", string(payload), now)
	})
}

func (s *Store) transitionEpicDomainAction(ctx context.Context, action EpicDomainAction, owner, state, detail string, next, now time.Time) error {
	nextText, deadText := formatOptionalTime(next), ""
	if state == "dead_letter" {
		deadText = now.UTC().Format(rfc3339)
	}
	return s.tx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `UPDATE epic_actions SET state=?,last_error=?,next_attempt_at=?,
			dead_lettered_at=?,claim_owner='',claim_deadline_at='',updated_at=?
			WHERE id=? AND state='delivering' AND claim_owner=? AND action_epoch=?`, state, detail,
			nextText, deadText, now.UTC().Format(rfc3339), action.ID, owner, action.Epoch)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return errors.New("stale epic effect epoch")
		}
		if (state != "pending" && state != "dead_letter") || action.Kind != "merge_dispatch" {
			return nil
		}
		var projectID string
		var version int
		if err := tx.QueryRowContext(ctx, `SELECT project_id,state_version FROM epic_deliveries
			WHERE epic_id=? AND state='merging'`, action.EpicID).Scan(&projectID, &version); err != nil {
			return err
		}
		nowText := now.UTC().Format(rfc3339)
		deliveryDue := nextText
		if deliveryDue == "" {
			deliveryDue = now.Add(10 * time.Minute).UTC().Format(rfc3339)
		}
		res, err = tx.ExecContext(ctx, `UPDATE epic_deliveries SET state='merge_queued',state_version=state_version+1,
			state_entered_at=?,state_due_at=?,fact_progress_at=?,updated_at=?
			WHERE epic_id=? AND state='merging' AND state_version=?`, nowText,
			deliveryDue, nowText, nowText, action.EpicID, version)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return errors.New("epic merge delivery changed while retrying effect")
		}
		eventKind := "merge_retry_scheduled"
		if state == "dead_letter" {
			eventKind = "merge_dead_lettered"
		}
		return appendEpicControlEventTx(ctx, tx, projectID, action.EpicID, eventKind,
			"merging", "merge_queued", version+1, "github_effect", fmt.Sprintf(`{"detail":%q}`, detail), now)
	})
}

// MarkEpicMergeSuperseded fences a GitHub head-modified response. It can never
// convert that response into a retry of the unreviewed head.
func (s *Store) MarkEpicMergeSuperseded(ctx context.Context, action EpicDomainAction, owner, detail string, now time.Time) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		var projectID, state string
		var version int
		if err := tx.QueryRowContext(ctx, `SELECT project_id,state,state_version FROM epic_deliveries WHERE epic_id=?`, action.EpicID).
			Scan(&projectID, &state, &version); err != nil {
			return err
		}
		res, err := tx.ExecContext(ctx, `UPDATE epic_actions SET state='cancelled_superseded',last_error=?,
			claim_owner='',claim_deadline_at='',updated_at=? WHERE id=? AND action_epoch=?
			AND ((state='delivering' AND claim_owner=?) OR (state='verifying' AND ?=''))`,
			detail, now.UTC().Format(rfc3339), action.ID, action.Epoch, owner, owner)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return errors.New("stale epic effect epoch")
		}
		nowText := now.UTC().Format(rfc3339)
		res, err = tx.ExecContext(ctx, `UPDATE epic_deliveries SET state='awaiting_ci',state_version=state_version+1,
			verdict='',verdict_head_sha='',verdict_base_sha='',review_job_id='',reviewer_identity='',
			reviewer_model_family='',review_eligible_at='',ci_green_observed_at='',state_entered_at=?,
			state_due_at=?,fact_progress_at=?,updated_at=? WHERE epic_id=? AND state='merging' AND state_version=?`,
			nowText, now.Add(30*time.Minute).UTC().Format(rfc3339), nowText, nowText, action.EpicID, version)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return errors.New("epic merge delivery changed while superseding effect")
		}
		return appendEpicControlEventTx(ctx, tx, projectID, action.EpicID, "merge_superseded", state,
			"awaiting_ci", version+1, "github_effect", fmt.Sprintf(`{"detail":%q}`, detail), now)
	})
}

// MarkEpicMergeConflict records the definite conflict response and creates one
// SHA-bound resolver obligation. The resolver's eventual new head can only travel
// through artifact reconciliation -> fresh CI -> a new independent review.
func (s *Store) MarkEpicMergeConflict(ctx context.Context, action EpicDomainAction, owner, detail string, now time.Time) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		var projectID, state string
		var version int
		if err := tx.QueryRowContext(ctx, `SELECT project_id,state,state_version FROM epic_deliveries WHERE epic_id=?`, action.EpicID).
			Scan(&projectID, &state, &version); err != nil {
			return err
		}
		res, err := tx.ExecContext(ctx, `UPDATE epic_actions SET state='acknowledged',acknowledged_at=?,
			last_error=?,claim_owner='',claim_deadline_at='',updated_at=?
			WHERE id=? AND state='delivering' AND claim_owner=? AND action_epoch=?`, now.UTC().Format(rfc3339),
			detail, now.UTC().Format(rfc3339), action.ID, owner, action.Epoch)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return errors.New("stale epic effect epoch")
		}
		dedup := fmt.Sprintf("%s:%s:conflict_resolution:%s:%s", projectID, action.EpicID, action.HeadSHA, action.BaseSHA)
		payload, _ := json.Marshal(map[string]string{"epic_id": action.EpicID, "head_sha": action.HeadSHA,
			"base_sha": action.BaseSHA, "reason": detail})
		if err := ensureBuilderConflictResolutionActionTx(ctx, tx, projectID, action.EpicID, dedup,
			string(payload), action.HeadSHA, action.BaseSHA, now); err != nil {
			return err
		}
		nowText := now.UTC().Format(rfc3339)
		res, err = tx.ExecContext(ctx, `UPDATE epic_deliveries SET state='conflict_resolution',
			builder_affinity_state='relaunching',
			state_version=state_version+1,state_entered_at=?,state_due_at=?,fact_progress_at=?,updated_at=?
			WHERE epic_id=? AND state='merging' AND state_version=?`, nowText,
			now.Add(30*time.Minute).UTC().Format(rfc3339), nowText, nowText, action.EpicID, version)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return errors.New("epic merge delivery changed while recording conflict")
		}
		return appendEpicControlEventTx(ctx, tx, projectID, action.EpicID, "merge_conflict", state,
			"conflict_resolution", version+1, "github_effect", fmt.Sprintf(`{"detail":%q}`, detail), now)
	})
}

// CompleteEpicCleanup consumes a mechanically verified cleanup effect and is the
// sole normal edge to complete. Replaying it is harmless.
func (s *Store) CompleteEpicCleanup(ctx context.Context, action EpicDomainAction, owner string, now time.Time) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		var projectID, state string
		var version int
		if err := tx.QueryRowContext(ctx, `SELECT project_id,state,state_version FROM epic_deliveries WHERE epic_id=?`, action.EpicID).
			Scan(&projectID, &state, &version); err != nil {
			return err
		}
		dedicatedWorkers, err := dedicatedEpicWorkersEnabledTx(ctx, s, tx)
		if err != nil {
			return err
		}
		if dedicatedWorkers {
			var total, stopped int
			if err := tx.QueryRowContext(ctx, `SELECT COUNT(*),
				COALESCE(SUM(CASE WHEN state='stopped' THEN 1 ELSE 0 END),0)
				FROM epic_worker_sessions WHERE epic_id=?`, action.EpicID).Scan(&total, &stopped); err != nil {
				return err
			}
			if total != 2 || stopped != 2 {
				return fmt.Errorf("epic cleanup blocked until both dedicated workers are mechanically stopped: %d/%d", stopped, total)
			}
		}
		res, err := tx.ExecContext(ctx, `UPDATE epic_actions SET state='acknowledged',acknowledged_at=?,
			claim_owner='',claim_deadline_at='',last_error='',updated_at=?
			WHERE id=? AND state IN ('delivering','verifying') AND (claim_owner=? OR claim_owner='') AND action_epoch=?`,
			now.UTC().Format(rfc3339), now.UTC().Format(rfc3339), action.ID, owner, action.Epoch)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return errors.New("stale epic effect epoch")
		}
		nowText := now.UTC().Format(rfc3339)
		res, err = tx.ExecContext(ctx, `UPDATE epic_deliveries SET state='complete',state_version=state_version+1,
			review_required=0,builder_affinity_state='complete',state_entered_at=?,state_due_at='',
			fact_progress_at=?,alert_pending=0,updated_at=? WHERE epic_id=? AND state='cleanup_pending' AND state_version=?`,
			nowText, nowText, nowText, action.EpicID, version)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return errors.New("epic cleanup delivery changed concurrently")
		}
		return appendEpicControlEventTx(ctx, tx, projectID, action.EpicID, "cleanup_complete", state,
			"complete", version+1, "github_effect", `{}`, now)
	})
}

// ReclaimExpiredEpicDomainActions preserves uncertainty across executor death.
// A replacement process verifies the original epoch against GitHub facts; it does
// not make the action pending and blindly repeat the mutation.
func (s *Store) ReclaimExpiredEpicDomainActions(ctx context.Context, now time.Time) (int64, error) {
	res, err := s.DB.ExecContext(ctx, `UPDATE epic_actions SET state='verifying',claim_owner='',claim_deadline_at='',
		last_error='executor_claim_expired',updated_at=? WHERE executor_kind='domain'
		AND kind IN ('merge_dispatch','cleanup') AND state='delivering' AND claim_deadline_at<>''
		AND julianday(claim_deadline_at)<=julianday(?)`, now.UTC().Format(rfc3339), now.UTC().Format(rfc3339))
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// GrantEpicActionRecoveryBudget is the typed human-clear edge for an exhausted
// effect. It re-arms the same dead-lettered row and records the authorization in
// the epic ledger; it never creates a replacement semantic key.
func (s *Store) GrantEpicActionRecoveryBudget(ctx context.Context, epicID, headSHA, recoveryCode string, now time.Time) (bool, error) {
	kind := map[string]string{
		"merge_dispatch_stalled":      "merge_dispatch",
		"merge_outcome_uncertain":     "merge_dispatch",
		"cleanup_overdue":             "cleanup",
		"rework_dispatch_stalled":     "builder_rework",
		"conflict_resolution_stalled": "conflict_resolution",
	}[recoveryCode]
	if epicID == "" || headSHA == "" || kind == "" {
		return false, errors.New("epic action recovery requires epic, head SHA, and known recovery code")
	}
	granted := false
	err := s.tx(ctx, func(tx *sql.Tx) error {
		var projectID, actionID string
		var version int
		err := tx.QueryRowContext(ctx, `SELECT d.project_id,d.state_version,x.id
			FROM epic_deliveries d JOIN epic_actions x ON x.epic_id=d.epic_id
			WHERE d.epic_id=? AND x.kind=? AND x.head_sha=? AND x.state='dead_letter'
			ORDER BY x.updated_at DESC,x.id DESC LIMIT 1`, epicID, kind, headSHA).
			Scan(&projectID, &version, &actionID)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		nowText := now.UTC().Format(rfc3339)
		res, err := tx.ExecContext(ctx, `UPDATE epic_actions SET state='pending',recovery_count=0,
			next_attempt_at=?,claim_owner='',claim_deadline_at='',dead_lettered_at='',last_error='',updated_at=?
			WHERE id=? AND state='dead_letter'`, nowText, nowText, actionID)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return nil
		}
		payload, _ := json.Marshal(map[string]string{"action_id": actionID, "head_sha": headSHA,
			"recovery_code": recoveryCode})
		if err := appendEpicControlEventTx(ctx, tx, projectID, epicID, "effect_recovery_budget_granted",
			"", "", version, "human_decision", string(payload), now); err != nil {
			return err
		}
		granted = true
		return nil
	})
	return granted, err
}

func (s *Store) NextVerifyingEpicDomainAction(ctx context.Context, now time.Time) (EpicDomainAction, bool, error) {
	var out EpicDomainAction
	err := s.DB.QueryRowContext(ctx, `SELECT x.id,x.project_id,x.epic_id,x.kind,x.state,x.action_epoch,
		x.head_sha,x.base_sha,d.delivery_repo,d.branch,COALESCE(a.pr_number,0),COALESCE(a.merge_commit_sha,'')
		FROM epic_actions x JOIN epic_deliveries d ON d.epic_id=x.epic_id
		LEFT JOIN epic_artifacts a ON a.epic_id=x.epic_id
		WHERE x.state='verifying' AND x.executor_kind='domain' AND x.kind IN ('merge_dispatch','cleanup')
		  AND (x.next_attempt_at='' OR julianday(x.next_attempt_at)<=julianday(?))
		ORDER BY x.updated_at,x.id LIMIT 1`, now.UTC().Format(rfc3339)).Scan(&out.ID, &out.ProjectID, &out.EpicID, &out.Kind,
		&out.State, &out.Epoch, &out.HeadSHA, &out.BaseSHA, &out.Repo, &out.Branch,
		&out.PRNumber, &out.MergeCommitSHA)
	if errors.Is(err, sql.ErrNoRows) {
		return EpicDomainAction{}, false, nil
	}
	return out, err == nil, err
}

func (s *Store) DeferVerifyingEpicDomainAction(ctx context.Context, action EpicDomainAction, detail string, next, now time.Time) error {
	res, err := s.DB.ExecContext(ctx, `UPDATE epic_actions SET next_attempt_at=?,last_error=?,updated_at=?
		WHERE id=? AND state='verifying' AND action_epoch=?`, next.UTC().Format(rfc3339), detail,
		now.UTC().Format(rfc3339), action.ID, action.Epoch)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return errors.New("stale epic effect epoch")
	}
	return nil
}

func (s *Store) AcknowledgeVerifiedMerge(ctx context.Context, action EpicDomainAction, now time.Time) error {
	res, err := s.DB.ExecContext(ctx, `UPDATE epic_actions SET state='acknowledged',acknowledged_at=?,
		last_error='',updated_at=? WHERE id=? AND state='verifying' AND action_epoch=?`,
		now.UTC().Format(rfc3339), now.UTC().Format(rfc3339), action.ID, action.Epoch)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return errors.New("stale epic effect epoch")
	}
	return nil
}

// RecordEpicMergeFactFromEffect folds an authoritative PR read obtained while
// verifying an uncertain merge. The concrete merge commit and cleanup obligation
// are committed together, closing the post-merge-fact/pre-cleanup crash seam.
func (s *Store) RecordEpicMergeFactFromEffect(ctx context.Context, action EpicDomainAction, mergeCommit string, now time.Time) error {
	if mergeCommit == "" {
		return errors.New("verified merge fact requires a concrete merge commit")
	}
	return s.tx(ctx, func(tx *sql.Tx) error {
		var projectID, state, repo, branch string
		var version int
		if err := tx.QueryRowContext(ctx, `SELECT project_id,state,state_version,delivery_repo,branch
			FROM epic_deliveries WHERE epic_id=?`, action.EpicID).
			Scan(&projectID, &state, &version, &repo, &branch); err != nil {
			return err
		}
		res, err := tx.ExecContext(ctx, `UPDATE epic_actions SET state='acknowledged',acknowledged_at=?,
			last_error='',claim_owner='',claim_deadline_at='',updated_at=?
			WHERE id=? AND state='verifying' AND action_epoch=?`, now.UTC().Format(rfc3339),
			now.UTC().Format(rfc3339), action.ID, action.Epoch)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return errors.New("stale epic effect epoch")
		}
		nowText := now.UTC().Format(rfc3339)
		if _, err := tx.ExecContext(ctx, `UPDATE epic_artifacts SET merged=1,pr_open=0,
			merge_commit_sha=?,source_observed_at=?,updated_at=? WHERE epic_id=?`, mergeCommit,
			nowText, nowText, action.EpicID); err != nil {
			return err
		}
		dedup := fmt.Sprintf("%s:%s:cleanup:%s", projectID, action.EpicID, mergeCommit)
		payload, _ := json.Marshal(map[string]any{"epic_id": action.EpicID, "repo": repo,
			"branch": branch, "merge_commit_sha": mergeCommit,
			"targets": []string{"registered_branch", "delivery_reservations"}})
		if err := ensureEpicActionTx(ctx, tx, projectID, action.EpicID, "cleanup", dedup,
			string(payload), mergeCommit, "", now); err != nil {
			return err
		}
		res, err = tx.ExecContext(ctx, `UPDATE epic_deliveries SET state='cleanup_pending',
			state_version=state_version+1,state_entered_at=?,state_due_at=?,fact_progress_at=?,updated_at=?
			WHERE epic_id=? AND state IN ('merging','merged') AND state_version=?`, nowText,
			now.Add(10*time.Minute).UTC().Format(rfc3339), nowText, nowText, action.EpicID, version)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return errors.New("epic merge delivery changed while folding merge fact")
		}
		return appendEpicControlEventTx(ctx, tx, projectID, action.EpicID, "merge_verified", state,
			"cleanup_pending", version+1, "github_reconcile", fmt.Sprintf(`{"merge_commit_sha":%q}`, mergeCommit), now)
	})
}

// RequeueVerifiedEpicDomainAction is legal only after an authoritative read has
// proved the uncertain mutation did not take effect. It retains the same row and
// semantic key.
func (s *Store) RequeueVerifiedEpicDomainAction(ctx context.Context, action EpicDomainAction, detail string, next, now time.Time) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `UPDATE epic_actions SET state='pending',next_attempt_at=?,
			last_error=?,claim_owner='',claim_deadline_at='',updated_at=?
			WHERE id=? AND state='verifying' AND action_epoch=?`, next.UTC().Format(rfc3339),
			detail, now.UTC().Format(rfc3339), action.ID, action.Epoch)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return errors.New("stale epic effect epoch")
		}
		if action.Kind != "merge_dispatch" {
			return nil
		}
		var projectID string
		var version int
		if err := tx.QueryRowContext(ctx, `SELECT project_id,state_version FROM epic_deliveries
			WHERE epic_id=? AND state='merging'`, action.EpicID).Scan(&projectID, &version); err != nil {
			return err
		}
		nowText := now.UTC().Format(rfc3339)
		res, err = tx.ExecContext(ctx, `UPDATE epic_deliveries SET state='merge_queued',state_version=state_version+1,
			state_entered_at=?,state_due_at=?,fact_progress_at=?,updated_at=?
			WHERE epic_id=? AND state='merging' AND state_version=?`, nowText,
			next.UTC().Format(rfc3339), nowText, nowText, action.EpicID, version)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return errors.New("epic merge delivery changed while requeueing verified no-effect")
		}
		return appendEpicControlEventTx(ctx, tx, projectID, action.EpicID, "merge_no_effect_verified",
			"merging", "merge_queued", version+1, "github_reconcile", fmt.Sprintf(`{"detail":%q}`, detail), now)
	})
}
