package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/samhotchkiss/flowbee/internal/job"
	"github.com/samhotchkiss/flowbee/internal/ledger"
)

var (
	ErrEpicArtifactOwnershipAmbiguous = errors.New("epic artifact ownership is ambiguous")
	ErrEpicArtifactIdentityMismatch   = errors.New("epic artifact repository or branch does not match its delivery")
	ErrEpicArtifactPRConflict         = errors.New("epic artifact is already bound to a different pull request")
	ErrEpicArtifactProjectMismatch    = errors.New("epic artifact project does not own epic")
)

// EpicDeliveryOwner is the admission-owned identity used to associate a later
// GitHub artifact. Repo and Branch are desired state; neither labels nor PR
// titles can manufacture this record.
type EpicDeliveryOwner struct {
	EpicID, ProjectID, Repo, Branch string
}

// EpicDeliveryForRepoBranch resolves an exact admitted delivery branch. It
// refuses ambiguity rather than guessing across projects that accidentally
// selected the same repository branch.
func (s *Store) EpicDeliveryForRepoBranch(ctx context.Context, repo, branch string) (EpicDeliveryOwner, bool, error) {
	if branch == "" {
		return EpicDeliveryOwner{}, false, nil
	}
	rows, err := s.DB.QueryContext(ctx, `
		SELECT epic_id, project_id, delivery_repo, branch
		  FROM epic_deliveries
		 WHERE delivery_repo=? AND branch=?
		   AND state NOT IN ('complete','abandoned')
		 ORDER BY epic_id
		 LIMIT 2`, repo, branch)
	if err != nil {
		return EpicDeliveryOwner{}, false, err
	}
	defer rows.Close()
	var owners []EpicDeliveryOwner
	for rows.Next() {
		var owner EpicDeliveryOwner
		if err := rows.Scan(&owner.EpicID, &owner.ProjectID, &owner.Repo, &owner.Branch); err != nil {
			return EpicDeliveryOwner{}, false, err
		}
		owners = append(owners, owner)
	}
	if err := rows.Err(); err != nil {
		return EpicDeliveryOwner{}, false, err
	}
	if len(owners) == 0 {
		return EpicDeliveryOwner{}, false, nil
	}
	if len(owners) > 1 {
		return EpicDeliveryOwner{}, false, fmt.Errorf("%w: repo=%q branch=%q", ErrEpicArtifactOwnershipAmbiguous, repo, branch)
	}
	return owners[0], true, nil
}

// EpicArtifactFact is authoritative reconcile-in data. Agent prose must never
// populate it; callers build it only from the delivery repository and CI APIs.
type EpicArtifactFact struct {
	EpicID string
	// ProjectID is an optional caller assertion. Ownership is always loaded from
	// the admitted epic delivery; a mismatch is rejected and never persisted.
	ProjectID                   string
	Repo, Branch                string
	PRNumber                    int
	PROpen, Draft, Merged       bool
	HeadSHA, BaseSHA            string
	CIState                     string // unknown|none|pending|green|red|infra_red
	CIHasRealSuccess            bool
	RequiredChecksPresentPassed bool
	CheckContextsTruncated      bool
	RequiredChecks              []string
	MergeableState              string
	MergeCommitSHA              string
	BuilderComplete             bool
	SourceWatermark             int64
	SourceUpdatedAt             time.Time
}

