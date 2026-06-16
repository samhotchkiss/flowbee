package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/samhotchkiss/flowbee/internal/job"
	"github.com/samhotchkiss/flowbee/internal/ledger"
)

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
			Payload: ledger.Payload{PRNumber: prNumber},
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
		var headSHA, state string
		if err := tx.QueryRowContext(ctx,
			`SELECT pr_number, COALESCE(head_sha,''), state FROM jobs WHERE id = ?`, jobID).
			Scan(&prNumber, &headSHA, &state); err != nil {
			return err
		}
		if !prNumber.Valid || prNumber.Int64 <= 0 {
			return nil // no PR to enqueue
		}
		if job.State(state) != job.StateMerging && job.State(state) != job.StateMergeHandoff {
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
