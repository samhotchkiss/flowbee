package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/samhotchkiss/flowbee/internal/job"
	"github.com/samhotchkiss/flowbee/internal/ledger"
	"github.com/samhotchkiss/flowbee/internal/spec"
)

// renderDecomposition is the markdown the epic-review barrier presents: the goal plus
// each child issue's task + acceptance, so the reviewer can judge scope, coverage,
// dependency ordering, and standards over the WHOLE decomposition in one review.
func renderDecomposition(goal string, issues []EpicIssue) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Epic decomposition\n\n**Goal:** %s\n\n", strings.TrimSpace(goal))
	fmt.Fprintf(&b, "This epic decomposes the goal into %d issue(s). Review the decomposition as a whole: "+
		"is the scope right, is the goal fully covered, are the issues correctly ordered and independent, "+
		"do they meet standards?\n\n", len(issues))
	for i, iss := range issues {
		fmt.Fprintf(&b, "## Issue %d\n\n%s\n\n", i+1, strings.TrimSpace(iss.Task))
		if strings.TrimSpace(iss.Acceptance) != "" {
			fmt.Fprintf(&b, "**Acceptance:** %s\n\n", strings.TrimSpace(iss.Acceptance))
		}
	}
	return b.String()
}

// SeedEpicParams seeds an epic decomposition (F4): ONE epic-barrier spec job (the
// epic-level issue-review gate) plus N child issue jobs that sit in `backlog`
// (tracked but NOT scheduled) until the epic-level review passes. Issue-review runs
// ONCE over the whole epic — scope · coverage · dep-graph · standards — before any
// issue fans out.
type SeedEpicParams struct {
	EpicID     string      // the epic-barrier job's id (children point at it via epic_id)
	ChatRef    string      // lineage root
	AuthorLens string      // the lens that authored the decomposition (anti-affinity)
	Repo       string      // the repo the epic + its issues build/merge in (F9 scope)
	Issues     []EpicIssue // the decomposed child issues WITH content (preferred)
	IssueIDs   []string    // legacy: child ids only, no content (used iff Issues is empty)
	Priority   int
	Now        time.Time
}

// EpicIssue is one decomposed child issue carrying the content a spec_author needs to
// draft its spec once the epic barrier passes and it fans out.
type EpicIssue struct {
	ID         string
	Task       string // the imperative the issue must satisfy ($FLOWBEE_TASK)
	Acceptance string // the DONE-WHEN for the issue
}

func (p SeedEpicParams) issues() []EpicIssue {
	if len(p.Issues) > 0 {
		return p.Issues
	}
	out := make([]EpicIssue, len(p.IssueIDs))
	for i, id := range p.IssueIDs {
		out[i] = EpicIssue{ID: id}
	}
	return out
}

// SeedEpic creates the epic barrier + its child issues. The epic barrier starts in
// spec_review (the epic-level issue-review gate is armed immediately — there is no
// authoring step for the barrier itself; the decomposition is the artifact under
// review). Each child issue is created in `backlog`: visible + tracked, but never
// leased until the epic-level review releases it. This is the single barrier over
// the whole decomposition.
func (s *Store) SeedEpic(ctx context.Context, p SeedEpicParams) error {
	// the decomposition IS the artifact the barrier review judges — render it as the
	// barrier's spec so the reviewer actually SEES the issues (scope · coverage ·
	// dep-graph · standards) and bind a content hash so the sign-off has something to
	// bind to. Without this the barrier shipped an empty spec and the reviewer bounced
	// it to spec_authoring — where an epic has no author, a dead end.
	decomp := renderDecomposition(p.ChatRef, p.issues())
	decompHash := spec.ContentHash([]byte(decomp))
	return s.tx(ctx, func(tx *sql.Tx) error {
		// the epic barrier: a spec_review job carrying the decomposition. It is the
		// one issue-review the epic gets (a barrier over all the issues).
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO jobs (id, kind, flow, stage, state, role, chat_ref, author_lens, priority, repo,
			                  spec_text, spec_content_hash, spec_version,
			                  blocked_by, required_capabilities, enqueued_at,
			                  lease_epoch, attempts, max_attempts, bounces, max_bounces, job_seq,
			                  epic_id, is_epic)
			VALUES (?, 'spec', 'spec', 'epic_review', 'spec_review', 'spec_reviewer', ?, ?, ?, ?, ?, ?, 1, '[]', ?, ?, 0, 0, 5, 0, 9, 1, ?, 1)`,
			p.EpicID, p.ChatRef, p.AuthorLens, p.Priority, p.Repo,
			decomp, decompHash,
			marshalStrings([]string{"role:spec_reviewer"}), p.Now.Format(rfc3339), p.EpicID); err != nil {
			return fmt.Errorf("insert epic barrier: %w", err)
		}
		ev := ledger.Event{
			JobID: p.EpicID, JobSeq: 1, Kind: ledger.KindJobCreated,
			ToState: job.StateSpecReview, Actor: "system", CreatedAt: p.Now,
			Payload: ledger.Payload{
				Kind: job.KindSpec, Flow: "spec", Stage: "epic_review", Role: job.RoleSpecReviewer,
				Priority: p.Priority, RequiredCapabilities: []string{"role:spec_reviewer"},
				SpecText: decomp, SpecContentHash: decompHash, SpecVersion: 1,
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
		for _, iss := range p.issues() {
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO jobs (id, kind, flow, stage, state, role, chat_ref, priority, repo,
				                  task_text, acceptance_criteria,
				                  blocked_by, required_capabilities, enqueued_at,
				                  lease_epoch, attempts, max_attempts, bounces, max_bounces, job_seq,
				                  epic_id, is_epic)
				VALUES (?, 'spec', 'spec', 'epic_issue', 'backlog', 'spec_author', ?, ?, ?, ?, ?, '[]', ?, ?, 0, 0, 5, 0, 9, 1, ?, 0)`,
				iss.ID, p.ChatRef, p.Priority, p.Repo, iss.Task, iss.Acceptance,
				marshalStrings([]string{"role:spec_author"}), p.Now.Format(rfc3339), p.EpicID); err != nil {
				return fmt.Errorf("insert epic child %s: %w", iss.ID, err)
			}
			cev := ledger.Event{
				JobID: iss.ID, JobSeq: 1, Kind: ledger.KindJobCreated,
				ToState: job.StateBacklog, Actor: "system", CreatedAt: p.Now,
				Payload: ledger.Payload{
					Kind: job.KindSpec, Flow: "spec", Stage: "epic_issue", Role: job.RoleSpecAuthor,
					Priority: p.Priority, RequiredCapabilities: []string{"role:spec_author"},
					TaskText: iss.Task, AcceptanceCriteria: iss.Acceptance,
				},
			}
			if err := appendEvent(ctx, tx, cev); err != nil {
				return err
			}
			if err := setJobSeq(ctx, tx, iss.ID, 1); err != nil {
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
		r, err := epicFanOutTx(ctx, tx, epicID, now)
		released = r
		return err
	})
	if err != nil {
		return nil, err
	}
	return released, nil
}

