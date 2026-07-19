package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"time"
)

func stableID(value string) string {
	h := sha256.Sum256([]byte(value))
	return hex.EncodeToString(h[:12])
}

type ControlAlert struct {
	ID, ProjectID, EpicID, Kind, DedupKey, Payload string
	Epoch, Attempts                                int
}

func ensureControlAlertTx(ctx context.Context, tx *sql.Tx, projectID, epicID, kind, dedup, payload string, now time.Time) error {
	id := "alert-" + stableID(dedup)
	var epicRef any
	if epicID != "" {
		epicRef = epicID
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO control_alerts
		(id,project_id,epic_id,kind,dedup_key,payload_json,state,created_at,updated_at)
		VALUES (?,?,?,?,?,?,'pending',?,?)`, id, projectID, epicRef, kind, dedup, payload,
		now.UTC().Format(rfc3339), now.UTC().Format(rfc3339))
	if err == nil || isUniqueConstraintErr(err) {
		return nil
	}
	return err
}

// ClaimNextControlAlert is the exactly-one publisher claim. The epoch fences a
// publisher that resumes after its claim deadline and prevents it acknowledging a
// retry owned by another process incarnation.
func (s *Store) ClaimNextControlAlert(ctx context.Context, owner string, now time.Time, ttl time.Duration) (ControlAlert, bool, error) {
	if owner == "" {
		return ControlAlert{}, false, errors.New("control alert claim requires owner")
	}
	if ttl <= 0 {
		ttl = time.Minute
	}
	var out ControlAlert
	err := s.tx(ctx, func(tx *sql.Tx) error {
		nowText := now.UTC().Format(rfc3339)
		err := tx.QueryRowContext(ctx, `SELECT id,project_id,COALESCE(epic_id,''),kind,dedup_key,payload_json,alert_epoch,attempts
			FROM control_alerts WHERE state='pending' AND (next_attempt_at='' OR julianday(next_attempt_at)<=julianday(?))
			ORDER BY created_at,id LIMIT 1`, nowText).Scan(&out.ID, &out.ProjectID, &out.EpicID,
			&out.Kind, &out.DedupKey, &out.Payload, &out.Epoch, &out.Attempts)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		res, err := tx.ExecContext(ctx, `UPDATE control_alerts SET state='delivering',alert_epoch=alert_epoch+1,
			attempts=attempts+1,claim_owner=?,claim_deadline_at=?,updated_at=?
			WHERE id=? AND state='pending' AND alert_epoch=?`, owner, now.Add(ttl).UTC().Format(rfc3339),
			nowText, out.ID, out.Epoch)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			out = ControlAlert{}
			return nil
		}
		out.Epoch++
		out.Attempts++
		return nil
	})
	return out, out.ID != "", err
}

func (s *Store) AcknowledgeControlAlert(ctx context.Context, id, owner string, epoch int, now time.Time) error {
	return s.finishControlAlert(ctx, id, owner, epoch, "acknowledged", "", time.Time{}, now)
}

func (s *Store) RetryControlAlert(ctx context.Context, id, owner string, epoch int, detail string, next, now time.Time) error {
	return s.finishControlAlert(ctx, id, owner, epoch, "pending", detail, next, now)
}

func (s *Store) DeadLetterControlAlert(ctx context.Context, id, owner string, epoch int, detail string, now time.Time) error {
	return s.finishControlAlert(ctx, id, owner, epoch, "dead_letter", detail, time.Time{}, now)
}

func (s *Store) finishControlAlert(ctx context.Context, id, owner string, epoch int, state, detail string, next, now time.Time) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		nowText, nextText := now.UTC().Format(rfc3339), ""
		if !next.IsZero() {
			nextText = next.UTC().Format(rfc3339)
		}
		ack, dead := "", ""
		if state == "acknowledged" {
			ack = nowText
		}
		if state == "dead_letter" {
			dead = nowText
		}
		res, err := tx.ExecContext(ctx, `UPDATE control_alerts SET state=?,next_attempt_at=?,claim_owner='',
			claim_deadline_at='',acknowledged_at=?,dead_lettered_at=?,last_error=?,updated_at=?
			WHERE id=? AND state='delivering' AND claim_owner=? AND alert_epoch=?`, state, nextText,
			ack, dead, detail, nowText, id, owner, epoch)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return fmt.Errorf("stale control alert claim")
		}
		if state == "acknowledged" {
			if _, err := tx.ExecContext(ctx, `UPDATE epic_deliveries SET alert_pending=0
				WHERE epic_id=(SELECT epic_id FROM control_alerts WHERE id=?)`, id); err != nil {
				return err
			}
		}
		return nil
	})
}

// ReclaimExpiredControlAlerts makes a publisher crash recoverable. Delivery is
// at-least-once at the webhook boundary; dedup_key is included for receiver-side
// idempotency, while Flowbee never loses the committed alert.
func (s *Store) ReclaimExpiredControlAlerts(ctx context.Context, now time.Time) (int64, error) {
	res, err := s.DB.ExecContext(ctx, `UPDATE control_alerts SET state='pending',claim_owner='',
		claim_deadline_at='',next_attempt_at=?,last_error='publisher_claim_expired',updated_at=?
		WHERE state='delivering' AND claim_deadline_at<>'' AND julianday(claim_deadline_at)<=julianday(?)`,
		now.UTC().Format(rfc3339), now.UTC().Format(rfc3339), now.UTC().Format(rfc3339))
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
