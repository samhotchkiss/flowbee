package store

import (
	"context"
	"database/sql"
	"strings"
	"time"

	"github.com/samhotchkiss/flowbee/internal/intake"
	"github.com/samhotchkiss/flowbee/internal/job"
	"github.com/samhotchkiss/flowbee/internal/ulid"
)

// AdoptIssueAsBuild creates a ready build job from a label-opted GitHub issue (the
// issue body IS the work request — no spec-authoring needed; the human wrote it).
// Idempotent: if a job already tracks this (repo, issue_number) it is a no-op and
// returns "". The resulting build flows to a PR that Closes the issue on merge.
func (s *Store) AdoptIssueAsBuild(ctx context.Context, repo string, issueNumber int, title, body, baseSHA string, priority int, now time.Time) (string, error) {
	return s.adoptIssueAsBuild(ctx, "default", repo, false, issueNumber, title, body, baseSHA, priority, now)
}

// AdoptIssueAsBuildForProject is the Phase-2 admission path. projectID is an
// assertion checked against the sole active project_repos owner in the same
// transaction as dedup+seed; it is never caller-selected routing authority.
func (s *Store) AdoptIssueAsBuildForProject(ctx context.Context, projectID, repo string, issueNumber int, title, body, baseSHA string, priority int, now time.Time) (string, error) {
	return s.adoptIssueAsBuild(ctx, projectID, repo, true, issueNumber, title, body, baseSHA, priority, now)
}

func (s *Store) adoptIssueAsBuild(ctx context.Context, projectID, repo string, exactRoute bool, issueNumber int, title, body, baseSHA string, priority int, now time.Time) (string, error) {
	id := ulid.New()
	// Parse the issue body into task / spec / acceptance the SAME way the spec-flow adopt
	// path does (adopt.go) — otherwise the whole body (acceptance criteria and all) collapses
	// into TaskText, the worker gets no $FLOWBEE_ACCEPTANCE, and the reviewer gate has no
	// done-when to judge against. The issue title leads the task prose as human context.
	parsed := intake.TaskFromIssueBody(body)
	task := strings.TrimSpace(title)
	if t := strings.TrimSpace(parsed.Text); t != "" {
		if task != "" {
			task = task + "\n\n" + t
		} else {
			task = t
		}
	}
	adopted := ""
	var routeErr error
	// The dedup check AND the seed run in ONE transaction. Two intake paths call this
	// concurrently for the same labeled issue — the webhook-driven sweep and the periodic
	// floor-poll sweep — so a check-then-insert split across statements raced (both saw
	// COUNT=0, both inserted -> two builds -> two PRs for one issue). The store serializes
	// writes on a single connection (MaxOpenConns=1), so wrapping both in one tx makes the
	// loser block until the winner commits, THEN see the row and no-op. issue_number is
	// stamped in the INSERT (SeedParams.IssueNumber), so the dedup predicate is satisfied
	// the instant the winner commits — no follow-up UPDATE window.
	err := s.tx(ctx, func(tx *sql.Tx) error {
		if exactRoute {
			if err := assertLegacyRepoAdmissionProjectTx(ctx, tx, projectID, repo); err != nil {
				routeErr = err
				return upsertRepoAdmissionHoldTx(ctx, tx, repo, err, now)
			}
		}
		var existing int
		// A CANCELLED job is abandoned work — it must NOT count as "tracked", or an issue
		// whose prior build was cancelled can never be re-adopted after a re-label (a real
		// dead end). A done (merged) or in-flight job still blocks: don't rebuild merged
		// work, don't duplicate live work.
		if err := tx.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM jobs WHERE project_id = ? AND issue_number = ? AND COALESCE(repo,'') = ? AND state != 'cancelled'`,
			projectID, issueNumber, repo).Scan(&existing); err != nil {
			return err
		}
		if existing > 0 {
			return nil // already adopted (live or merged) — idempotent no-op
		}
		if err := s.seedJobTx(ctx, tx, SeedParams{
			ID: id, ProjectID: projectID, Kind: job.KindBuild, Flow: "build", Stage: "build", Role: job.RoleEngWorker,
			BaseSHA: baseSHA, Repo: repo, TaskText: task, SpecText: parsed.Spec,
			AcceptanceCriteria: parsed.AcceptanceCriteria, IssueNumber: &issueNumber,
			// 1..10, lower = more urgent; an unlabeled issue normalizes to the default 5.
			Priority: job.NormalizePriority(priority), Now: now,
		}); err != nil {
			return err
		}
		adopted = id
		return nil
	})
	if err == nil && routeErr != nil {
		err = routeErr
	}
	return adopted, err
}
