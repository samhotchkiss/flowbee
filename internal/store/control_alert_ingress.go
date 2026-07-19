package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

var (
	ErrControlAlertIngressConflict            = errors.New("control alert ingress idempotency conflict")
	ErrControlAlertIngressProjectUnauthorized = errors.New("control alert ingress project is not active or authorized")
)

type ControlAlertIngressInput struct {
	IdempotencyKey string
	BodySHA256     string
	Body           []byte
	AlertID        string
	ProjectID      string
	EpicID         string
	Kind           string
	PayloadJSON    string
}

// AcceptControlAlertIngress commits the exact authenticated request body and
// its project control alert atomically. Exact replay is a no-op. A changed body
// or any attempt to bind an existing alert/dedup identity without the original
// ingress row is a conflict, never an implicit adoption of unrelated state.
func (s *Store) AcceptControlAlertIngress(ctx context.Context, in ControlAlertIngressInput, now time.Time) error {
	in.IdempotencyKey = strings.TrimSpace(in.IdempotencyKey)
	in.BodySHA256 = strings.TrimSpace(in.BodySHA256)
	in.AlertID = strings.TrimSpace(in.AlertID)
	in.ProjectID = strings.TrimSpace(in.ProjectID)
	in.EpicID = strings.TrimSpace(in.EpicID)
	in.Kind = strings.TrimSpace(in.Kind)
	if in.IdempotencyKey == "" || len(in.IdempotencyKey) > 512 || in.AlertID == "" ||
		in.ProjectID == "" || in.Kind == "" || len(in.Body) == 0 || !json.Valid([]byte(in.PayloadJSON)) {
		return errors.New("control alert ingress submission is incomplete")
	}
	computed := sha256.Sum256(in.Body)
	computedText := hex.EncodeToString(computed[:])
	if in.BodySHA256 != computedText {
		return errors.New("control alert ingress body sha256 does not match exact bytes")
	}

	return s.tx(ctx, func(tx *sql.Tx) error {
		var existingSHA, existingProject, existingAlertID, existingEnvelopeID, existingKind string
		var existingBody []byte
		err := tx.QueryRowContext(ctx, `SELECT body_sha256,body,project_id,control_alert_id,
			envelope_id,envelope_kind FROM control_alert_ingress_submissions
			WHERE idempotency_key=?`, in.IdempotencyKey).
			Scan(&existingSHA, &existingBody, &existingProject, &existingAlertID, &existingEnvelopeID, &existingKind)
		if err == nil {
			if existingSHA != in.BodySHA256 || !bytes.Equal(existingBody, in.Body) ||
				existingProject != in.ProjectID || existingAlertID != in.AlertID ||
				existingEnvelopeID != in.AlertID || existingKind != in.Kind {
				return ErrControlAlertIngressConflict
			}
			var alertProject, alertEpic, alertKind, alertDedup, alertPayload string
			if err := tx.QueryRowContext(ctx, `SELECT project_id,COALESCE(epic_id,''),kind,dedup_key,payload_json
				FROM control_alerts WHERE id=?`, in.AlertID).
				Scan(&alertProject, &alertEpic, &alertKind, &alertDedup, &alertPayload); err != nil {
				return fmt.Errorf("replayed control alert ingress lost its alert: %w", err)
			}
			if alertProject != in.ProjectID || alertEpic != in.EpicID || alertKind != in.Kind ||
				alertDedup != in.IdempotencyKey || alertPayload != in.PayloadJSON {
				return errors.New("replayed control alert ingress no longer matches immutable alert identity")
			}
			return nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}

		var projectActive int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM projects WHERE id=? AND state='active'`,
			in.ProjectID).Scan(&projectActive); err != nil {
			return err
		}
		if projectActive != 1 {
			return ErrControlAlertIngressProjectUnauthorized
		}
		if in.EpicID != "" {
			var epicOwned int
			if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM epics WHERE id=? AND project_id=?`,
				in.EpicID, in.ProjectID).Scan(&epicOwned); err != nil {
				return err
			}
			if epicOwned != 1 {
				return ErrControlAlertIngressProjectUnauthorized
			}
		}

		stamp := now.UTC().Format(rfc3339)
		var epicRef any
		if in.EpicID != "" {
			epicRef = in.EpicID
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO control_alerts
			(id,project_id,epic_id,kind,dedup_key,payload_json,state,created_at,updated_at)
			VALUES (?,?,?,?,?,?,'pending',?,?)`, in.AlertID, in.ProjectID, epicRef, in.Kind,
			in.IdempotencyKey, in.PayloadJSON, stamp, stamp); err != nil {
			if isUniqueConstraintErr(err) {
				return ErrControlAlertIngressConflict
			}
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO control_alert_ingress_submissions
			(idempotency_key,body_sha256,body,project_id,control_alert_id,envelope_id,envelope_kind,created_at)
			VALUES (?,?,?,?,?,?,?,?)`, in.IdempotencyKey, in.BodySHA256, bytes.Clone(in.Body),
			in.ProjectID, in.AlertID, in.AlertID, in.Kind, stamp); err != nil {
			if isUniqueConstraintErr(err) {
				return ErrControlAlertIngressConflict
			}
			return err
		}
		return nil
	})
}
