package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/samhotchkiss/flowbee/internal/job"
	"github.com/samhotchkiss/flowbee/internal/ledger"
)

// UmbrellaLabel is the single yellow `flowbee` umbrella label applied to EVERY
// actively-tracked issue (flow-pass §D, F7): the at-a-glance marker that Flowbee
// is steering this issue. It is rendered alongside a per-stage label (below) via
// project-OUT — Flowbee is the sole writer (R4).
const UmbrellaLabel = "flowbee"

// StageLabel maps a job's lifecycle STATE to the per-stage GitHub label rendered
// alongside the umbrella label (F7). The label namespace is flowbee:stage:* so the
// SetLabels rendering replaces the whole flowbee-owned set on each stage change
// without clobbering human labels.
func StageLabel(state job.State) string {
	return "flowbee:stage:" + string(state)
}

// TrackingLabelsForState returns the full flowbee-owned label set for an
// actively-tracked issue in a given state: the yellow umbrella label first, then
// the per-stage label. project-OUT's SetLabels replaces the flowbee:* set with
// exactly this list (a rendering of Domain-A stage, §8.2.1).
func TrackingLabelsForState(state job.State) []string {
	return []string{UmbrellaLabel, StageLabel(state)}
}

// activelyTracked reports whether a job is an actively-tracked issue that should
// carry the umbrella label: it has a GitHub issue number, is NOT adopted-quiescent
// (a human's quiescent issue is never re-rendered, §8.2.3 / I-16), and is not in a
// terminal sink (done/cancelled/superseded — those carry no live stage marker).
func activelyTracked(state job.State, issueNumber, adopted, optedIn int) bool {
	if issueNumber == 0 {
		return false
	}
	if adopted == 1 && optedIn == 0 {
		return false // quiescent: never reasserted OUT
	}
	switch state {
	case job.StateDone, job.StateCancelled, job.StateSuperseded:
		return false
	}
	return true
}

// EnqueueTrackingLabels enqueues the yellow `flowbee` umbrella label + the per-
// stage label for an actively-tracked issue (F7), rendered OUT by project-OUT
// (Flowbee is the sole GitHub writer, R4). It is idempotent per (issue, stage):
// the outbox (job_id, action, head_sha) dedupe collapses re-enqueues for the same
// stage, and the tracking_label_stage marker lets a sweep cheaply skip an issue
// already labelled for its current stage. Returns whether a fresh render was
// enqueued (false = not tracked, or already labelled for this stage). The label
// row keys head_sha on the stage string so each stage transition enqueues exactly
// one fresh render even on the same issue.
func (s *Store) EnqueueTrackingLabels(ctx context.Context, jobID string, now time.Time) (bool, error) {
	enqueued := false
	err := s.tx(ctx, func(tx *sql.Tx) error {
		var state string
		var issueNumber sql.NullInt64
		var adopted, optedIn int
		var lastStage string
		if err := tx.QueryRowContext(ctx, `
			SELECT state, issue_number, adopted, opted_in, COALESCE(tracking_label_stage,'')
			  FROM jobs WHERE id = ?`, jobID).
			Scan(&state, &issueNumber, &adopted, &optedIn, &lastStage); err != nil {
			return err
		}
		issue := 0
		if issueNumber.Valid {
			issue = int(issueNumber.Int64)
		}
		st := job.State(state)
		if !activelyTracked(st, issue, adopted, optedIn) {
			return nil
		}
		if lastStage == state {
			return nil // already labelled for this exact stage
		}
		enqueued = true
		labels := TrackingLabelsForState(st)
		if err := enqueueOutboxTx(ctx, tx, OutboxRow{
			JobID:   jobID,
			Action:  ActionSetLabels,
			HeadSHA: "stage:" + state, // key per-stage so each transition renders once
			Payload: outboxPayload(map[string]any{"number": issue, "labels": labels}),
		}); err != nil {
			return fmt.Errorf("enqueue tracking labels: %w", err)
		}
		seq := 0
		if err := tx.QueryRowContext(ctx, `SELECT job_seq FROM jobs WHERE id = ?`, jobID).Scan(&seq); err != nil {
			return err
		}
		nextSeq := seq + 1
		ev := ledger.Event{
			JobID: jobID, JobSeq: nextSeq, Kind: ledger.KindTrackingLabelled,
			FromState: st, ToState: st, Actor: "project-out", CreatedAt: now,
			Payload: ledger.Payload{IssueNumber: issue},
		}
		if err := appendEvent(ctx, tx, ev); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE jobs SET tracking_label_stage = ?, updated_at = datetime('now') WHERE id = ?`,
			state, jobID); err != nil {
			return fmt.Errorf("mark tracking stage: %w", err)
		}
		return setJobSeq(ctx, tx, jobID, nextSeq)
	})
	if err != nil {
		return false, err
	}
	return enqueued, nil
}
