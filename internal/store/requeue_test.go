package store_test

import (
	"context"
	"errors"
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
	// strand it: needs_human, budgets burned, an old epoch.
	if _, err := st.DB.ExecContext(ctx,
		`UPDATE jobs SET state='needs_human', attempts=2, bounces=1, stall_revocations=4, lease_epoch=6 WHERE id='j'`); err != nil {
		t.Fatal(err)
	}

	final, err := st.RequeueJob(ctx, "j", false, now)
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
	if j.Attempts != 0 || j.Bounces != 0 || j.StallRevocations != 0 {
		t.Fatalf("budget not reset: attempts=%d bounces=%d stall_revocations=%d, want 0/0/0", j.Attempts, j.Bounces, j.StallRevocations)
	}
	if j.LeaseEpoch != 7 {
		t.Fatalf("epoch=%d, want 7 (bumped to fence zombies)", j.LeaseEpoch)
	}
	if j.Role != job.RoleEngWorker {
		t.Fatalf("role=%s, want eng_worker (re-armed for a build)", j.Role)
	}
}

// TestRequeueNotFound: requeueing a non-existent job id (e.g. a truncated id, an
// operator typo) returns ErrJobNotFound so the API answers 404, not a misleading 500
// (the §43 symptom — the documented recovery path looked broken on a bad id).
func TestRequeueNotFound(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	_, err := st.RequeueJob(ctx, "01KV9R0HWS-truncated", false, time.Unix(1000, 0))
	if !errors.Is(err, store.ErrJobNotFound) {
		t.Fatalf("requeue of missing job: err=%v, want ErrJobNotFound", err)
	}
}

// TestRequeueRejectsActivelyLeasedJob: requeue is for STRANDED jobs. Re-arming a job
// that holds an ACTIVE lease bumps the epoch and fences the live worker — silently
// discarding its in-flight build. Without force it must be rejected (a mistyped id or a
// just-picked-up job); with force it proceeds (the operator's deliberate override).
func TestRequeueRejectsActivelyLeasedJob(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	now := time.Unix(1000, 0)
	if _, err := st.SeedJob(ctx, store.SeedParams{
		ID: "live", Kind: job.KindBuild, Flow: "build", Stage: "build", Role: job.RoleEngWorker,
		RequiredCapabilities: []string{"role:eng_worker"}, Now: now,
	}); err != nil {
		t.Fatal(err)
	}
	// a worker claims it: now leased (an active lease).
	if _, err := st.ClaimReadyJob(ctx, store.ClaimParams{
		JobID: "live", LeaseID: "L", Identity: "w", ModelFamily: "claude",
		Role: job.RoleEngWorker, Attested: []string{"role:eng_worker"}, TTL: time.Minute, Now: now,
	}); err != nil {
		t.Fatalf("claim: %v", err)
	}

	// unforced requeue must be rejected, leaving the live lease untouched.
	if _, err := st.RequeueJob(ctx, "live", false, now); !errors.Is(err, store.ErrJobActivelyLeased) {
		t.Fatalf("want ErrJobActivelyLeased, got %v", err)
	}
	if j, _ := st.GetJob(ctx, "live"); j.State != job.StateLeased || j.LeaseID != "L" {
		t.Fatalf("rejected requeue mutated the live lease: state=%s lease=%s", j.State, j.LeaseID)
	}

	// forced requeue proceeds (deliberate override) — re-arms to ready, fencing the worker.
	if _, err := st.RequeueJob(ctx, "live", true, now); err != nil {
		t.Fatalf("forced requeue: %v", err)
	}
	if j, _ := st.GetJob(ctx, "live"); j.State != job.StateReady {
		t.Fatalf("forced requeue state=%s want ready", j.State)
	}
}

// TestCancelJob: the operator "give up" complement to requeue — a stranded needs_human
// job cancels to the terminal `cancelled` state, fold-consistently. An active-leased job
// is rejected without force (cancelling fences the live worker); already-terminal is a
// no-op.
func TestCancelJob(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	now := time.Unix(1000, 0)
	mk := func(id, state string) {
		if _, err := st.SeedJob(ctx, store.SeedParams{ID: id, Kind: job.KindBuild, Flow: "build", Stage: "build", Role: job.RoleEngWorker, Now: now}); err != nil {
			t.Fatal(err)
		}
		if state != "ready" {
			st.DB.ExecContext(ctx, `UPDATE jobs SET state=? WHERE id=?`, state, id)
		}
	}

	// a stranded needs_human job cancels to terminal + folds consistently.
	mk("stranded", "needs_human")
	final, err := st.CancelJob(ctx, "stranded", false, now)
	if err != nil || final != job.StateCancelled {
		t.Fatalf("cancel stranded -> %s err=%v, want cancelled", final, err)
	}
	if j, _ := st.GetJob(ctx, "stranded"); j.State != job.StateCancelled {
		t.Fatalf("state=%s want cancelled", j.State)
	}
	assertFoldMatchesProjection(t, st, "stranded")

	// an actively-leased job is rejected without force.
	mk("live", "ready")
	if _, err := st.ClaimReadyJob(ctx, store.ClaimParams{JobID: "live", LeaseID: "L", Identity: "w", ModelFamily: "claude", Role: job.RoleEngWorker, Attested: []string{"role:eng_worker"}, TTL: time.Minute, Now: now}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CancelJob(ctx, "live", false, now); !errors.Is(err, store.ErrJobActivelyLeased) {
		t.Fatalf("cancel of leased job want ErrJobActivelyLeased, got %v", err)
	}
	if _, err := st.CancelJob(ctx, "live", true, now); err != nil { // force fences + cancels
		t.Fatalf("forced cancel: %v", err)
	}
	if j, _ := st.GetJob(ctx, "live"); j.State != job.StateCancelled {
		t.Fatalf("forced cancel state=%s want cancelled", j.State)
	}

	// already-terminal is an idempotent no-op.
	if final, err := st.CancelJob(ctx, "stranded", false, now); err != nil || final != job.StateCancelled {
		t.Fatalf("cancel of already-cancelled -> %s err=%v (must be idempotent)", final, err)
	}
}