// ObserveEpicArtifactFact folds one authoritative repository observation and
// the delivery transition in one transaction. A real-green fact durably enters
// awaiting_review_dispatch before any external review action can occur.
func (s *Store) ObserveEpicArtifactFact(ctx context.Context, fact EpicArtifactFact, now time.Time) error {
	if fact.EpicID == "" {
		return errors.New("epic artifact fact: epic id is required")
	}
	checks, err := json.Marshal(fact.RequiredChecks)
	if err != nil {
		return err
	}
	nowText := now.UTC().Format(rfc3339)
	return s.tx(ctx, func(tx *sql.Tx) error {
		var projectID, deliveryRepo, deliveryBranch, oldState, oldHead, oldBase, oldGreenAt, hold string
		var stateVersion, artifactVersion, reviewRequired int
		var boundPR int
		var oldSourceWatermark int64
		err := tx.QueryRowContext(ctx, `
			SELECT d.project_id, d.delivery_repo, d.branch, d.state, d.head_sha,
			       d.base_sha, d.hold_reason, d.state_version, d.artifact_version,
			       d.review_required, COALESCE(a.pr_number,0),
			       COALESCE(a.ci_green_observed_at,''), COALESCE(a.source_watermark,0)
			  FROM epic_deliveries d
			  LEFT JOIN epic_artifacts a ON a.epic_id=d.epic_id
			 WHERE d.epic_id=?`, fact.EpicID).Scan(
			&projectID, &deliveryRepo, &deliveryBranch, &oldState, &oldHead,
			&oldBase, &hold, &stateVersion, &artifactVersion, &reviewRequired,
			&boundPR, &oldGreenAt, &oldSourceWatermark)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrEpicRunNotFound
		}
		if err != nil {
			return err
		}
		if fact.ProjectID != "" && fact.ProjectID != projectID {
			return fmt.Errorf("%w: epic=%s owner=%s asserted=%s",
				ErrEpicArtifactProjectMismatch, fact.EpicID, projectID, fact.ProjectID)
		}
		if oldState == "complete" || oldState == "abandoned" || reviewRequired == 0 {
			return nil
		}
		if fact.Repo != deliveryRepo || fact.Branch != deliveryBranch {
			return fmt.Errorf("%w: epic=%s want=%s:%s got=%s:%s",
				ErrEpicArtifactIdentityMismatch, fact.EpicID, deliveryRepo, deliveryBranch, fact.Repo, fact.Branch)
		}
		if boundPR > 0 && fact.PRNumber > 0 && boundPR != fact.PRNumber {
			return fmt.Errorf("%w: epic=%s bound_pr=%d observed_pr=%d",
				ErrEpicArtifactPRConflict, fact.EpicID, boundPR, fact.PRNumber)
		}
		if fact.SourceWatermark > 0 && oldSourceWatermark > fact.SourceWatermark {
			return nil // a delayed observation cannot move authoritative facts backward
		}
		realGreen := fact.CIState == "green" && fact.CIHasRealSuccess &&
			fact.RequiredChecksPresentPassed && !fact.CheckContextsTruncated &&
			fact.PROpen && !fact.Draft && fact.HeadSHA != "" && fact.BaseSHA != ""
		ciState := fact.CIState
		if ciState == "green" && !realGreen {
			ciState = "pending"
		}
		moved := (oldHead != "" || oldBase != "") && (oldHead != fact.HeadSHA || oldBase != fact.BaseSHA)
		superseded := moved && !fact.Merged
		if oldHead == "" && fact.HeadSHA != "" {
			artifactVersion++
		} else if moved {
			artifactVersion++
		}
		if artifactVersion == 0 && fact.HeadSHA != "" {
			artifactVersion = 1
		}
		greenAt := ""
		if realGreen {
			// The first-green time is the recovery clock. Re-observing the same
			// green SHA must not postpone dispatch forever on every sweep.
			if !moved && oldHead == fact.HeadSHA && oldBase == fact.BaseSHA && oldGreenAt != "" {
				greenAt = oldGreenAt
			} else {
				greenAt = nowText
			}
		}
		var prNumber any
		if fact.PRNumber > 0 {
			prNumber = fact.PRNumber
		}
		_, err = tx.ExecContext(ctx, `
			INSERT INTO epic_artifacts
			 (epic_id, project_id, repo, branch, pr_number, pr_bound_at, head_sha,
			  base_sha, head_updated_at, artifact_version, is_draft, pr_open,
			  closed_unmerged, ci_state, ci_has_real_success, required_checks_json,
			  check_contexts_truncated, ci_green_observed_at, mergeable_state, merged,
			  merge_commit_sha, source_observed_at, source_updated_at, source_watermark,
			  created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, CASE WHEN ?>0 THEN ? ELSE '' END, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(epic_id) DO UPDATE SET
			 repo=excluded.repo, branch=excluded.branch, pr_number=excluded.pr_number,
			 pr_bound_at=CASE WHEN epic_artifacts.pr_bound_at='' AND excluded.pr_number IS NOT NULL THEN excluded.pr_bound_at ELSE epic_artifacts.pr_bound_at END,
			 head_sha=excluded.head_sha, base_sha=excluded.base_sha,
			 head_updated_at=excluded.head_updated_at, artifact_version=excluded.artifact_version,
			 is_draft=excluded.is_draft, pr_open=excluded.pr_open,
			 closed_unmerged=excluded.closed_unmerged, ci_state=excluded.ci_state,
			 ci_has_real_success=excluded.ci_has_real_success,
			 required_checks_json=excluded.required_checks_json,
			 check_contexts_truncated=excluded.check_contexts_truncated,
			 ci_green_observed_at=excluded.ci_green_observed_at,
			 mergeable_state=excluded.mergeable_state, merged=excluded.merged,
			 merge_commit_sha=excluded.merge_commit_sha,
			 source_observed_at=excluded.source_observed_at,
			 source_updated_at=excluded.source_updated_at,
			 source_watermark=excluded.source_watermark, updated_at=excluded.updated_at`,
			fact.EpicID, projectID, fact.Repo, fact.Branch, prNumber, fact.PRNumber, nowText,
			fact.HeadSHA, fact.BaseSHA, nowText, artifactVersion, boolInt(fact.Draft),
			boolInt(fact.PROpen), boolInt(!fact.PROpen && fact.PRNumber > 0 && !fact.Merged),
			ciState, boolInt(fact.CIHasRealSuccess), string(checks), boolInt(fact.CheckContextsTruncated),
			greenAt, fact.MergeableState, boolInt(fact.Merged), fact.MergeCommitSHA, nowText,
			formatOptionalTime(fact.SourceUpdatedAt), fact.SourceWatermark, nowText, nowText)
		if err != nil {
			return fmt.Errorf("upsert epic artifact: %w", err)
		}

		newState := oldState
		due := now.Add(15 * time.Minute)
		switch {
		case fact.Merged && fact.MergeCommitSHA != "":
			// The authoritative merge fact and its cleanup obligation are one
			// transaction. A crash after this commit cannot strand a bare `merged`
			// delivery with no action.
			newState, due = "cleanup_pending", now.Add(10*time.Minute)
			cleanupDedup := fmt.Sprintf("%s:%s:cleanup:%s", projectID, fact.EpicID, fact.MergeCommitSHA)
			cleanupPayload, _ := json.Marshal(map[string]any{
				"epic_id": fact.EpicID, "repo": deliveryRepo, "branch": deliveryBranch,
				"merge_commit_sha": fact.MergeCommitSHA,
				"targets":          []string{"registered_branch", "delivery_reservations"},
			})
			if err := ensureEpicActionTx(ctx, tx, projectID, fact.EpicID, "cleanup", cleanupDedup,
				string(cleanupPayload), fact.MergeCommitSHA, "", now); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `UPDATE epic_actions SET state='acknowledged',
				acknowledged_at=?,claim_owner='',claim_deadline_at='',last_error='merge_fact_observed',updated_at=?
				WHERE epic_id=? AND kind='merge_dispatch' AND state IN ('pending','delivering','verifying')
				  AND head_sha=? AND base_sha=?`, nowText, nowText, fact.EpicID, fact.HeadSHA, fact.BaseSHA); err != nil {
				return err
			}
			if err := ensureEpicWorkerStopIntentsTx(ctx, tx, projectID, fact.EpicID, now); err != nil {
				return err
			}
		case fact.Merged:
			// GitHub has not yet supplied the concrete commit identity needed to key
			// cleanup. Keep the explicit intermediate state under its due clock.
			newState, due = "merged", now.Add(5*time.Minute)
			if err := ensureEpicWorkerStopIntentsTx(ctx, tx, projectID, fact.EpicID, now); err != nil {
				return err
			}
		case fact.PRNumber == 0 || !fact.PROpen:
			if fact.BuilderComplete || oldState != "building" {
				newState, due = "awaiting_artifact", now.Add(10*time.Minute)
			}
		case realGreen:
			switch oldState {
			case "review_queued", "in_review", "approved", "merge_queued", "merging", "merged", "cleanup_pending":
				// An unchanged green fact cannot regress a delivery whose review or
				// downstream work is already durable.
				newState = oldState
			default:
				newState, due = "awaiting_review_dispatch", now.Add(5*time.Minute)
			}
		case ciState == "red":
			newState, due = "rebuild_in_flight", now.Add(10*time.Minute)
		default:
			newState, due = "awaiting_ci", now.Add(30*time.Minute)
		}
		if superseded {
			newState = "awaiting_ci"
			if _, err := tx.ExecContext(ctx, `
				UPDATE epic_actions SET state='cancelled_superseded', updated_at=?
				 WHERE epic_id=? AND state IN ('pending','delivering','verifying')
				   AND (head_sha<>? OR base_sha<>?)`, nowText, fact.EpicID, fact.HeadSHA, fact.BaseSHA); err != nil {
				return err
			}
			// Fence an active or completed review job for the old artifact. The
			// materializer intentionally reuses this durable row only after the new
			// SHA is independently CI-green; no old lease/verdict survives the move.
			if _, err := tx.ExecContext(ctx, `UPDATE jobs SET state='cancelled',verdict=NULL,
				lease_epoch=lease_epoch+1,lease_id=NULL,bound_identity=NULL,bound_model_family=NULL,
				bound_lens=NULL,lease_hb_due=NULL,lease_deadline=NULL,phase_deadline_at=NULL,updated_at=?
				WHERE workflow_domain='epic_v2' AND epic_delivery_id=?`, nowText, fact.EpicID); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `UPDATE leases SET ended_at=?,end_reason='artifact_superseded'
				WHERE job_id IN (SELECT id FROM jobs WHERE workflow_domain='epic_v2' AND epic_delivery_id=?)
				  AND ended_at IS NULL`, nowText, fact.EpicID); err != nil {
				return err
			}
		}
		stateVersion++
		_, err = tx.ExecContext(ctx, `
			UPDATE epic_deliveries
			   SET state=?, state_version=?, ci_state=?, artifact_version=?, head_sha=?, base_sha=?,
			       ci_green_observed_at=?, review_eligible_at=?, dispatch_due_at=?,
			       state_entered_at=CASE WHEN state<>? THEN ? ELSE state_entered_at END,
			       state_due_at=?, fact_progress_at=?,
			       review_job_id=CASE WHEN ? THEN '' ELSE review_job_id END,
			       reviewer_identity=CASE WHEN ? THEN '' ELSE reviewer_identity END,
			       reviewer_model_family=CASE WHEN ? THEN '' ELSE reviewer_model_family END,
			       verdict=CASE WHEN ? THEN '' ELSE verdict END,
			       verdict_head_sha=CASE WHEN ? THEN '' ELSE verdict_head_sha END,
			       verdict_base_sha=CASE WHEN ? THEN '' ELSE verdict_base_sha END,
			       updated_at=?
			 WHERE epic_id=? AND state_version=?`,
			newState, stateVersion, ciState, artifactVersion, fact.HeadSHA, fact.BaseSHA,
			greenAt, greenAt, due.UTC().Format(rfc3339), newState, nowText,
			due.UTC().Format(rfc3339), nowText,
			superseded, superseded, superseded, superseded, superseded, superseded, nowText, fact.EpicID, stateVersion-1)
		if err != nil {
			return err
		}
		if fact.PRNumber > 0 {
			if err := absorbOwnedEpicAdoptedJobsTx(ctx, tx, fact, now); err != nil {
				return err
			}
		}
		var epicSeq int
		if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(epic_seq),0)+1 FROM control_events WHERE epic_id=?`, fact.EpicID).Scan(&epicSeq); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO control_events
			(project_id, epic_id, kind, from_state, to_state, state_version, epic_seq, actor_kind, payload_json, created_at)
			VALUES (?, ?, 'artifact_reconciled', ?, ?, ?, ?, 'github_reconcile', ?, ?)`,
			projectID, fact.EpicID, oldState, newState, stateVersion, epicSeq,
			fmt.Sprintf(`{"head_sha":%q,"base_sha":%q,"ci_state":%q,"real_green":%t}`, fact.HeadSHA, fact.BaseSHA, ciState, realGreen), nowText)
		return err
	})
}

