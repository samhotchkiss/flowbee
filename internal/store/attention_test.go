package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/samhotchkiss/flowbee/internal/lease"
	"github.com/samhotchkiss/flowbee/internal/ledger"
	"github.com/samhotchkiss/flowbee/internal/store"
	"github.com/samhotchkiss/flowbee/internal/testutil"
)

var attnT0 = time.Date(2026, 7, 16, 9, 0, 0, 0, time.UTC)

func registerMaster(t *testing.T, st *store.Store, ctx context.Context, label string, now time.Time) store.SupervisorRegistration {
	t.Helper()
	reg, err := st.RegisterSupervisor(ctx, store.Supervisor{Label: label, Kind: "claude", ModelFamily: "claude"}, now)
	if err != nil {
		t.Fatalf("register %q: %v", label, err)
	}
	return reg
}

func upsertItem(t *testing.T, st *store.Store, ctx context.Context, item store.AttentionItem, now time.Time) (bool, string) {
	t.Helper()
	created, id, err := st.UpsertAttentionItem(ctx, item, now)
	if err != nil {
		t.Fatalf("upsert %q: %v", item.ID, err)
	}
	return created, id
}

// TestAttentionDedup: the same active condition never spawns a second row; occurrences
// and evidence refresh instead. A resolved condition that recurs is a NEW item.
func TestAttentionDedup(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()

	created, id := upsertItem(t, st, ctx, store.AttentionItem{
		ID: "att1", Kind: "needs_input", EpicID: "e1", Repo: "russ", Priority: 20,
		DedupKey: "e1:needs_input", Evidence: map[string]string{"pane": "h1"},
	}, attnT0)
	if !created {
		t.Fatalf("first upsert should create")
	}

	created2, id2 := upsertItem(t, st, ctx, store.AttentionItem{
		ID: "att-DIFFERENT", Kind: "needs_input", EpicID: "e1", Repo: "russ", Priority: 20,
		DedupKey: "e1:needs_input", Evidence: map[string]string{"pane": "h2"},
	}, attnT0.Add(time.Minute))
	if created2 {
		t.Fatalf("re-seen active dedup_key must REFRESH, not create a second row")
	}
	if id2 != id {
		t.Fatalf("refresh should return the existing id %q, got %q", id, id2)
	}
	got, err := st.GetAttentionItem(ctx, id)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Occurrences != 2 {
		t.Fatalf("occurrences = %d, want 2", got.Occurrences)
	}
	if got.Evidence["pane"] != "h2" {
		t.Fatalf("evidence not refreshed: %v", got.Evidence)
	}
	open, err := st.ListOpenAttention(ctx, "open", nil, "")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(open) != 1 {
		t.Fatalf("dedup produced %d rows, want exactly 1", len(open))
	}

	// clear the condition, then re-observe: a fresh row is legitimate.
	resolved, err := st.AutoResolveCleared(ctx, "e1:needs_input", attnT0.Add(2*time.Minute))
	if err != nil || !resolved {
		t.Fatalf("AutoResolveCleared: resolved=%v err=%v", resolved, err)
	}
	created3, _ := upsertItem(t, st, ctx, store.AttentionItem{
		ID: "att2", Kind: "needs_input", EpicID: "e1", Repo: "russ", Priority: 20,
		DedupKey: "e1:needs_input",
	}, attnT0.Add(3*time.Minute))
	if !created3 {
		t.Fatalf("a recurred condition after resolve should create a NEW item")
	}
}

// TestAttentionPartialUniqueIndex: the partial UNIQUE index structurally forbids two
// ACTIVE rows with the same dedup_key (a direct insert bypassing UpsertAttentionItem).
func TestAttentionPartialUniqueIndex(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	ins := func(id, state string) error {
		_, err := st.DB.ExecContext(ctx, `
			INSERT INTO attention_items (id, kind, dedup_key, state, created_at, updated_at, first_seen_at, last_seen_at)
			VALUES (?, 'needs_input', 'e1:dup', ?, '', '', '', '')`, id, state)
		return err
	}
	if err := ins("a", "open"); err != nil {
		t.Fatalf("first active insert: %v", err)
	}
	if err := ins("b", "leased"); err == nil {
		t.Fatalf("a second ACTIVE row with the same dedup_key must violate the partial unique index")
	}
	// a resolved row with the same key is fine (the index only covers active states).
	if err := ins("c", "resolved"); err != nil {
		t.Fatalf("a resolved row with the same key should be allowed: %v", err)
	}
}

