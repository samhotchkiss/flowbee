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
//
// Seats model the REAL fleet (per coordinator intel): a `box` is a full ssh destination
// in user@host form (e.g. "claude1@localhost") — same host, different unix user — which
// the ladder passes verbatim to `ssh -t -- <box>`, so the gate must carry it through
// unchanged.
func TestEpicSelectSeat(t *testing.T) {
	ctx := context.Background()
	now := time.Now()

	addSeat := func(st *store.Store, box, acct string) {
		t.Helper()
		if err := st.AddSeat(ctx, store.Seat{
			Box: box, AgentFamily: "claude", ConfigDir: "/home/" + box + "/.claude",
			AccountKey: acct, Health: store.SeatReady,
		}, now); err != nil {
			t.Fatalf("add seat %s/%s: %v", box, acct, err)
		}
	}
	// foldCritical folds an account reading whose ONLY critical window is `kind` — so a
	// KindWeeklyScoped case proves the 6a concern: an account with a per-model
	// weekly_scoped limit at 100% is severity=critical even though its aggregate
	// worker_accounts.usage_pct (max of session/weekly_all) stays low.
	foldCritical := func(st *store.Store, acct string, kind acctprobe.WindowKind) {
		t.Helper()
		if err := st.UpsertAccountLimits(ctx, acctprobe.Result{
			Identity:   acctprobe.Identity{Provider: "claude", AccountKey: acct, Email: acct + "@x"},
			Usage:      acctprobe.Usage{Windows: acctprobe.Windows{{Kind: kind, Percent: 100, Severity: acctprobe.SeverityCritical, Scope: "fable"}}},
			TrustState: acctprobe.TrustVerified, CapturedAt: now,
		}, now); err != nil {
			t.Fatalf("fold critical %s: %v", acct, err)
		}
	}

	t.Run("ready seat with headroom is chosen", func(t *testing.T) {
		st := testutil.NewStore(t)
		addSeat(st, "claude1@localhost", "pearl@swh.me")
		seat, gate, err := epicSelectSeat(ctx, st, "claude", "", nil)
		if err != nil {
			t.Fatalf("select: %v", err)
		}
		if gate.refuse {
			t.Fatalf("expected a headroom seat, refused: %s", gate.reason)
		}
		if seat.AccountKey != "pearl@swh.me" || seat.Box != "claude1@localhost" {
			t.Fatalf("chose %+v, want claude1@localhost / pearl@swh.me", seat)
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
		addSeat(st, "claude1@localhost", "pearl@swh.me")
		foldCritical(st, "pearl@swh.me", acctprobe.KindWeeklyAll)
		_, gate, err := epicSelectSeat(ctx, st, "claude", "", nil)
		if err != nil {
			t.Fatalf("select: %v", err)
		}
		if !gate.refuse {
			t.Fatalf("expected a refusal for a weekly-critical account, got a seat")
		}
	})

	t.Run("weekly_SCOPED-only critical is refused (usage_pct alone would miss it)", func(t *testing.T) {
		st := testutil.NewStore(t)
		addSeat(st, "claude1@localhost", "pearl@swh.me")
		foldCritical(st, "pearl@swh.me", acctprobe.KindWeeklyScoped)
		// worker_accounts.usage_pct is low (no session/weekly_all window), yet the seat
		// must be excluded because account_windows.severity is critical.
		if aw, ok, _ := st.GetAccountWindow(ctx, "pearl@swh.me"); !ok || aw.Severity != "critical" {
			t.Fatalf("precondition: want severity=critical, got %+v (ok=%v)", aw, ok)
		}
		_, gate, err := epicSelectSeat(ctx, st, "claude", "", nil)
		if err != nil {
			t.Fatalf("select: %v", err)
		}
		if !gate.refuse {
			t.Fatalf("a weekly_scoped-only critical seat must be excluded (severity, not usage_pct)")
		}
	})

	t.Run("anti-collocation prefers a non-busy account", func(t *testing.T) {
		st := testutil.NewStore(t)
		addSeat(st, "claude1@localhost", "pearl@swh.me")
		addSeat(st, "claude2@localhost", "s@swh.me")
		active := []store.EpicRun{{ID: "e1", AccountKey: "pearl@swh.me", State: "running"}}
		seat, gate, err := epicSelectSeat(ctx, st, "claude", "", active)
		if err != nil {
			t.Fatalf("select: %v", err)
		}
		if gate.refuse {
			t.Fatalf("expected a seat, refused: %s", gate.reason)
		}
		if seat.AccountKey != "s@swh.me" {
			t.Fatalf("anti-collocation chose %q, want s@swh.me", seat.AccountKey)
		}
	})

	t.Run("host filter restricts to a box", func(t *testing.T) {
		st := testutil.NewStore(t)
		addSeat(st, "claude1@localhost", "pearl@swh.me")
		addSeat(st, "claude2@localhost", "s@swh.me")
		seat, gate, err := epicSelectSeat(ctx, st, "claude", "claude2@localhost", nil)
		if err != nil {
			t.Fatalf("select: %v", err)
		}
		if gate.refuse || seat.Box != "claude2@localhost" {
			t.Fatalf("host filter chose %+v (refuse=%v), want claude2@localhost", seat, gate.refuse)
		}
	})
}
