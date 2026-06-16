package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	gh "github.com/samhotchkiss/flowbee/internal/github"
	"github.com/samhotchkiss/flowbee/internal/job"
	"github.com/samhotchkiss/flowbee/internal/ledger"
)

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
				INSERT INTO jobs (id, kind, flow, stage, state, role, pr_number, base_sha, head_sha,
				                  blocked_by, required_capabilities, enqueued_at,
				                  lease_epoch, attempts, max_attempts, bounces, max_bounces, job_seq,
				                  adopted, opted_in)
				VALUES (?, 'build', 'build', 'review', ?, 'code_reviewer', ?, ?, ?, '[]', ?, ?, 0, 0, 5, 0, 3, 1, 1, ?)`,
				id, string(state), pr.Number, pr.BaseRefOid, pr.HeadRefOid,
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
		return nil
	})
	if err != nil {
		return nil, err
	}
	return adopted, nil
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

// OptIn promotes a quiescent adopted job into Flowbee's control (§12.7): the
// operator's deliberate decision, one item at a time. It leaves quiescent and
// enters the normal DAG (review_pending). project-OUT now renders it.
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