// TestAttentionLeaseAndDelivery walks the happy path: lease -> begin delivery -> strong
// verdict -> awaiting_ack -> ack -> resolved, and asserts the ledger trail.
func TestAttentionLeaseAndDelivery(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	reg := registerMaster(t, st, ctx, "master-a", attnT0)

	upsertItem(t, st, ctx, store.AttentionItem{
		ID: "att1", Kind: "needs_input", EpicID: "e1", Repo: "russ", Priority: 20, DedupKey: "e1:needs_input",
	}, attnT0)

	leased, err := st.LeaseAttention(ctx, reg.MasterID, reg.Epoch, 5, nil, 5*time.Minute, attnT0.Add(time.Minute))
	if err != nil {
		t.Fatalf("lease: %v", err)
	}
	if len(leased) != 1 || leased[0].State != "leased" || leased[0].ItemEpoch != 1 || leased[0].LeasedBy != "master-a" {
		t.Fatalf("lease result wrong: %+v", leased)
	}

	if err := st.BeginDelivery(ctx, "att1", reg.MasterID, reg.Epoch, 1, "idem-1", attnT0.Add(2*time.Minute)); err != nil {
		t.Fatalf("begin delivery: %v", err)
	}
	if err := st.RecordDeliveryVerdict(ctx, "att1", reg.MasterID, reg.Epoch, 1, "strong", attnT0.Add(3*time.Minute)); err != nil {
		t.Fatalf("record verdict: %v", err)
	}
	got, _ := st.GetAttentionItem(ctx, "att1")
	if got.State != "awaiting_ack" || got.Verdict != "strong" {
		t.Fatalf("after strong verdict: %+v (want awaiting_ack/strong)", got)
	}
	if err := st.AckAttention(ctx, "att1", attnT0.Add(4*time.Minute)); err != nil {
		t.Fatalf("ack: %v", err)
	}
	got, _ = st.GetAttentionItem(ctx, "att1")
	if got.State != "resolved" || got.Resolution != "acked" {
		t.Fatalf("after ack: %+v (want resolved/acked)", got)
	}

	// ledger (keyed on the epic id) carries the intervention timeline.
	evs, err := st.LoadEvents(ctx, "e1")
	if err != nil {
		t.Fatalf("load events: %v", err)
	}
	want := []ledger.EventKind{
		ledger.KindAttentionOpened, ledger.KindAttentionLeased,
		ledger.KindEpicIntervention, ledger.KindAttentionResolved,
	}
	if len(evs) != len(want) {
		t.Fatalf("ledger has %d events, want %d: %+v", len(evs), len(want), evs)
	}
	for i, k := range want {
		if evs[i].Kind != k {
			t.Fatalf("ledger[%d] = %s, want %s", i, evs[i].Kind, k)
		}
	}
	if evs[2].Payload.LeaseID != "att1" || evs[2].Payload.RevokeReason != "strong" {
		t.Fatalf("epic_intervention payload wrong: %+v", evs[2].Payload)
	}
}

// TestFenceRejectsStaleItemEpoch: a fenced call with the wrong item_epoch is rejected
// (409 fenced) while the lease is otherwise live.
func TestFenceRejectsStaleItemEpoch(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	reg := registerMaster(t, st, ctx, "master-a", attnT0)
	upsertItem(t, st, ctx, store.AttentionItem{ID: "att1", Kind: "stalled", EpicID: "e1", Priority: 15, DedupKey: "e1:stalled"}, attnT0)
	if _, err := st.LeaseAttention(ctx, reg.MasterID, reg.Epoch, 5, nil, 5*time.Minute, attnT0.Add(time.Minute)); err != nil {
		t.Fatalf("lease: %v", err)
	}
	// live item_epoch is 1; claim 99.
	err := st.BeginDelivery(ctx, "att1", reg.MasterID, reg.Epoch, 99, "idem", attnT0.Add(2*time.Minute))
	if !errors.Is(err, lease.ErrStaleEpoch) {
		t.Fatalf("stale item_epoch: err = %v, want ErrStaleEpoch", err)
	}
}

