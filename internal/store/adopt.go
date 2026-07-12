package store

import (
	"context"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"time"

	gh "github.com/samhotchkiss/flowbee/internal/github"
	"github.com/samhotchkiss/flowbee/internal/intake"
	"github.com/samhotchkiss/flowbee/internal/job"
	"github.com/samhotchkiss/flowbee/internal/ledger"
	"github.com/samhotchkiss/flowbee/internal/ulid"
)

func intFromBool(v bool) int {
	if v {
		return 1
	}
	return 0
}

func adoptedPRJobID(repo string, prNumber int) string {
	if repo == "" {
		return fmt.Sprintf("adopt-pr-%d", prNumber)
	}
	return fmt.Sprintf("adopt-pr-repo-%s-%d", base64.RawURLEncoding.EncodeToString([]byte(repo)), prNumber)
}

// AdoptSweep is the ADOPT-mode first-boot import (§12.7, I-16). It imports every
// open/merged PR on the board as a Domain-A job in state `quiescent`: reconciled
// (full Domain-B facts) but NEVER scheduled and NEVER rendered OUT. A job leaves
// quiescent ONLY on deliberate opt-in (watermark or flowbee:adopt label) — Flowbee
// never seizes work it didn't originate. Returns the ids of newly adopted jobs.
//
// watermark: PRs whose updatedAt is at/after it are opted-in on import (the clean
// "start fresh" default); the rest stay quiescent. The flowbee:adopt label opts a
// specific PR in regardless of the watermark.
func (s *Store) AdoptSweep(ctx context.Context, snap gh.BoardSnapshot, watermark time.Time, now time.Time) ([]string, error) {
	var adopted []string
	err := s.tx(ctx, func(tx *sql.Tx) error {
		for _, pr := range snap.PullRequests {
			// skip PRs already bound to a Flowbee-originated job (idempotent re-sweep).
			var existing string
			err := tx.QueryRowContext(ctx,
				`SELECT id FROM jobs WHERE pr_number = ? LIMIT 1`, pr.Number).Scan(&existing)
			if err == nil {
				continue // already known (originated or previously adopted)
			}
			if err != sql.ErrNoRows {
				return fmt.Errorf("adopt lookup pr %d: %w", pr.Number, err)
			}
			optIn := !pr.UpdatedAt.IsZero() && !pr.UpdatedAt.Before(watermark)
			if hasLabel(pr.Labels, "flowbee:adopt") {
				optIn = true
			}
			id := fmt.Sprintf("adopt-pr-%d", pr.Number)
			state := job.StateQuiescent
			optInI := 0
			if optIn {
				// an opted-in adopted job enters the normal DAG as a build job in
				// review_pending (its PR exists; awaiting review). project-OUT renders it.
				state = job.StateReviewPending
				optInI = 1
			}
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO jobs (id, kind, flow, stage, state, role, pr_number, base_sha, head_sha, head_ref,
				                  blocked_by, required_capabilities, enqueued_at,
				                  lease_epoch, attempts, max_attempts, bounces, max_bounces, job_seq,
				                  adopted, opted_in, priority)
				VALUES (?, 'build', 'build', 'review', ?, 'code_reviewer', ?, ?, ?, ?, '[]', ?, ?, 0, 0, 5, 0, 4, 1, 1, ?, 5)`,
				id, string(state), pr.Number, pr.BaseRefOid, pr.HeadRefOid, pr.HeadRefName,
				marshalStrings([]string{"role:code_reviewer"}), now.Format(rfc3339), optInI); err != nil {
				return fmt.Errorf("insert adopted job pr %d: %w", pr.Number, err)
			}
			ev := ledger.Event{
				JobID: id, JobSeq: 1, Kind: ledger.KindAdopted,
				ToState: state, Actor: "adopt", CreatedAt: now,
				Payload: ledger.Payload{PRNumber: pr.Number},
			}
			if err := appendEvent(ctx, tx, ev); err != nil {
				return err
			}
			if err := setJobSeq(ctx, tx, id, 1); err != nil {
				return err
			}
			// reconcile the full Domain-B facts for the imported PR (it IS mirrored).
			if err := upsertDomainBFactsTx(ctx, tx, id, ReconciledPR{
				Number: pr.Number, HeadSHA: pr.HeadRefOid, BaseSHA: pr.BaseRefOid,
				Merged: pr.Merged, MergeCommit: pr.MergeCommit, IsDraft: pr.IsDraft,
				CIGreen: pr.CIRollup == gh.CISuccess, UpdatedAt: pr.UpdatedAt,
			}); err != nil {
				return err
			}
			adopted = append(adopted, id)
		}
		// F7: direct-to-GitHub ISSUES are mirrored-but-quiescent by default. An issue
		// with a flowbee:adopt label opts in to a STANDALONE single-issue flow entering
		// at issue-review (spec_review over the issue body as the spec). The rest stay
		// quiescent — Flowbee never seizes an issue it didn't originate.
		for _, iss := range snap.Issues {
			id := fmt.Sprintf("adopt-issue-%d", iss.Number)
			var existing string
			err := tx.QueryRowContext(ctx,
				`SELECT id FROM jobs WHERE id = ? OR issue_number = ? LIMIT 1`, id, iss.Number).Scan(&existing)
			if err == nil {
				continue // already known (originated or previously adopted)
			}
			if err != sql.ErrNoRows {
				return fmt.Errorf("adopt lookup issue %d: %w", iss.Number, err)
			}
			optIn := hasLabel(iss.Labels, "flowbee:adopt")
			state := job.StateQuiescent
			stage, role := "review", "code_reviewer" // placeholder for a quiescent issue
			optInI := 0
			kind := ledger.KindAdopted
			if optIn {
				// opt-in: enter a single-issue flow at issue-review (spec_review). The
				// issue body becomes the spec/task the reviewer judges + amends.
				state = job.StateSpecReview
				stage, role = "review", string(job.RoleSpecReviewer)
				optInI = 1
				kind = ledger.KindIssueAdopted
			}
			t := intake.TaskFromIssueBody(iss.Body)
			var issueCol any = iss.Number
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO jobs (id, kind, flow, stage, state, role, issue_number,
				                  blocked_by, required_capabilities, enqueued_at,
				                  lease_epoch, attempts, max_attempts, bounces, max_bounces, job_seq,
				                  adopted, opted_in, priority, task_text, spec_text, acceptance_criteria)
				VALUES (?, 'spec', 'spec', ?, ?, ?, ?, '[]', ?, ?, 0, 0, 5, 0, 4, 1, 1, ?, 5, ?, ?, ?)`,
				id, stage, string(state), role, issueCol,
				marshalStrings([]string{"role:" + role}), now.Format(rfc3339), optInI,
				t.Text, t.Spec, t.AcceptanceCriteria); err != nil {
				return fmt.Errorf("insert adopted issue %d: %w", iss.Number, err)
			}
			ev := ledger.Event{
				JobID: id, JobSeq: 1, Kind: kind,
				ToState: state, Actor: "adopt", CreatedAt: now,
				Payload: ledger.Payload{
					IssueNumber: iss.Number, Stage: stage, Role: job.Role(role),
					TaskText: t.Text, SpecText: t.Spec, AcceptanceCriteria: t.AcceptanceCriteria,
				},
			}
			if err := appendEvent(ctx, tx, ev); err != nil {
				return err
			}
			if err := setJobSeq(ctx, tx, id, 1); err != nil {
				return err
			}
			adopted = append(adopted, id)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return adopted, nil
}

// AdoptPRForReview imports a SINGLE pre-existing PR (one not originated by Flowbee —
// e.g. an external agent-pool branch) directly into Flowbee's review pipeline: an
// opted-in adopted `code_reviewer` job in review_pending, with its Domain-B facts
// reconciled. Flowbee's reviewer then judges the diff and, on approval + green CI,
// self-merges — or routes to needs_human on changes_requested (there is no
// eng_worker bound to a foreign branch to bounce back to).
//
// This is the TARGETED, operator-driven counterpart to AdoptSweep's first-boot mass
// import: `flowbee adopt <pr>` calls it for one PR on demand, rather than importing
// the whole board. It is idempotent — a PR already bound to any non-cancelled job
// (Flowbee-originated OR previously adopted) is a no-op returning ("", false) — so a
// repeated adopt, or an adopt of a PR Flowbee already tracks, never creates a duplicate.
// If the already-adopted PR's authoritative base/head moved, it returns the existing
// job id with rearmed=true after superseding stale review authorization and re-arming
// code review against the refreshed diff.
func (s *Store) AdoptPRForReview(ctx context.Context, repo string, prNumber int, baseSHA, headSHA, patchDiff string, diffEmpty bool, merged, ciGreen, isDraft bool, updatedAt, now time.Time) (string, bool, error) {
	return s.AdoptPRForReviewWithHeadRef(ctx, repo, prNumber, baseSHA, headSHA, "", patchDiff, diffEmpty, merged, ciGreen, isDraft, updatedAt, now)
}

func (s *Store) AdoptPRForReviewWithHeadRef(ctx context.Context, repo string, prNumber int, baseSHA, headSHA, headRefName, patchDiff string, diffEmpty bool, merged, ciGreen, isDraft bool, updatedAt, now time.Time) (string, bool, error) {
	id := adoptedPRJobID(repo, prNumber)
	replacementID := id + "-" + ulid.New()
	adopted := ""
	rearmed := false
	err := s.tx(ctx, func(tx *sql.Tx) error {
		var existing string
		var existingAdopted int
		err := tx.QueryRowContext(ctx,
			`SELECT id, COALESCE(adopted,0)
			   FROM jobs
			  WHERE repo = ? AND pr_number = ? AND state != 'cancelled'
			  LIMIT 1`, repo, prNumber).Scan(&existing, &existingAdopted)
		if err == nil {
			if existingAdopted == 1 {
				j, seq, err := loadJobTx(ctx, tx, existing)
				if err != nil {
					return err
				}
				moved := j.BaseSHA != baseSHA || j.HeadSHA != headSHA
				if moved {
					nextSeq := seq + 1
					ev := ledger.Event{
						JobID: existing, JobSeq: nextSeq, Kind: ledger.KindAdoptRearmed,
						FromState: j.State, ToState: job.StateReviewPending, LeaseEpoch: j.LeaseEpoch + 1,
						Actor: "operator", CreatedAt: now,
						Payload: ledger.Payload{PRNumber: prNumber, BaseSHA: baseSHA, HeadSHA: headSHA},
					}
					if err := appendEvent(ctx, tx, ev); err != nil {
						return err
					}
					if _, err := tx.ExecContext(ctx, `
						UPDATE jobs
						   SET state = 'review_pending', role = 'code_reviewer', stage = 'review',
						       required_capabilities = ?,
						       base_sha = ?, head_sha = ?, head_ref = COALESCE(NULLIF(?,''), head_ref),
						       patch_diff = ?, diff_empty = ?,
						       verdict = NULL,
						       lease_epoch = lease_epoch + 1,
						       lease_id = NULL, bound_identity = NULL, bound_model_family = NULL, bound_lens = NULL,
						       lease_hb_due = NULL, lease_deadline = NULL, phase_deadline_at = NULL,
						       agent_health = '', rung1_class = '', rung2_last_verdict = 'abstain',
						       enqueued_at = ?, updated_at = datetime('now')
						 WHERE id = ?`,
						marshalStrings([]string{"role:code_reviewer"}),
						baseSHA, headSHA, headRefName, patchDiff, intFromBool(diffEmpty), now.Format(rfc3339), existing); err != nil {
						return fmt.Errorf("re-arm adopted pr %d: %w", prNumber, err)
					}
					if _, err := tx.ExecContext(ctx, `
						UPDATE outbox SET status='abandoned', sent_at=datetime('now')
						 WHERE job_id=? AND status='pending' AND head_sha<>''
						   AND (?='' OR head_sha<>?)`, existing, headSHA, headSHA); err != nil {
						return fmt.Errorf("abandon re-armed adopted outbox: %w", err)
					}
					if _, err := tx.ExecContext(ctx, `
						UPDATE leases SET ended_at = datetime('now'), end_reason = 'superseded'
						 WHERE job_id = ? AND ended_at IS NULL`, existing); err != nil {
						return fmt.Errorf("close re-armed adopted lease: %w", err)
					}
					if err := setJobSeq(ctx, tx, existing, nextSeq); err != nil {
						return err
					}
					adopted = existing
					rearmed = true
				} else {
					// Same SHA: allow targeted adopt to backfill a missing/legacy diff, but do not
					// disturb state, verdict, lease, or outbox authorization.
					if _, err := tx.ExecContext(ctx, `
						UPDATE jobs
						   SET base_sha = ?, head_sha = ?, head_ref = COALESCE(NULLIF(?,''), head_ref),
						       patch_diff = ?, diff_empty = ?,
						       updated_at = datetime('now')
						 WHERE id = ?`,
						baseSHA, headSHA, headRefName, patchDiff, intFromBool(diffEmpty), existing); err != nil {
						return fmt.Errorf("refresh adopted pr %d: %w", prNumber, err)
					}
				}
				if _, err := tx.ExecContext(ctx, `
					UPDATE jobs SET updated_at = datetime('now') WHERE id = ?`, existing); err != nil {
					return err
				}
				if err := upsertDomainBFactsTx(ctx, tx, existing, ReconciledPR{
					Number: prNumber, HeadSHA: headSHA, BaseSHA: baseSHA,
					Merged: merged, IsDraft: isDraft, CIGreen: ciGreen, UpdatedAt: updatedAt,
				}); err != nil {
					return err
				}
			}
			return nil // already known (originated or previously adopted) — idempotent no-op
		}
		if err != sql.ErrNoRows {
			return fmt.Errorf("adopt lookup pr %d: %w", prNumber, err)
		}
		// The stable first-adoption id may belong to cancelled history. Preserve that
		// terminal audit row and allocate a fresh id, matching issue re-adoption.
		var idCollision int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM jobs WHERE id = ?`, id).Scan(&idCollision); err != nil {
			return fmt.Errorf("adopt lookup cancelled pr %d: %w", prNumber, err)
		}
		if idCollision > 0 {
			id = replacementID
		}
		// repo MUST be set: project-OUT drains the outbox per repo (NextPendingOutboxForRepo
		// joins on jobs.repo), so an empty-repo adopted job in a multi-repo control plane has
		// its merge/comment actions stranded forever — reviewed and merge_started, but the PR
		// never actually merges. (AdoptSweep's first-boot import has this same latent gap; it
		// predates multi-repo. This targeted path is repo-scoped from the reconciler, so it
		// always has the right repo.)
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO jobs (id, kind, flow, stage, state, role, repo, pr_number, base_sha, head_sha, head_ref,
			                  patch_diff, diff_empty,
			                  blocked_by, required_capabilities, enqueued_at,
			                  lease_epoch, attempts, max_attempts, bounces, max_bounces, job_seq,
			                  adopted, opted_in, priority)
			VALUES (?, 'build', 'build', 'review', ?, 'code_reviewer', ?, ?, ?, ?, ?, ?, ?, '[]', ?, ?, 0, 0, 5, 0, 4, 1, 1, 1, 5)`,
			id, string(job.StateReviewPending), repo, prNumber, baseSHA, headSHA, headRefName, patchDiff, intFromBool(diffEmpty),
			marshalStrings([]string{"role:code_reviewer"}), now.Format(rfc3339)); err != nil {
			return fmt.Errorf("insert adopted job pr %d: %w", prNumber, err)
		}
		ev := ledger.Event{
			JobID: id, JobSeq: 1, Kind: ledger.KindAdopted,
			ToState: job.StateReviewPending, Actor: "operator", CreatedAt: now,
			Payload: ledger.Payload{PRNumber: prNumber},
		}
		if err := appendEvent(ctx, tx, ev); err != nil {
			return err
		}
		if err := setJobSeq(ctx, tx, id, 1); err != nil {
			return err
		}
		if err := upsertDomainBFactsTx(ctx, tx, id, ReconciledPR{
			Number: prNumber, HeadSHA: headSHA, BaseSHA: baseSHA,
			Merged: merged, IsDraft: isDraft, CIGreen: ciGreen, UpdatedAt: updatedAt,
		}); err != nil {
			return err
		}
		adopted = id
		return nil
	})
	return adopted, rearmed, err
}

// IsQuiescent reports whether a job is an adopted-but-not-opted-in job (§12.7).
// project-OUT MUST suppress every action on such a job (the §8.2.3 exception).
func (s *Store) IsQuiescent(ctx context.Context, jobID string) (bool, error) {
	var adopted, optedIn int
	err := s.DB.QueryRowContext(ctx,
		`SELECT adopted, opted_in FROM jobs WHERE id = ?`, jobID).Scan(&adopted, &optedIn)
	if err != nil {
		return false, err
	}
	return adopted == 1 && optedIn == 0, nil
}

// AdoptedPatchForRebuild returns the cumulative PR patch retained across an
// adopted code-review bounce. Base-starting worker modes apply it before asking
// the builder for the correction, so the next result remains the full PR diff.
func (s *Store) AdoptedPatchForRebuild(ctx context.Context, jobID string) (string, bool, error) {
	var patch string
	err := s.DB.QueryRowContext(ctx, `
		SELECT COALESCE(patch_diff,'') FROM jobs
		 WHERE id=? AND adopted=1`, jobID).Scan(&patch)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return patch, patch != "", nil
}

// OptIn promotes a quiescent adopted job into Flowbee's control (§12.7): the
// operator's deliberate decision, one item at a time. It leaves quiescent and
// enters the normal DAG. project-OUT now renders it.
//
// An adopted PR opts in to review_pending (its PR exists; awaiting review). An
// adopted ISSUE (F7) — no PR — opts in to a standalone single-issue flow entering
// at issue-review (spec_review over the issue body), via the quiescent ->
// spec_review edge (TriggerAdoptedForReview).
func (s *Store) OptIn(ctx context.Context, jobID string, now time.Time) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		j, seq, err := loadJobTx(ctx, tx, jobID)
		if err != nil {
			return err
		}
		if j.State != job.StateQuiescent {
			return nil // already in the DAG
		}
		nextSeq := seq + 1
		// route by what was adopted: an issue (no PR) enters issue-review; a PR
		// enters review_pending.
		if j.PRNumber == 0 && j.IssueNum != 0 {
			to := job.StateSpecReview
			ev := ledger.Event{
				JobID: jobID, JobSeq: nextSeq, Kind: ledger.KindIssueAdopted,
				FromState: job.StateQuiescent, ToState: to,
				LeaseEpoch: j.LeaseEpoch, Actor: "operator", CreatedAt: now,
				Payload: ledger.Payload{IssueNumber: j.IssueNum, Stage: "review", Role: job.RoleSpecReviewer},
			}
			if err := appendEvent(ctx, tx, ev); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `
				UPDATE jobs SET state='spec_review', stage='review', role='spec_reviewer',
				                required_capabilities=?, opted_in=1, enqueued_at=?, updated_at=datetime('now')
				 WHERE id = ?`,
				marshalStrings([]string{"role:spec_reviewer"}), now.Format(rfc3339), jobID); err != nil {
				return fmt.Errorf("opt in issue: %w", err)
			}
			return setJobSeq(ctx, tx, jobID, nextSeq)
		}
		ev := ledger.Event{
			JobID: jobID, JobSeq: nextSeq, Kind: ledger.KindStateChanged,
			FromState: job.StateQuiescent, ToState: job.StateReviewPending,
			LeaseEpoch: j.LeaseEpoch, Actor: "operator", CreatedAt: now,
		}
		if err := appendEvent(ctx, tx, ev); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE jobs SET state='review_pending', opted_in=1, updated_at=datetime('now') WHERE id = ?`,
			jobID); err != nil {
			return fmt.Errorf("opt in: %w", err)
		}
		return setJobSeq(ctx, tx, jobID, nextSeq)
	})
}

func hasLabel(labels []string, want string) bool {
	for _, l := range labels {
		if l == want {
			return true
		}
	}
	return false
}
