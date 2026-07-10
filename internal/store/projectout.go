package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/samhotchkiss/flowbee/internal/job"
	"github.com/samhotchkiss/flowbee/internal/ledger"
)

// PRBranch is the deterministic GitHub branch name the control plane publishes a
// job's build commit to (and opens the PR from). Computed the same way on both the
// push side (result handler) and the PR-open side (project-OUT) so they agree.
func PRBranch(jobID string) string { return "flowbee/" + jobID }

// IssueBranch is the per-issue branch the whole pipeline commits to (build-list:
// "each issue is a branch; each node commits to it so the history shows how we got
// here"). It is deterministic and stable for an issue's whole life: `flowbee/issue-N`
// when the job is bound to a materialized/adopted issue, falling back to the per-job
// branch only when no issue is bound yet (defensive — a normal build always has one).
func IssueBranch(issueNum int, jobID string) string {
	if issueNum > 0 {
		return fmt.Sprintf("flowbee/issue-%d", issueNum)
	}
	return PRBranch(jobID)
}

// ResolveIssueNum returns the GitHub issue a job belongs to: an adopted issue is
// stamped on the build job itself; a spec-flow build descends from the spec job that
// carries the materialized issue number (flow_id). 0 when none is bound yet. This is
// the single resolution both the result handler (branch push) and project-OUT
// (PR-open / issue-comment) use, so a job's branch + comments agree on one issue.
func (s *Store) ResolveIssueNum(ctx context.Context, jobID string) int {
	j, err := s.GetJob(ctx, jobID)
	if err != nil {
		return 0
	}
	if j.IssueNum > 0 {
		return j.IssueNum
	}
	if j.FlowID != "" && j.FlowID != j.ID {
		if spec, err := s.GetJob(ctx, j.FlowID); err == nil {
			return spec.IssueNum
		}
	}
	return 0
}

// EnqueuePROpen enqueues the canonical PR-open trigger (§7.3, §8.2.1): after an
// eng_worker's build result lands review_pending and Flowbee has validated +
// promoted the epoch ref, Flowbee opens the PR. The worker NEVER supplies a PR
// field — Domain B owns PR existence (§3.4). headSHA keys the outbox row so a
// re-enqueue for the same SHA collapses to one PR-open. Returns whether a row was
// newly enqueued (the job had a promoted head and no PR yet).
func (s *Store) EnqueuePROpen(ctx context.Context, jobID, headSHA, baseRef string) (bool, error) {
	enqueued := false
	err := s.tx(ctx, func(tx *sql.Tx) error {
		var prNumber sql.NullInt64
		var headRef sql.NullString
		if err := tx.QueryRowContext(ctx,
			`SELECT pr_number, head_ref FROM jobs WHERE id = ?`, jobID).Scan(&prNumber, &headRef); err != nil {
			return err
		}
		// already has a PR: nothing to open (Domain B owns existence thereafter).
		if prNumber.Valid && prNumber.Int64 > 0 {
			return nil
		}
		if err := enqueueOutboxTx(ctx, tx, OutboxRow{
			JobID: jobID, Action: ActionOpenPR, HeadSHA: headSHA,
			Payload: outboxPayload(map[string]any{
				"head_ref": headRef.String, "base_ref": baseRef, "draft": true,
			}),
		}); err != nil {
			return err
		}
		enqueued = true
		return nil
	})
	return enqueued, err
}

// EnqueueIssueComment enqueues an issues.comment action (build-list §F): the
// reviewer's verdict + findings, rendered to markdown, posted into the originating
// GitHub issue so the issue is the durable human-readable record of the review. The
// dedupeKey becomes the outbox head_sha so the (job, action, head_sha) idempotency
// key collapses a retried submission to one comment, while a NEW review (a fresh
// epoch -> a new key) posts again. The control plane is the sole GitHub writer (R4);
// the body is pre-rendered by the caller from the reviewer's own findings.
func (s *Store) EnqueueIssueComment(ctx context.Context, jobID, body, dedupeKey string) (bool, error) {
	enqueued := false
	err := s.tx(ctx, func(tx *sql.Tx) error {
		if err := enqueueOutboxTx(ctx, tx, OutboxRow{
			JobID: jobID, Action: ActionComment, HeadSHA: dedupeKey,
			Payload: outboxPayload(map[string]any{"body": body}),
		}); err != nil {
			return err
		}
		enqueued = true
		return nil
	})
	return enqueued, err
}