// TestFenceRejectsStaleSupervisorEpoch: after a re-registration bumps the master epoch,
// a fenced call carrying the OLD supervisor epoch on a still-delivering item is rejected.
// (This isolates the supervisor-epoch arm of the fence: state/leaseholder/item_epoch all
// still match, only the supervisor epoch is stale.)
func TestFenceRejectsStaleSupervisorEpoch(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	reg := registerMaster(t, st, ctx, "master-a", attnT0) // epoch 1
	upsertItem(t, st, ctx, store.AttentionItem{ID: "att1", Kind: "stalled", EpicID: "e1", Priority: 15, DedupKey: "e1:stalled"}, attnT0)
	if _, err := st.LeaseAttention(ctx, reg.MasterID, 1, 5, nil, 5*time.Minute, attnT0.Add(time.Minute)); err != nil {
		t.Fatalf("lease: %v", err)
	}
	if err := st.BeginDelivery(ctx, "att1", reg.MasterID, 1, 1, "idem", attnT0.Add(2*time.Minute)); err != nil {
		t.Fatalf("begin delivery: %v", err)
	}
	// re-register: epoch -> 2. A delivering item is NOT reopened, so state/leaseholder/
	// item_epoch stay matched; only the supervisor epoch the old incarnation holds is stale.
	reg2 := registerMaster(t, st, ctx, "master-a", attnT0.Add(3*time.Minute))
	if reg2.Epoch != 2 {
		t.Fatalf("re-registration epoch = %d, want 2", reg2.Epoch)
	}
	err := st.RecordDeliveryVerdict(ctx, "att1", "master-a", 1 /* stale */, 1, "strong", attnT0.Add(4*time.Minute))
	if !errors.Is(err, lease.ErrStaleEpoch) {
		t.Fatalf("stale supervisor epoch: err = %v, want ErrStaleEpoch", err)
	}
	// the live incarnation (epoch 2) is honored.
	if err := st.RecordDeliveryVerdict(ctx, "att1", "master-a", 2, 1, "strong", attnT0.Add(5*time.Minute)); err != nil {
		t.Fatalf("live incarnation verdict: %v", err)
	}
}

// TestReRegistrationOrphansLeases: a re-registration bumps epoch and returns the prior
// incarnation's still-leased items to open (a fresh master re-leases from scratch).
func TestReRegistrationOrphansLeases(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	reg := registerMaster(t, st, ctx, "master-a", attnT0)
	upsertItem(t, st, ctx, store.AttentionItem{ID: "att1", Kind: "stalled", EpicID: "e1", Priority: 15, DedupKey: "e1:stalled"}, attnT0)
	if _, err := st.LeaseAttention(ctx, reg.MasterID, reg.Epoch, 5, nil, 5*time.Minute, attnT0.Add(time.Minute)); err != nil {
		t.Fatalf("lease: %v", err)
	}
	reg2 := registerMaster(t, st, ctx, "master-a", attnT0.Add(2*time.Minute))
	if reg2.Epoch != 2 || reg2.RevokedLeases != 1 {
		t.Fatalf("re-register = %+v, want epoch 2 + 1 revoked lease", reg2)
	}
	got, _ := st.GetAttentionItem(ctx, "att1")
	if got.State != "open" || got.LeasedBy != "" {
		t.Fatalf("orphaned item = %+v, want open + no leaseholder", got)
	}
}

