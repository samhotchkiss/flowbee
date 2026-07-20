package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
)

var ErrControlPlaneWriterLockRequired = errors.New("control-plane writer lock is required")

type ControlPlaneIncarnation struct {
	ID                  string    `json:"incarnation_id"`
	State               string    `json:"state"`
	Version             string    `json:"version"`
	SourceCommit        string    `json:"source_commit,omitempty"`
	ConfigPostureSHA256 string    `json:"config_posture_sha256"`
	ProcessID           int       `json:"process_id"`
	StartedAt           time.Time `json:"started_at"`
	StoppedAt           time.Time `json:"stopped_at,omitempty"`
	StopReason          string    `json:"stop_reason,omitempty"`
	SupersededBy        string    `json:"superseded_by,omitempty"`
}

type StartControlPlaneIncarnationInput struct {
	ID, Version, SourceCommit, ConfigPostureSHA256 string
	ProcessID                                      int
	StartedAt                                      time.Time
}

type ControlPlaneIncarnationStart struct {
	Current           ControlPlaneIncarnation
	RecoveredPriorIDs []string
	IdempotentReplay  bool
}

type ControlPlaneIncarnationEvent struct {
	Seq                  int64
	IncarnationID        string
	Kind                 string
	RelatedIncarnationID string
	Reason               string
	CreatedAt            time.Time
}

func NewControlPlaneIncarnationID() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("mint control-plane incarnation id: %w", err)
	}
	return "cpi-" + hex.EncodeToString(raw[:]), nil
}

// StartControlPlaneIncarnation durably installs this process as the only current
// authority. The caller must already hold the process-lifetime OS writer lock and
// have applied migrations. Any active row left by a killed process is recovered
// in the same transaction that installs its replacement.
func (s *Store) StartControlPlaneIncarnation(ctx context.Context,
	in StartControlPlaneIncarnationInput) (ControlPlaneIncarnationStart, error) {
	var out ControlPlaneIncarnationStart
	if s == nil || !s.writerLockHeld {
		return out, ErrControlPlaneWriterLockRequired
	}
	in.ID, in.Version = strings.TrimSpace(in.ID), strings.TrimSpace(in.Version)
	in.ConfigPostureSHA256 = strings.TrimSpace(in.ConfigPostureSHA256)
	_, idErr := hex.DecodeString(strings.TrimPrefix(in.ID, "cpi-"))
	_, postureErr := hex.DecodeString(strings.TrimPrefix(in.ConfigPostureSHA256, "sha256:"))
	if len(in.ID) != len("cpi-")+32 || !strings.HasPrefix(in.ID, "cpi-") || idErr != nil ||
		in.Version == "" || len(in.ConfigPostureSHA256) != len("sha256:")+64 ||
		!strings.HasPrefix(in.ConfigPostureSHA256, "sha256:") || postureErr != nil ||
		in.ProcessID <= 0 || in.StartedAt.IsZero() {
		return out, errors.New("invalid control-plane incarnation identity")
	}
	stamp := in.StartedAt.UTC().Format(rfc3339)
	err := s.tx(ctx, func(tx *sql.Tx) error {
		var existing ControlPlaneIncarnation
		var started, stopped string
		err := tx.QueryRowContext(ctx, `SELECT incarnation_id,state,version,source_commit,
			config_posture_sha256,process_id,started_at,stopped_at,stop_reason,superseded_by
			FROM control_plane_incarnations WHERE incarnation_id=?`, in.ID).Scan(
			&existing.ID, &existing.State, &existing.Version, &existing.SourceCommit,
			&existing.ConfigPostureSHA256, &existing.ProcessID, &started, &stopped,
			&existing.StopReason, &existing.SupersededBy)
		if err == nil {
			existing.StartedAt = parseOptionalTime(started)
			existing.StoppedAt = parseOptionalTime(stopped)
			if existing.State != "active" || existing.Version != in.Version ||
				existing.SourceCommit != in.SourceCommit || existing.ConfigPostureSHA256 != in.ConfigPostureSHA256 ||
				existing.ProcessID != in.ProcessID || !existing.StartedAt.Equal(in.StartedAt.UTC()) {
				return errors.New("control-plane incarnation idempotency conflict")
			}
			var currentID string
			if err := tx.QueryRowContext(ctx, `SELECT current_incarnation_id
				FROM control_plane_state WHERE singleton=1`).Scan(&currentID); err != nil || currentID != in.ID {
				return errors.New("control-plane incarnation projection mismatch")
			}
			out.Current, out.IdempotentReplay = existing, true
			return nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		rows, err := tx.QueryContext(ctx, `SELECT incarnation_id FROM control_plane_incarnations
			WHERE state='active' ORDER BY started_at,incarnation_id`)
		if err != nil {
			return err
		}
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return err
			}
			out.RecoveredPriorIDs = append(out.RecoveredPriorIDs, id)
		}
		if err := rows.Close(); err != nil {
			return err
		}
		for _, priorID := range out.RecoveredPriorIDs {
			if _, err := tx.ExecContext(ctx, `UPDATE control_plane_incarnations
				SET state='superseded',stopped_at=?,stop_reason='unclean_restart',
				superseded_by=?,updated_at=? WHERE incarnation_id=? AND state='active'`,
				stamp, in.ID, stamp, priorID); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO control_plane_incarnation_events
				(incarnation_id,kind,related_incarnation_id,reason,created_at)
				VALUES (?,'superseded',?,'unclean_restart',?)`, priorID, in.ID, stamp); err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO control_plane_incarnations
			(incarnation_id,state,version,source_commit,config_posture_sha256,process_id,
			 started_at,created_at,updated_at) VALUES (?,'active',?,?,?,?,?,?,?)`,
			in.ID, in.Version, in.SourceCommit, in.ConfigPostureSHA256, in.ProcessID,
			stamp, stamp, stamp); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO control_plane_incarnation_events
			(incarnation_id,kind,reason,created_at) VALUES (?,'started','writer_lock_acquired',?)`,
			in.ID, stamp); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO control_plane_state
			(singleton,current_incarnation_id,state_version,updated_at) VALUES (1,?,1,?)
			ON CONFLICT(singleton) DO UPDATE SET current_incarnation_id=excluded.current_incarnation_id,
			state_version=control_plane_state.state_version+1,updated_at=excluded.updated_at`,
			in.ID, stamp); err != nil {
			return err
		}
		out.Current = ControlPlaneIncarnation{ID: in.ID, State: "active", Version: in.Version,
			SourceCommit: in.SourceCommit, ConfigPostureSHA256: in.ConfigPostureSHA256,
			ProcessID: in.ProcessID, StartedAt: in.StartedAt.UTC()}
		return nil
	})
	return out, err
}

