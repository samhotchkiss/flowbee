package store_test

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/samhotchkiss/flowbee/internal/job"
	"github.com/samhotchkiss/flowbee/internal/ledger"
	"github.com/samhotchkiss/flowbee/internal/store"
	"github.com/samhotchkiss/flowbee/internal/testutil"
)

// TestCIFailBouncesThenEscalates pins the CI-red handling: a review_pending build
// whose CI is definitively red bounces back to build (rebuild), and after
// max_bounces escalates to needs_human — never silently parks.
// TestGreenMainHoldsCIFailBounce: the green-main invariant (russ #214). A review_pending PR
// whose CI is red is NOT bounced when MAIN itself is red — the failure is inherited, not the
// PR's fault, so it's HELD in review_pending instead of being penalized to needs_human. When
// main is green, the same red CI bounces (it IS this change that broke).
func TestGreenMainHoldsCIFailBounce(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	now := time.Unix(1000, 0)

	seedReview := func(id string) {
		if _, err := st.SeedJob(ctx, store.SeedParams{
			ID: id, Kind: job.KindBuild, Flow: "build", Stage: "build", Role: job.RoleEngWorker, Now: now,
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := st.DB.ExecContext(ctx, `UPDATE jobs SET state='review_pending' WHERE id=?`, id); err != nil {
			t.Fatal(err)
		}
	}

	// main RED: a red-CI PR is HELD (stays review_pending), not bounced.
	seedReview("held")
	if _, err := st.ApplyReconciledPR(ctx, "held",
		store.ReconciledPR{Number: 1, HeadSHA: "h", BaseSHA: "b", CIFailed: true, MainCIRed: true}, now); err != nil {
		t.Fatal(err)
	}
	if j, _ := st.GetJob(ctx, "held"); j.State != job.StateReviewPending || j.Bounces != 0 {
		t.Fatalf("red main: state=%s bounces=%d, want review_pending/0 (held, not bounced over a red main)", j.State, j.Bounces)
	}

	// main GREEN: the same red-CI PR IS bounced (this change is genuinely broken).
	seedReview("bounced")
	if _, err := st.ApplyReconciledPR(ctx, "bounced",
		store.ReconciledPR{Number: 2, HeadSHA: "h2", BaseSHA: "b2", CIFailed: true, MainCIRed: false}, now); err != nil {
		t.Fatal(err)
	}
	if j, _ := st.GetJob(ctx, "bounced"); j.State != job.StateReady || j.Bounces != 1 {
		t.Fatalf("green main: state=%s bounces=%d, want ready/1 (bounced to rebuild)", j.State, j.Bounces)
	}
}

func TestCIFailBouncesThenEscalates(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	now := time.Unix(1000, 0)

	if _, err := st.SeedJob(ctx, store.SeedParams{
		ID: "j", Kind: job.KindBuild, Flow: "build", Stage: "build", Role: job.RoleEngWorker, Now: now,
	}); err != nil {
		t.Fatal(err)
	}
	// pin max_bounces=3 for this test so it asserts the ESCALATION MECHANISM, not the
	// shipped default (which is the higher total-bounce backstop now that the per-
	// review-node rejection cap is the primary review-loop trigger).
	if _, err := st.DB.ExecContext(ctx, `UPDATE jobs SET max_bounces=3 WHERE id='j'`); err != nil {
		t.Fatal(err)
	}
	toReview := func() {
		// mimic the real build->review_pending transition: the cap flips to the
		// reviewer role. The CI-fail bounce MUST reset it back to eng_worker, or the
		// re-armed `ready` build is unleaseable (no builder matches role:code_reviewer).
		if _, err := st.DB.ExecContext(ctx,
			`UPDATE jobs SET state='review_pending', required_capabilities='["role:code_reviewer"]' WHERE id='j'`); err != nil {
			t.Fatal(err)
		}
	}
	failPR := store.ReconciledPR{Number: 1, HeadSHA: "h1", BaseSHA: "b1", CIFailed: true}

	// max_bounces defaults to 3: three rebuilds, then escalate on the fourth.
	for i := 1; i <= 3; i++ {
		toReview()
		if _, err := st.ApplyReconciledPR(ctx, "j", failPR, now); err != nil {
			t.Fatal(err)
		}
		j, _ := st.GetJob(ctx, "j")
		if j.State != job.StateReady || j.Bounces != i {
			t.Fatalf("bounce %d: state=%s bounces=%d, want ready/%d", i, j.State, j.Bounces, i)
		}
		if j.Role != job.RoleEngWorker {
			t.Fatalf("bounce %d: role=%s, want eng_worker (re-armed for rebuild)", i, j.Role)
		}
		// the re-armed ready build MUST require the builder cap, not the reviewer cap —
		// else no worker can claim it and it wedges (the live #2217 stall).
		if len(j.RequiredCapabilities) != 1 || j.RequiredCapabilities[0] != "role:eng_worker" {
			t.Fatalf("bounce %d: required_capabilities=%v, want [role:eng_worker] (else unleaseable)", i, j.RequiredCapabilities)
		}
	}
	toReview()
	if _, err := st.ApplyReconciledPR(ctx, "j", failPR, now); err != nil {
		t.Fatal(err)
	}
	j, _ := st.GetJob(ctx, "j")
	if j.State != job.StateNeedsHuman {
		t.Fatalf("after max_bounces CI failures state=%s, want needs_human", j.State)
	}
}

// TestFoldEqualsProjectionAcrossCIBounce drives a build through a REAL CI-fail bounce
// (seed → claim → result → reconciled CI red → re-armed ready) entirely through store
// methods — every step appends a ledger event — then asserts Fold(events) deep-equals
// the projection on the re-armed field set, crucially RequiredCapabilities. That is the
// field the stranded-ready bug (#2217) diverged on: the projection reset the build cap
// but an earlier Fold did not, so a projection-RESYNC (which replays the ledger) folded
// the job back to role:code_reviewer caps no builder could claim — wedging it. No raw
// UPDATEs here: this is exactly the replay a resync performs, so a fold that forgets to
// reset the caps strands the re-armed job in THIS test, not in production.
func TestFoldEqualsProjectionAcrossCIBounce(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()

	seedBuild(t, st, "j")
	ls, err := claim(st, "j", "w1")
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if _, err := st.Heartbeat(ctx, store.HeartbeatParams{JobID: "j", Epoch: ls.Epoch, Now: time.Unix(3000, 0)}); err != nil {
		t.Fatalf("heartbeat: %v", err)
	}
	if _, err := st.Result(ctx, store.ResultParams{JobID: "j", Epoch: ls.Epoch, Now: time.Unix(3200, 0)}); err != nil {
		t.Fatalf("result: %v", err)
	}
	// reconciled CI is definitively red → KindReviewBounced → ready (caps MUST reset).
	if _, err := st.ApplyReconciledPR(ctx, "j",
		store.ReconciledPR{Number: 1, HeadSHA: "h1", BaseSHA: "b1", CIFailed: true}, time.Unix(3500, 0)); err != nil {
		t.Fatalf("reconcile ci-fail: %v", err)
	}

	proj, _ := st.GetJob(ctx, "j")
	evs, _ := st.LoadEvents(ctx, "j")
	folded, err := ledger.Fold(evs)
	if err != nil {
		t.Fatalf("fold: %v", err)
	}

	// the re-armed build must be a leasable eng_worker build in BOTH the live projection
	// and the replay — or a resync strands it (no builder matches role:code_reviewer).
	if proj.State != job.StateReady {
		t.Fatalf("projection state=%s, want ready (re-armed build)", proj.State)
	}
	if folded.State != proj.State {
		t.Fatalf("state: fold=%s proj=%s", folded.State, proj.State)
	}
	if folded.Role != proj.Role || folded.Role != job.RoleEngWorker {
		t.Fatalf("role: fold=%s proj=%s want eng_worker", folded.Role, proj.Role)
	}
	if !reflect.DeepEqual(folded.RequiredCapabilities, proj.RequiredCapabilities) {
		t.Fatalf("required_capabilities DIVERGED on the bounce: fold=%v proj=%v (the stranded-ready bug class)",
			folded.RequiredCapabilities, proj.RequiredCapabilities)
	}
	if len(proj.RequiredCapabilities) != 1 || proj.RequiredCapabilities[0] != "role:eng_worker" {
		t.Fatalf("re-armed caps=%v, want [role:eng_worker] (else unleaseable)", proj.RequiredCapabilities)
	}
	if folded.Bounces != proj.Bounces || folded.Bounces != 1 {
		t.Fatalf("bounces: fold=%d proj=%d want 1", folded.Bounces, proj.Bounces)
	}
	if folded.LeaseEpoch != proj.LeaseEpoch {
		t.Fatalf("epoch: fold=%d proj=%d", folded.LeaseEpoch, proj.LeaseEpoch)
	}
	if folded.BaseSHA != proj.BaseSHA {
		t.Fatalf("base_sha: fold=%q proj=%q", folded.BaseSHA, proj.BaseSHA)
	}
}

// TestCIPendingDoesNotBounce: a not-yet-green (pending) PR must NOT bounce — only a
// DEFINITIVE failure does. CIFailed=false models pending.
func TestCIPendingDoesNotBounce(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	now := time.Unix(1000, 0)
	if _, err := st.SeedJob(ctx, store.SeedParams{
		ID: "p", Kind: job.KindBuild, Flow: "build", Stage: "build", Role: job.RoleEngWorker, Now: now,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB.ExecContext(ctx, `UPDATE jobs SET state='review_pending' WHERE id='p'`); err != nil {
		t.Fatal(err)
	}
	if _, err := st.ApplyReconciledPR(ctx, "p", store.ReconciledPR{Number: 2, HeadSHA: "h", BaseSHA: "b", CIFailed: false}, now); err != nil {
		t.Fatal(err)
	}
	j, _ := st.GetJob(ctx, "p")
	if j.State != job.StateReviewPending {
		t.Fatalf("pending CI must not bounce: state=%s want review_pending", j.State)
	}
}