// TestOneInFlightPerEpic: two open items on one epic yield at most ONE lease.
func TestOneInFlightPerEpic(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	reg := registerMaster(t, st, ctx, "master-a", attnT0)
	upsertItem(t, st, ctx, store.AttentionItem{ID: "a", Kind: "scope_violation", EpicID: "e1", Priority: 5, DedupKey: "e1:scope"}, attnT0)
	upsertItem(t, st, ctx, store.AttentionItem{ID: "b", Kind: "stalled", EpicID: "e1", Priority: 15, DedupKey: "e1:stalled"}, attnT0)
	leased, err := st.LeaseAttention(ctx, reg.MasterID, reg.Epoch, 5, nil, 5*time.Minute, attnT0.Add(time.Minute))
	if err != nil {
		t.Fatalf("lease: %v", err)
	}
	if len(leased) != 1 || leased[0].ID != "a" {
		t.Fatalf("one-in-flight-per-epic: leased %d items %+v, want just [a]", len(leased), leased)
	}
	// a second master cannot grab the sibling item on the same in-flight epic.
	reg2 := registerMaster(t, st, ctx, "master-b", attnT0)
	leased2, err := st.LeaseAttention(ctx, reg2.MasterID, reg2.Epoch, 5, nil, 5*time.Minute, attnT0.Add(2*time.Minute))
	if err != nil {
		t.Fatalf("lease master-b: %v", err)
	}
	if len(leased2) != 0 {
		t.Fatalf("epic e1 already in-flight: master-b should get nothing, got %+v", leased2)
	}
}

// TestReapExpiredLeases: a leased item past its TTL returns to open; a delivering item
// past its TTL is NOT reaped here (that is ListStrandedDeliveries' job).
func TestReapExpiredLeases(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	reg := registerMaster(t, st, ctx, "master-a", attnT0)
	upsertItem(t, st, ctx, store.AttentionItem{ID: "att1", Kind: "stalled", EpicID: "e1", Priority: 15, DedupKey: "e1:stalled"}, attnT0)
	if _, err := st.LeaseAttention(ctx, reg.MasterID, reg.Epoch, 5, nil, 2*time.Minute, attnT0); err != nil {
		t.Fatalf("lease: %v", err)
	}
	// not yet expired.
	if reaped, _ := st.ReapExpiredLeases(ctx, attnT0.Add(time.Minute)); len(reaped) != 0 {
		t.Fatalf("premature reap: %+v", reaped)
	}
	reaped, err := st.ReapExpiredLeases(ctx, attnT0.Add(3*time.Minute))
	if err != nil {
		t.Fatalf("reap: %v", err)
	}
	if len(reaped) != 1 || reaped[0].State != "open" {
		t.Fatalf("reap = %+v, want one item back to open", reaped)
	}
}

// TestStrandedDeliveriesDoNotAutoReopen: the crash-window handling lists delivering
// items past TTL for a pane re-check WITHOUT mutating them (never a blind reopen).
func TestStrandedDeliveriesDoNotAutoReopen(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	reg := registerMaster(t, st, ctx, "master-a", attnT0)
	upsertItem(t, st, ctx, store.AttentionItem{ID: "att1", Kind: "needs_input", EpicID: "e1", Priority: 20, DedupKey: "e1:needs_input"}, attnT0)
	if _, err := st.LeaseAttention(ctx, reg.MasterID, reg.Epoch, 5, nil, 2*time.Minute, attnT0); err != nil {
		t.Fatalf("lease: %v", err)
	}
	if err := st.BeginDelivery(ctx, "att1", reg.MasterID, reg.Epoch, 1, "idem", attnT0.Add(30*time.Second)); err != nil {
		t.Fatalf("begin delivery: %v", err)
	}
	stranded, err := st.ListStrandedDeliveries(ctx, attnT0.Add(3*time.Minute))
	if err != nil {
		t.Fatalf("list stranded: %v", err)
	}
	if len(stranded) != 1 || stranded[0].ID != "att1" {
		t.Fatalf("stranded = %+v, want [att1]", stranded)
	}
	// crux: the row is untouched — still delivering, lease intact.
	got, _ := st.GetAttentionItem(ctx, "att1")
	if got.State != "delivering" || got.DeliveryKey != "idem" {
		t.Fatalf("stranded row was mutated: %+v (must stay delivering)", got)
	}
	// and the plain lease reaper leaves delivering rows alone too.
	if reaped, _ := st.ReapExpiredLeases(ctx, attnT0.Add(3*time.Minute)); len(reaped) != 0 {
		t.Fatalf("ReapExpiredLeases must not touch delivering rows: %+v", reaped)
	}
}

