package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"strings"
	"time"
)

type ExternalWatchdogHeartbeatInput struct {
	IdempotencyKey string
	BodySHA256     string
	Body           []byte
	EnvelopeID     string
	ProjectID      string
	WatchdogID     string
	Target         string
	Sequence       int64
	ObservedAt     time.Time
}

type ExternalWatchdogLease struct {
	ProjectID      string
	WatchdogID     string
	Target         string
	LastSequence   int64
	LastObservedAt time.Time
	LastReceivedAt time.Time
	IdempotencyKey string
}

// AcceptExternalWatchdogHeartbeat commits an exact signed-ingress replay
// binding and advances the one project-bound watchdog lease atomically. It does
// not create a control_alert: heartbeat is readiness evidence, not a human
// notification.
func (s *Store) AcceptExternalWatchdogHeartbeat(ctx context.Context,
	in ExternalWatchdogHeartbeatInput, receivedAt time.Time) error {
	in.IdempotencyKey = strings.TrimSpace(in.IdempotencyKey)
	in.BodySHA256 = strings.TrimSpace(in.BodySHA256)
	in.EnvelopeID = strings.TrimSpace(in.EnvelopeID)
	in.ProjectID = strings.TrimSpace(in.ProjectID)
	in.WatchdogID = strings.TrimSpace(in.WatchdogID)
	in.Target = strings.TrimSpace(in.Target)
	if in.IdempotencyKey == "" || len(in.IdempotencyKey) > 512 || in.EnvelopeID == "" ||
		in.ProjectID == "" || in.WatchdogID == "" || in.Target == "" || in.Sequence < 1 ||
		in.ObservedAt.IsZero() || len(in.Body) == 0 {
		return errors.New("external watchdog heartbeat is incomplete")
	}
	digest := sha256.Sum256(in.Body)
	if in.BodySHA256 != hex.EncodeToString(digest[:]) {
		return errors.New("external watchdog heartbeat body sha256 does not match exact bytes")
	}
	return s.tx(ctx, func(tx *sql.Tx) error {
		var bodySHA, envelopeID, projectID, watchdogID, target string
		var body []byte
		var sequence int64
		err := tx.QueryRowContext(ctx, `SELECT body_sha256,body,envelope_id,project_id,watchdog_id,target,sequence
			FROM external_watchdog_heartbeat_submissions WHERE idempotency_key=?`, in.IdempotencyKey).
			Scan(&bodySHA, &body, &envelopeID, &projectID, &watchdogID, &target, &sequence)
		if err == nil {
			if bodySHA != in.BodySHA256 || !bytes.Equal(body, in.Body) || envelopeID != in.EnvelopeID ||
				projectID != in.ProjectID || watchdogID != in.WatchdogID || target != in.Target || sequence != in.Sequence {
				return ErrControlAlertIngressConflict
			}
			return nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		var active int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM projects WHERE id=? AND state='active'`,
			in.ProjectID).Scan(&active); err != nil {
			return err
		}
		if active != 1 {
			return ErrControlAlertIngressProjectUnauthorized
		}
		var leaseWatchdog, leaseTarget string
		var lastSequence int64
		err = tx.QueryRowContext(ctx, `SELECT watchdog_id,target,last_sequence FROM external_watchdog_leases
			WHERE project_id=?`, in.ProjectID).Scan(&leaseWatchdog, &leaseTarget, &lastSequence)
		if err == nil {
			if leaseWatchdog != in.WatchdogID || leaseTarget != in.Target || in.Sequence <= lastSequence {
				return ErrControlAlertIngressConflict
			}
		} else if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		observed := in.ObservedAt.UTC().Format(rfc3339)
		received := receivedAt.UTC().Format(rfc3339)
		if _, err := tx.ExecContext(ctx, `INSERT INTO external_watchdog_heartbeat_submissions
			(idempotency_key,body_sha256,body,envelope_id,project_id,watchdog_id,target,sequence,observed_at,received_at)
			VALUES (?,?,?,?,?,?,?,?,?,?)`, in.IdempotencyKey, in.BodySHA256, bytes.Clone(in.Body),
			in.EnvelopeID, in.ProjectID, in.WatchdogID, in.Target, in.Sequence, observed, received); err != nil {
			if isUniqueConstraintErr(err) {
				return ErrControlAlertIngressConflict
			}
			return err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO external_watchdog_leases
			(project_id,watchdog_id,target,last_sequence,last_observed_at,last_received_at,last_idempotency_key)
			VALUES (?,?,?,?,?,?,?)
			ON CONFLICT(project_id) DO UPDATE SET last_sequence=excluded.last_sequence,
			last_observed_at=excluded.last_observed_at,last_received_at=excluded.last_received_at,
			last_idempotency_key=excluded.last_idempotency_key`, in.ProjectID, in.WatchdogID, in.Target,
			in.Sequence, observed, received, in.IdempotencyKey)
		return err
	})
}

func (s *Store) ExternalWatchdogLease(ctx context.Context, projectID string) (ExternalWatchdogLease, error) {
	var lease ExternalWatchdogLease
	var observed, received string
	err := s.DB.QueryRowContext(ctx, `SELECT project_id,watchdog_id,target,last_sequence,
		last_observed_at,last_received_at,last_idempotency_key FROM external_watchdog_leases WHERE project_id=?`,
		projectID).Scan(&lease.ProjectID, &lease.WatchdogID, &lease.Target, &lease.LastSequence,
		&observed, &received, &lease.IdempotencyKey)
	if err != nil {
		return ExternalWatchdogLease{}, err
	}
	lease.LastObservedAt, _ = time.Parse(rfc3339, observed)
	lease.LastReceivedAt, _ = time.Parse(rfc3339, received)
	return lease, nil
}

func (s *Store) ExternalWatchdogLeaseFresh(ctx context.Context, projectID, watchdogID string,
	now time.Time, freshFor time.Duration) (ExternalWatchdogLease, bool, error) {
	lease, err := s.ExternalWatchdogLease(ctx, projectID)
	if errors.Is(err, sql.ErrNoRows) {
		return ExternalWatchdogLease{}, false, nil
	}
	if err != nil {
		return ExternalWatchdogLease{}, false, err
	}
	age := now.Sub(lease.LastReceivedAt)
	fresh := lease.ProjectID == projectID && lease.WatchdogID == watchdogID && freshFor > 0 &&
		!lease.LastReceivedAt.IsZero() && age >= 0 && age <= freshFor
	return lease, fresh, nil
}
