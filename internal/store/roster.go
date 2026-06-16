package store

import (
	"context"
	"time"
)

// RosterWorker is one row of the worker roster (DESIGN §12.6.2): who's
// connected, on what, where, with the active lease and last-heartbeat age so a
// partition (stale-hb) is visible.
type RosterWorker struct {
	WorkerID    string   `json:"worker_id"`
	Identity    string   `json:"identity"`
	Host        string   `json:"host"`
	Arch        string   `json:"arch"`
	OS          string   `json:"os"`
	Attested    []string `json:"attested_capabilities"`
	ActiveJob   string   `json:"active_job"`   // the job this worker currently holds a lease on, or ""
	ActiveEpoch int      `json:"active_epoch"` // that lease's epoch, or 0
	LastSeenAgo int      `json:"last_seen_s"`  // seconds since last contact (now - last_seen_at)
	StaleHB     bool     `json:"stale_hb"`     // last_seen older than the stale threshold
}

// Roster returns the worker roster, computing each worker's active lease (by
// matching workers.identity to jobs.bound_identity in an active-lease state) and
// last-heartbeat age relative to now. A worker whose last contact is older than
// staleAfter is badged stale-hb — the worker-partitioned signal (§12.6.2),
// distinct from an agent stall.
func (s *Store) Roster(ctx context.Context, now time.Time, staleAfter time.Duration) ([]RosterWorker, error) {
	rows, err := s.DB.QueryContext(ctx, `
		SELECT w.worker_id, w.identity, w.host, w.arch, w.os, w.attested_capabilities,
		       w.last_seen_at,
		       COALESCE(j.id, ''), COALESCE(j.lease_epoch, 0)
		  FROM workers w
		  LEFT JOIN jobs j
		         ON j.bound_identity = w.identity
		        AND j.state IN ('leased','building','code_review','merging',
		                        'merge_handoff','spec_authoring','spec_review')
		 ORDER BY w.identity ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RosterWorker
	for rows.Next() {
		var r RosterWorker
		var attestedJSON, lastSeen string
		if err := rows.Scan(&r.WorkerID, &r.Identity, &r.Host, &r.Arch, &r.OS,
			&attestedJSON, &lastSeen, &r.ActiveJob, &r.ActiveEpoch); err != nil {
			return nil, err
		}
		r.Attested = unmarshalStrings(attestedJSON)
		if ts, perr := time.Parse(rfc3339, lastSeen); perr == nil {
			age := now.Sub(ts)
			if age < 0 {
				age = 0
			}
			r.LastSeenAgo = int(age / time.Second)
			r.StaleHB = age > staleAfter
		} else {
			// non-RFC3339 (datetime('now') default): treat as fresh.
			r.LastSeenAgo = 0
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