// TestDeliveryFailedReopens: a failed verdict returns the item to open with
// detail=delivery_failed (the fast master-retry tier).
func TestDeliveryFailedReopens(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	reg := registerMaster(t, st, ctx, "master-a", attnT0)
	upsertItem(t, st, ctx, store.AttentionItem{ID: "att1", Kind: "needs_input", EpicID: "e1", Priority: 20, DedupKey: "e1:needs_input"}, attnT0)
	st.LeaseAttention(ctx, reg.MasterID, reg.Epoch, 5, nil, 5*time.Minute, attnT0.Add(time.Minute))
	st.BeginDelivery(ctx, "att1", reg.MasterID, reg.Epoch, 1, "idem", attnT0.Add(2*time.Minute))
	if err := st.RecordDeliveryVerdict(ctx, "att1", reg.MasterID, reg.Epoch, 1, "failed", attnT0.Add(3*time.Minute)); err != nil {
		t.Fatalf("failed verdict: %v", err)
	}
	got, _ := st.GetAttentionItem(ctx, "att1")
	if got.State != "open" || got.Detail != "delivery_failed" || got.LeasedBy != "" {
		t.Fatalf("after failed delivery: %+v (want open/delivery_failed/no-lease)", got)
	}
}

// TestReopenUnacked: the send-and-ack loop reopens an unprocessed steer.
func TestReopenUnacked(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	reg := registerMaster(t, st, ctx, "master-a", attnT0)
	upsertItem(t, st, ctx, store.AttentionItem{ID: "att1", Kind: "needs_input", EpicID: "e1", Priority: 20, DedupKey: "e1:needs_input"}, attnT0)
	st.LeaseAttention(ctx, reg.MasterID, reg.Epoch, 5, nil, 5*time.Minute, attnT0.Add(time.Minute))
	st.BeginDelivery(ctx, "att1", reg.MasterID, reg.Epoch, 1, "idem", attnT0.Add(2*time.Minute))
	st.RecordDeliveryVerdict(ctx, "att1", reg.MasterID, reg.Epoch, 1, "weak", attnT0.Add(3*time.Minute))
	if err := st.ReopenUnacked(ctx, "att1", attnT0.Add(9*time.Minute)); err != nil {
		t.Fatalf("reopen unacked: %v", err)
	}
	got, _ := st.GetAttentionItem(ctx, "att1")
	if got.State != "open" || got.Detail != "steer_not_processed" {
		t.Fatalf("after reopen: %+v (want open/steer_not_processed)", got)
	}
	// reopening a non-awaiting_ack item is a state error.
	if err := st.ReopenUnacked(ctx, "att1", attnT0.Add(10*time.Minute)); !errors.Is(err, store.ErrAttentionState) {
		t.Fatalf("reopen of an open item = %v, want ErrAttentionState", err)
	}
}

// TestResolveDismissAndEscalate: dismiss ledgers attention_resolved; escalate ledgers
// attention_escalated.
func TestResolveDismissAndEscalate(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	upsertItem(t, st, ctx, store.AttentionItem{ID: "a", Kind: "epic_finished", EpicID: "e1", Priority: 40, DedupKey: "e1:finished"}, attnT0)
	upsertItem(t, st, ctx, store.AttentionItem{ID: "b", Kind: "auth_dead", EpicID: "e2", Priority: 10, DedupKey: "e2:auth_dead"}, attnT0)
	if err := st.ResolveAttention(ctx, "a", "dismissed", attnT0.Add(time.Minute)); err != nil {
		t.Fatalf("dismiss: %v", err)
	}
	if err := st.ResolveAttention(ctx, "b", "escalated", attnT0.Add(time.Minute)); err != nil {
		t.Fatalf("escalate: %v", err)
	}
	e1, _ := st.LoadEvents(ctx, "e1")
	if e1[len(e1)-1].Kind != ledger.KindAttentionResolved {
		t.Fatalf("dismiss ledger = %s, want attention_resolved", e1[len(e1)-1].Kind)
	}
	e2, _ := st.LoadEvents(ctx, "e2")
	if e2[len(e2)-1].Kind != ledger.KindAttentionEscalated {
		t.Fatalf("escalate ledger = %s, want attention_escalated", e2[len(e2)-1].Kind)
	}
	// double-resolve is a state error.
	if err := st.ResolveAttention(ctx, "a", "dismissed", attnT0.Add(2*time.Minute)); !errors.Is(err, store.ErrAttentionState) {
		t.Fatalf("double resolve = %v, want ErrAttentionState", err)
	}
}

