package store

import (
	"context"
	"errors"
	"time"
)

// Epic runtime-state setters (0028_epic_capacity.sql, epic-lane Phase 6). The launch
// gate BINDS an epic to its account+seat (SetEpicSeatBinding); the consolidated
// supervision ticker writes the disk-derived runtime facts each pass
// (SetEpicRuntimeState). Kept adjacent to the epicdigest read model these columns feed.

// EpicRuntimeState is one supervision pass's disk/pane-derived observation of a running
// epic. ContextPct is the REMAINING-context percentage (0..100; higher = healthier);
// pass ContextPctUnknown (-1) when ctxprobe could not resolve it (e.g. the Claude
// on-disk transcript lacks the window size — we NEVER guess it, plan §12.4). A
// compaction event RAISES ContextPct (context freed) and must not be read as drift
// (plan §15.3 — the epicdigest.CompactionJumped helper the ticker uses).
type EpicRuntimeState struct {
	ContextPct   float64 // -1 = unknown
	PaneState    string  // tmuxio.Classify token
	AuthState    string  // '' | ok | auth_dead
	LastCommitAt string  // RFC3339; '' = unknown/none
}

// ContextPctUnknown is the sentinel stored when the running session's remaining-context
// percentage could not be resolved from disk. It mirrors the account_windows -1
// convention: a real 0% (context full) is distinct from "unknown".
const ContextPctUnknown = -1.0

// SetEpicRuntimeState writes one supervision pass's observed runtime facts onto the
// epic row. It updates ONLY these four columns (plus updated_at) so it composes with
// the independent status-ingestion write (UpsertEpicStatus) without either clobbering
// the other's fields. ErrEpicRunNotFound if the epic is gone.
func (s *Store) SetEpicRuntimeState(ctx context.Context, id string, rs EpicRuntimeState, now time.Time) error {
	ts := now.Format(rfc3339)
	res, err := s.DB.ExecContext(ctx, `
		UPDATE epics
		   SET context_pct = ?, pane_state = ?, auth_state = ?, last_commit_at = ?, updated_at = ?
		 WHERE id = ?`,
		rs.ContextPct, rs.PaneState, rs.AuthState, rs.LastCommitAt, ts, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrEpicRunNotFound
	}
	return nil
}

// ErrEpicSeatRebind protects the admission invariant: once a launching/running row has
// a seat, moving it after AddEpicRun's capacity check could overbook the destination.
var ErrEpicSeatRebind = errors.New("epic seat binding is immutable")

// SetEpicSeatBinding is the legacy one-time backfill seam for rows created before the
// binding became part of AddEpicRun's atomic insert. A blank binding may be filled and
// an identical binding refreshed, but an existing seat can never be changed. New launch
// code must pass SeatID to AddEpicRun instead. ErrEpicRunNotFound if the epic is gone.
func (s *Store) SetEpicSeatBinding(ctx context.Context, id, accountKey, seatID, builderModelFamily string, now time.Time) error {
	ts := now.Format(rfc3339)
	res, err := s.DB.ExecContext(ctx, `
		UPDATE epics
		   SET account_key = ?, seat_id = ?, builder_model_family = ?, updated_at = ?
		 WHERE id = ? AND (seat_id = '' OR seat_id = ?)`,
		accountKey, seatID, builderModelFamily, ts, id, seatID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		if _, getErr := s.GetEpicRun(ctx, id); getErr != nil {
			return getErr
		}
		return ErrEpicSeatRebind
	}
	return nil
}

// SetEpicExplainerPath records the per-epic visual explainer file discovered on the
// epic branch (plan §15.14; the dashboard serves it sandboxed). ” clears it.
// ErrEpicRunNotFound if the epic is gone.
func (s *Store) SetEpicExplainerPath(ctx context.Context, id, path string, now time.Time) error {
	ts := now.Format(rfc3339)
	res, err := s.DB.ExecContext(ctx,
		`UPDATE epics SET explainer_path = ?, updated_at = ? WHERE id = ?`, path, ts, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrEpicRunNotFound
	}
	return nil
}
