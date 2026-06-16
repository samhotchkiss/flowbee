package store

import (
	"context"
	"database/sql"
	"errors"
	"time"

	gh "github.com/samhotchkiss/flowbee/internal/github"
)

// RecordDelivery writes a webhook to the durable inbox BEFORE it acts (I-2,
// §8.1.3). It is the dedupe point: a delivery id already present means a
// replayed/forged-with-same-id delivery — fresh=false, and the caller must NOT
// act on it. A new id is recorded 'pending' (crash-replay safe) and fresh=true.
// The write-ahead is the whole point: if the process crashes after recording but
// before the refetch, the pending row is reprocessable on boot.
func (s *Store) RecordDelivery(ctx context.Context, deliveryID, event string, prNumber int) (fresh bool, err error) {
	err = s.tx(ctx, func(tx *sql.Tx) error {
		var exists int
		if e := tx.QueryRowContext(ctx,
			`SELECT EXISTS(SELECT 1 FROM webhook_inbox WHERE delivery_id = ?)`, deliveryID).
			Scan(&exists); e != nil {
			return e
		}
		if exists == 1 {
			fresh = false
			return nil
		}
		var pr any
		if prNumber > 0 {
			pr = prNumber
		}
		if _, e := tx.ExecContext(ctx, `
			INSERT INTO webhook_inbox (delivery_id, event, pr_number, status)
			VALUES (?, ?, ?, 'pending')`, deliveryID, event, pr); e != nil {
			return e
		}
		// advance the delivery high-water-mark (gap detection, §8.1.4).
		if _, e := tx.ExecContext(ctx,
			`UPDATE reconcile_state SET last_delivery_id = ? WHERE id = 1`, deliveryID); e != nil {
			return e
		}
		fresh = true
		return nil
	})
	return fresh, err
}

// MarkDeliveryProcessed flips an inbox row to processed once its targeted refetch
// completed (crash-replay: only 'pending' rows are reprocessed on boot).
func (s *Store) MarkDeliveryProcessed(ctx context.Context, deliveryID string) error {
	_, err := s.DB.ExecContext(ctx,
		`UPDATE webhook_inbox SET status = 'processed' WHERE delivery_id = ?`, deliveryID)
	return err
}

// PendingDeliveries returns inbox rows still 'pending' (crash-replay on boot).
func (s *Store) PendingDeliveries(ctx context.Context) ([]PendingDelivery, error) {
	rows, err := s.DB.QueryContext(ctx,
		`SELECT delivery_id, event, COALESCE(pr_number, 0) FROM webhook_inbox
		  WHERE status = 'pending' ORDER BY received_at ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PendingDelivery
	for rows.Next() {
		var d PendingDelivery
		if err := rows.Scan(&d.DeliveryID, &d.Event, &d.PRNumber); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// PendingDelivery is one un-processed inbox row.
type PendingDelivery struct {
	DeliveryID string
	Event      string
	PRNumber   int
}

// DeliverySeen reports whether a delivery id is already in the inbox (test helper
// + gap-detection probe).
func (s *Store) DeliverySeen(ctx context.Context, deliveryID string) (bool, error) {
	var n int
	err := s.DB.QueryRowContext(ctx,
		`SELECT EXISTS(SELECT 1 FROM webhook_inbox WHERE delivery_id = ?)`, deliveryID).Scan(&n)
	return n == 1, err
}

// RecordRateLimit stores the single installation token's budget gauge from a
// sweep (I-14, §12.6). One bucket to watch.
func (s *Store) RecordRateLimit(ctx context.Context, r gh.RateLimit, sweptAt time.Time) error {
	reset := ""
	if !r.ResetAt.IsZero() {
		reset = r.ResetAt.Format(rfc3339)
	}
	_, err := s.DB.ExecContext(ctx, `
		UPDATE reconcile_state
		   SET rate_limit_remaining = ?, rate_limit_limit = ?,
		       rate_limit_reset_at = ?, last_sweep_at = ?
		 WHERE id = 1`,
		r.Remaining, r.Limit, reset, sweptAt.Format(rfc3339))
	return err
}

// RateLimitGauge is the live identity-budget gauge (I-14).
type RateLimitGauge struct {
	Remaining int       `json:"remaining"`
	Limit     int       `json:"limit"`
	ResetAt   time.Time `json:"reset_at"`
	LastSweep time.Time `json:"last_sweep_at"`
}

// RateLimit reads the current identity-budget gauge.
func (s *Store) RateLimit(ctx context.Context) (RateLimitGauge, error) {
	var g RateLimitGauge
	var reset, swept string
	err := s.DB.QueryRowContext(ctx, `
		SELECT rate_limit_remaining, rate_limit_limit, rate_limit_reset_at, last_sweep_at
		  FROM reconcile_state WHERE id = 1`).Scan(&g.Remaining, &g.Limit, &reset, &swept)
	if errors.Is(err, sql.ErrNoRows) {
		return g, nil
	}
	if err != nil {
		return g, err
	}
	if reset != "" {
		if ts, perr := time.Parse(rfc3339, reset); perr == nil {
			g.ResetAt = ts
		}
	}
	if swept != "" {
		if ts, perr := time.Parse(rfc3339, swept); perr == nil {
			g.LastSweep = ts
		}
	}
	return g, nil
}