// TestLeaseFencesStaleMaster: a lease attempt carrying a stale supervisor epoch (or from
// a stale master) is rejected.
func TestLeaseFencesStaleMaster(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	registerMaster(t, st, ctx, "master-a", attnT0)                          // epoch 1
	reg2 := registerMaster(t, st, ctx, "master-a", attnT0.Add(time.Minute)) // epoch 2
	upsertItem(t, st, ctx, store.AttentionItem{ID: "att1", Kind: "stalled", EpicID: "e1", Priority: 15, DedupKey: "e1:stalled"}, attnT0)
	if _, err := st.LeaseAttention(ctx, "master-a", 1 /* stale */, 5, nil, 5*time.Minute, attnT0.Add(2*time.Minute)); !errors.Is(err, lease.ErrStaleEpoch) {
		t.Fatalf("stale-epoch lease = %v, want ErrStaleEpoch", err)
	}
	// the live epoch works.
	if _, err := st.LeaseAttention(ctx, "master-a", reg2.Epoch, 5, nil, 5*time.Minute, attnT0.Add(3*time.Minute)); err != nil {
		t.Fatalf("live-epoch lease: %v", err)
	}
}

// TestDepFailedDedup (plan §15.12): re-observing a blocker failure for the same
// (epic, blocker) never spawns a second row.
func TestDepFailedDedup(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	created, _ := upsertItem(t, st, ctx, store.AttentionItem{
		ID: "d1", Kind: "dep_failed", EpicID: "e2", Priority: 15, DedupKey: "e2:dep_failed:blk-a",
	}, attnT0)
	if !created {
		t.Fatalf("first dep_failed should create")
	}
	created2, id2 := upsertItem(t, st, ctx, store.AttentionItem{
		ID: "d2", Kind: "dep_failed", EpicID: "e2", Priority: 15, DedupKey: "e2:dep_failed:blk-a",
	}, attnT0.Add(time.Minute))
	if created2 {
		t.Fatalf("re-seen dep_failed for the same blocker must dedup")
	}
	got, _ := st.GetAttentionItem(ctx, id2)
	if got.Occurrences != 2 {
		t.Fatalf("dep_failed occurrences = %d, want 2", got.Occurrences)
	}
}

// TestListOpenAttentionFilters exercises the state/kind/repo filters + ordering.
func TestListOpenAttentionFilters(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	upsertItem(t, st, ctx, store.AttentionItem{ID: "a", Kind: "scope_violation", EpicID: "e1", Repo: "russ", Priority: 5, DedupKey: "e1:scope"}, attnT0)
	upsertItem(t, st, ctx, store.AttentionItem{ID: "b", Kind: "needs_input", EpicID: "e2", Repo: "flowbee", Priority: 20, DedupKey: "e2:ni"}, attnT0)
	upsertItem(t, st, ctx, store.AttentionItem{ID: "c", Kind: "stalled", EpicID: "e3", Repo: "russ", Priority: 15, DedupKey: "e3:stalled"}, attnT0)

	all, _ := st.ListOpenAttention(ctx, "", nil, "")
	if len(all) != 3 || all[0].ID != "a" || all[2].ID != "b" {
		t.Fatalf("ordering wrong: %v", ids(all))
	}
	byRepo, _ := st.ListOpenAttention(ctx, "", nil, "russ")
	if len(byRepo) != 2 {
		t.Fatalf("repo filter: %v", ids(byRepo))
	}
	byKind, _ := st.ListOpenAttention(ctx, "", []string{"needs_input"}, "")
	if len(byKind) != 1 || byKind[0].ID != "b" {
		t.Fatalf("kind filter: %v", ids(byKind))
	}
}

func ids(items []store.AttentionItem) []string {
	var out []string
	for _, it := range items {
		out = append(out, it.ID)
	}
	return out
}