// EpicArtifactView is the project-owned artifact projection exposed to the
// workspace API. It contains repository facts only; workflow transition truth
// remains in epic_deliveries.
type EpicArtifactView struct {
	ProjectID       string `json:"project_id"`
	EpicID          string `json:"epic_id"`
	Repo            string `json:"repo"`
	Branch          string `json:"branch"`
	PRNumber        int    `json:"pr_number"`
	HeadSHA         string `json:"head_sha"`
	BaseSHA         string `json:"base_sha"`
	ArtifactVersion int    `json:"artifact_version"`
	CIState         string `json:"ci_state"`
	Merged          bool   `json:"merged"`
	MergeCommitSHA  string `json:"merge_commit_sha"`
}

// ListEpicArtifactsForProject filters in SQL by the owning project. Repeated
// branch labels or other human-facing artifact names in another project cannot
// enter the returned slice.
func (s *Store) ListEpicArtifactsForProject(ctx context.Context, projectID string) ([]EpicArtifactView, error) {
	projectID = strings.TrimSpace(projectID)
	if projectID == "" {
		return nil, ErrProjectNotFound
	}
	rows, err := s.DB.QueryContext(ctx, `SELECT project_id,epic_id,repo,branch,
		COALESCE(pr_number,0),head_sha,base_sha,artifact_version,ci_state,merged,merge_commit_sha
		FROM epic_artifacts WHERE project_id=? ORDER BY epic_id`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []EpicArtifactView{}
	for rows.Next() {
		var item EpicArtifactView
		var merged int
		if err := rows.Scan(&item.ProjectID, &item.EpicID, &item.Repo, &item.Branch,
			&item.PRNumber, &item.HeadSHA, &item.BaseSHA, &item.ArtifactVersion,
			&item.CIState, &merged, &item.MergeCommitSHA); err != nil {
			return nil, err
		}
		item.Merged = merged != 0
		out = append(out, item)
	}
	return out, rows.Err()
}

// GetEpicArtifactForProject resolves an artifact only inside an exact project.
// A real artifact owned by another project is intentionally indistinguishable
// from a missing artifact to a project-scoped caller.
func (s *Store) GetEpicArtifactForProject(ctx context.Context, projectID, epicID string) (EpicArtifactView, error) {
	projectID, epicID = strings.TrimSpace(projectID), strings.TrimSpace(epicID)
	if projectID == "" || epicID == "" {
		return EpicArtifactView{}, ErrEpicRunNotFound
	}
	var item EpicArtifactView
	var merged int
	err := s.DB.QueryRowContext(ctx, `SELECT project_id,epic_id,repo,branch,
		COALESCE(pr_number,0),head_sha,base_sha,artifact_version,ci_state,merged,merge_commit_sha
		FROM epic_artifacts WHERE project_id=? AND epic_id=?`, projectID, epicID).Scan(
		&item.ProjectID, &item.EpicID, &item.Repo, &item.Branch, &item.PRNumber,
		&item.HeadSHA, &item.BaseSHA, &item.ArtifactVersion, &item.CIState, &merged, &item.MergeCommitSHA)
	if errors.Is(err, sql.ErrNoRows) {
		return EpicArtifactView{}, ErrEpicRunNotFound
	}
	if err != nil {
		return EpicArtifactView{}, err
	}
	item.Merged = merged != 0
	return item, nil
}

// absorbOwnedEpicAdoptedJobsTx supersedes the legitimate collision left by a
// rolling deploy: a legacy label sweep may already have created adopted review
// work before the epic-owned branch fence was active. Every such execution is
// retained as cancelled history, but its epoch, lease, timers, pending effects,
// and stale verdict are fenced in the same transaction as the authoritative
// artifact fact. The handoff reconciler later creates one deterministic native
// review job; it never revives this legacy execution.
func absorbOwnedEpicAdoptedJobsTx(ctx context.Context, tx *sql.Tx, fact EpicArtifactFact, now time.Time) error {
	return supersedeLegacyAdoptedReviewsForPRTx(ctx, tx, fact.Repo, fact.PRNumber, fact.EpicID, now)
}

// supersedeLegacyAdoptedReviewsForPRTx is shared by artifact ingestion and the
// handoff materializer's recovery path, so there is exactly one implementation
// of legacy execution fencing.
func supersedeLegacyAdoptedReviewsForPRTx(ctx context.Context, tx *sql.Tx, repo string, prNumber int, epicID string, now time.Time) error {
	rows, err := tx.QueryContext(ctx, `
		SELECT id
		  FROM jobs
		 WHERE repo=? AND pr_number=? AND adopted=1 AND state<>'cancelled'
		 ORDER BY id`, repo, prNumber)
	if err != nil {
		return err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if err := rows.Err(); err != nil {
		return err
	}
	nowText := now.UTC().Format(rfc3339)
	for _, id := range ids {
		j, seq, err := loadJobTx(ctx, tx, id)
		if err != nil {
			return err
		}
		ev := ledger.Event{
			JobID: id, JobSeq: seq + 1, Kind: ledger.KindEpicAdoptAbsorbed,
			FromState: j.State, ToState: job.StateCancelled, LeaseEpoch: j.LeaseEpoch + 1,
			Actor: "epic-artifact-reconciler", CreatedAt: now,
			Payload: ledger.Payload{PRNumber: prNumber,
				RevokeReason: "legacy adopted review superseded by owned epic " + epicID},
		}
		if err := appendEvent(ctx, tx, ev); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `UPDATE jobs SET
			workflow_domain='epic_v2_absorbed', epic_delivery_id=?,
			state='cancelled',
			verdict=NULL, lease_epoch=lease_epoch+1, lease_id=NULL,
			bound_identity=NULL, bound_model_family=NULL, bound_lens=NULL,
			lease_hb_due=NULL, lease_deadline=NULL, phase_deadline_at=NULL,
			updated_at=? WHERE id=?`, epicID, nowText, id)
		if err != nil {
			return fmt.Errorf("absorb adopted review %s: %w", id, err)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE leases SET ended_at=?, end_reason='epic_v2_absorbed'
			WHERE job_id=? AND ended_at IS NULL`, nowText, id); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE outbox SET status='abandoned', sent_at=?
			WHERE job_id=? AND status='pending'`, nowText, id); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE timers SET fired=1 WHERE job_id=? AND fired=0`, id); err != nil {
			return err
		}
		if err := setJobSeq(ctx, tx, id, seq+1); err != nil {
			return err
		}
	}
	return nil
}

func boolInt(v bool) int {
	if v {
		return 1
	}
	return 0
}

func formatOptionalTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(rfc3339)
}
