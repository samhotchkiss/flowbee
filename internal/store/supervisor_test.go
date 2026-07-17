package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/samhotchkiss/flowbee/internal/ledger"
	"github.com/samhotchkiss/flowbee/internal/store"
	"github.com/samhotchkiss/flowbee/internal/testutil"
)

var supT0 = time.Date(2026, 7, 16, 8, 0, 0, 0, time.UTC)

// TestRegisterSupervisorIdempotent: registration is an idempotent upsert on label that
// bumps epoch each time; the master_id is stable across a `/clear` (re-registration).
func TestRegisterSupervisorIdempotent(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()

	reg, err := st.RegisterSupervisor(ctx, store.Supervisor{
		Label: "master-pearl", Kind: "claude", ModelFamily: "claude", Box: "buncher",
		TmuxName: "master", Repos: []string{"flowbee", "russ"},
	}, supT0)
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	if reg.MasterID != "master-pearl" || reg.Epoch != 1 {
		t.Fatalf("first register = %+v, want id master-pearl / epoch 1", reg)
	}

	reg2, err := st.RegisterSupervisor(ctx, store.Supervisor{
		Label: "master-pearl", Kind: "claude", ModelFamily: "claude", Repos: []string{"flowbee", "russ"},
	}, supT0.Add(time.Hour))
	if err != nil {
		t.Fatalf("re-register: %v", err)
	}
	if reg2.MasterID != "master-pearl" || reg2.Epoch != 2 {
		t.Fatalf("re-register = %+v, want SAME id / epoch 2", reg2)
	}

	sup, err := st.GetSupervisor(ctx, "master-pearl")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if sup.State != "active" || sup.Epoch != 2 || len(sup.Repos) != 2 {
		t.Fatalf("supervisor state = %+v", sup)
	}

	// registration ledgered (keyed on the label) — twice.
	evs, _ := st.LoadEvents(ctx, "sup:master-pearl")
	n := 0
	for _, e := range evs {
		if e.Kind == ledger.KindSupervisorRegistered {
			n++
		}
	}
	if n != 2 {
		t.Fatalf("supervisor_registered events = %d, want 2", n)
	}

	// argv-hostile box is rejected at registration.
	if _, err := st.RegisterSupervisor(ctx, store.Supervisor{Label: "bad", Box: "-oProxyCommand=x"}, supT0); err == nil {
		t.Fatalf("expected an argv-safety rejection for a leading-dash box")
	}
}

// TestRegisterSupervisorRevokedNotResurrected (n2): a re-registration must NOT silently
// resurrect a deliberately revoked master.
func TestRegisterSupervisorRevokedNotResurrected(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	registerMaster(t, st, ctx, "master-a", supT0)
	// operator revokes it (no CLI yet; simulate the retired state directly).
	if _, err := st.DB.ExecContext(ctx, `UPDATE supervisors SET state='revoked' WHERE id='master-a'`); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if _, err := st.RegisterSupervisor(ctx, store.Supervisor{Label: "master-a", Kind: "claude"}, supT0.Add(time.Hour)); !errors.Is(err, store.ErrSupervisorRevoked) {
		t.Fatalf("re-register of a revoked master = %v, want ErrSupervisorRevoked", err)
	}
	// it stays revoked.
	if sup, _ := st.GetSupervisor(ctx, "master-a"); sup.State != "revoked" {
		t.Fatalf("state = %q, want it to stay revoked", sup.State)
	}
}

// TestSupervisorHeartbeat: a matching epoch refreshes liveness; an older epoch (a
// superseded incarnation) is told revoked.
func TestSupervisorHeartbeat(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	reg := registerMaster(t, st, ctx, "master-a", supT0)

	revoked, err := st.SupervisorHeartbeat(ctx, "master-a", reg.Epoch, supT0.Add(time.Minute))
	if err != nil || revoked {
		t.Fatalf("live heartbeat: revoked=%v err=%v", revoked, err)
	}
	// re-register -> epoch 2; the old incarnation's heartbeat is revoked.
	registerMaster(t, st, ctx, "master-a", supT0.Add(2*time.Minute))
	revoked, err = st.SupervisorHeartbeat(ctx, "master-a", 1, supT0.Add(3*time.Minute))
	if err != nil {
		t.Fatalf("stale heartbeat err: %v", err)
	}
	if !revoked {
		t.Fatalf("a stale-epoch heartbeat must report revoked=true")
	}
	// unknown master.
	if _, err := st.SupervisorHeartbeat(ctx, "nope", 1, supT0); !errors.Is(err, store.ErrSupervisorNotFound) {
		t.Fatalf("heartbeat of unknown master = %v, want ErrSupervisorNotFound", err)
	}
}