func (s *Store) StopControlPlaneIncarnation(ctx context.Context, id, reason string,
	now time.Time) error {
	if s == nil || !s.writerLockHeld {
		return ErrControlPlaneWriterLockRequired
	}
	id, reason = strings.TrimSpace(id), strings.TrimSpace(reason)
	if id == "" || reason == "" || now.IsZero() {
		return errors.New("invalid control-plane stop")
	}
	stamp := now.UTC().Format(rfc3339)
	return s.tx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `UPDATE control_plane_incarnations SET state='stopped',
			stopped_at=?,stop_reason=?,updated_at=? WHERE incarnation_id=? AND state='active'`,
			stamp, reason, stamp, id)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			var state, existingReason string
			if err := tx.QueryRowContext(ctx, `SELECT state,stop_reason FROM control_plane_incarnations
				WHERE incarnation_id=?`, id).Scan(&state, &existingReason); err != nil {
				return err
			}
			if state == "stopped" && existingReason == reason {
				return nil
			}
			return errors.New("control-plane incarnation is not active")
		}
		if _, err := tx.ExecContext(ctx, `UPDATE control_plane_state SET
			state_version=state_version+1,updated_at=?
			WHERE singleton=1 AND current_incarnation_id=?`, stamp, id); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO control_plane_incarnation_events
			(incarnation_id,kind,reason,created_at) VALUES (?,'stopped',?,?)`, id, reason, stamp)
		return err
	})
}

func (s *Store) CurrentControlPlaneIncarnation(ctx context.Context) (ControlPlaneIncarnation, error) {
	var out ControlPlaneIncarnation
	var started, stopped string
	err := s.DB.QueryRowContext(ctx, `SELECT i.incarnation_id,i.state,i.version,i.source_commit,
		i.config_posture_sha256,i.process_id,i.started_at,i.stopped_at,i.stop_reason,i.superseded_by
		FROM control_plane_state c JOIN control_plane_incarnations i
		ON i.incarnation_id=c.current_incarnation_id WHERE c.singleton=1`).Scan(
		&out.ID, &out.State, &out.Version, &out.SourceCommit, &out.ConfigPostureSHA256,
		&out.ProcessID, &started, &stopped, &out.StopReason, &out.SupersededBy)
	out.StartedAt, out.StoppedAt = parseOptionalTime(started), parseOptionalTime(stopped)
	return out, err
}

func (s *Store) ListControlPlaneIncarnations(ctx context.Context) ([]ControlPlaneIncarnation, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT incarnation_id,state,version,source_commit,
		config_posture_sha256,process_id,started_at,stopped_at,stop_reason,superseded_by
		FROM control_plane_incarnations ORDER BY started_at,incarnation_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ControlPlaneIncarnation
	for rows.Next() {
		var item ControlPlaneIncarnation
		var started, stopped string
		if err := rows.Scan(&item.ID, &item.State, &item.Version, &item.SourceCommit,
			&item.ConfigPostureSHA256, &item.ProcessID, &started, &stopped,
			&item.StopReason, &item.SupersededBy); err != nil {
			return nil, err
		}
		item.StartedAt, item.StoppedAt = parseOptionalTime(started), parseOptionalTime(stopped)
		out = append(out, item)
	}
	return out, rows.Err()
}

func (s *Store) ControlPlaneIncarnationEvents(ctx context.Context) ([]ControlPlaneIncarnationEvent, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT seq,incarnation_id,kind,
		related_incarnation_id,reason,created_at FROM control_plane_incarnation_events ORDER BY seq`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ControlPlaneIncarnationEvent
	for rows.Next() {
		var event ControlPlaneIncarnationEvent
		var created string
		if err := rows.Scan(&event.Seq, &event.IncarnationID, &event.Kind,
			&event.RelatedIncarnationID, &event.Reason, &created); err != nil {
			return nil, err
		}
		event.CreatedAt = parseOptionalTime(created)
		out = append(out, event)
	}
	return out, rows.Err()
}
