// F6 acceptance: worker capacity — per-model slots, named accounts, usage,
// ceilings, rollover — proven end-to-end over the real HTTP surface against a real
// SQLite store. No GitHub, no LLM.
//
// DONE-WHEN (each proven below by a real, non-skipped test):
//   - a box advertises concurrency PER MODEL (claude:3, codex:3) and runs 3 claude
//     + 3 codex leases CONCURRENTLY; a 7th claude job is gated (slot full);
//   - named per-model ACCOUNTS with ceiling_pct + ordered preference: a claude job
//     ROLLS OVER from the preferred account to the fallback once the preferred is
//     reported at/over its ceiling;
//   - usage reporting via POST /v1/workers/usage GATES dispatch: when every account
//     for a model is at/over ceiling, the box WAITS (the job stays ready);
//   - holds under -race.
package acceptance

import (
	"context"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/samhotchkiss/flowbee/client"
	"github.com/samhotchkiss/flowbee/internal/api"
	"github.com/samhotchkiss/flowbee/internal/clock"
	"github.com/samhotchkiss/flowbee/internal/job"
	"github.com/samhotchkiss/flowbee/internal/store"
	"github.com/samhotchkiss/flowbee/internal/testutil"
	"github.com/samhotchkiss/flowbee/internal/ulid"
)

func newCapacityServer(t *testing.T) (*store.Store, string, func()) {
	t.Helper()
	st := testutil.NewStore(t)
	srv := api.New(st, clock.Real{}, ulid.NewMinter(nil), api.Config{
		LeaseTTL: 5 * time.Minute, LongPollWait: 2 * time.Second,
		LeaseTTLS: 300, HeartbeatIntervalS: 30,
	}, "test")
	ts := httptest.NewServer(srv.PrivateHandler())
	return st, ts.URL, ts.Close
}

func seedReady(t *testing.T, st *store.Store, model string) string {
	t.Helper()
	id := ulid.New()
	if _, err := st.SeedJob(context.Background(), store.SeedParams{
		ID: id, Kind: job.KindBuild, Flow: "build", Stage: "build", Role: job.RoleEngWorker,
		BaseSHA: "base", RequiredCapabilities: []string{"role:eng_worker", "model_family:" + model},
		Now: time.Unix(1, 0),
	}); err != nil {
		t.Fatalf("seed %s: %v", model, err)
	}
	return id
}

// TestF6BoxRunsThreeClaudeThreeCodexConcurrently proves the per-model slot
// advertisement: a single box advertising claude:3, codex:3 holds 3 claude + 3
// codex leases at once; the 7th claude lease is gated (slot full) while a fresh
// codex lease still succeeds (the slots are independent per model).
func TestF6BoxRunsThreeClaudeThreeCodexConcurrently(t *testing.T) {
	st, url, closeFn := newCapacityServer(t)
	defer closeFn()
	ctx := context.Background()

	// seed 4 claude + 3 codex ready jobs.
	for i := 0; i < 4; i++ {
		seedReady(t, st, "claude")
	}
	for i := 0; i < 3; i++ {
		seedReady(t, st, "codex")
	}

	// the box registers ONCE advertising both models' concurrency + the accounts.
	c := client.New(url)
	if _, err := c.Register(ctx, client.Registration{
		Identity: "box-a", Host: "h",
		Capabilities: []string{"role:eng_worker", "model_family:claude", "model_family:codex"},
		ModelSlots:   map[string]int{"claude": 3, "codex": 3},
		Weight:       2,
		Accounts: []client.AccountSpecMsg{
			{AccountID: "claude-primary", ModelFamily: "claude", CeilingPct: 90, PreferenceRank: 0},
			{AccountID: "codex-primary", ModelFamily: "codex", CeilingPct: 90, PreferenceRank: 0},
		},
	}); err != nil {
		t.Fatalf("register: %v", err)
	}

	// lease 3 claude jobs CONCURRENTLY — all 3 must be granted (slots claude:3).
	claudeGrants := leaseN(t, ctx, url, "box-a", "claude", 3)
	if len(claudeGrants) != 3 {
		t.Fatalf("expected 3 concurrent claude leases, got %d", len(claudeGrants))
	}
	// and 3 codex jobs concurrently — independent slot budget.
	codexGrants := leaseN(t, ctx, url, "box-a", "codex", 3)
	if len(codexGrants) != 3 {
		t.Fatalf("expected 3 concurrent codex leases, got %d", len(codexGrants))
	}

	// all 6 are live at once: distinct jobs, all in an active-lease state.
	live := map[string]bool{}
	for _, g := range append(append([]client.LeaseGrant{}, claudeGrants...), codexGrants...) {
		if live[g.JobID] {
			t.Fatalf("job %s leased twice", g.JobID)
		}
		live[g.JobID] = true
		j, _ := st.GetJob(ctx, g.JobID)
		if j.State != job.StateLeased {
			t.Fatalf("job %s state=%s want leased (concurrent)", g.JobID, j.State)
		}
	}
	if len(live) != 6 {
		t.Fatalf("expected 6 concurrent leases, got %d", len(live))
	}

	// the 7th claude job: the box's claude:3 slot budget is full -> 204 (gated).
	if _, ok, err := c.Lease(ctx, "box-a", "claude", ""); err != nil || ok {
		t.Fatalf("7th claude lease should be gated (slot full): ok=%v err=%v", ok, err)
	}
	// the claude job stays ready (claimable once a claude slot frees).
	stillReady := 0
	board, _ := st.ReadyCandidates(ctx)
	for range board {
		stillReady++
	}
	if stillReady != 1 {
		t.Fatalf("expected exactly 1 claude job still ready (gated), got %d", stillReady)
	}
}

