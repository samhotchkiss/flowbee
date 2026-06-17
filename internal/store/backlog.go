package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/samhotchkiss/flowbee/internal/job"
	"github.com/samhotchkiss/flowbee/internal/ledger"
)

// SeedBacklogParams seeds a tracked-but-NOT-scheduled `backlog` job (flow-pass §D,
// F7). A backlog item is visible on the board (the Backlog lane) and carries the
// human intent, but is NEVER leased until deliberately PROMOTED. NeedsFullSpec
// marks an item that must be SPEC'd before it can build (promotion routes it into
// the spec flow); a NeedsFullSpec=false item is a ready-to-build item promotion
// drops straight into `ready`.
type SeedBacklogParams struct {
	ID                 string
	ChatRef            string
	IssueNumber        int // the GitHub issue this tracks, if any (0 = none)
	Priority           int
	NeedsFullSpec      bool
	TaskText           string
	SpecText           string
	AcceptanceCriteria string
	Now                time.Time
}

// SeedBacklog inserts a `backlog` job + its backlogged event in one transaction.
// The job holds NO active lease (backlog is not in ActiveLeaseStates) and is never
// returned by the scheduler's ready-claim (which only touches state='ready'), so
// it is structurally un-leasable until PromoteBacklog releases it.
func (s *Store) SeedBacklog(ctx context.Context, p SeedBacklogParams) (job.Job, error) {
	err := s.tx(ctx, func(tx *sql.Tx) error {
		needs := 0
		if p.NeedsFullSpec {
			needs = 1
		}
		var issue any
		if p.IssueNumber != 0 {
			issue = p.IssueNumber
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO jobs (id, kind, flow, stage, state, role, chat_ref, issue_number, priority,
			                  blocked_by, required_capabilities, enqueued_at,
			                  lease_epoch, attempts, max_attempts, bounces, max_bounces, job_seq,
			                  needs_full_spec, task_text, spec_text, acceptance_criteria)
			VALUES (?, 'spec', 'spec', 'backlog', 'backlog', 'spec_author', ?, ?, ?, '[]', ?, ?, 0, 0, 5, 0, 3, 1, ?, ?, ?, ?)`,
			p.ID, p.ChatRef, issue, p.Priority,
			marshalStrings([]string{"role:spec_author"}), p.Now.Format(rfc3339),
			needs, p.TaskText, p.SpecText, p.AcceptanceCriteria); err != nil {
			return fmt.Errorf("insert backlog job: %w", err)
		}
		ev := ledger.Event{
			JobID: p.ID, JobSeq: 1, Kind: ledger.KindBacklogged,
			ToState: job.StateBacklog, Actor: "system", CreatedAt: p.Now,
			Payload: ledger.Payload{
				Kind: job.KindSpec, Flow: "spec", Stage: "backlog", Role: job.RoleSpecAuthor,
				Priority: p.Priority, IssueNumber: p.IssueNumber,
				TaskText: p.TaskText, SpecText: p.SpecText, AcceptanceCriteria: p.AcceptanceCriteria,
			},
		}
		if err := appendEvent(ctx, tx, ev); err != nil {
			return err
		}
		return setJobSeq(ctx, tx, p.ID, 1)
	})
	if err != nil {
		return job.Job{}, err
	}
	return s.GetJob(ctx, p.ID)
}

// PromoteBacklog releases a `backlog` job into its flow (F7) — the deliberate
// "promote when ready" decision. A needs_full_spec item promotes into the spec
// flow (-> spec_authoring, an author drafts its spec); a ready-to-build item
// promotes straight to `ready` (leasable). It is the single point at which a
// backlog item becomes schedulable; before it the job was never leasable. Calling
// it on a non-backlog job is an error (the promotion edge must hold).
func (s *Store) PromoteBacklog(ctx context.Context, jobID string, now time.Time) (job.State, error) {
	var to job.State
	err := s.tx(ctx, func(tx *sql.Tx) error {
		j, seq, err := loadJobTx(ctx, tx, jobID)
		if err != nil {
			return err
		}
		if j.State != job.StateBacklog {
			return fmt.Errorf("promote: job %s not in backlog (%s)", jobID, j.State)
		}
		var needs int
		if err := tx.QueryRowContext(ctx,
			`SELECT COALESCE(needs_full_spec,0) FROM jobs WHERE id = ?`, jobID).Scan(&needs); err != nil {
			return err
		}
		nextSeq := seq + 1
		var stage, role string
		if needs != 0 {
			// needs a full spec first: enter the spec flow (an author drafts the spec).
			to = job.StateSpecAuthoring
			stage, role = "author", string(job.RoleSpecAuthor)
		} else {
			// ready to build: enter the build flow directly (leasable by an eng_worker).
			to = job.StateReady
			stage, role = "build", string(job.RoleEngWorker)
		}
		ev := ledger.Event{
			JobID: jobID, JobSeq: nextSeq, Kind: ledger.KindPromoted,
			FromState: job.StateBacklog, ToState: to,
			Actor: "operator", CreatedAt: now,
			Payload: ledger.Payload{Stage: stage, Role: job.Role(role)},
		}
		if err := appendEvent(ctx, tx, ev); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE jobs SET state = ?, stage = ?, role = ?, required_capabilities = ?,
			                enqueued_at = ?, updated_at = datetime('now')
			 WHERE id = ? AND state = 'backlog'`,
			string(to), stage, role, marshalStrings([]string{"role:" + role}),
			now.Format(rfc3339), jobID); err != nil {
			return fmt.Errorf("promote projection: %w", err)
		}
		if err := setJobSeq(ctx, tx, jobID, nextSeq); err != nil {
			return err
		}
		// a job promoted to `ready` arms the no_eligible_worker alarm like any seed.
		if to == job.StateReady {
			if err := s.armNoEligibleTimerTx(ctx, tx, jobID, j.LeaseEpoch, now); err != nil {
				return fmt.Errorf("promote arm alarm: %w", err)
			}
		}
		return nil
	})
	return to, err
}

// BacklogItem is one row of the Backlog lane (F7 board view). It surfaces the
// tracked-but-not-scheduled items with their "needs full spec" flag so the board
// can show the Backlog lane and the user-agent can decide what to promote.
type BacklogItem struct {
	JobID         string `json:"job_id"`
	State         string `json:"state"`
	IssueNumber   int    `json:"issue_number,omitempty"`
	NeedsFullSpec bool   `json:"needs_full_spec"`
	Priority      int    `json:"priority"`
	TaskText      string `json:"task_text,omitempty"`
}

// Backlog returns every job in `backlog` (tracked, not scheduled), oldest first.
// It is the read-model behind the board's Backlog lane and the user-agent loop.
func (s *Store) Backlog(ctx context.Context) ([]BacklogItem, error) {
	rows, err := s.DB.QueryContext(ctx, `
		SELECT id, state, COALESCE(issue_number,0), COALESCE(needs_full_spec,0),
		       priority, COALESCE(task_text,'')
		  FROM jobs WHERE state = 'backlog' ORDER BY priority DESC, id ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []BacklogItem{}
	for rows.Next() {
		var it BacklogItem
		var needs int
		if err := rows.Scan(&it.JobID, &it.State, &it.IssueNumber, &needs, &it.Priority, &it.TaskText); err != nil {
			return nil, err
		}
		it.NeedsFullSpec = needs != 0
		out = append(out, it)
	}
	return out, rows.Err()
}
