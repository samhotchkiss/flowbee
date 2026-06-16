package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/samhotchkiss/flowbee/internal/job"
	"github.com/samhotchkiss/flowbee/internal/ledger"
)

// SeedEpicParams seeds an epic decomposition (F4): ONE epic-barrier spec job (the
// epic-level issue-review gate) plus N child issue jobs that sit in `backlog`
// (tracked but NOT scheduled) until the epic-level review passes. Issue-review runs
// ONCE over the whole epic — scope · coverage · dep-graph · standards — before any
// issue fans out.
type SeedEpicParams struct {
	EpicID     string   // the epic-barrier job's id (children point at it via epic_id)
	ChatRef    string   // lineage root
	AuthorLens string   // the lens that authored the decomposition (anti-affinity)
	IssueIDs   []string // the child issue job ids (each created in backlog)
	Priority   int
	Now        time.Time
}

// SeedEpic creates the epic barrier + its child issues. The epic barrier starts in
// spec_review (the epic-level issue-review gate is armed immediately — there is no
// authoring step for the barrier itself; the decomposition is the artifact under
// review). Each child issue is created in `backlog`: visible + tracked, but never
// leased until the epic-level review releases it. This is the single barrier over
// the whole decomposition.
func (s *Store) SeedEpic(ctx context.Context, p SeedEpicParams) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		// the epic barrier: a spec_review job carrying the decomposition. It is the
		// one issue-review the epic gets (a barrier over all the issues).
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO jobs (id, kind, flow, stage, state, role, chat_ref, author_lens, priority,
			                  blocked_by, required_capabilities, enqueued_at,
			                  lease_epoch, attempts, max_attempts, bounces, max_bounces, job_seq,
			                  epic_id, is_epic)
			VALUES (?, 'spec', 'spec', 'epic_review', 'spec_review', 'spec_reviewer', ?, ?, ?, '[]', ?, ?, 0, 0, 5, 0, 3, 1, ?, 1)`,
			p.EpicID, p.ChatRef, p.AuthorLens, p.Priority,
			marshalStrings([]string{"role:spec_reviewer"}), p.Now.Format(rfc3339), p.EpicID); err != nil {
			return fmt.Errorf("insert epic barrier: %w", err)
		}
		ev := ledger.Event{
			JobID: p.EpicID, JobSeq: 1, Kind: ledger.KindJobCreated,
			ToState: job.StateSpecReview, Actor: "system", CreatedAt: p.Now,
			Payload: ledger.Payload{
				Kind: job.KindSpec, Flow: "spec", Stage: "epic_review", Role: job.RoleSpecReviewer,
				Priority: p.Priority, RequiredCapabilities: []string{"role:spec_reviewer"},
			},
		}
		if err := appendEvent(ctx, tx, ev); err != nil {
			return err
		}
		if err := setJobSeq(ctx, tx, p.EpicID, 1); err != nil {
			return err
		}

		// each child issue: created in `backlog` (tracked, NOT scheduled), pointing at
		// the epic. It cannot be leased until EpicFanOut releases it.
		for _, id := range p.IssueIDs {
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO jobs (id, kind, flow, stage, state, role, chat_ref, priority,
				                  blocked_by, required_capabilities, enqueued_at,
				                  lease_epoch, attempts, max_attempts, bounces, max_bounces, job_seq,
				                  epic_id, is_epic)
				VALUES (?, 'spec', 'spec', 'epic_issue', 'backlog', 'spec_author', ?, ?, '[]', ?, ?, 0, 0, 5, 0, 3, 1, ?, 0)`,
				id, p.ChatRef, p.Priority,
				marshalStrings([]string{"role:spec_author"}), p.Now.Format(rfc3339), p.EpicID); err != nil {
				return fmt.Errorf("insert epic child %s: %w", id, err)
			}
			cev := ledger.Event{
				JobID: id, JobSeq: 1, Kind: ledger.KindJobCreated,
				ToState: job.StateBacklog, Actor: "system", CreatedAt: p.Now,
				Payload: ledger.Payload{
					Kind: job.KindSpec, Flow: "spec", Stage: "epic_issue", Role: job.RoleSpecAuthor,
					Priority: p.Priority, RequiredCapabilities: []string{"role:spec_author"},
				},
			}
			if err := appendEvent(ctx, tx, cev); err != nil {
				return err
			}
			if err := setJobSeq(ctx, tx, id, 1); err != nil {
				return err
			}
		}
		return nil
	})
}

