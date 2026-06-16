package store

import "context"

// BoardJob is a single row of the live board (DESIGN §12.6).
type BoardJob struct {
	ID         string `json:"id"`
	Kind       string `json:"kind"`
	State      string `json:"state"`
	Role       string `json:"role"`
	LeaseEpoch int    `json:"lease_epoch"`
	Identity   string `json:"bound_identity"`
}

// BoardSnapshot returns all jobs ordered by recency for the board / SSE bootstrap.
func (s *Store) BoardSnapshot(ctx context.Context) ([]BoardJob, error) {
	rows, err := s.DB.QueryContext(ctx, `
		SELECT id, kind, state, role, lease_epoch, COALESCE(bound_identity,'')
		  FROM jobs ORDER BY updated_at DESC, id ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []BoardJob
	for rows.Next() {
		var b BoardJob
		if err := rows.Scan(&b.ID, &b.Kind, &b.State, &b.Role, &b.LeaseEpoch, &b.Identity); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}
