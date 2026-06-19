package store

import (
	"context"
	"testing"
	"time"

	"github.com/samhotchkiss/flowbee/internal/job"
	"github.com/samhotchkiss/flowbee/internal/ledger"
)

// TestRequeueZeroesCountersInFold: an operator requeue zeroes attempts + bounces in
// the live projection via a direct UPDATE. The requeue event must carry that reset so
// a rebuild-from-ledger reproduces it — otherwise the fold keeps the pre-requeue
// counts and the job could re-escalate prematurely after a DR rebuild. (Same latent
// class as escalation_reason/over_budget; this closes the counters half of it.)
func TestRequeueZeroesCountersInFold(t *testing.T) {
	st := newLiveStore(t)
	ctx := context.Background()
	now := time.Unix(1000, 0)
	if _, err := st.SeedJob(ctx, SeedParams{
		ID: "p", Kind: job.KindBuild, Flow: "build", Stage: "build",
		Role: job.RoleEngWorker, RequiredCapabilities: []string{"role:eng_worker"}, Now: now,
	}); err != nil {
		t.Fatal(err)
	}
	// burn 2 attempts via penalty release (re-arms ready each time, attempts < 5).
	for i := 0; i < 2; i++ {
		ls, err := st.ClaimReadyJob(ctx, ClaimParams{
			JobID: "p", LeaseID: "l" + string(rune('a'+i)), Identity: "w", ModelFamily: "codex",
			Role: job.RoleEngWorker, Attested: []string{"role:eng_worker"}, TTL: time.Minute, Now: now,
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := st.Release(ctx, ReleaseParams{JobID: "p", Epoch: ls.Epoch, Now: now}); err != nil {
			t.Fatal(err)
		}
	}
	if j, _ := st.GetJob(ctx, "p"); j.Attempts != 2 {
		t.Fatalf("setup: want attempts=2, got %d", j.Attempts)
	}

	if _, err := st.RequeueJob(ctx, "p", now); err != nil {
		t.Fatalf("requeue: %v", err)
	}
	proj, _ := st.GetJob(ctx, "p")
	evs, _ := st.LoadEvents(ctx, "p")
	folded, err := ledger.Fold(evs)
	if err != nil {
		t.Fatalf("fold: %v", err)
	}
	if proj.Attempts != 0 || proj.Bounces != 0 {
		t.Fatalf("requeue must zero live counters: attempts=%d bounces=%d", proj.Attempts, proj.Bounces)
	}
	if folded.Attempts != proj.Attempts {
		t.Errorf("fold attempts=%d != projection %d (requeue reset not folded)", folded.Attempts, proj.Attempts)
	}
	if folded.Bounces != proj.Bounces {
		t.Errorf("fold bounces=%d != projection %d", folded.Bounces, proj.Bounces)
	}
}
