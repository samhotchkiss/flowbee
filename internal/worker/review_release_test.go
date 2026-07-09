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

// TestCodeReviewerReleasesLeaseOnAgentFailure is the regression for the live chronic
// needs_human bounce on russ #3388 (PR #3396, a large doc-sweep diff): the review harness
// embeds the FULL diff inline into the reviewer's brief (task.md), which becomes a SINGLE
// shell argv element passed to the review agent (`codex exec "$(cat "$FLOWBEE_TASK_FILE")"`,
// see cmd/flowbee/fleet.go). Linux's MAX_ARG_STRLEN caps any ONE argv string at ~128KiB
// regardless of the much larger total ARG_MAX (2MiB) — confirmed live: a real 269,705-byte
// review brief for this PR made every reviewer's exec fail with "Argument list too long"
// (exit 126), reproduced directly against a real fleet box (feller) while diagnosing this
// job's ledger.
//
// Before this fix, RunOnceReviewHarness returned that error straight up WITHOUT releasing
// the lease — the review-path sibling of the conflict_resolver bug fixed in commit 7b5cc91
// for the BUILD harnesses (RunOnceHarness/Bundle/Remote), which never touched this review
// harness. The lease sat un-heartbeated (one heartbeat right after claim, then silence) until
// the control plane's heartbeat-stale reap (~4min) handed it to the next reviewer, who failed
// identically — the exact "review_claimed -> silence -> stall_escalated -> operator reset ->
// repeat" cycle the job's ledger shows across 250+ events and multiple babysitter shifts.
//
// This proves the fix: when the review agent CLI fails to even run, the harness releases the
// lease AT ONCE (as FAILED, burning an attempt) so a persistent failure fails fast and
// escalates to needs_human after max_attempts instead of thrashing in ~4min blackout cycles
// forever, and the job is immediately re-claimable for a clean retry.
func TestCodeReviewerReleasesLeaseOnAgentFailure(t *testing.T) {
	ctx := context.Background()
	st := testutil.NewStore(t)
	clk := clock.NewFake(time.Unix(1000, 0))
	srv := api.New(st, clk, ulid.NewMinter(nil), api.Config{
		LeaseTTL: time.Minute, LongPollWait: time.Second, LeaseTTLS: 300, HeartbeatIntervalS: 30,
	}, "test")
	ts := httptest.NewServer(srv.PrivateHandler())
	defer ts.Close()

	jobID := ulid.New()
	if _, err := st.SeedJob(ctx, store.SeedParams{
		ID: jobID, Kind: job.KindBuild, Flow: "build", Stage: "code_review",
		Role: job.RoleCodeReviewer, RequiredCapabilities: []string{"role:code_reviewer"},
		BaseSHA: "base", Now: clk.Now(),
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	// A code_reviewer job is only an offerable lease candidate once its diff is set, the
	// state is review_pending, and CI is reconciled green (mirrors
	// TestLeaseReviewAccountPin in internal/api/control_test.go).
	const hugeDiff = "diff --git a/x b/x\n+the full PR diff, in this real bug ~266KB of it"
	if _, err := st.DB.ExecContext(ctx,
		`UPDATE jobs SET state='review_pending', head_sha='head', patch_diff=? WHERE id=?`, hugeDiff, jobID); err != nil {
		t.Fatalf("set review_pending + diff: %v", err)
	}
	if _, err := st.DB.ExecContext(ctx,
		`INSERT INTO domain_b_facts (job_id, pr_exists, pr_number, head_sha, base_sha, ci_green, merged)
		 VALUES (?,1,1,'head','base',1,0)`, jobID); err != nil {
		t.Fatalf("seed domain_b_facts: %v", err)
	}
	before, err := st.GetJob(ctx, jobID)
	if err != nil || before.State != job.StateReviewPending {
		t.Fatalf("job not set up in review_pending: %+v err=%v", before, err)
	}

	// The agent CLI fails to even start — the exact shape of the real, live failure (exit
	// 126, "Argument list too long") — before it ever does any work or beats its heartbeat
	// ticker once.
	const failingAgentCmd = `echo "sh: 1: codex: Argument list too long" >&2; exit 126`

	out, err := worker.RunOnceReviewHarness(ctx, worker.HarnessConfig{
		BaseURL: ts.URL, Identity: "reviewer-1", ModelFamily: "opus",
		Role: string(job.RoleCodeReviewer), AgentCmd: failingAgentCmd,
		Capabilities: []string{"role:code_reviewer", "model_family:opus"},
	})
	if err == nil {
		t.Fatalf("expected RunOnceReviewHarness to surface the agent's exec failure, got nil (out=%+v)", out)
	}
	if !out.Got || out.JobID != jobID {
		t.Fatalf("harness did not lease the review job: %+v (err=%v)", out, err)
	}

	// THE FIX: the lease must be released at once, not abandoned for the reaper.
	after, err := st.GetJob(ctx, jobID)
	if err != nil {
		t.Fatalf("get job after failure: %v", err)
	}
	if after.LeaseID != "" {
		t.Fatalf("lease NOT released after the agent's exec failure: job still holds lease_id=%q — "+
			"it will sit silent until the server's heartbeat-stale reap instead of failing fast "+
			"(the exact bug behind russ #3388's chronic needs_human bounce)", after.LeaseID)
	}
	if after.State != job.StateReviewPending {
		t.Fatalf("state=%s, want review_pending (re-armed for the next reviewer attempt)", after.State)
	}
	if after.Attempts != before.Attempts+1 {
		t.Fatalf("attempts=%d, want %d — a failed review agent run must burn an attempt so persistent "+
			"failure escalates to needs_human after max_attempts instead of looping forever",
			after.Attempts, before.Attempts+1)
	}

	// AND it must be immediately re-claimable by the next reviewer — no multi-minute blackout
	// waiting on the heartbeat-stale reaper.
	c := client.New(ts.URL)
	if _, err := c.Register(ctx, client.Registration{
		Identity: "reviewer-2", Host: "h",
		Capabilities: []string{"role:code_reviewer", "model_family:opus"},
	}); err != nil {
		t.Fatalf("register reviewer-2: %v", err)
	}
	g2, ok, err := c.Lease(ctx, "reviewer-2", "opus", "code_reviewer")
	if err != nil || !ok || g2.JobID != jobID {
		t.Fatalf("job not immediately re-claimable after the release: ok=%v jobID=%s err=%v", ok, g2.JobID, err)
	}
}
