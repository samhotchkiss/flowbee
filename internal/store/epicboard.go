package store

import (
	"context"
	"time"

	"github.com/samhotchkiss/flowbee/internal/epicdigest"
	"github.com/samhotchkiss/flowbee/internal/ledger"
)

// epicboard.go is the READ seam the epic-lane digest/summary endpoints (Phase 6b,
// plan §2.1 + §15.16) assemble from. It carries no new table — digest_seq is derived
// from the observable-state rows' timestamps (so it is migration-free and bumps on any
// write) and recent_interventions reads the ledger the resolve path already writes.

// EpicDigestSeq is the monotonic digest sequence the §2.1 digest / §15.16c summary
// carry and the ETag/304 poll keys on: it BUMPS whenever any observable epic-lane state
// changes and is STABLE otherwise, so a poll that finds it unchanged returns 304 ("sleep",
// plan §2.1). It is derived — NOT a stored counter — as the newest updated_at (in unix
// milliseconds) across every table whose change is observable in the board: epics,
// attention_items, supervisors, account_windows, seats, wip_markers. Every store write in
// those tables stamps updated_at/reported_at, so any observable mutation advances the seq.
// Timestamps are parsed in Go (RFC3339Nano does not sort lexically across the fractional-
// second boundary — the same reason ReapExpiredLeases parses rather than string-compares).
func (s *Store) EpicDigestSeq(ctx context.Context) (int64, error) {
	rows, err := s.DB.QueryContext(ctx, `
		SELECT COALESCE(MAX(updated_at),'') FROM epics
		UNION ALL SELECT COALESCE(MAX(updated_at),'') FROM attention_items
		UNION ALL SELECT COALESCE(MAX(updated_at),'') FROM supervisors
		UNION ALL SELECT COALESCE(MAX(reported_at),'') FROM account_windows
		UNION ALL SELECT COALESCE(MAX(updated_at),'') FROM seats
		UNION ALL SELECT COALESCE(MAX(updated_at),'') FROM wip_markers
		UNION ALL SELECT COALESCE(MAX(updated_at),'') FROM epic_deliveries
		UNION ALL SELECT COALESCE(MAX(updated_at),'') FROM epic_artifacts
		UNION ALL SELECT COALESCE(MAX(updated_at),'') FROM epic_actions`)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	var best int64
	for rows.Next() {
		var ts string
		if err := rows.Scan(&ts); err != nil {
			return 0, err
		}
		if ts == "" {
			continue
		}
		t, perr := time.Parse(rfc3339, ts)
		if perr != nil {
			continue // an unparseable stamp reads as "no signal", never a panic
		}
		if ms := t.UnixMilli(); ms > best {
			best = ms
		}
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	if err := rows.Close(); err != nil {
		return 0, err
	}
	var controlSeq int64
	if err := s.DB.QueryRowContext(ctx, `SELECT COALESCE(MAX(seq),0) FROM control_events`).Scan(&controlSeq); err != nil {
		return 0, err
	}
	if controlSeq > best {
		best = controlSeq
	}
	return best, nil
}

// ParseTimeOrZero parses an RFC3339Nano store timestamp, returning the zero time on any
// error (an unparseable/empty stamp reads as "unknown", never a panic). Exported so the
// api digest-assembly seam can turn stored timestamps into the ages the pure epicdigest
// core computes, without re-implementing the store's timestamp format.
func ParseTimeOrZero(s string) time.Time { return parseStoreTime(s) }

// RecentInterventions returns the last ≤max ledgered master steers on an epic (plan
// §15.11a) so a post-`/clear` master neither repeats nor contradicts its own prior steer.
// Read from the epic's "att:"-prefixed ledger stream (the resolve path writes
// KindEpicIntervention there) — NO new table. Ordered oldest→newest (the ledger's own
// order), clamped to the freshest max by the caller/epicdigest. The summary is the
// recorded verdict/reason (never raw pane text — the ledger never stored the payload body).
func (s *Store) RecentInterventions(ctx context.Context, epicID string, max int) ([]epicdigest.Intervention, error) {
	if epicID == "" {
		return nil, nil
	}
	events, err := s.LoadEvents(ctx, attnLedgerPrefix+epicID)
	if err != nil {
		return nil, err
	}
	var out []epicdigest.Intervention
	for _, e := range events {
		if e.Kind != ledger.KindEpicIntervention {
			continue
		}
		out = append(out, epicdigest.Intervention{
			Actor:   e.Actor,
			At:      e.CreatedAt,
			Summary: e.Payload.RevokeReason,
		})
	}
	if max > 0 && len(out) > max {
		out = out[len(out)-max:]
	}
	return out, nil
}
