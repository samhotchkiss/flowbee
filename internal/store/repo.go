package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// Repo is one managed GitHub repository in the F9 multi-repo registry (build-list
// F9). One Flowbee control plane manages a SET of repos: each carries its own
// GitHub coords (Owner/Repo) and integration branch, and each gets its own
// reconcile-IN + project-OUT loop, but the scheduler and worker fleet are shared
// and repo-agnostic.
type Repo struct {
	ID            string // short stable handle, the repo-scope key on jobs
	Owner         string // GitHub owner/org
	Repo          string // GitHub repo name
	DefaultBranch string // integration branch (PR base + I-8 protection target)
	Active        bool   // false parks the repo (its loops + scheduling stop)
}

// ErrRepoNotFound is returned by GetRepo when no registry row matches.
var ErrRepoNotFound = errors.New("repo not found")

// RegisterRepo upserts a repo into the registry (idempotent on the id). Re-running
// it with the same id updates the coords/branch/active flag — so `flowbee init`
// and config reloads are replayable. DefaultBranch defaults to "main" when empty.
func (s *Store) RegisterRepo(ctx context.Context, r Repo) error {
	if r.ID == "" {
		return errors.New("repo id is required")
	}
	if r.Owner == "" || r.Repo == "" {
		return errors.New("repo owner and repo name are required")
	}
	branch := r.DefaultBranch
	if branch == "" {
		branch = "main"
	}
	active := 0
	if r.Active {
		active = 1
	}
	_, err := s.DB.ExecContext(ctx, `
		INSERT INTO repos (id, owner, repo, default_branch, active)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT (id) DO UPDATE SET
			owner = excluded.owner,
			repo = excluded.repo,
			default_branch = excluded.default_branch,
			active = excluded.active`,
		r.ID, r.Owner, r.Repo, branch, active)
	if err != nil {
		return fmt.Errorf("register repo %q: %w", r.ID, err)
	}
	return nil
}

// GetRepo returns one repo by its id. ErrRepoNotFound if absent.
func (s *Store) GetRepo(ctx context.Context, id string) (Repo, error) {
	var r Repo
	var active int
	err := s.DB.QueryRowContext(ctx,
		`SELECT id, owner, repo, default_branch, active FROM repos WHERE id = ?`, id).
		Scan(&r.ID, &r.Owner, &r.Repo, &r.DefaultBranch, &active)
	if errors.Is(err, sql.ErrNoRows) {
		return Repo{}, ErrRepoNotFound
	}
	if err != nil {
		return Repo{}, err
	}
	r.Active = active != 0
	return r, nil
}

// ListRepos returns all registered repos ordered by id (stable). When onlyActive
// is true, parked repos are omitted (the set the loops + scheduler operate over).
func (s *Store) ListRepos(ctx context.Context, onlyActive bool) ([]Repo, error) {
	q := `SELECT id, owner, repo, default_branch, active FROM repos`
	if onlyActive {
		q += ` WHERE active = 1`
	}
	q += ` ORDER BY id`
	rows, err := s.DB.QueryContext(ctx, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Repo
	for rows.Next() {
		var r Repo
		var active int
		if err := rows.Scan(&r.ID, &r.Owner, &r.Repo, &r.DefaultBranch, &active); err != nil {
			return nil, err
		}
		r.Active = active != 0
		out = append(out, r)
	}
	return out, rows.Err()
}

// SetRepoActive parks (active=false) or resumes (active=true) a repo without
// deleting its history — its reconcile/project loops stop and its jobs drop out of
// the scheduler's union while parked.
func (s *Store) SetRepoActive(ctx context.Context, id string, active bool) error {
	a := 0
	if active {
		a = 1
	}
	res, err := s.DB.ExecContext(ctx,
		`UPDATE repos SET active = ? WHERE id = ?`, a, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrRepoNotFound
	}
	return nil
}

// JobIDForPRInRepo resolves the job bound to a GitHub PR number WITHIN a repo
// (F9): PR numbers are repo-scoped, so the per-repo reconcile sweep must bind its
// swept PRs only to its own repo's jobs — #1000 in repo A never binds to #1000 in
// repo B. ok=false if no job in that repo is bound to the PR. An empty repo scopes
// to the legacy single-repo (repo=”) jobs, so the pre-F9 JobIDForPR path is the
// degenerate single-repo case of this one.
func (s *Store) JobIDForPRInRepo(ctx context.Context, repo string, prNumber int) (string, bool, error) {
	var id string
	err := s.DB.QueryRowContext(ctx,
		`SELECT id FROM jobs
		  WHERE repo = ? AND pr_number = ?
		  ORDER BY CASE
		    WHEN state IN ('ready','leased','building','review_pending','code_review',
		                   'mergeable','merging','merge_handoff','resolving_conflict') THEN 0
		    ELSE 1
		  END, updated_at DESC, id DESC
		  LIMIT 1`, repo, prNumber).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return id, true, nil
}

// ReadyCandidatesForRepo returns the ready candidates scoped to one repo (the
// per-repo board view). The scheduler itself ranks the GLOBAL union (ReadyCandidates);
// this is the repo-scoped projection for diagnostics / per-repo dispatch accounting.
func (s *Store) ReadyCandidatesForRepo(ctx context.Context, repo string) ([]string, error) {
	rows, err := s.DB.QueryContext(ctx,
		`SELECT id FROM jobs WHERE state='ready' AND repo = ? ORDER BY priority DESC, enqueued_at`, repo)
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
