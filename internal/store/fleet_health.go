package store

import (
	"context"
	"database/sql"
	"strings"
	"time"
)

// WorkerIDForIdentity returns the worker_id already registered under this identity,
// or "" if none. The workers table is UNIQUE(identity) but the registration upsert is
// keyed ON CONFLICT(worker_id); a re-registering worker that sends an empty worker_id
// (so the server mints a fresh one) would therefore collide on the identity constraint
// and fail — freezing its stored capabilities at the first registration. Resolving the
// existing worker_id first makes the upsert UPDATE the right row, so a worker that
// changed its model_family/role actually refreshes.
func (s *Store) WorkerIDForIdentity(ctx context.Context, identity string) (string, error) {
	var wid string
	err := s.DB.QueryRowContext(ctx,
		`SELECT worker_id FROM workers WHERE identity = ?`, identity).Scan(&wid)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return wid, err
}

// RecordWorkerSeen bumps a worker's last_seen — proof of liveness from any worker
// call, notably the lease long-poll. An idle worker polling for work IS alive even
// with no active lease to heartbeat; without this it shows stale on the roster and
// falsely trips the fleet-health watchdog (an idle fleet read as a down fleet).
func (s *Store) RecordWorkerSeen(ctx context.Context, identity string, now time.Time) error {
	_, err := s.DB.ExecContext(ctx,
		`UPDATE workers SET last_seen_at = ? WHERE identity = ?`, now.UTC().Format(rfc3339), identity)
	return err
}

// FleetHealth is the operator-facing "is anyone home?" snapshot: how many workers are
// live vs stale, and how many jobs are waiting for one. A positive WaitingJobs with
// zero LiveWorkers is the silent-stall signature — a `ready` job that nobody can pick
// up because the fleet is down/disconnected (it otherwise sits forever with no alarm).
type FleetHealth struct {
	LiveWorkers  int
	StaleWorkers int
	WaitingJobs  int
	// ByModel counts LIVE workers per actual backend/model (the `model:<x>` capability the
	// worker registers — "codex", "sonnet", …), so the operator can see which model the
	// fleet is running at a glance. Empty when no worker advertises one (older workers).
	ByModel map[string]int
	// StaleByModel counts STALE workers per advertised backend/model. The human status line
	// intentionally keeps showing live models only; machine status uses this for totals.
	StaleByModel map[string]int
}

// Stranded reports the silent-stall condition: work to do, but no live worker for it.
func (h FleetHealth) Stranded() bool { return h.WaitingJobs > 0 && h.LiveWorkers == 0 }

// FleetHealth computes the snapshot. A worker is live if its last heartbeat is within
// staleAfter. Waiting jobs are those in a state that needs a worker to claim them
// (ready / review / spec / conflict-resolve), across all repos.
func (s *Store) FleetHealth(ctx context.Context, now time.Time, staleAfter time.Duration) (FleetHealth, error) {
	var h FleetHealth
	roster, err := s.Roster(ctx, now, staleAfter)
	if err != nil {
		return h, err
	}
	for _, w := range roster {
		model := ""
		for _, cap := range w.Attested {
			if m, ok := strings.CutPrefix(cap, "model:"); ok && m != "" {
				model = m
				break
			}
		}
		if w.StaleHB {
			h.StaleWorkers++
			if model != "" {
				if h.StaleByModel == nil {
					h.StaleByModel = map[string]int{}
				}
				h.StaleByModel[model]++
			}
			continue
		}
		h.LiveWorkers++
		if model != "" {
			if h.ByModel == nil {
				h.ByModel = map[string]int{}
			}
			h.ByModel[model]++
		}
	}
	if err := s.DB.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM jobs
		 WHERE state IN ('ready','review_pending','spec_authoring','spec_review','resolving_conflict')`).
		Scan(&h.WaitingJobs); err != nil {
		return h, err
	}
	return h, nil
}
