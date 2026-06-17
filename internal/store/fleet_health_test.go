package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/samhotchkiss/flowbee/internal/job"
	"github.com/samhotchkiss/flowbee/internal/store"
	"github.com/samhotchkiss/flowbee/internal/testutil"
)

// TestFleetHealthStranded: a ready job with no live worker is the silent-stall the
// watchdog surfaces. A live worker clears it; a stale worker (down fleet) brings it back.
func TestFleetHealthStranded(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	now := time.Unix(1_000_000, 0)
	stale := 90 * time.Second

	if _, err := st.SeedJob(ctx, store.SeedParams{
		ID: "j", Kind: job.KindBuild, Flow: "build", Stage: "build", Role: job.RoleEngWorker, Now: now,
	}); err != nil {
		t.Fatal(err)
	}

	// ready job, zero workers -> stranded.
	h, err := st.FleetHealth(ctx, now, stale)
	if err != nil {
		t.Fatalf("FleetHealth: %v", err)
	}
	if h.WaitingJobs != 1 || h.LiveWorkers != 0 || !h.Stranded() {
		t.Fatalf("ready + no workers: %+v want waiting=1 live=0 stranded", h)
	}

	// a worker that just heartbeated -> live, not stranded.
	exp := now.Add(time.Hour).UTC().Format(time.RFC3339Nano)
	if _, err := st.DB.ExecContext(ctx,
		`INSERT INTO workers (worker_id, identity, host, attestation_expires_at, last_seen_at)
		 VALUES ('w1','w1','box',?,?)`, exp, now.UTC().Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	if h, _ = st.FleetHealth(ctx, now, stale); h.LiveWorkers != 1 || h.Stranded() {
		t.Fatalf("live worker: %+v want live=1 not stranded", h)
	}

	// the worker goes stale (fleet down) -> stranded again.
	if _, err := st.DB.ExecContext(ctx,
		`UPDATE workers SET last_seen_at=? WHERE identity='w1'`,
		now.Add(-10*time.Minute).UTC().Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	if h, _ = st.FleetHealth(ctx, now, stale); h.LiveWorkers != 0 || h.StaleWorkers != 1 || !h.Stranded() {
		t.Fatalf("stale worker (down fleet): %+v want stale=1 live=0 stranded", h)
	}
}