// TestStaleSupervisorReaping: a master whose heartbeat is older than 3x the interval is
// listed stale and, when marked, its leases are reaped.
func TestStaleSupervisorReaping(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	reg := registerMaster(t, st, ctx, "master-a", supT0) // heartbeat @ supT0
	upsertItem(t, st, ctx, store.AttentionItem{ID: "att1", Kind: "stalled", EpicID: "e1", Priority: 15, DedupKey: "e1:stalled"}, supT0)
	if _, err := st.LeaseAttention(ctx, reg.MasterID, reg.Epoch, 5, nil, time.Hour, supT0); err != nil {
		t.Fatalf("lease: %v", err)
	}

	interval := 30 * time.Second // stale after 90s
	// within 3x interval: not stale.
	if stale, _ := st.ListStaleSupervisors(ctx, interval, supT0.Add(time.Minute)); len(stale) != 0 {
		t.Fatalf("premature stale: %+v", stale)
	}
	stale, err := st.ListStaleSupervisors(ctx, interval, supT0.Add(2*time.Minute))
	if err != nil {
		t.Fatalf("list stale: %v", err)
	}
	if len(stale) != 1 || stale[0].ID != "master-a" {
		t.Fatalf("stale = %+v, want [master-a]", stale)
	}

	if err := st.MarkSupervisorStale(ctx, "master-a", supT0.Add(2*time.Minute)); err != nil {
		t.Fatalf("mark stale: %v", err)
	}
	sup, _ := st.GetSupervisor(ctx, "master-a")
	if sup.State != "stale" {
		t.Fatalf("state = %q, want stale", sup.State)
	}
	got, _ := st.GetAttentionItem(ctx, "att1")
	if got.State != "open" || got.LeasedBy != "" {
		t.Fatalf("stale reap should return the lease to open: %+v", got)
	}
}

// TestSetSupervisorLastReport (plan §15.7).
func TestSetSupervisorLastReport(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	registerMaster(t, st, ctx, "master-a", supT0)
	if err := st.SetSupervisorLastReport(ctx, "master-a", "shipped 3 epics; e2 waiting on review", supT0.Add(time.Minute)); err != nil {
		t.Fatalf("set report: %v", err)
	}
	sup, _ := st.GetSupervisor(ctx, "master-a")
	if sup.LastReportedStatus != "shipped 3 epics; e2 waiting on review" || sup.LastReportedAt == "" {
		t.Fatalf("last report not recorded: %+v", sup)
	}
	if err := st.SetSupervisorLastReport(ctx, "nope", "x", supT0); !errors.Is(err, store.ErrSupervisorNotFound) {
		t.Fatalf("report on unknown master = %v, want ErrSupervisorNotFound", err)
	}
}

// TestWIPMarkers (plan §15.7): register/list/clear; upsert is idempotent on id so a
// post-compaction master does not double-register.
func TestWIPMarkers(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()

	m := store.WIPMarker{
		ID: "fix:e1:auth", EpicID: "e1", PRNumber: 42, Label: "re-auth in flight",
		RegisteredBy: "master-a", ETA: "10m",
	}
	if err := st.UpsertWIPMarker(ctx, m, supT0); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	// idempotent re-register (same id): still one active marker.
	if err := st.UpsertWIPMarker(ctx, m, supT0.Add(time.Minute)); err != nil {
		t.Fatalf("re-upsert: %v", err)
	}
	active, err := st.ListWIPMarkers(ctx, "e1")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(active) != 1 || active[0].PRNumber != 42 || active[0].EpicID != "e1" {
		t.Fatalf("active markers = %+v, want one for e1/PR42", active)
	}
	// a marker with no PR stores 0 (NULL) rather than a phantom PR.
	if err := st.UpsertWIPMarker(ctx, store.WIPMarker{ID: "fix:e2", EpicID: "e2", RegisteredBy: "master-a"}, supT0); err != nil {
		t.Fatalf("upsert no-pr: %v", err)
	}
	e2, _ := st.ListWIPMarkers(ctx, "e2")
	if len(e2) != 1 || e2[0].PRNumber != 0 {
		t.Fatalf("no-pr marker = %+v, want PRNumber 0", e2)
	}

	if err := st.ClearWIPMarker(ctx, "fix:e1:auth", supT0.Add(2*time.Minute)); err != nil {
		t.Fatalf("clear: %v", err)
	}
	if left, _ := st.ListWIPMarkers(ctx, "e1"); len(left) != 0 {
		t.Fatalf("cleared marker still active: %+v", left)
	}
	// clearing an already-cleared/unknown marker is an error.
	if err := st.ClearWIPMarker(ctx, "fix:e1:auth", supT0.Add(3*time.Minute)); !errors.Is(err, store.ErrWIPMarkerNotFound) {
		t.Fatalf("double clear = %v, want ErrWIPMarkerNotFound", err)
	}
}
