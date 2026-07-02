package worker_test

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/samhotchkiss/flowbee/client"
	"github.com/samhotchkiss/flowbee/internal/api"
	"github.com/samhotchkiss/flowbee/internal/clock"
	"github.com/samhotchkiss/flowbee/internal/job"
	"github.com/samhotchkiss/flowbee/internal/store"
	"github.com/samhotchkiss/flowbee/internal/testutil"
	"github.com/samhotchkiss/flowbee/internal/ulid"
	"github.com/samhotchkiss/flowbee/internal/worker"
)

// TestConflictResolverReleasesLeaseOnAgentFailure is the regression for the live
// conflict_resolver thrash bug (russ #3470/#3498/#3566): a resolving_conflict job's
// task embeds the FULL original diff on top of the normal task text, so its agent
// invocation is far more likely than an ordinary build's to overrun the OS exec argv
// limit — confirmed live on the imac fleet box:
//
//	work cycle: agent cmd: exit status 126: sh: 1: codex: Argument list too long
//
// Before the fix, RunOnceHarness / RunOnceHarnessBundle / RunOnceHarnessRemote all
// returned that error straight up WITHOUT releasing the lease. The worker moved on to
// poll for other work, but the failed job's lease sat there un-heartbeated — the "one
// heartbeat right after claim, then total silence" signature — until the control
// plane's HeartbeatReapAfter (~4x the heartbeat interval, ~4min) finally reaped it via
// the unilateral heartbeat_stale kill, at which point it was handed straight back to a
// resolver that would fail the exact same way, forever (that's the 14-28hr thrash).
//
// This proves the fix: when the agent CLI fails to even run, the harness releases the
// lease AT ONCE (burning an attempt, so a persistently-failing resolution still
// escalates to needs_human after max_attempts) and the job is immediately
// re-claimable — no multi-minute blackout.
func TestConflictResolverReleasesLeaseOnAgentFailure(t *testing.T) {
	mirrorPath, baseSHA := newBareMirror(t)

	st := testutil.NewStore(t)
	srv := api.New(st, clock.Real{}, ulid.NewMinter(nil), api.Config{
		LeaseTTL: time.Minute, LongPollWait: time.Second, LeaseTTLS: 300, HeartbeatIntervalS: 30,
		MirrorPath: mirrorPath,
	}, "test")
	ts := httptest.NewServer(srv.PrivateHandler())
	defer ts.Close()
	ctx := context.Background()

	// Seed a job already diverted to resolving_conflict, the way a real rebase
	// conflict lands it (mirrors TestReportRebaseConflictDivertsToResolver /
	// TestLeaseResolverAccountPin): a conflict_resolver-only job carrying the
	// original branch diff as its patch_diff (the text that gets embedded into the
	// agent's task file/prompt).
	jobID := ulid.New()
	if _, err := st.SeedJob(ctx, store.SeedParams{
		ID: jobID, Kind: job.KindBuild, Flow: "build", Stage: "resolve_conflict",
		Role: job.RoleConflictResolver, RequiredCapabilities: []string{"role:conflict_resolver"},
		BaseSHA: baseSHA, Now: time.Unix(1, 0),
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	const diff = "diff --git a/x b/x\n+conflicting change"
	if _, err := st.DB.ExecContext(ctx,
		`UPDATE jobs SET state='resolving_conflict', patch_diff=? WHERE id=?`, diff, jobID); err != nil {
		t.Fatalf("divert to resolving_conflict: %v", err)
	}
	before, err := st.GetJob(ctx, jobID)
	if err != nil || before.State != job.StateResolvingConflict {
		t.Fatalf("job not set up in resolving_conflict: %+v err=%v", before, err)
	}

	// The agent CLI fails to even start — the exact shape of the real, live failure
	// (exit 126, "Argument list too long") — before it ever does any work or beats
	// its heartbeat ticker once.
	const failingAgentCmd = `echo "sh: 1: codex: Argument list too long" >&2; exit 126`

	out, err := worker.RunOnceHarness(ctx, worker.HarnessConfig{
		BaseURL: ts.URL, Identity: "resolver-1", ModelFamily: "opus",
		Role: string(job.RoleConflictResolver), AgentCmd: failingAgentCmd,
		// RunOnceHarness's Capabilities default (when unset) is hardcoded to
		// role:eng_worker regardless of Role — a resolver must claim its own caps
		// explicitly to match the job's required_capabilities.
		Capabilities: []string{"role:conflict_resolver", "model_family:opus"},
	})
	if err == nil {
		t.Fatalf("expected RunOnceHarness to surface the agent's exec failure, got nil (out=%+v)", out)
	}
	if !out.Got || out.JobID != jobID {
		t.Fatalf("harness did not lease the diverted conflict job: %+v (err=%v)", out, err)
	}

	// THE FIX: the lease must be released at once, not abandoned for the reaper.
	after, err := st.GetJob(ctx, jobID)
	if err != nil {
		t.Fatalf("get job after failure: %v", err)
	}
	if after.LeaseID != "" {
		t.Fatalf("lease NOT released after the agent's exec failure: job still holds lease_id=%q — "+
			"it will sit silent until the server's heartbeat-stale reap instead of failing fast "+
			"(the exact bug behind the 14-28hr conflict_resolver thrash)", after.LeaseID)
	}
	if after.State != job.StateResolvingConflict {
		t.Fatalf("state=%s, want resolving_conflict (re-armed for the next resolver attempt)", after.State)
	}
	if after.Attempts != before.Attempts+1 {
		t.Fatalf("attempts=%d, want %d — a failed agent run must burn an attempt so persistent "+
			"failure escalates to needs_human after max_attempts instead of looping forever",
			after.Attempts, before.Attempts+1)
	}

	// AND it must be immediately re-claimable by the next resolver — no multi-minute
	// blackout waiting on the heartbeat-stale reaper.
	c := client.New(ts.URL)
	if _, err := c.Register(ctx, client.Registration{
		Identity: "resolver-2", Host: "h",
		Capabilities: []string{"role:conflict_resolver", "model_family:opus"},
	}); err != nil {
		t.Fatalf("register resolver-2: %v", err)
	}
	g2, ok, err := c.Lease(ctx, "resolver-2", "opus", "conflict_resolver")
	if err != nil || !ok || g2.JobID != jobID {
		t.Fatalf("job not immediately re-claimable after the release: ok=%v jobID=%s err=%v", ok, g2.JobID, err)
	}
}
