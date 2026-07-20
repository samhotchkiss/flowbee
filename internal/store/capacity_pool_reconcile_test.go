package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/samhotchkiss/flowbee/internal/capacity"
	"github.com/samhotchkiss/flowbee/internal/store"
	"github.com/samhotchkiss/flowbee/internal/testutil"
)

func TestZeroRoutableRequiredPoolRaisesOneDurableAlertAndRecovers(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 19, 18, 0, 0, 0, time.UTC)
	req := store.CapacityPoolRequirement{ProjectID: "default", Pool: "review", Provider: "codex", QueuedWork: 2}
	rep, err := st.ReconcileCapacityPools(ctx, []store.CapacityPoolRequirement{req}, now, 5*time.Minute, 15*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Pending != 1 || rep.Alerted != 0 {
		t.Fatalf("initial report=%+v", rep)
	}
	var attentionCount, alertCount int
	_ = st.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM attention_items WHERE kind='capacity_pool_exhausted' AND state='open'`).Scan(&attentionCount)
	_ = st.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM control_alerts WHERE kind='capacity_pool_exhausted'`).Scan(&alertCount)
	if attentionCount != 1 || alertCount != 0 {
		t.Fatalf("attention=%d alert=%d", attentionCount, alertCount)
	}
	for _, at := range []time.Time{now.Add(6 * time.Minute), now.Add(7 * time.Minute)} {
		if _, err := st.ReconcileCapacityPools(ctx, []store.CapacityPoolRequirement{req}, at, 5*time.Minute, 15*time.Minute); err != nil {
			t.Fatal(err)
		}
	}
	_ = st.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM control_alerts WHERE kind='capacity_pool_exhausted'`).Scan(&alertCount)
	if alertCount != 1 {
		t.Fatalf("repeated ticks created %d alerts", alertCount)
	}

	seat := store.Seat{Box: "host-a", AgentFamily: "codex", CodexHome: "/codex/a", Health: store.SeatReady, MaxConcurrent: 1}
	if err := st.AddSeat(ctx, seat, now); err != nil {
		t.Fatal(err)
	}
	seat.ID = seat.ComposeID()
	if err := st.BindCapacitySeatIdentity(ctx, store.CapacitySeatIdentity{SeatID: seat.ID, HostID: "host-a", AccountKey: "acct", CredentialLineage: "lineage", ReservePct: 10, AccountMaximum: 1}, now); err != nil {
		t.Fatal(err)
	}
	obs := store.CapacitySeatObservation{ObservationID: "obs", SeatID: seat.ID, HostID: "host-a", Provider: "codex", AccountKey: "acct", CredentialLineage: "lineage", CollectorID: "collector", Source: "live_app_server", TrustState: "verified", IntegrityState: "verified", Windows: []capacity.RouteWindow{{Kind: "weekly", Applicable: true, Known: true, Percent: 20}}, FetchedAt: now.Add(8 * time.Minute), RawSHA256: "sha256:x", AdapterVersion: "fixture/v1"}
	if err := st.CommitCapacityGeneration(ctx, store.CapacityGeneration{ID: "gen", StartedAt: now.Add(8 * time.Minute), ExpectedSeatIDs: []string{seat.ID}, Observations: []store.CapacitySeatObservation{obs}}, now.Add(8*time.Minute)); err != nil {
		t.Fatal(err)
	}
	rep, err = st.ReconcileCapacityPools(ctx, []store.CapacityPoolRequirement{req}, now.Add(9*time.Minute), 5*time.Minute, 15*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Resolved != 1 {
		t.Fatalf("recovery report=%+v", rep)
	}
	_ = st.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM attention_items WHERE kind='capacity_pool_exhausted' AND state='resolved'`).Scan(&attentionCount)
	if attentionCount != 1 {
		t.Fatalf("resolved attention=%d", attentionCount)
	}
}
