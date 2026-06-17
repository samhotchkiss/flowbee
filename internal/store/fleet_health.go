package store

import (
	"context"
	"time"
)

// FleetHealth is the operator-facing "is anyone home?" snapshot: how many workers are
// live vs stale, and how many jobs are waiting for one. A positive WaitingJobs with
// zero LiveWorkers is the silent-stall signature — a `ready` job that nobody can pick
// up because the fleet is down/disconnected (it otherwise sits forever with no alarm).
type FleetHealth struct {
	LiveWorkers  int
	StaleWorkers int
	WaitingJobs  int
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
		if w.StaleHB {
			h.StaleWorkers++
		} else {
			h.LiveWorkers++
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
