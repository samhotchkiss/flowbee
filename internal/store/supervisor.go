package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/samhotchkiss/flowbee/internal/ledger"
)

// Supervisor is one registered master (0027, epic-lane Phase 5, plan §1.1). A master is
// a long-lived interactive Claude/Codex tmux session that registers as a supervised
// actor and supplies LEASED judgment — NOT a worker and NOT a goal_sessions row (so
// watchdog.Pass never types `/goal resume` into its pane). It lives here, structurally
// separate, exactly for that reason (plan bet #4).
type Supervisor struct {
	ID                 string // stable opaque master_id (defaults to Label on first register)
	Label              string // the idempotency key registration upserts on
	Epoch              int    // the fence: bumped on every (re-)registration
	State              string // active | stale | revoked
	Kind               string // the master's own agent binary (claude|codex)
	ModelFamily        string // anti-affinity family tag
	Box                string
	TmuxName           string
	Repos              []string
	LastHeartbeatAt    string
	LastReportedStatus string // last human-facing update (plan §15.7)
	LastReportedAt     string
	CreatedAt          string
	UpdatedAt          string
}

// SupervisorRegistration is the result of RegisterSupervisor: the stable master_id, the
// (bumped) epoch to fence subsequent calls with, and how many prior leases the
// re-registration orphaned (returned to open).
type SupervisorRegistration struct {
	MasterID      string
	Epoch         int
	RevokedLeases int
}

// ErrSupervisorNotFound is returned when a master id/label is unknown.
var ErrSupervisorNotFound = errors.New("supervisor not found")

// ErrSupervisorRevoked is returned when a re-registration targets a label whose master
// was deliberately revoked (operator retirement): registration will NOT resurrect it
// (n2). Re-enabling a revoked master is a separate, explicit action.
var ErrSupervisorRevoked = errors.New("supervisor was revoked; re-enable explicitly")

