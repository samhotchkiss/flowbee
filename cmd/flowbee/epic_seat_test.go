package main

import (
	"context"
	"testing"
	"time"

	"github.com/samhotchkiss/flowbee/internal/acctprobe"
	"github.com/samhotchkiss/flowbee/internal/store"
	"github.com/samhotchkiss/flowbee/internal/testutil"
)

// TestEpicSelectSeat proves the Phase 6b limits-aware launch gate (plan §15.13c / §4.3):
// a ready seat with weekly HEADROOM is chosen; a seat whose account is weekly-CRITICAL
// (read off account_windows.severity, NOT just worker_accounts.usage_pct) is refused; no
// ready seat is refused; and anti-collocation prefers a non-busy account.
func TestEpicSelectSeat(t *testing.T) {
	ctx := context.Background()
	now := time.Now()

	addSeat := func(st *store.Store, box, acct string) {
		t.Helper()
		if err := st.AddSeat(ctx, store.Seat{
			Box: box, AgentFamily: "claude", ConfigDir: "/home/u/.claude-" + box,
			AccountKey: acct, Health: store.SeatReady,
		}, now); err != nil {
			t.Fatalf("add seat %s/%s: %v", box, acct, err)
		}
	}
	foldCritical := func(st *store.Store, acct string) {
		t.Helper()
		if err := st.UpsertAccountLimits(ctx, acctprobe.Result{
			Identity:   acctprobe.Identity{Provider: "claude", AccountKey: acct, Email: acct + "@x"},
			Usage:      acctprobe.Usage{Windows: acctprobe.Windows{{Kind: acctprobe.KindWeeklyAll, Percent: 97, Severity: acctprobe.SeverityCritical}}},
			TrustState: acctprobe.TrustVerified, CapturedAt: now,
		}, now); err != nil {
			t.Fatalf("fold critical %s: %v", acct, err)
		}
	}

	t.Run("ready seat with headroom is chosen", func(t *testing.T) {
		st := testutil.NewStore(t)
		addSeat(st, "box1", "acct1")
		seat, gate, err := epicSelectSeat(ctx, st, "claude", "", nil)
		if err != nil {
			t.Fatalf("select: %v", err)
		}
		if gate.refuse {
			t.Fatalf("expected a headroom seat, refused: %s", gate.reason)
		}
		if seat.AccountKey != "acct1" {
			t.Fatalf("chose %q, want acct1", seat.AccountKey)
		}
	})

	t.Run("no ready seat is refused", func(t *testing.T) {
		st := testutil.NewStore(t)
		_, gate, err := epicSelectSeat(ctx, st, "claude", "", nil)
		if err != nil {
			t.Fatalf("select: %v", err)
		}
		if !gate.refuse {
			t.Fatalf("expected a refusal with no seats")
		}
	})

	t.Run("weekly-critical account is refused (severity, not usage_pct)", func(t *testing.T) {
		st := testutil.NewStore(t)
		addSeat(st, "box1", "acctC")
		foldCritical(st, "acctC")
		_, gate, err := epicSelectSeat(ctx, st, "claude", "", nil)
		if err != nil {
			t.Fatalf("select: %v", err)
		}
		if !gate.refuse {
			t.Fatalf("expected a refusal for a weekly-critical account, got a seat")
		}
	})

	t.Run("anti-collocation prefers a non-busy account", func(t *testing.T) {
		st := testutil.NewStore(t)
		addSeat(st, "box1", "acctBusy")
		addSeat(st, "box2", "acctFree")
		active := []store.EpicRun{{ID: "e1", AccountKey: "acctBusy", State: "running"}}
		seat, gate, err := epicSelectSeat(ctx, st, "claude", "", active)
		if err != nil {
			t.Fatalf("select: %v", err)
		}
		if gate.refuse {
			t.Fatalf("expected a seat, refused: %s", gate.reason)
		}
		if seat.AccountKey != "acctFree" {
			t.Fatalf("anti-collocation chose %q, want acctFree", seat.AccountKey)
		}
	})

	t.Run("host filter restricts to a box", func(t *testing.T) {
		st := testutil.NewStore(t)
		addSeat(st, "box1", "acct1")
		addSeat(st, "box2", "acct2")
		seat, gate, err := epicSelectSeat(ctx, st, "claude", "box2", nil)
		if err != nil {
			t.Fatalf("select: %v", err)
		}
		if gate.refuse || seat.Box != "box2" {
			t.Fatalf("host filter chose %+v (refuse=%v), want box2", seat, gate.refuse)
		}
	})
}
