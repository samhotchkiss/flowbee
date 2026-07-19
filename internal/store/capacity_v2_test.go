package store_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/samhotchkiss/flowbee/internal/capacity"
	"github.com/samhotchkiss/flowbee/internal/store"
	"github.com/samhotchkiss/flowbee/internal/testutil"
)

func TestNativeReviewCapacityHoldRearmsOnlyAfterCompleteGeneration(t *testing.T) {
	ctx := context.Background()
	st, jobID, now := seedNativeReviewObligation(t, "capacity-reviewer")
	st.EnableCapacityV2 = true

	ls, err := st.ClaimReviewJob(ctx, store.ClaimReviewParams{
		JobID: jobID, LeaseID: "held-lease", Identity: "capacity-reviewer",
		ModelFamily: "codex", Attested: []string{"role:code_reviewer"},
		TTL: 5 * time.Minute, Now: now.Add(time.Minute),
	})
	if ls != nil || !errors.Is(err, store.ErrNoCapacity) {
		t.Fatalf("missing-seat claim lease=%+v err=%v", ls, err)
	}
	var hold, jobState string
	var actions, attentions, alerts int
	if err := st.DB.QueryRowContext(ctx, `SELECT hold_kind FROM epic_deliveries
		WHERE epic_id='epic-driver-review'`).Scan(&hold); err != nil {
		t.Fatal(err)
	}
	_ = st.DB.QueryRowContext(ctx, `SELECT state FROM jobs WHERE id=?`, jobID).Scan(&jobState)
	_ = st.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM epic_actions WHERE epic_id='epic-driver-review'`).Scan(&actions)
	_ = st.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM attention_items WHERE epic_id='epic-driver-review'
		AND kind='review_capacity_exhausted' AND state='open'`).Scan(&attentions)
	_ = st.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM control_alerts WHERE epic_id='epic-driver-review'
		AND kind='capacity_pool_exhausted'`).Scan(&alerts)
	if hold != "review_capacity_unavailable" || jobState != "review_pending" || actions != 0 || attentions != 1 || alerts != 1 {
		t.Fatalf("hold=%q job=%q actions=%d attentions=%d alerts=%d", hold, jobState, actions, attentions, alerts)
	}

	seat := addBoundCapacitySeat(t, st, now, "/codex/reviewer", "host-review", "review-account", "lineage-review")
	observation := liveCapacityObservation("obs-review", seat, now.Add(2*time.Minute), "review-account", "lineage-review")
	if err := st.CommitCapacityGeneration(ctx, store.CapacityGeneration{
		ID: "generation-review", StartedAt: now.Add(2 * time.Minute),
		ExpectedSeatIDs: []string{seat.ID}, Observations: []store.CapacitySeatObservation{observation},
	}, now.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := st.DB.QueryRowContext(ctx, `SELECT hold_kind FROM epic_deliveries
		WHERE epic_id='epic-driver-review'`).Scan(&hold); err != nil || hold != "" {
		t.Fatalf("capacity generation did not rearm hold=%q err=%v", hold, err)
	}
	if err := st.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM attention_items WHERE epic_id='epic-driver-review'
		AND kind='review_capacity_exhausted' AND state='resolved'`).Scan(&attentions); err != nil || attentions != 1 {
		t.Fatalf("resolved capacity attention=%d err=%v", attentions, err)
	}

	bindReviewDriverRoute(t, st, "capacity-reviewer", now.Add(2*time.Minute))
	ls, err = st.ClaimReviewJob(ctx, store.ClaimReviewParams{
		JobID: jobID, LeaseID: "routable-lease", Identity: "capacity-reviewer", SeatID: seat.ID,
		ModelFamily: "codex", Attested: []string{"role:code_reviewer"},
		TTL: 5 * time.Minute, Now: now.Add(3 * time.Minute),
	})
	if err != nil || ls == nil {
		t.Fatalf("fresh exact-seat claim lease=%+v err=%v", ls, err)
	}
	if err := st.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM epic_actions WHERE epic_id='epic-driver-review'
		AND kind='review_wake'`).Scan(&actions); err != nil || actions != 1 {
		t.Fatalf("review wake actions=%d err=%v", actions, err)
	}
}

func addBoundCapacitySeat(t *testing.T, st *store.Store, now time.Time, home, host, account, lineage string) store.Seat {
	t.Helper()
	seat := store.Seat{Box: host, AgentFamily: "codex", CodexHome: home, Health: store.SeatReady, MaxConcurrent: 2}
	if err := st.AddSeat(context.Background(), seat, now); err != nil {
		t.Fatal(err)
	}
	seat.ID = seat.ComposeID()
	if err := st.BindCapacitySeatIdentity(context.Background(), store.CapacitySeatIdentity{
		SeatID: seat.ID, HostID: host, AccountKey: account, CredentialLineage: lineage,
		ReservePct: 10, AccountMaximum: 3,
	}, now); err != nil {
		t.Fatal(err)
	}
	return seat
}

func liveCapacityObservation(id string, seat store.Seat, now time.Time, account, lineage string) store.CapacitySeatObservation {
	return store.CapacitySeatObservation{
		ObservationID: id, SeatID: seat.ID, HostID: seat.Box, Provider: "codex",
		AccountKey: account, CredentialLineage: lineage, CollectorID: "collector-" + seat.ID,
		Source: "live_app_server", TrustState: "verified", IntegrityState: "verified",
		Windows:   []capacity.RouteWindow{{Kind: "weekly", Applicable: true, Known: true, Percent: 40}},
		FetchedAt: now, RawSHA256: "sha256:raw", AdapterVersion: "codex-live/v1",
	}
}

func TestCapacityGenerationIsAtomicIdempotentAndFailsClosedAtReadTime(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 19, 19, 0, 0, 0, time.UTC)
	seat := addBoundCapacitySeat(t, st, now, "/codex/a", "host-a", "account-a", "lineage-a")
	if err := st.UpdateSeatHealth(ctx, seat.ID, store.SeatUnreachable, "not yet live-probed", now); err != nil {
		t.Fatal(err)
	}
	observation := liveCapacityObservation("obs-1", seat, now, "account-a", "lineage-a")
	generation := store.CapacityGeneration{ID: "generation-1", StartedAt: now,
		ExpectedSeatIDs: []string{seat.ID}, Observations: []store.CapacitySeatObservation{observation}}
	if err := st.CommitCapacityGeneration(ctx, generation, now); err != nil {
		t.Fatal(err)
	}
	projectedSeat, err := st.GetSeat(ctx, seat.ID)
	if err != nil || projectedSeat.Health != store.SeatReady || projectedSeat.LastProbeAt == "" {
		t.Fatalf("accepted generation did not atomically project ready seat: seat=%+v err=%v", projectedSeat, err)
	}
	if err := st.CommitCapacityGeneration(ctx, generation, now.Add(time.Second)); err != nil {
		t.Fatalf("exact retry: %v", err)
	}
	var observations int
	if err := st.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM account_usage_observations`).Scan(&observations); err != nil || observations != 1 {
		t.Fatalf("observations=%d err=%v", observations, err)
	}
	decision, err := st.CapacityRouteForSeat(ctx, seat.ID, now.Add(time.Minute), 5*time.Minute)
	if err != nil || !decision.Routable {
		t.Fatalf("fresh route=%+v err=%v", decision, err)
	}
	decision, err = st.CapacityRouteForSeat(ctx, seat.ID, now.Add(6*time.Minute), 5*time.Minute)
	if err != nil || decision.Routable || !containsReason(decision.Reasons, capacity.HoldObservationStale) {
		t.Fatalf("aged route=%+v err=%v", decision, err)
	}
	changed := generation
	changed.Observations = append([]store.CapacitySeatObservation(nil), generation.Observations...)
	changed.Observations[0].Windows = []capacity.RouteWindow{{Kind: "weekly", Applicable: true, Known: true, Percent: 50}}
	if err := st.CommitCapacityGeneration(ctx, changed, now.Add(time.Minute)); err == nil || !strings.Contains(err.Error(), "idempotency conflict") {
		t.Fatalf("changed generation replay err=%v", err)
	}
}

