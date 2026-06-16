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

// TimerKind enumerates the hand-rolled durable timers (project override #2: a
// timers table with due_at + ONE polling goroutine, epoch-guarded — replacing
// River's cadence role). M2 uses exactly one kind.
const TimerNoEligibleWorker = "no_eligible_worker"

// ArmTimer inserts a durable timer for a job, guarded by the lease_epoch in force
// when armed. The single polling goroutine fires it only if still pending and due.
func (s *Store) ArmTimer(ctx context.Context, id, jobID, kind string, dueAt time.Time, expectedEpoch int) error {
	_, err := s.DB.ExecContext(ctx, `
		INSERT INTO timers (id, job_id, kind, due_at, expected_epoch, fired)
		VALUES (?, ?, ?, ?, ?, 0)`,
		id, jobID, kind, dueAt.Format(rfc3339), expectedEpoch)
	if err != nil {
		return fmt.Errorf("arm timer: %w", err)
	}
	return nil
}

// armNoEligibleTimerTx arms a no_eligible_worker timer within an existing tx for
// a job that just entered `ready`, if the store has a non-zero delay configured.
// The timer id is unique per (job, epoch, enqueue instant) so re-arming after a
// release/unblock cycle does not collide. The expected_epoch guard makes a stale
// timer a no-op.
func (s *Store) armNoEligibleTimerTx(ctx context.Context, tx *sql.Tx, jobID string, epoch int, now time.Time) error {
	if s.NoEligibleWorkerDelay <= 0 {
		return nil
	}
	id := fmt.Sprintf("%s-noelig-e%d-%d", jobID, epoch, now.UnixNano())
	_, err := tx.ExecContext(ctx, `
		INSERT INTO timers (id, job_id, kind, due_at, expected_epoch, fired)
		VALUES (?, ?, ?, ?, ?, 0)`,
		id, jobID, TimerNoEligibleWorker,
		now.Add(s.NoEligibleWorkerDelay).Format(rfc3339), epoch)
	return err
}

// DueTimer is one pending, past-due timer the poller must evaluate.
type DueTimer struct {
	ID            string
	JobID         string
	Kind          string
	ExpectedEpoch int
}

// DueTimers returns pending timers whose due_at <= now, oldest first.
func (s *Store) DueTimers(ctx context.Context, now time.Time) ([]DueTimer, error) {
	rows, err := s.DB.QueryContext(ctx, `
		SELECT id, job_id, kind, expected_epoch FROM timers
		 WHERE fired = 0 AND due_at <= ?
		 ORDER BY due_at ASC`, now.Format(rfc3339))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []DueTimer
	for rows.Next() {
		var t DueTimer
		if err := rows.Scan(&t.ID, &t.JobID, &t.Kind, &t.ExpectedEpoch); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// markTimerFired flips a timer to fired (within tx).
func markTimerFired(ctx context.Context, tx *sql.Tx, id string) error {
	_, err := tx.ExecContext(ctx, `UPDATE timers SET fired = 1 WHERE id = ?`, id)
	return err
}

// FireNoEligibleWorker evaluates one due no_eligible_worker timer. It is a no-op
// (returns fired=false) when the timer is stale (lease_epoch advanced => the job
// was leased/changed since arming) or the job is no longer `ready`. Otherwise it
// records the alarm (job_alarms row + a no_eligible_worker ledger event) and
// marks the timer fired, all in one serialized transaction. The epoch guard is
// what makes a stale timer harmless (project override #2).
func (s *Store) FireNoEligibleWorker(ctx context.Context, t DueTimer, now time.Time) (bool, error) {
	fired := false
	err := s.tx(ctx, func(tx *sql.Tx) error {
		var state string
		var epoch, seq int
		err := tx.QueryRowContext(ctx,
			`SELECT state, lease_epoch, job_seq FROM jobs WHERE id = ?`, t.JobID).Scan(&state, &epoch, &seq)
		if errors.Is(err, sql.ErrNoRows) {
			return markTimerFired(ctx, tx, t.ID) // job gone: cancel timer
		}
		if err != nil {
			return fmt.Errorf("load job for alarm: %w", err)
		}
		// epoch guard: if the epoch moved, the job was claimed (or otherwise
		// progressed) since the timer was armed — the timer is stale, no-op.
		if epoch != t.ExpectedEpoch || job.State(state) != job.StateReady {
			return markTimerFired(ctx, tx, t.ID)
		}
		// still ready under the same epoch: the alarm fires (I-6).
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO job_alarms (job_id, kind, fired_at, detail)
			VALUES (?, ?, ?, ?)
			ON CONFLICT (job_id, kind) DO UPDATE SET fired_at = excluded.fired_at`,
			t.JobID, t.Kind, now.Format(rfc3339), "no compliant worker leased the job before the alarm window"); err != nil {
			return fmt.Errorf("record alarm: %w", err)
		}
		nextSeq := seq + 1
		ev := ledger.Event{
			JobID: t.JobID, JobSeq: nextSeq, Kind: ledger.KindNoEligibleWorker,
			FromState: job.StateReady, ToState: job.StateReady, LeaseEpoch: epoch,
			Actor: "system", CreatedAt: now,
		}
		if err := appendEvent(ctx, tx, ev); err != nil {
			return err
		}
		if err := setJobSeq(ctx, tx, t.JobID, nextSeq); err != nil {
			return err
		}
		if err := markTimerFired(ctx, tx, t.ID); err != nil {
			return err
		}
		fired = true
		return nil
	})
	if err != nil {
		return false, err
	}
	return fired, nil
}

// AlarmFired reports whether a (job, kind) alarm has been recorded.
func (s *Store) AlarmFired(ctx context.Context, jobID, kind string) (bool, error) {
	var n int
	err := s.DB.QueryRowContext(ctx,
		`SELECT EXISTS(SELECT 1 FROM job_alarms WHERE job_id=? AND kind=?)`, jobID, kind).Scan(&n)
	if err != nil {
		return false, err
	}
	return n == 1, nil
}

// CancelJobTimers marks all pending timers for a job fired (called when a job
// leaves `ready`, so an armed alarm doesn't fire spuriously — though the epoch
// guard already makes it a no-op).
func (s *Store) CancelJobTimers(ctx context.Context, jobID string) error {
	_, err := s.DB.ExecContext(ctx,
		`UPDATE timers SET fired = 1 WHERE job_id = ? AND fired = 0`, jobID)
	return err
}
