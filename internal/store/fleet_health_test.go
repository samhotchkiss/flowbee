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
// TestFleetHealthByModel: FleetHealth tallies LIVE workers by their advertised backend
// (the `model:<x>` capability), so `flowbee status` can show "fleet on codex". Stale
// workers are excluded; a worker with no model: cap simply isn't counted.
func TestFleetHealthByModel(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	now := time.Unix(1_000_000, 0)
	stale := 90 * time.Second
	exp := now.Add(time.Hour).UTC().Format(time.RFC3339Nano)
	live := now.UTC().Format(time.RFC3339Nano)
	old := now.Add(-10 * time.Minute).UTC().Format(time.RFC3339Nano)

	ins := func(id, caps, seen string) {
		if _, err := st.DB.ExecContext(ctx,
			`INSERT INTO workers (worker_id, identity, host, attested_capabilities, attestation_expires_at, last_seen_at)
			 VALUES (?,?,'box',?,?,?)`, id, id, caps, exp, seen); err != nil {
			t.Fatal(err)
		}
	}
	ins("c1", `["role:eng_worker","model:codex"]`, live)
	ins("c2", `["role:code_reviewer","model:codex"]`, live)
	ins("s1", `["role:eng_worker","model:sonnet"]`, live)
	ins("stale", `["role:eng_worker","model:codex"]`, old)     // stale -> excluded
	ins("nomodel", `["role:eng_worker"]`, live)                // no model: cap -> uncounted

	h, err := st.FleetHealth(ctx, now, stale)
	if err != nil {
		t.Fatalf("FleetHealth: %v", err)
	}
	if h.LiveWorkers != 4 || h.StaleWorkers != 1 {
		t.Fatalf("live=%d stale=%d want live=4 stale=1", h.LiveWorkers, h.StaleWorkers)
	}
	if h.ByModel["codex"] != 2 {
		t.Errorf("codex=%d want 2 (the stale codex worker must NOT count)", h.ByModel["codex"])
	}
	if h.ByModel["sonnet"] != 1 {
		t.Errorf("sonnet=%d want 1", h.ByModel["sonnet"])
	}
}

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