// StampPRNumber records the GitHub PR number a pulls.create drain returned (§7.3):
// Flowbee opened the PR and stamps the number. pr_number is GitHub-owned; written
// ONLY by this PR-open path / reconcile binding — never by a worker. It also seeds
// the domain_b_facts row so reconcile-IN can later flip the job to done on merge.
func (s *Store) StampPRNumber(ctx context.Context, jobID string, prNumber int, headSHA, baseSHA string, now time.Time) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		j, seq, err := loadJobTx(ctx, tx, jobID)
		if err != nil {
			return err
		}
		nextSeq := seq + 1
		ev := ledger.Event{
			JobID: jobID, JobSeq: nextSeq, Kind: ledger.KindPROpened,
			FromState: j.State, ToState: j.State, LeaseEpoch: j.LeaseEpoch,
			Actor: "project-out", CreatedAt: now,
			Payload: ledger.Payload{PRNumber: prNumber, HeadSHA: headSHA},
		}
		if err := appendEvent(ctx, tx, ev); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE jobs SET pr_number = ?, head_sha = COALESCE(NULLIF(?,''), head_sha), updated_at = datetime('now') WHERE id = ?`,
			prNumber, headSHA, jobID); err != nil {
			return fmt.Errorf("stamp pr number: %w", err)
		}
		// seed the Domain-B facts row so reconcile-IN binds this PR to the job.
		if err := upsertDomainBFactsTx(ctx, tx, jobID, ReconciledPR{
			Number: prNumber, HeadSHA: headSHA, BaseSHA: baseSHA,
		}); err != nil {
			return err
		}
		return setJobSeq(ctx, tx, jobID, nextSeq)
	})
}

// EnqueueMergeForJob enqueues the mergeQueue.enqueue action (§8.5) for a job that
// has cleared the gate (mergeable/merge_handoff) and has a stamped PR. Both merge
// arms (§5.4) physically merge by Flowbee enqueuing to GitHub's native queue;
// workers never call GitHub. Keyed on the reviewed head_sha (batch-size-1, §8.5.2).
func (s *Store) EnqueueMergeForJob(ctx context.Context, jobID string, now time.Time) (bool, error) {
	enqueued := false
	err := s.tx(ctx, func(tx *sql.Tx) error {
		var prNumber sql.NullInt64
		var headSHA, baseSHA, state, verdictJSON string
		if err := tx.QueryRowContext(ctx,
			`SELECT pr_number, COALESCE(head_sha,''), COALESCE(base_sha,''), state, COALESCE(verdict,'')
			   FROM jobs WHERE id = ?`, jobID).
			Scan(&prNumber, &headSHA, &baseSHA, &state, &verdictJSON); err != nil {
			return err
		}
		if !prNumber.Valid || prNumber.Int64 <= 0 {
			return nil // no PR to enqueue
		}
		if job.State(state) != job.StateMerging && job.State(state) != job.StateMergeHandoff {
			return nil
		}
		var verdict job.Verdict
		if verdictJSON == "" || json.Unmarshal([]byte(verdictJSON), &verdict) != nil ||
			!verdict.Verify(headSHA, baseSHA) {
			return nil
		}
		if err := enqueueOutboxTx(ctx, tx, OutboxRow{
			JobID: jobID, Action: ActionEnqueueMerge, HeadSHA: headSHA,
			Payload: outboxPayload(map[string]any{"pr_number": int(prNumber.Int64)}),
		}); err != nil {
			return err
		}
		enqueued = true
		return nil
	})
	return enqueued, err
}

// InvalidateStaleMergeAuthorization fails closed after GitHub rejects an
// expected-head merge or project-OUT detects that its outbox row no longer
// matches the persisted verdict. The verdict is invalidated, the build is
// re-armed, and every pending SHA-bound rendering is retained as abandoned
// history. For adopted PRs, keep the cumulative patch so base-starting builders
// can apply it before making the requested correction.
func (s *Store) InvalidateStaleMergeAuthorization(ctx context.Context, jobID string, now time.Time) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		j, seq, err := loadJobTx(ctx, tx, jobID)
		if err != nil {
			return err
		}
		if j.State != job.StateMerging && j.State != job.StateMergeHandoff {
			_, err := tx.ExecContext(ctx, `
				UPDATE outbox SET status='abandoned', sent_at=datetime('now')
				 WHERE job_id=? AND action=? AND status='pending'`, jobID, ActionEnqueueMerge)
			return err
		}
		var adopted int
		var cumulativePatch, declared string
		if err := tx.QueryRowContext(ctx,
			`SELECT COALESCE(adopted,0), COALESCE(patch_diff,''), COALESCE(declared_blast_radius,'')
			   FROM jobs WHERE id=?`, jobID).Scan(&adopted, &cumulativePatch, &declared); err != nil {
			return err
		}
		if err := supersedeTx(ctx, tx, &j, seq, ReconciledPR{BaseSHA: j.BaseSHA}, "project-out", now); err != nil {
			return err
		}
		if adopted == 1 && cumulativePatch != "" {
			if _, err := tx.ExecContext(ctx,
				`UPDATE jobs SET patch_diff=?, declared_blast_radius=? WHERE id=?`,
				cumulativePatch, declared, jobID); err != nil {
				return err
			}
		}
		return nil
	})
}

// RoutePostApprovalCIFailure handles a required CI check that turns red after a
// verdict was minted but before the merge is sent. The stale approval is no longer
// sufficient, so re-arm the job through the normal builder repair path and carry
// the failed check names into the next lease brief.
func (s *Store) RoutePostApprovalCIFailure(ctx context.Context, jobID string, failingChecks []string, now time.Time) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		j, seq, err := loadJobTx(ctx, tx, jobID)
		if err != nil {
			return err
		}
		if j.State != job.StateMerging && j.State != job.StateMergeHandoff && j.State != job.StateMergeable {
			return nil
		}
		if err := supersedeTx(ctx, tx, &j, seq, ReconciledPR{BaseSHA: j.BaseSHA, HeadSHA: j.HeadSHA}, "project-out", now); err != nil {
			return err
		}
		if msg := strings.Join(failingChecks, "\n"); msg != "" {
			if _, err := tx.ExecContext(ctx,
				`UPDATE jobs SET last_ci_failures=? WHERE id=?`, msg, jobID); err != nil {
				return err
			}
		}
		_, err = tx.ExecContext(ctx, `
			UPDATE outbox SET status='abandoned', sent_at=datetime('now')
			 WHERE job_id=? AND action=? AND status='pending'`, jobID, ActionEnqueueMerge)
		return err
	})
}

// JobPR returns the stamped PR number for a job (0 if none).
func (s *Store) JobPR(ctx context.Context, jobID string) (int, error) {
	var pr sql.NullInt64
	err := s.DB.QueryRowContext(ctx, `SELECT pr_number FROM jobs WHERE id = ?`, jobID).Scan(&pr)
	if err != nil {
		return 0, err
	}
	if pr.Valid {
		return int(pr.Int64), nil
	}
	return 0, nil
}