func TestCapacityGenerationProjectsIdentityDriftAsAuthDead(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 19, 19, 30, 0, 0, time.UTC)
	seat := addBoundCapacitySeat(t, st, now, "/codex/drift", "host-drift", "account-drift", "lineage-good")
	observation := liveCapacityObservation("obs-drift", seat, now, "account-drift", "lineage-wrong")
	if err := st.CommitCapacityGeneration(ctx, store.CapacityGeneration{
		ID: "generation-drift", StartedAt: now, ExpectedSeatIDs: []string{seat.ID},
		Observations: []store.CapacitySeatObservation{observation},
	}, now); err != nil {
		t.Fatal(err)
	}
	projectedSeat, err := st.GetSeat(ctx, seat.ID)
	if err != nil {
		t.Fatal(err)
	}
	if projectedSeat.Health != store.SeatAuthDead || !strings.Contains(projectedSeat.HealthDetail, "credential_lineage_mismatch") {
		t.Fatalf("identity drift projection=%+v", projectedSeat)
	}
	if decision, err := st.CapacityRouteForSeat(ctx, seat.ID, now, 5*time.Minute); err != nil || decision.Routable {
		t.Fatalf("identity-drifted seat routed: decision=%+v err=%v", decision, err)
	}
}

func TestPartialOrCacheGenerationNeverPublishesRoutableTruth(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 19, 20, 0, 0, 0, time.UTC)
	a := addBoundCapacitySeat(t, st, now, "/codex/a", "host-a", "account", "lineage-a")
	b := addBoundCapacitySeat(t, st, now, "/codex/b", "host-b", "account", "lineage-b")
	partial := store.CapacityGeneration{ID: "partial", StartedAt: now,
		ExpectedSeatIDs: []string{a.ID, b.ID}, Observations: []store.CapacitySeatObservation{liveCapacityObservation("obs-a", a, now, "account", "lineage-a")}}
	if err := st.CommitCapacityGeneration(ctx, partial, now); err == nil {
		t.Fatal("partial generation committed")
	}
	var generations int
	_ = st.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM capacity_generations`).Scan(&generations)
	if generations != 0 {
		t.Fatalf("partial generation left rows: %d", generations)
	}

	goodA := liveCapacityObservation("obs-a2", a, now, "account", "lineage-a")
	badB := liveCapacityObservation("obs-b2", b, now, "account", "wrong-lineage")
	if err := st.CommitCapacityGeneration(ctx, store.CapacityGeneration{ID: "mixed", StartedAt: now,
		ExpectedSeatIDs: []string{a.ID, b.ID}, Observations: []store.CapacitySeatObservation{badB, goodA}}, now); err != nil {
		t.Fatal(err)
	}
	if got, err := st.CapacityRouteForSeat(ctx, a.ID, now, 5*time.Minute); err != nil || !got.Routable {
		t.Fatalf("good shared-account seat held: %+v err=%v", got, err)
	}
	if got, err := st.CapacityRouteForSeat(ctx, b.ID, now, 5*time.Minute); err != nil || got.Routable || !containsReason(got.Reasons, capacity.HoldCredentialMismatch) {
		t.Fatalf("drifted seat routed: %+v err=%v", got, err)
	}

	cache := liveCapacityObservation("obs-a3", a, now.Add(time.Minute), "account", "lineage-a")
	cache.Source = "cache"
	if err := st.CommitCapacityGeneration(ctx, store.CapacityGeneration{ID: "cache", StartedAt: now.Add(time.Minute),
		ExpectedSeatIDs: []string{a.ID}, Observations: []store.CapacitySeatObservation{cache}}, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if got, err := st.CapacityRouteForSeat(ctx, a.ID, now.Add(time.Minute), 5*time.Minute); err != nil || got.Routable || !containsReason(got.Reasons, capacity.HoldLiveSourceRequired) {
		t.Fatalf("cache generation routed or wrong hold: %+v err=%v", got, err)
	}
}

func containsReason(reasons []string, want string) bool {
	for _, reason := range reasons {
		if reason == want {
			return true
		}
	}
	return false
}
