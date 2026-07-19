package capacitycollector

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"errors"
	"math"
	"time"
)

// SQLBackoffStore persists provider/account protection independently of an active
// capacity generation. Restarting the control plane therefore cannot erase a 429 or
// turn a provider outage into an immediate probe storm.
type SQLBackoffStore struct{ DB *sql.DB }

func (s SQLBackoffStore) Get(ctx context.Context, kind, key string) (BackoffState, error) {
	var out BackoffState
	var retry string
	err := s.DB.QueryRowContext(ctx, `SELECT consecutive_failures,retry_at,last_reason
		FROM capacity_probe_backoff WHERE scope_kind=? AND scope_key=?`, kind, key).
		Scan(&out.Failures, &retry, &out.Reason)
	if errors.Is(err, sql.ErrNoRows) {
		return BackoffState{}, nil
	}
	if err != nil {
		return BackoffState{}, err
	}
	out.RetryAt, _ = time.Parse(time.RFC3339Nano, retry)
	return out, nil
}

func (s SQLBackoffStore) Failure(ctx context.Context, kind, key, reason string, now, retryAt time.Time, base, maximum time.Duration) (BackoffState, error) {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return BackoffState{}, err
	}
	defer func() { _ = tx.Rollback() }()
	var failures int
	err = tx.QueryRowContext(ctx, `SELECT consecutive_failures FROM capacity_probe_backoff
		WHERE scope_kind=? AND scope_key=?`, kind, key).Scan(&failures)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return BackoffState{}, err
	}
	failures++
	exponent := math.Min(float64(failures-1), 12)
	delay := time.Duration(float64(base) * math.Pow(2, exponent))
	// Deterministic bounded jitter (0..20%) spreads a fleet after restart without
	// making tests or persisted policy depend on process-global randomness.
	digest := sha256.Sum256([]byte(kind + "\x00" + key + "\x00" + reason))
	fraction := float64(binary.BigEndian.Uint16(digest[:2])) / 65535.0
	delay += time.Duration(float64(delay) * 0.20 * fraction)
	if delay > maximum {
		delay = maximum
	}
	computed := now.Add(delay)
	if retryAt.After(computed) {
		computed = retryAt
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO capacity_probe_backoff
		(scope_kind,scope_key,consecutive_failures,retry_at,last_reason,last_failure_at,last_success_at,updated_at)
		VALUES (?,?,?,?,? ,?,'',?)
		ON CONFLICT(scope_kind,scope_key) DO UPDATE SET
		consecutive_failures=excluded.consecutive_failures,retry_at=excluded.retry_at,
		last_reason=excluded.last_reason,last_failure_at=excluded.last_failure_at,updated_at=excluded.updated_at`,
		kind, key, failures, computed.UTC().Format(time.RFC3339Nano), reason,
		now.UTC().Format(time.RFC3339Nano), now.UTC().Format(time.RFC3339Nano))
	if err != nil {
		return BackoffState{}, err
	}
	if err := tx.Commit(); err != nil {
		return BackoffState{}, err
	}
	return BackoffState{RetryAt: computed.UTC(), Failures: failures, Reason: reason}, nil
}

func (s SQLBackoffStore) Success(ctx context.Context, kind, key string, now time.Time) error {
	// Provider breakers close gradually: one success removes one failure. Account
	// backoff closes fully because this exact identity just proved live.
	if kind == "provider" {
		_, err := s.DB.ExecContext(ctx, `UPDATE capacity_probe_backoff SET
			consecutive_failures=MAX(consecutive_failures-1,0),retry_at='',last_reason='',
			last_success_at=?,updated_at=? WHERE scope_kind=? AND scope_key=?`,
			now.UTC().Format(time.RFC3339Nano), now.UTC().Format(time.RFC3339Nano), kind, key)
		return err
	}
	_, err := s.DB.ExecContext(ctx, `INSERT INTO capacity_probe_backoff
		(scope_kind,scope_key,consecutive_failures,retry_at,last_reason,last_failure_at,last_success_at,updated_at)
		VALUES (?,?,0,'','','',?,?) ON CONFLICT(scope_kind,scope_key) DO UPDATE SET
		consecutive_failures=0,retry_at='',last_reason='',last_success_at=excluded.last_success_at,
		updated_at=excluded.updated_at`, kind, key, now.UTC().Format(time.RFC3339Nano), now.UTC().Format(time.RFC3339Nano))
	return err
}
