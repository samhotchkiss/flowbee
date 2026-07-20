package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// EpicHost is one registered placement target for the epic lane (0026_epics.sql,
// epic-lane Phase 2): a box an epic can be launched onto. See the migration's
// comment for why occupancy (one-box-one-epic) is a launch-time QUERY over the
// epics table rather than a column here — "active" is a state predicate, not a
// static flag this row can carry.
type EpicHost struct {
	Name      string
	Note      string
	Enabled   bool
	CreatedAt string
	UpdatedAt string
}

var (
	ErrEpicHostNotFound = errors.New("epic host not found")
	ErrEpicHostExists   = errors.New("epic host already registered")
)

// AddEpicHost registers a new placement host (`flowbee host add`). Not an upsert —
// same rationale as AddGoalSession: re-adding an existing name is almost always an
// operator typo, and silently accepting it could paper over "wait, is this the same
// box as before?" confusion right before a multi-day launch.
func (s *Store) AddEpicHost(ctx context.Context, h EpicHost, now time.Time) error {
	if h.Name == "" {
		return errors.New("host name is required")
	}
	// same argv-safety gate AddGoalSession applies to box/tmux_name (review F6): a
	// registered host name flows into `flowbee epic start`'s ssh/tmux launch argv
	// (internal/watchdog's remoteWrap), where a leading '-' reads as an ssh OPTION
	// (`-oProxyCommand=...` is local RCE) and whitespace/control chars split argv.
	// The `--` separator downstream is the primary fix; rejecting such a name at
	// REGISTRATION makes it unregistrable in the first place (defense in depth,
	// identical posture to the goal-session registry).
	if err := validateArgvSafe("host name", h.Name); err != nil {
		return err
	}
	ts := now.Format(rfc3339)
	_, err := s.DB.ExecContext(ctx, `
		INSERT INTO epic_hosts (name, note, enabled, created_at, updated_at)
		VALUES (?, ?, 1, ?, ?)`,
		h.Name, h.Note, ts, ts)
	if err != nil {
		if isUniqueConstraintErr(err) {
			return ErrEpicHostExists
		}
		return fmt.Errorf("add epic host %q: %w", h.Name, err)
	}
	return nil
}

// GetEpicHost returns one host by name. ErrEpicHostNotFound if absent.
func (s *Store) GetEpicHost(ctx context.Context, name string) (EpicHost, error) {
	return scanEpicHost(s.DB.QueryRowContext(ctx, epicHostSelect+` WHERE name = ?`, name))
}

const epicHostSelect = `SELECT name, note, enabled, created_at, updated_at FROM epic_hosts`

func scanEpicHost(row rowScanner) (EpicHost, error) {
	var h EpicHost
	var enabled int
	err := row.Scan(&h.Name, &h.Note, &enabled, &h.CreatedAt, &h.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return EpicHost{}, ErrEpicHostNotFound
	}
	if err != nil {
		return EpicHost{}, err
	}
	h.Enabled = enabled != 0
	return h, nil
}

// ListEpicHosts returns every registered host, ordered by name (`flowbee host
// list`).
func (s *Store) ListEpicHosts(ctx context.Context) ([]EpicHost, error) {
	rows, err := s.DB.QueryContext(ctx, epicHostSelect+` ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []EpicHost
	for rows.Next() {
		h, err := scanEpicHost(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// RemoveEpicHost deletes a host from the registry (`flowbee host rm`).
// ErrEpicHostNotFound if it never existed. Deliberately does NOT check for an
// active epic on this host — an operator removing a host mid-epic is trusted to
// know what they're doing; the epics row itself is untouched, so `flowbee epic
// status` still shows the epic with its (now-unregistered) host name.
func (s *Store) RemoveEpicHost(ctx context.Context, name string) error {
	res, err := s.DB.ExecContext(ctx, `DELETE FROM epic_hosts WHERE name = ?`, name)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrEpicHostNotFound
	}
	return nil
}
