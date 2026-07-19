package main

import (
	"context"
	"testing"
	"time"

	"github.com/samhotchkiss/flowbee/internal/acctprobe"
	"github.com/samhotchkiss/flowbee/internal/capacity"
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

	// addCapSeat registers a ready seat of the given family with an explicit per-seat
	// concurrency cap — the input to the 2-concurrent-epics-per-seat placement tests.
	addCapSeat := func(st *store.Store, box, acct, family string, cap int) {
		t.Helper()
		seat := store.Seat{Box: box, AgentFamily: family, AccountKey: acct, Health: store.SeatReady, MaxConcurrent: cap}
		if family == "codex" {
			seat.CodexHome = "/home/" + box + "/.codex"
		} else {
			seat.ConfigDir = "/home/" + box + "/." + family
		}
		if err := st.AddSeat(ctx, seat, now); err != nil {
			t.Fatalf("add cap seat %s/%s: %v", box, acct, err)
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

	t.Run("force quota still returns a real capacity-bound seat", func(t *testing.T) {
		st := testutil.NewStore(t)
		addCapSeat(st, "claude1@localhost", "acct-critical", "claude", 1)
		foldCritical(st, "acct-critical", acctprobe.KindWeeklyAll)
		registered, err := st.ListSeats(ctx)
		if err != nil || len(registered) != 1 {
			t.Fatalf("list registered critical seat: seats=%+v err=%v", registered, err)
		}
		if err := st.UpdateSeatHealth(ctx, registered[0].ID, store.SeatLimitCritical, "weekly cap", now); err != nil {
			t.Fatalf("mark seat quota-limited: %v", err)
		}
		seat, gate, err := epicSelectSeatWithQuotaOverride(ctx, st, "claude", "", nil, true)
		if err != nil {
			t.Fatalf("select with override: %v", err)
		}
		if gate.refuse || gate.hardNoSeat || seat.ID == "" || seat.AccountKey != "acct-critical" {
			t.Fatalf("override must choose the registered critical seat, got seat=%+v gate=%+v", seat, gate)
		}
		if gate.warning == "" {
			t.Fatalf("override must surface its quota risk")
		}
		active := []store.EpicRun{{ID: "e1", Host: seat.Box, SeatID: seat.ID, AccountKey: seat.AccountKey, State: "running"}}
		_, full, err := epicSelectSeatWithQuotaOverride(ctx, st, "claude", "", active, true)
		if err != nil {
			t.Fatalf("select full override: %v", err)
		}
		if !full.refuse || !full.hardNoSeat {
			t.Fatalf("--force-quota must never bypass exact-seat capacity, got %+v", full)
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

	// ── 2-concurrent-epics-per-seat: capacity-aware placement (0029) ──

	t.Run("a cap-2 seat keeps capacity with one active epic", func(t *testing.T) {
		st := testutil.NewStore(t)
		addCapSeat(st, "codex1@localhost", "acc-c1", "codex", 2)
		// one active epic already on this box+account (boxLoad=1, below the cap of 2).
		active := []store.EpicRun{{ID: "e1", Host: "codex1@localhost", AccountKey: "acc-c1", State: "running"}}
		seat, gate, err := epicSelectSeat(ctx, st, "codex", "", active)
		if err != nil {
			t.Fatalf("select: %v", err)
		}
		// the box still has a free slot, so the seat is placed (a cap-1 box would be refused
		// here — see the next subtest). The seat's account already powers e1, so it comes
		// back as a collocated pick with a warning, not a hard refusal.
		if gate.refuse || seat.Box != "codex1@localhost" {
			t.Fatalf("a cap-2 seat with one epic must still place (got refuse=%v seat=%+v)", gate.refuse, seat)
		}
	})

	t.Run("a cap-1 seat with an active epic is refused", func(t *testing.T) {
		st := testutil.NewStore(t)
		addCapSeat(st, "codex1@localhost", "acc-c1", "codex", 1)
		active := []store.EpicRun{{ID: "e1", Host: "codex1@localhost", AccountKey: "acc-c1", State: "running"}}
		_, gate, err := epicSelectSeat(ctx, st, "codex", "", active)
		if err != nil {
			t.Fatalf("select: %v", err)
		}
		if !gate.refuse || !gate.hardNoSeat {
			t.Fatalf("a cap-1 box already holding an epic must hard-refuse (got %+v)", gate)
		}
	})

	t.Run("a second cap-1 seat on the same host remains eligible", func(t *testing.T) {
		st := testutil.NewStore(t)
		first := store.Seat{Box: "shared-host", AgentFamily: "codex", CodexHome: "/cfg/codex1", AccountKey: "acc-1", Health: store.SeatReady, MaxConcurrent: 1}
		second := store.Seat{Box: "shared-host", AgentFamily: "codex", CodexHome: "/cfg/codex2", AccountKey: "acc-2", Health: store.SeatReady, MaxConcurrent: 1}
		for _, seat := range []store.Seat{first, second} {
			if err := st.AddSeat(ctx, seat, now); err != nil {
				t.Fatalf("add seat: %v", err)
			}
		}
		active := []store.EpicRun{{
			ID: "e1", Host: "shared-host", SeatID: first.ComposeID(), AccountKey: "acc-1", State: "running",
		}}
		seat, gate, err := epicSelectSeat(ctx, st, "codex", "shared-host", active)
		if err != nil {
			t.Fatalf("select: %v", err)
		}
		if gate.refuse || seat.ID != second.ComposeID() {
			t.Fatalf("distinct seat on same host should provide throughput; got refuse=%v seat=%+v", gate.refuse, seat)
		}
	})

	t.Run("least-loaded box is preferred (spread load)", func(t *testing.T) {
		st := testutil.NewStore(t)
		addCapSeat(st, "codexA@localhost", "acc-A", "codex", 2)
		addCapSeat(st, "codexB@localhost", "acc-B", "codex", 2)
		// an active epic on codexA under a THIRD account, so NEITHER seat is anti-collocated
		// — this isolates the least-loaded-box preference from anti-collocation. boxLoad:
		// codexA=1, codexB=0, both under cap 2, so both land in the headroom bucket.
		active := []store.EpicRun{{ID: "e1", Host: "codexA@localhost", AccountKey: "acc-external", State: "running"}}
		seat, gate, err := epicSelectSeat(ctx, st, "codex", "", active)
		if err != nil {
			t.Fatalf("select: %v", err)
		}
		if gate.refuse || seat.Box != "codexB@localhost" {
			t.Fatalf("least-loaded should pick the empty box codexB, got refuse=%v seat=%+v", gate.refuse, seat)
		}
	})

	t.Run("every seat at cap is a hard refusal", func(t *testing.T) {
		st := testutil.NewStore(t)
		addCapSeat(st, "codexA@localhost", "acc-A", "codex", 1)
		addCapSeat(st, "codexB@localhost", "acc-B", "codex", 1)
		active := []store.EpicRun{
			{ID: "e1", Host: "codexA@localhost", AccountKey: "acc-A", State: "running"},
			{ID: "e2", Host: "codexB@localhost", AccountKey: "acc-B", State: "running"},
		}
		_, gate, err := epicSelectSeat(ctx, st, "codex", "", active)
		if err != nil {
			t.Fatalf("select: %v", err)
		}
		if !gate.refuse || !gate.hardNoSeat {
			t.Fatalf("all boxes at cap must hard-refuse (--force-quota must not conjure a slot), got %+v", gate)
		}
	})

	t.Run("v2 builder selector fails closed then uses the active generation", func(t *testing.T) {
		st := testutil.NewStore(t)
		st.EnableCapacityV2 = true
		addCapSeat(st, "codex-v2@localhost", "account-v2", "codex", 2)
		seats, err := st.ListSeats(ctx)
		if err != nil || len(seats) != 1 {
			t.Fatalf("seats=%+v err=%v", seats, err)
		}
		seat := seats[0]
		if err := st.BindCapacitySeatIdentity(ctx, store.CapacitySeatIdentity{
			SeatID: seat.ID, HostID: seat.Box, AccountKey: seat.AccountKey,
			CredentialLineage: "lineage-v2", ReservePct: 10, AccountMaximum: 3,
		}, now); err != nil {
			t.Fatal(err)
		}
		if _, gate, err := epicSelectSeat(ctx, st, "codex", "", nil); err != nil || !gate.refuse {
			t.Fatalf("missing active generation must refuse: gate=%+v err=%v", gate, err)
		}
		observed := time.Now().UTC()
		if err := st.CommitCapacityGeneration(ctx, store.CapacityGeneration{
			ID: "builder-generation", StartedAt: observed,
			ExpectedSeatIDs: []string{seat.ID},
			Observations: []store.CapacitySeatObservation{{
				ObservationID: "builder-observation", SeatID: seat.ID, HostID: seat.Box,
				Provider: "codex", AccountKey: seat.AccountKey, CredentialLineage: "lineage-v2",
				CollectorID: "collector-v2", Source: "live_app_server", TrustState: "verified",
				IntegrityState: "verified", FetchedAt: observed, AdapterVersion: "codex-live/v1",
				Windows: []capacity.RouteWindow{{Kind: "weekly", Applicable: true, Known: true, Percent: 20}},
			}},
		}, observed); err != nil {
			t.Fatal(err)
		}
		got, gate, err := epicSelectSeat(ctx, st, "codex", "", nil)
		if err != nil || gate.refuse || got.ID != seat.ID {
			t.Fatalf("active generation selection seat=%+v gate=%+v err=%v", got, gate, err)
		}
	})
}