// RegisterSupervisor is the IDEMPOTENT upsert keyed on Label (plan §1.2) — the opposite
// of AddGoalSession/AddEpicRun, which fail loud, because re-registration is EXPECTED on
// every `/clear` or restart. A brand-new master and a post-`/clear` master are the same
// code path. Every registration BUMPS epoch (fencing every lease the prior incarnation
// held) and, on a re-registration, ORPHANS the prior incarnation's still-`leased` items
// back to open (a master that died does not remember what it leased). Ledgers
// supervisor_registered.
func (s *Store) RegisterSupervisor(ctx context.Context, sup Supervisor, now time.Time) (SupervisorRegistration, error) {
	if sup.Label == "" {
		return SupervisorRegistration{}, errors.New("supervisor label is required")
	}
	// box/tmux_name feed ssh/tmux argv construction (like goal sessions) — reject
	// anything argv-hostile at registration, belt-and-suspenders with shQuote downstream.
	if err := validateArgvSafe("box", sup.Box); err != nil {
		return SupervisorRegistration{}, err
	}
	if err := validateArgvSafe("tmux_name", sup.TmuxName); err != nil {
		return SupervisorRegistration{}, err
	}
	ts := now.Format(rfc3339)
	var reg SupervisorRegistration
	err := s.tx(ctx, func(tx *sql.Tx) error {
		var id, state string
		var epoch int
		e := tx.QueryRowContext(ctx, `SELECT id, epoch, state FROM supervisors WHERE label = ?`, sup.Label).
			Scan(&id, &epoch, &state)
		switch {
		case errors.Is(e, sql.ErrNoRows):
			id = sup.Label // id defaults to the label on first registration
			epoch = 1
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO supervisors
				    (id, label, epoch, state, kind, model_family, box, tmux_name, repos_json,
				     last_heartbeat_at, last_reported_status, last_reported_at, created_at, updated_at)
				VALUES (?, ?, ?, 'active', ?, ?, ?, ?, ?, ?, '', '', ?, ?)`,
				id, sup.Label, epoch, sup.Kind, sup.ModelFamily, sup.Box, sup.TmuxName,
				marshalStrings(sup.Repos), ts, ts, ts); err != nil {
				if isUniqueConstraintErr(err) {
					return ErrSupervisorExists
				}
				return fmt.Errorf("register supervisor %q: %w", sup.Label, err)
			}
		case e != nil:
			return e
		case state == "revoked":
			// a DELIBERATELY revoked master (operator retirement) is NOT resurrected by a
			// re-registration — that would silently re-enable a master an operator killed.
			// Explicit re-enable is a separate, intentional action (n2).
			return ErrSupervisorRevoked
		default:
			epoch++
			if _, err := tx.ExecContext(ctx, `
				UPDATE supervisors
				   SET epoch = ?, state = 'active', kind = ?, model_family = ?, box = ?,
				       tmux_name = ?, repos_json = ?, last_heartbeat_at = ?, updated_at = ?
				 WHERE id = ?`,
				epoch, sup.Kind, sup.ModelFamily, sup.Box, sup.TmuxName, marshalStrings(sup.Repos),
				ts, ts, id); err != nil {
				return err
			}
			n, err := reopenSupervisorLeasesTx(ctx, tx, id, now)
			if err != nil {
				return err
			}
			reg.RevokedLeases = n
		}
		reg.MasterID = id
		reg.Epoch = epoch
		return appendEpicLedger(ctx, tx, supLedgerPrefix+id, ledger.KindSupervisorRegistered, id, epoch, "", "registered", now)
	})
	if err != nil {
		return SupervisorRegistration{}, err
	}
	return reg, nil
}

// ErrSupervisorExists guards the (unreachable-under-serialization) label race on insert.
var ErrSupervisorExists = errors.New("supervisor already registered")

// SupervisorHeartbeat refreshes a master's liveness. revoked=true means a NEWER
// registration superseded this incarnation (the caller's epoch no longer matches the
// live one) OR the master was revoked — the caller must stop and re-register (plan §1.2).
// A matching epoch refreshes last_heartbeat_at and reactivates a merely-stale master.
func (s *Store) SupervisorHeartbeat(ctx context.Context, id string, epoch int, now time.Time) (revoked bool, err error) {
	ts := now.Format(rfc3339)
	err = s.tx(ctx, func(tx *sql.Tx) error {
		var liveEpoch int
		var state string
		e := tx.QueryRowContext(ctx, `SELECT epoch, state FROM supervisors WHERE id = ?`, id).
			Scan(&liveEpoch, &state)
		if errors.Is(e, sql.ErrNoRows) {
			return ErrSupervisorNotFound
		}
		if e != nil {
			return e
		}
		if state == "revoked" || liveEpoch != epoch {
			revoked = true
			return nil // superseded/retired: no write, tell the caller to re-register
		}
		_, err := tx.ExecContext(ctx,
			`UPDATE supervisors SET last_heartbeat_at = ?, state = 'active', updated_at = ? WHERE id = ?`,
			ts, ts, id)
		return err
	})
	return revoked, err
}

// SetSupervisorLastReport records the master's last human-facing update (plan §15.7): a
// fresh post-`/clear` master reads it to CONTINUE the thread rather than re-reporting or
// contradicting itself.
func (s *Store) SetSupervisorLastReport(ctx context.Context, id, status string, now time.Time) error {
	ts := now.Format(rfc3339)
	res, err := s.DB.ExecContext(ctx,
		`UPDATE supervisors SET last_reported_status = ?, last_reported_at = ?, updated_at = ? WHERE id = ?`,
		status, ts, ts, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrSupervisorNotFound
	}
	return nil
}

// ListStaleSupervisors returns every ACTIVE master whose last heartbeat is older than
// 3× the heartbeat interval (plan §1.6) at `now` — the liveness reaper's input. The
// cutoff is computed and compared in Go (RFC3339Nano does not sort lexically across the
// fractional-second boundary, so an SQL string compare would be subtly wrong).
func (s *Store) ListStaleSupervisors(ctx context.Context, interval time.Duration, now time.Time) ([]Supervisor, error) {
	all, err := s.listSupervisorsWhere(ctx, `WHERE state = 'active'`)
	if err != nil {
		return nil, err
	}
	cutoff := now.Add(-3 * interval)
	var stale []Supervisor
	for _, sup := range all {
		if sup.LastHeartbeatAt == "" {
			stale = append(stale, sup)
			continue
		}
		if t, perr := time.Parse(rfc3339, sup.LastHeartbeatAt); perr == nil && t.Before(cutoff) {
			stale = append(stale, sup)
		}
	}
	return stale, nil
}

// MarkSupervisorStale flips a master to state='stale' and reaps its still-`leased` items
// back to open (plan §1.6) — the items are durable and wait for the next live master.
// Does NOT touch delivering/awaiting_ack rows (the crash-window + ack loop own those).
func (s *Store) MarkSupervisorStale(ctx context.Context, id string, now time.Time) error {
	ts := now.Format(rfc3339)
	return s.tx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx,
			`UPDATE supervisors SET state = 'stale', updated_at = ? WHERE id = ?`, ts, id)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrSupervisorNotFound
		}
		_, err = reopenSupervisorLeasesTx(ctx, tx, id, now)
		return err
	})
}

// GetSupervisor returns one master by id. ErrSupervisorNotFound if absent.
func (s *Store) GetSupervisor(ctx context.Context, id string) (Supervisor, error) {
	sups, err := s.listSupervisorsWhere(ctx, `WHERE id = ?`, id)
	if err != nil {
		return Supervisor{}, err
	}
	if len(sups) == 0 {
		return Supervisor{}, ErrSupervisorNotFound
	}
	return sups[0], nil
}

// ListSupervisors returns every registered master ordered by label.
func (s *Store) ListSupervisors(ctx context.Context) ([]Supervisor, error) {
	return s.listSupervisorsWhere(ctx, `ORDER BY label`)
}

const supervisorSelect = `
	SELECT id, label, epoch, state, kind, model_family, box, tmux_name, repos_json,
	       last_heartbeat_at, last_reported_status, last_reported_at, created_at, updated_at
	  FROM supervisors`

func (s *Store) listSupervisorsWhere(ctx context.Context, clause string, args ...any) ([]Supervisor, error) {
	rows, err := s.DB.QueryContext(ctx, supervisorSelect+" "+clause, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Supervisor
	for rows.Next() {
		var sup Supervisor
		var reposJSON string
		if err := rows.Scan(&sup.ID, &sup.Label, &sup.Epoch, &sup.State, &sup.Kind, &sup.ModelFamily,
			&sup.Box, &sup.TmuxName, &reposJSON, &sup.LastHeartbeatAt, &sup.LastReportedStatus,
			&sup.LastReportedAt, &sup.CreatedAt, &sup.UpdatedAt); err != nil {
			return nil, err
		}
		sup.Repos = unmarshalStrings(reposJSON)
		out = append(out, sup)
	}
	return out, rows.Err()
}

// reopenSupervisorLeasesTx returns every still-`leased` item held by a master back to
// open (clearing the lease) and returns the count. The shared orphaning step behind both
// re-registration (plan §1.4) and stale-reaping (plan §1.6). delivering/awaiting_ack rows
// are deliberately untouched.
func reopenSupervisorLeasesTx(ctx context.Context, tx *sql.Tx, masterID string, now time.Time) (int, error) {
	res, err := tx.ExecContext(ctx, `
		UPDATE attention_items
		   SET state = 'open', leased_by = '', delivery_key = '', lease_expires_at = '', updated_at = ?
		 WHERE leased_by = ? AND state = 'leased'`,
		now.Format(rfc3339), masterID)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// ── WIP markers (plan §15.7) ──

// WIPMarker is a master-registered "a fix is already in flight" marker. These SURVIVE
// master compaction/`/clear` so a fresh master does NOT re-dispatch a fix it already
// launched. PRNumber is 0 when the marker binds only an epic (no PR yet).
type WIPMarker struct {
	ID           string
	EpicID       string
	PRNumber     int
	Label        string
	RegisteredBy string // supervisors.id
	StartedAt    string
	ETA          string
	ClearedAt    string // "" while in flight
	CreatedAt    string
	UpdatedAt    string
}

// UpsertWIPMarker registers (or refreshes) a WIP marker, keyed on id — idempotent so a
// post-compaction master re-registering the same marker never duplicates it.
func (s *Store) UpsertWIPMarker(ctx context.Context, m WIPMarker, now time.Time) error {
	if m.ID == "" {
		return errors.New("wip marker id is required")
	}
	ts := now.Format(rfc3339)
	pr := sql.NullInt64{Int64: int64(m.PRNumber), Valid: m.PRNumber > 0}
	started := m.StartedAt
	if started == "" {
		started = ts
	}
	_, err := s.DB.ExecContext(ctx, `
		INSERT INTO wip_markers (id, epic_id, pr_number, label, registered_by, started_at, eta, cleared_at, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, NULL, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
		    epic_id = excluded.epic_id, pr_number = excluded.pr_number, label = excluded.label,
		    registered_by = excluded.registered_by, eta = excluded.eta, updated_at = excluded.updated_at`,
		m.ID, m.EpicID, pr, m.Label, m.RegisteredBy, started, m.ETA, ts, ts)
	return err
}

// ClearWIPMarker marks a WIP marker cleared (the fix landed / was abandoned).
func (s *Store) ClearWIPMarker(ctx context.Context, id string, now time.Time) error {
	ts := now.Format(rfc3339)
	res, err := s.DB.ExecContext(ctx,
		`UPDATE wip_markers SET cleared_at = ?, updated_at = ? WHERE id = ? AND cleared_at IS NULL`,
		ts, ts, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrWIPMarkerNotFound
	}
	return nil
}

// ErrWIPMarkerNotFound is returned when clearing an unknown or already-cleared marker.
var ErrWIPMarkerNotFound = errors.New("wip marker not found or already cleared")

// ListWIPMarkers returns ACTIVE (uncleared) markers — all epics when epicID is "", or a
// single epic's markers otherwise. The read a fresh master consults before dispatching a
// fix subagent (plan §15.7).
func (s *Store) ListWIPMarkers(ctx context.Context, epicID string) ([]WIPMarker, error) {
	q := `SELECT id, epic_id, pr_number, label, registered_by, started_at, eta, cleared_at, created_at, updated_at
	        FROM wip_markers WHERE cleared_at IS NULL`
	var args []any
	if epicID != "" {
		q += ` AND epic_id = ?`
		args = append(args, epicID)
	}
	q += ` ORDER BY started_at ASC, id ASC`
	rows, err := s.DB.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []WIPMarker
	for rows.Next() {
		var m WIPMarker
		var pr sql.NullInt64
		var cleared sql.NullString
		if err := rows.Scan(&m.ID, &m.EpicID, &pr, &m.Label, &m.RegisteredBy, &m.StartedAt, &m.ETA,
			&cleared, &m.CreatedAt, &m.UpdatedAt); err != nil {
			return nil, err
		}
		if pr.Valid {
			m.PRNumber = int(pr.Int64)
		}
		m.ClearedAt = cleared.String
		out = append(out, m)
	}
	return out, rows.Err()
}
