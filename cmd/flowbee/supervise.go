package main

import (
	"context"
	"log/slog"
	"time"

	"github.com/samhotchkiss/flowbee/internal/store"
)

// foldSeatCapacity is the staggered capacity/seat fold body (epic-lane Phase 6b, plan
// §12.2 + §15.13d): probe each registered seat (acctprobe over ssh, via probeSeatDir), fold
// its real 5h/7d% into account_windows (UpsertAccountLimits, respecting acctprobe's trust
// semantics), back-fill the seat's resolved account_key, and refresh its health — the truth
// the launch gate + usage_critical producer read. Silent, per-seat error-isolated (a wedged
// box must never blind the fold for every OTHER seat), and identical to `flowbee seat probe`
// minus the tabular output.
func foldSeatCapacity(ctx context.Context, logger *slog.Logger, st *store.Store, now time.Time) {
	// v2 has exactly one projection writer: the authenticated live collector builds
	// a complete generation and CommitCapacityGeneration advances its pointer. The
	// legacy seat-by-seat UpsertAccountLimits fold is order-dependent and must remain
	// read-only/off while fail-closed routing is enabled.
	if st.EnableCapacityV2 {
		logger.Debug("legacy capacity fold disabled under capacity routing v2")
		return
	}
	seats, err := st.ListSeats(ctx)
	if err != nil {
		logger.Warn("capacity fold: list seats", "err", err)
		return
	}
	for _, s := range seats {
		if !s.Enabled {
			continue
		}
		res, perr := probeSeatDir(ctx, s)
		health, detail := classifySeatHealth(res, perr)
		if res != nil && res.Identity.AccountKey != "" {
			if uerr := st.UpsertAccountLimits(ctx, *res, now); uerr != nil {
				detail = "fold failed: " + uerr.Error()
			}
			if s.AccountKey == "" {
				_ = st.SetSeatAccountKey(ctx, s.ID, res.Identity.AccountKey, now)
			}
		}
		if uerr := st.UpdateSeatHealth(ctx, s.ID, health, detail, now); uerr != nil {
			logger.Warn("capacity fold: update seat health", "seat", s.ID, "err", uerr)
		}
	}
}