// TestF6AccountRolloverAndUsageGate proves the ceiling-gated rollover + usage gate:
// the preferred account serves the first claude job; a usage report (POST
// /v1/workers/usage) pushes the preferred over ceiling; the next claude job ROLLS
// OVER to the fallback; pushing the fallback over too gates dispatch (the box waits).
func TestF6AccountRolloverAndUsageGate(t *testing.T) {
	st, url, closeFn := newCapacityServer(t)
	defer closeFn()
	ctx := context.Background()

	c := client.New(url)
	if _, err := c.Register(ctx, client.Registration{
		Identity: "box-a", Host: "h",
		Capabilities: []string{"role:eng_worker", "model_family:claude"},
		ModelSlots:   map[string]int{"claude": 5},
		Accounts: []client.AccountSpecMsg{
			{AccountID: "claude-primary", ModelFamily: "claude", CeilingPct: 90, PreferenceRank: 0},
			{AccountID: "claude-fallback", ModelFamily: "claude", CeilingPct: 90, PreferenceRank: 1},
		},
	}); err != nil {
		t.Fatalf("register: %v", err)
	}

	// first claude job -> bound to the PREFERRED account.
	id1 := seedReady(t, st, "claude")
	g1, ok, err := c.Lease(ctx, "box-a", "claude", "")
	if err != nil || !ok || g1.JobID != id1 {
		t.Fatalf("lease1 ok=%v err=%v job=%s", ok, err, g1.JobID)
	}
	if got := boundAcct(t, st, id1); got != "claude-primary" {
		t.Fatalf("lease1 account=%q want claude-primary", got)
	}

	// usage report pushes the preferred over its ceiling (the ~15min / immediate-on-429
	// surface). POST /v1/workers/usage.
	if s, err := c.ReportUsage(ctx, []client.UsageReport{
		{AccountID: "claude-primary", ModelFamily: "claude", UsagePct: 95},
	}); err != nil || s != 200 {
		t.Fatalf("report usage status=%d err=%v", s, err)
	}

	// next claude job ROLLS OVER to the fallback (the preferred is gated).
	id2 := seedReady(t, st, "claude")
	g2, ok, err := c.Lease(ctx, "box-a", "claude", "")
	if err != nil || !ok || g2.JobID != id2 {
		t.Fatalf("lease2 ok=%v err=%v job=%s", ok, err, g2.JobID)
	}
	if got := boundAcct(t, st, id2); got != "claude-fallback" {
		t.Fatalf("lease2 account=%q want claude-fallback (rollover)", got)
	}

	// a 429-triggered usage report on the fallback pins it out immediately, and a
	// normal over-ceiling report on the primary keeps it gated -> dispatch must WAIT.
	if s, err := c.ReportUsage(ctx, []client.UsageReport{
		{AccountID: "claude-fallback", ModelFamily: "claude", UsagePct: 0, RateLimited: true},
	}); err != nil || s != 200 {
		t.Fatalf("report 429 status=%d err=%v", s, err)
	}

	// the next claude job is GATED: every account is at/over ceiling -> 204 (wait).
	id3 := seedReady(t, st, "claude")
	if _, ok, err := c.Lease(ctx, "box-a", "claude", ""); err != nil || ok {
		t.Fatalf("lease3 should be gated (all accounts at ceiling): ok=%v err=%v", ok, err)
	}
	if j, _ := st.GetJob(ctx, id3); j.State != job.StateReady {
		t.Fatalf("gated job state=%s want ready (waiting)", j.State)
	}

	// the fleet view reflects the gated state (the §G usage gauges).
	rows, err := st.AllAccountUsage(ctx)
	if err != nil {
		t.Fatalf("fleet: %v", err)
	}
	gated := 0
	for _, r := range rows {
		if r.AtCeiling {
			gated++
		}
	}
	if gated != 2 {
		t.Fatalf("expected 2 accounts at ceiling, got %d (%+v)", gated, rows)
	}
}

// leaseN leases n jobs of a model CONCURRENTLY against the same box identity and
// returns the grants (each is heartbeated to keep the lease live). It proves the
// box holds n leases AT ONCE — the per-model slot budget admits exactly n.
func leaseN(t *testing.T, ctx context.Context, url, identity, model string, n int) []client.LeaseGrant {
	t.Helper()
	type res struct {
		g  client.LeaseGrant
		ok bool
	}
	out := make(chan res, n)
	var start sync.WaitGroup
	start.Add(1)
	for i := 0; i < n; i++ {
		go func() {
			start.Wait()
			c := client.New(url)
			g, ok, _ := c.Lease(ctx, identity, model, "")
			out <- res{g, ok}
		}()
	}
	start.Done()
	var grants []client.LeaseGrant
	for i := 0; i < n; i++ {
		r := <-out
		if r.ok {
			grants = append(grants, r.g)
		}
	}
	return grants
}

func boundAcct(t *testing.T, st *store.Store, jobID string) string {
	t.Helper()
	var acct string
	if err := st.DB.QueryRowContext(context.Background(),
		`SELECT bound_account FROM jobs WHERE id = ?`, jobID).Scan(&acct); err != nil {
		t.Fatalf("read bound_account: %v", err)
	}
	return acct
}
