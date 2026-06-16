package store

import (
	"context"
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
	var existing int
	if err := s.DB.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM jobs WHERE issue_number = ? AND COALESCE(repo,'') = ?`,
		issueNumber, repo).Scan(&existing); err != nil {
		return "", err
	}
	if existing > 0 {
		return "", nil // already adopted
	}
	id := ulid.New()
	task := strings.TrimSpace(title)
	if b := strings.TrimSpace(body); b != "" {
		task = task + "\n\n" + b
	}
	if _, err := s.SeedJob(ctx, SeedParams{
		ID: id, Kind: job.KindBuild, Flow: "build", Stage: "build", Role: job.RoleEngWorker,
		BaseSHA: baseSHA, Repo: repo, TaskText: task, Now: now,
	}); err != nil {
		return "", err
	}
	// stamp the originating issue so the PR "Closes #N" and reconcile binds it.
	if _, err := s.DB.ExecContext(ctx,
		`UPDATE jobs SET issue_number = ? WHERE id = ?`, issueNumber, id); err != nil {
		return "", err
	}
	return id, nil
}
