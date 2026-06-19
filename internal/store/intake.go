package store

import (
	"context"
	"database/sql"
	"strings"
	"time"

	"github.com/samhotchkiss/flowbee/internal/job"
	"github.com/samhotchkiss/flowbee/internal/ulid"
)

// AdoptIssueAsBuild creates a ready build job from a label-opted GitHub issue (the
// issue body IS the work request — no spec-authoring needed; the human wrote it).
// Idempotent: if a job already tracks this (repo, issue_number) it is a no-op and
// returns "". The resulting build flows to a PR that Closes the issue on merge.
func (s *Store) AdoptIssueAsBuild(ctx context.Context, repo string, issueNumber int, title, body, baseSHA string, now time.Time) (string, error) {
	id := ulid.New()
	task := strings.TrimSpace(title)
	if b := strings.TrimSpace(body); b != "" {
		task = task + "\n\n" + b
	}
	adopted := ""
	// The dedup check AND the seed run in ONE transaction. Two intake paths call this
	// concurrently for the same labeled issue — the webhook-driven sweep and the periodic
	// floor-poll sweep — so a check-then-insert split across statements raced (both saw
	// COUNT=0, both inserted -> two builds -> two PRs for one issue). The store serializes
	// writes on a single connection (MaxOpenConns=1), so wrapping both in one tx makes the
	// loser block until the winner commits, THEN see the row and no-op. issue_number is
	// stamped in the INSERT (SeedParams.IssueNumber), so the dedup predicate is satisfied
	// the instant the winner commits — no follow-up UPDATE window.
	err := s.tx(ctx, func(tx *sql.Tx) error {
		var existing int
		// A CANCELLED job is abandoned work — it must NOT count as "tracked", or an issue
		// whose prior build was cancelled can never be re-adopted after a re-label (a real
		// dead end). A done (merged) or in-flight job still blocks: don't rebuild merged
		// work, don't duplicate live work.
		if err := tx.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM jobs WHERE issue_number = ? AND COALESCE(repo,'') = ? AND state != 'cancelled'`,
			issueNumber, repo).Scan(&existing); err != nil {
			return err
		}
		if existing > 0 {
			return nil // already adopted (live or merged) — idempotent no-op
		}
		if err := s.seedJobTx(ctx, tx, SeedParams{
			ID: id, Kind: job.KindBuild, Flow: "build", Stage: "build", Role: job.RoleEngWorker,
			BaseSHA: baseSHA, Repo: repo, TaskText: task, IssueNumber: &issueNumber, Now: now,
		}); err != nil {
			return err
		}
		adopted = id
		return nil
	})
	return adopted, err
}
