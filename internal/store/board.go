package store

import (
	"context"
	"time"
)

// BoardJob is a single row of the live board (DESIGN §12.6). It carries enough
// to render both the JSON board (SSE bootstrap / user-agent loop) and the
// operator `flowbee board` table: repo, issue, state/role, bounces, and the
// last-update instant used to age each row.
type BoardJob struct {
	ID          string    `json:"id"`
	Repo        string    `json:"repo"`
	Kind        string    `json:"kind"`
	State       string    `json:"state"`
	Role        string    `json:"role"`
	IssueNumber int       `json:"issue_number"` // 0 == not tied to an issue
	Bounces     int       `json:"bounces"`
	LeaseEpoch  int       `json:"lease_epoch"`
	Identity    string    `json:"bound_identity"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// BoardSnapshot returns all jobs ordered by recency for the board / SSE bootstrap.
func (s *Store) BoardSnapshot(ctx context.Context) ([]BoardJob, error) {
	rows, err := s.DB.QueryContext(ctx, `
		SELECT id, COALESCE(repo,''), kind, state, role,
		       COALESCE(issue_number,0), bounces, lease_epoch,
		       COALESCE(bound_identity,''), updated_at
		  FROM jobs ORDER BY updated_at DESC, id ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []BoardJob
	for rows.Next() {
		var (
			b       BoardJob
			updated string
		)
		if err := rows.Scan(&b.ID, &b.Repo, &b.Kind, &b.State, &b.Role,
			&b.IssueNumber, &b.Bounces, &b.LeaseEpoch, &b.Identity, &updated); err != nil {
			return nil, err
		}
		// updated_at is written as RFC3339Nano on every state change, but a row
		// that has never been touched since insert carries SQLite's default
		// `datetime('now')` ("2006-01-02 15:04:05"). Accept both; a zero time
		// just means the age renderer falls back to "0s".
		if ts, perr := time.Parse(rfc3339, updated); perr == nil {
			b.UpdatedAt = ts
		} else if ts, perr := time.Parse(sqliteTS, updated); perr == nil {
			b.UpdatedAt = ts
		}
		out = append(out, b)
	}
	return out, rows.Err()
}
