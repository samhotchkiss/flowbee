package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/samhotchkiss/flowbee/internal/job"
	"github.com/samhotchkiss/flowbee/internal/store"
	"github.com/samhotchkiss/flowbee/internal/testutil"
)

// TestRequeueJob: a job stranded in needs_human (escalated from a now-fixed transient
// failure) re-arms to ready with a FRESH attempt/bounce budget and a bumped epoch
// (fencing any zombie), so the fleet picks it up again.
func TestRequeueJob(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	now := time.Unix(1000, 0)

	if _, err := st.SeedJob(ctx, store.SeedParams{
		ID: "j", Kind: job.KindBuild, Flow: "build", Stage: "build",
		Role: job.RoleEngWorker, BaseSHA: "base", Now: now,
	}); err != nil {
		t.Fatal(err)
	}
	// strand it: needs_human, attempts burned, an old epoch.
	if _, err := st.DB.ExecContext(ctx,
		`UPDATE jobs SET state='needs_human', attempts=2, bounces=1, lease_epoch=6 WHERE id='j'`); err != nil {
		t.Fatal(err)
	}

	final, err := st.RequeueJob(ctx, "j", now)
	if err != nil {
		t.Fatalf("RequeueJob: %v", err)
	}
	if final != job.StateReady {
		t.Fatalf("requeue -> %s, want ready", final)
	}
	j, _ := st.GetJob(ctx, "j")
	if j.State != job.StateReady {
		t.Fatalf("state=%s, want ready", j.State)
	}
	if j.Attempts != 0 || j.Bounces != 0 {
		t.Fatalf("budget not reset: attempts=%d bounces=%d, want 0/0", j.Attempts, j.Bounces)
	}
	if j.LeaseEpoch != 7 {
		t.Fatalf("epoch=%d, want 7 (bumped to fence zombies)", j.LeaseEpoch)
	}
	if j.Role != job.RoleEngWorker {
		t.Fatalf("role=%s, want eng_worker (re-armed for a build)", j.Role)
	}
}
