package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/samhotchkiss/flowbee/internal/capacity"
	"github.com/samhotchkiss/flowbee/internal/job"
	"github.com/samhotchkiss/flowbee/internal/store"
	"github.com/samhotchkiss/flowbee/internal/testutil"
	"github.com/samhotchkiss/flowbee/internal/ulid"
)

// seedReady seeds a `ready` build job requiring a claude model_family.
func seedReadyClaude(t *testing.T, st *store.Store, id string) {
	t.Helper()
	if _, err := st.SeedJob(context.Background(), store.SeedParams{
		ID: id, Kind: job.KindBuild, Flow: "build", Stage: "build", Role: job.RoleEngWorker,
		BaseSHA: "base", RequiredCapabilities: []string{"role:eng_worker", "model_family:claude"},
		Now: time.Unix(1, 0),
	}); err != nil {
		t.Fatalf("seed %s: %v", id, err)
	}
}

// register a box advertising per-model slots + accounts via the store directly.
func enrollBox(t *testing.T, st *store.Store, workerID, identity string, slots map[string]int, accts []store.AccountSpec) {
	t.Helper()
	ctx := context.Background()
	now := time.Unix(10, 0)
	if _, err := st.DB.ExecContext(ctx, `
		INSERT INTO workers (worker_id, identity, host, claimed_capabilities, attested_capabilities, attestation_expires_at)
		VALUES (?, ?, 'h', '[]', '[]', ?)
		ON CONFLICT(worker_id) DO NOTHING`,
		workerID, identity, now.Add(time.Hour).Format(time.RFC3339Nano)); err != nil {
		t.Fatalf("insert worker: %v", err)
	}
	if err := st.SetWorkerModelSlots(ctx, workerID, slots, 1, now); err != nil {
		t.Fatalf("slots: %v", err)
	}
	if len(accts) > 0 {
		if err := st.UpsertAccounts(ctx, accts, now); err != nil {
			t.Fatalf("accounts: %v", err)
		}
	}
}

// TestModelSlotGate proves a box advertising claude:2 cannot hold a 3rd concurrent
// claude lease: the 3rd claim loses (slot full -> ErrLostRace -> the job stays ready).
func TestModelSlotGate(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	enrollBox(t, st, "box-1", "ident-1", map[string]int{"claude": 2}, nil)

	attested := []string{"role:eng_worker", "model_family:claude"}
	for i := 0; i < 2; i++ {
		id := ulid.New()
		seedReadyClaude(t, st, id)
		ls, err := st.ClaimReadyJob(ctx, store.ClaimParams{
			JobID: id, LeaseID: ulid.New(), Identity: "ident-1", ModelFamily: "claude",
			Role: job.RoleEngWorker, Attested: attested, TTL: time.Minute, Now: time.Unix(20, 0),
		})
		if err != nil || ls == nil {
			t.Fatalf("claim %d should succeed: %v", i, err)
		}
	}
	// the 3rd claude job: the box's claude:2 slot budget is full -> lost race.
	id3 := ulid.New()
	seedReadyClaude(t, st, id3)
	_, err := st.ClaimReadyJob(ctx, store.ClaimParams{
		JobID: id3, LeaseID: ulid.New(), Identity: "ident-1", ModelFamily: "claude",
		Role: job.RoleEngWorker, Attested: attested, TTL: time.Minute, Now: time.Unix(21, 0),
	})
	if err == nil {
		t.Fatal("3rd claude claim should fail (slot full)")
	}
	// the job is still ready (claimable once a slot frees).
	j, _ := st.GetJob(ctx, id3)
	if j.State != job.StateReady {
		t.Fatalf("blocked job state=%s want ready", j.State)
	}
}

