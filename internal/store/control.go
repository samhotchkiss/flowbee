package store

import (
	"context"
	"database/sql"
	"errors"
)

// metaDispatchPaused is the flowbee_meta key holding the global dispatch-pause flag.
const metaDispatchPaused = "dispatch_paused"

// metaEpicReviewHandoffV2 records which session-control boundary the last
// successfully selected serve mode owns. Unlike an environment variable, this
// survives process exit and is therefore visible to offline CLI commands. That
// prevents a CLI launched without the serve process's environment from silently
// re-enabling the legacy raw-tmux actuator against a v2 database.
const metaEpicReviewHandoffV2 = "runtime_epic_review_handoff_v2"

// SetDurableEpicReviewHandoffV2 persists the selected session-control boundary.
// Only the process holding the control-plane writer lock should call this method.
func (s *Store) SetDurableEpicReviewHandoffV2(ctx context.Context, enabled bool) error {
	v := "0"
	if enabled {
		v = "1"
	}
	_, err := s.DB.ExecContext(ctx,
		`INSERT INTO flowbee_meta (key, value) VALUES (?, ?)
		 ON CONFLICT (key) DO UPDATE SET value = excluded.value`, metaEpicReviewHandoffV2, v)
	return err
}

// DurableEpicReviewHandoffV2 returns the last writer-selected session-control
// boundary. An unset key means legacy mode for pre-v2 databases. Corrupt values
// fail closed rather than guessing that raw tmux is safe.
func (s *Store) DurableEpicReviewHandoffV2(ctx context.Context) (bool, error) {
	var v string
	err := s.DB.QueryRowContext(ctx, `SELECT value FROM flowbee_meta WHERE key = ?`,
		metaEpicReviewHandoffV2).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	switch v {
	case "0":
		return false, nil
	case "1":
		return true, nil
	default:
		return true, errors.New("invalid durable epic-review-handoff v2 activation value")
	}
}

// SetDispatchPaused globally pauses (true) or resumes (false) job dispatch. While paused,
// the lease endpoint hands NO work to any worker — running jobs finish, but nothing new is
// offered — so a client (the russ worker, an operator) can tell the dispatcher "pause
// everything" and have it take effect immediately for the whole fleet. DB-backed so it
// survives a control-plane restart/redeploy: a transient in-memory flag would silently
// un-pause on the next `kill -USR1` re-exec, which is exactly when you'd be mid-incident.
func (s *Store) SetDispatchPaused(ctx context.Context, paused bool) error {
	v := "0"
	if paused {
		v = "1"
	}
	_, err := s.DB.ExecContext(ctx,
		`INSERT INTO flowbee_meta (key, value) VALUES (?, ?)
		 ON CONFLICT (key) DO UPDATE SET value = excluded.value`, metaDispatchPaused, v)
	return err
}

// DispatchPaused reports the global dispatch-pause flag (default false when unset). Read on
// every lease attempt; the flowbee_meta single-row read is a cheap indexed PK lookup.
func (s *Store) DispatchPaused(ctx context.Context) (bool, error) {
	var v string
	err := s.DB.QueryRowContext(ctx, `SELECT value FROM flowbee_meta WHERE key = ?`, metaDispatchPaused).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return v == "1", nil
}

// RecordMainCIRed persists the integration-branch CI state for a repo (the green-main
// signal the reconcile sweep computes), so /metrics + `flowbee status` can surface a RED
// MAIN — the stop-the-line alarm: fix main green before feature PRs pile up un-mergeable
// over it (and file the fix as flowbee:p1 so it jumps the queue). The reconcile sweep
// already holds CI-failing PRs over a red main; this makes the red main itself VISIBLE so a
// human prioritizes the fix (Flowbee can't identify which PR fixes main on its own).
func (s *Store) RecordMainCIRed(ctx context.Context, repo string, red bool) error {
	v := "0"
	if red {
		v = "1"
	}
	_, err := s.DB.ExecContext(ctx,
		`INSERT INTO flowbee_meta (key, value) VALUES (?, ?)
		 ON CONFLICT (key) DO UPDATE SET value = excluded.value`, "main_ci_red:"+repo, v)
	return err
}

// RepoMainCIRed reads the per-repo main-CI-red flag (false when unset/unknown).
func (s *Store) RepoMainCIRed(ctx context.Context, repo string) (bool, error) {
	var v string
	err := s.DB.QueryRowContext(ctx, `SELECT value FROM flowbee_meta WHERE key = ?`, "main_ci_red:"+repo).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return v == "1", nil
}

// ParkedRepoJobIDs returns the set of leasable job ids belonging to a PARKED repo
// (repos.active=0) — the per-repo "pause russ" enforcement applied once at the lease
// chokepoint, so none of the six candidate queries needs a repos.active join. Fast path:
// when no repo is parked the JOIN yields zero rows and the map is empty, so the lease hot
// path pays one cheap indexed lookup and skips filtering entirely. Scoped to the leasable
// states (a parked repo's done/cancelled history is irrelevant + unbounded).
func (s *Store) ParkedRepoJobIDs(ctx context.Context) (map[string]struct{}, error) {
	rows, err := s.DB.QueryContext(ctx, `
		SELECT j.id FROM jobs j JOIN repos r ON r.id = j.repo
		 WHERE r.active = 0
		   AND j.state IN ('ready','review_pending','resolving_conflict','spec_authoring','spec_review')`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]struct{}{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = struct{}{}
	}
	return out, rows.Err()
}