// FanOutReviewedEpics is the production trigger the epic flow was missing: a drain that
// finds every epic whose barrier has PASSED (epic_reviewed=1) but still has children
// parked in backlog, and fans each one out. The §F4 design keeps fan-out a SEPARATE
// step from the barrier (so review and release stay distinct) — but nothing ever called
// it, so a reviewed epic's issues sat in backlog forever. A serve tick runs this; it is
// idempotent (a fully-fanned epic matches nothing). Returns the total children released.
func (s *Store) FanOutReviewedEpics(ctx context.Context, now time.Time) (int, error) {
	rows, err := s.DB.QueryContext(ctx, `
		SELECT DISTINCT e.id
		  FROM jobs e
		  JOIN jobs c ON c.epic_id = e.id AND c.is_epic = 0 AND c.state = 'backlog'
		 WHERE e.is_epic = 1 AND COALESCE(e.epic_reviewed,0) = 1`)
	if err != nil {
		return 0, err
	}
	var epicIDs []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, err
		}
		epicIDs = append(epicIDs, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	total := 0
	for _, id := range epicIDs {
		released, err := s.EpicFanOut(ctx, id, now)
		if err != nil {
			return total, fmt.Errorf("fan out reviewed epic %s: %w", id, err)
		}
		total += len(released)
	}
	return total, nil
}

// epicFanOutTx is EpicFanOut's body, callable inside an existing tx.
func epicFanOutTx(ctx context.Context, tx *sql.Tx, epicID string, now time.Time) ([]string, error) {
	var released []string
	{
		var reviewed int
		if err := tx.QueryRowContext(ctx,
			`SELECT COALESCE(epic_reviewed,0) FROM jobs WHERE id = ? AND is_epic = 1`, epicID).
			Scan(&reviewed); err != nil {
			if err == sql.ErrNoRows {
				return nil, fmt.Errorf("fan out: epic %s not found", epicID)
			}
			return nil, err
		}
		if reviewed == 0 {
			return nil, fmt.Errorf("fan out: epic %s not yet reviewed (barrier holds)", epicID)
		}
		rows, err := tx.QueryContext(ctx,
			`SELECT id, job_seq FROM jobs WHERE epic_id = ? AND is_epic = 0 AND state = 'backlog' ORDER BY id ASC`, epicID)
		if err != nil {
			return nil, err
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
				return nil, err
			}
			children = append(children, c)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
		for _, c := range children {
			nextSeq := c.seq + 1
			ev := ledger.Event{
				JobID: c.id, JobSeq: nextSeq, Kind: ledger.KindDepsCleared,
				FromState: job.StateBacklog, ToState: job.StateSpecAuthoring,
				Actor: "system", CreatedAt: now,
			}
			if err := appendEvent(ctx, tx, ev); err != nil {
				return nil, err
			}
			if _, err := tx.ExecContext(ctx, `
				UPDATE jobs
				   SET state = 'spec_authoring', stage = 'author', role = 'spec_author',
				       required_capabilities = ?, enqueued_at = ?, updated_at = datetime('now')
				 WHERE id = ? AND state = 'backlog'`,
				marshalStrings([]string{"role:spec_author"}), now.Format(rfc3339), c.id); err != nil {
				return nil, fmt.Errorf("fan out child %s: %w", c.id, err)
			}
			if err := setJobSeq(ctx, tx, c.id, nextSeq); err != nil {
				return nil, err
			}
			released = append(released, c.id)
		}
	}
	return released, nil
}