// TestAccountCeilingRollover proves dispatch rolls from the preferred account to the
// fallback when the preferred is at/over its ceiling, and binds the chosen account.
func TestAccountCeilingRollover(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	enrollBox(t, st, "box-1", "ident-1", map[string]int{"claude": 5}, []store.AccountSpec{
		{AccountID: "claude-primary", ModelFamily: "claude", CeilingPct: 90, PreferenceRank: 0},
		{AccountID: "claude-fallback", ModelFamily: "claude", CeilingPct: 90, PreferenceRank: 1},
	})
	attested := []string{"role:eng_worker", "model_family:claude"}

	// preferred under ceiling -> a claim binds the preferred account.
	id1 := ulid.New()
	seedReadyClaude(t, st, id1)
	if _, err := st.ClaimReadyJob(ctx, store.ClaimParams{
		JobID: id1, LeaseID: ulid.New(), Identity: "ident-1", ModelFamily: "claude",
		Role: job.RoleEngWorker, Attested: attested, TTL: time.Minute, Now: time.Unix(20, 0),
	}); err != nil {
		t.Fatalf("claim1: %v", err)
	}
	if got := boundAccount(t, st, id1); got != "claude-primary" {
		t.Fatalf("claim1 bound account=%q want claude-primary", got)
	}

	// usage report pushes the preferred over ceiling.
	if _, err := st.RecordUsage(ctx, []capacity.UsageReport{
		{AccountID: "claude-primary", ModelFamily: "claude", UsagePct: 95},
	}, time.Unix(25, 0)); err != nil {
		t.Fatalf("record usage: %v", err)
	}

	// the next claim ROLLS OVER to the fallback (preferred is gated).
	id2 := ulid.New()
	seedReadyClaude(t, st, id2)
	if _, err := st.ClaimReadyJob(ctx, store.ClaimParams{
		JobID: id2, LeaseID: ulid.New(), Identity: "ident-1", ModelFamily: "claude",
		Role: job.RoleEngWorker, Attested: attested, TTL: time.Minute, Now: time.Unix(26, 0),
	}); err != nil {
		t.Fatalf("claim2: %v", err)
	}
	if got := boundAccount(t, st, id2); got != "claude-fallback" {
		t.Fatalf("claim2 bound account=%q want claude-fallback (rollover)", got)
	}

	// push the fallback over ceiling too -> dispatch must WAIT (lost race, stays ready).
	if _, err := st.RecordUsage(ctx, []capacity.UsageReport{
		{AccountID: "claude-fallback", ModelFamily: "claude", UsagePct: 99},
	}, time.Unix(27, 0)); err != nil {
		t.Fatalf("record usage fb: %v", err)
	}
	id3 := ulid.New()
	seedReadyClaude(t, st, id3)
	if _, err := st.ClaimReadyJob(ctx, store.ClaimParams{
		JobID: id3, LeaseID: ulid.New(), Identity: "ident-1", ModelFamily: "claude",
		Role: job.RoleEngWorker, Attested: attested, TTL: time.Minute, Now: time.Unix(28, 0),
	}); err == nil {
		t.Fatal("claim3 should fail: all accounts at ceiling -> wait")
	}
	if j, _ := st.GetJob(ctx, id3); j.State != job.StateReady {
		t.Fatalf("claim3 job state=%s want ready", j.State)
	}
}

// Test429PinsAccount proves an immediate-on-429 usage report pins the account out
// of dispatch even when its percent is low (the cool-down).
func Test429PinsAccount(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	st.UpsertAccounts(ctx, []store.AccountSpec{
		{AccountID: "a", ModelFamily: "claude", CeilingPct: 90, PreferenceRank: 0},
		{AccountID: "b", ModelFamily: "claude", CeilingPct: 90, PreferenceRank: 1},
	}, time.Unix(1, 0))

	if _, err := st.RecordUsage(ctx, []capacity.UsageReport{
		{AccountID: "a", ModelFamily: "claude", UsagePct: 3, RateLimited: true},
	}, time.Unix(2, 0)); err != nil {
		t.Fatalf("429 report: %v", err)
	}
	a, ok, err := st.SelectAccountForModel(ctx, "claude")
	if err != nil {
		t.Fatal(err)
	}
	if !ok || a.AccountID != "b" {
		t.Fatalf("after 429 on a: selected %q ok=%v want b", a.AccountID, ok)
	}
}

func boundAccount(t *testing.T, st *store.Store, jobID string) string {
	t.Helper()
	var acct string
	if err := st.DB.QueryRowContext(context.Background(),
		`SELECT bound_account FROM jobs WHERE id = ?`, jobID).Scan(&acct); err != nil {
		t.Fatalf("read bound_account: %v", err)
	}
	return acct
}