// EpicChildren returns the child issue ids of an epic (F4), oldest first. The
// barrier reviews them together; on pass they fan out.
func (s *Store) EpicChildren(ctx context.Context, epicID string) ([]string, error) {
	rows, err := s.DB.QueryContext(ctx,
		`SELECT id FROM jobs WHERE epic_id = ? AND is_epic = 0 ORDER BY id ASC`, epicID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// EpicReviewed reports whether an epic's epic-level issue-review barrier has passed.
func (s *Store) EpicReviewed(ctx context.Context, epicID string) (bool, error) {
	var n int
	err := s.DB.QueryRowContext(ctx,
		`SELECT COALESCE(epic_reviewed,0) FROM jobs WHERE id = ? AND is_epic = 1`, epicID).Scan(&n)
	if err != nil {
		return false, err
	}
	return n != 0, nil
}

// EpicFanOut releases an epic's child issues from `backlog` into the spec flow AFTER
// the epic-level issue-review barrier has passed (F4). It is the single point at
// which the issues fan out — never before the one barrier over the whole epic. It
// is idempotent: a child already released (not in backlog) is left untouched, and a
// re-run after the epic is reviewed is a no-op for already-fanned children.
//
// It REQUIRES the epic barrier to have passed (epic_reviewed = 1); calling it on an
// un-reviewed epic returns an error (the barrier must hold). Each released child
// moves backlog -> spec_authoring (an author drafts the issue's spec), the canonical
// entry into the per-issue spec flow.
func (s *Store) EpicFanOut(ctx context.Context, epicID string, now time.Time) ([]string, error) {
	var released []string
	err := s.tx(ctx, func(tx *sql.Tx) error {
		var reviewed int
		if err := tx.QueryRowContext(ctx,
			`SELECT COALESCE(epic_reviewed,0) FROM jobs WHERE id = ? AND is_epic = 1`, epicID).
			Scan(&reviewed); err != nil {
			if err == sql.ErrNoRows {
				return fmt.Errorf("fan out: epic %s not found", epicID)
			}
			return err
		}
		if reviewed == 0 {
			return fmt.Errorf("fan out: epic %s not yet reviewed (barrier holds)", epicID)
		}
		rows, err := tx.QueryContext(ctx,
			`SELECT id, job_seq FROM jobs WHERE epic_id = ? AND is_epic = 0 AND state = 'backlog' ORDER BY id ASC`, epicID)
		if err != nil {
			return err
		}
		type child struct {
			id  string
			seq int
		}
		var children []child
		for rows.Next() {
			var c child
			if err := rows.Scan(&c.id, &c.seq); err != nil {
				rows.Close()
				return err
			}
			children = append(children, c)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		for _, c := range children {
			nextSeq := c.seq + 1
			ev := ledger.Event{
				JobID: c.id, JobSeq: nextSeq, Kind: ledger.KindDepsCleared,
				FromState: job.StateBacklog, ToState: job.StateSpecAuthoring,
				Actor: "system", CreatedAt: now,
			}
			if err := appendEvent(ctx, tx, ev); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `
				UPDATE jobs
				   SET state = 'spec_authoring', stage = 'author', role = 'spec_author',
				       required_capabilities = ?, enqueued_at = ?, updated_at = datetime('now')
				 WHERE id = ? AND state = 'backlog'`,
				marshalStrings([]string{"role:spec_author"}), now.Format(rfc3339), c.id); err != nil {
				return fmt.Errorf("fan out child %s: %w", c.id, err)
			}
			if err := setJobSeq(ctx, tx, c.id, nextSeq); err != nil {
				return err
			}
			released = append(released, c.id)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return released, nil
}
