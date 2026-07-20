package reconcile_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/samhotchkiss/flowbee/internal/clock"
	gh "github.com/samhotchkiss/flowbee/internal/github"
	"github.com/samhotchkiss/flowbee/internal/ledger"
	"github.com/samhotchkiss/flowbee/internal/reconcile"
	"github.com/samhotchkiss/flowbee/internal/store"
	"github.com/samhotchkiss/flowbee/internal/testutil"
)

func seedOwnedEpic(t *testing.T, st *store.Store, id, branch string, now time.Time) {
	t.Helper()
	if err := st.AddEpicRun(context.Background(), store.EpicRun{
		ID: id, Repo: "russ", Branch: branch, BuilderModelFamily: "codex",
	}, 1, now); err != nil {
		t.Fatalf("seed epic: %v", err)
	}
}

func TestV2SweepPersistsExactOwnedArtifactAndStableFirstGreenClock(t *testing.T) {
	ctx := context.Background()
	st := testutil.NewStore(t)
	st.EnableEpicReviewHandoffV2 = true
	now := time.Date(2026, 7, 19, 6, 0, 0, 0, time.UTC)
	seedOwnedEpic(t, st, "epic-owned", "epic/owned", now.Add(-time.Minute))

	f := gh.NewFake()
	f.SetBranchProtection("main", gh.Protection{RequiredChecks: []string{"unit"}})
	f.SetPR(gh.PullRequest{
		Number: 4950, HeadRefName: "epic/owned", HeadRefOid: "head-1", BaseRefOid: "base-1",
		CIRollup: gh.CISuccess, PassedChecks: []string{"unit"}, UpdatedAt: now.Add(-time.Minute),
	})
	clk := clock.NewFake(now)
	rec := reconcile.NewForRepo("russ", st, f, clk, nil).WithIntake(nil, "main")
	if _, err := rec.Sweep(ctx); err != nil {
		t.Fatalf("first sweep: %v", err)
	}
	var prNumber int
	var artifactBranch, artifactCI, deliveryState, firstGreen string
	if err := st.DB.QueryRowContext(ctx, `SELECT a.pr_number,a.branch,a.ci_state,d.state,a.ci_green_observed_at
		FROM epic_artifacts a JOIN epic_deliveries d ON d.epic_id=a.epic_id
		WHERE a.epic_id='epic-owned'`).Scan(
		&prNumber, &artifactBranch, &artifactCI, &deliveryState, &firstGreen); err != nil {
		t.Fatal(err)
	}
	if prNumber != 4950 || artifactBranch != "epic/owned" || artifactCI != "green" ||
		deliveryState != "awaiting_review_dispatch" || firstGreen == "" {
		t.Fatalf("artifact pr=%d branch=%q ci=%q delivery=%q green_at=%q",
			prNumber, artifactBranch, artifactCI, deliveryState, firstGreen)
	}

	clk.Advance(4 * time.Minute)
	if _, err := rec.Sweep(ctx); err != nil {
		t.Fatalf("repeat sweep: %v", err)
	}
	var repeatedGreen string
	if err := st.DB.QueryRowContext(ctx, `SELECT ci_green_observed_at FROM epic_artifacts WHERE epic_id='epic-owned'`).Scan(&repeatedGreen); err != nil {
		t.Fatal(err)
	}
	if repeatedGreen != firstGreen {
		t.Fatalf("unchanged green SHA postponed recovery clock: first=%q repeated=%q", firstGreen, repeatedGreen)
	}
	var jobs int
	if err := st.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM jobs WHERE repo='russ' AND pr_number=4950`).Scan(&jobs); err != nil || jobs != 0 {
		t.Fatalf("owned artifact entered legacy job domain: jobs=%d err=%v", jobs, err)
	}
}

func TestV2ArtifactGreenRequiresAggregateRealRequiredAndCompleteContexts(t *testing.T) {
	cases := []struct {
		name string
		pr   gh.PullRequest
	}{
		{
			name: "aggregate not successful",
			pr: gh.PullRequest{CIRollup: gh.CIPending, CIHasRealSuccess: true,
				PassedChecks: []string{"unit", "integration"}},
		},
		{
			name: "required check missing",
			pr: gh.PullRequest{CIRollup: gh.CISuccess, CIHasRealSuccess: true,
				PassedChecks: []string{"unit"}},
		},
		{
			name: "contexts truncated",
			pr: gh.PullRequest{CIRollup: gh.CISuccess, CIHasRealSuccess: true,
				PassedChecks: []string{"unit", "integration"}, CheckContextsTruncated: true},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			st := testutil.NewStore(t)
			st.EnableEpicReviewHandoffV2 = true
			now := time.Date(2026, 7, 19, 7, 0, 0, 0, time.UTC)
			seedOwnedEpic(t, st, "strict", "epic/strict", now.Add(-time.Minute))
			f := gh.NewFake()
			f.SetBranchProtection("main", gh.Protection{RequiredChecks: []string{"unit", "integration"}})
			tc.pr.Number, tc.pr.HeadRefName = 5010, "epic/strict"
			tc.pr.HeadRefOid, tc.pr.BaseRefOid, tc.pr.UpdatedAt = "head", "base", now
			f.SetPR(tc.pr)
			rec := reconcile.NewForRepo("russ", st, f, clock.NewFake(now), nil).WithIntake(nil, "main")
			if _, err := rec.Sweep(ctx); err != nil {
				t.Fatal(err)
			}
			var ci, state string
			if err := st.DB.QueryRowContext(ctx, `SELECT ci_state,state FROM epic_deliveries WHERE epic_id='strict'`).Scan(&ci, &state); err != nil {
				t.Fatal(err)
			}
			if ci == "green" || state == "awaiting_review_dispatch" {
				t.Fatalf("unsafe green accepted: ci=%q state=%q", ci, state)
			}
		})
	}
}

func TestV2SweepAbsorbsPreexistingAdoptedReviewWithoutDuplicate(t *testing.T) {
	ctx := context.Background()
	st := testutil.NewStore(t)
	now := time.Date(2026, 7, 19, 8, 0, 0, 0, time.UTC)
	seedOwnedEpic(t, st, "collision", "epic/collision", now.Add(-time.Minute))
	adoptedID, _, err := st.AdoptPRForReview(ctx, "russ", 4951, "base", "head",
		"diff --git a/x b/x\n", false, false, false, false, now, now)
	if err != nil || adoptedID == "" {
		t.Fatalf("seed adopted collision id=%q err=%v", adoptedID, err)
	}
	legacyLease, err := st.ClaimReviewJob(ctx, store.ClaimReviewParams{
		JobID: adoptedID, LeaseID: "legacy-review-lease", Identity: "legacy-reviewer",
		ModelFamily: "grok", Attested: []string{"role:code_reviewer"},
		TTL: 10 * time.Minute, Now: now,
	})
	if err != nil {
		t.Fatalf("claim legacy review: %v", err)
	}
	if err := st.ArmTimer(ctx, "legacy-review-timer", adoptedID, store.TimerNoEligibleWorker, now.Add(time.Minute), legacyLease.Epoch); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB.ExecContext(ctx, `INSERT INTO outbox (job_id,action,head_sha,status)
		VALUES (?, 'mergeQueue.enqueue', 'head', 'pending')`, adoptedID); err != nil {
		t.Fatal(err)
	}
	st.EnableEpicReviewHandoffV2 = true

	f := gh.NewFake()
	f.SetBranchProtection("main", gh.Protection{RequiredChecks: []string{"unit"}})
	f.SetPR(gh.PullRequest{
		Number: 4951, HeadRefName: "epic/collision", HeadRefOid: "head", BaseRefOid: "base",
		CIRollup: gh.CISuccess, PassedChecks: []string{"unit"}, UpdatedAt: now,
		Labels: []string{"needs-claude"},
	})
	clk := clock.NewFake(now)
	rec := reconcile.NewForRepo("russ", st, f, clk, nil).WithIntake(nil, "main")
	if _, err := rec.Sweep(ctx); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	for _, call := range f.Calls() {
		if strings.HasPrefix(call, "PullRequest") {
			t.Fatalf("owned labeled PR reached legacy adopt API: %q", call)
		}
	}
	var count, adopted, leaseEpoch int
	var id, domain, deliveryID, state string
	if err := st.DB.QueryRowContext(ctx, `SELECT COUNT(*),MIN(id),MIN(workflow_domain),MIN(epic_delivery_id),MIN(adopted),MIN(state),MIN(lease_epoch)
		FROM jobs WHERE repo='russ' AND pr_number=4951`).Scan(&count, &id, &domain, &deliveryID, &adopted, &state, &leaseEpoch); err != nil {
		t.Fatal(err)
	}
	if count != 1 || id != adoptedID || domain != "epic_v2_absorbed" || deliveryID != "collision" || adopted != 1 || state != "cancelled" || leaseEpoch != legacyLease.Epoch+1 {
		t.Fatalf("absorbed count=%d id=%q domain=%q delivery=%q adopted=%d state=%q epoch=%d",
			count, id, domain, deliveryID, adopted, state, leaseEpoch)
	}
	var leaseID, leaseEnded, outboxState string
	var timerFired int
	if err := st.DB.QueryRowContext(ctx, `SELECT COALESCE(lease_id,'') FROM jobs WHERE id=?`, adoptedID).Scan(&leaseID); err != nil {
		t.Fatal(err)
	}
	if err := st.DB.QueryRowContext(ctx, `SELECT COALESCE(ended_at,'') FROM leases WHERE lease_id='legacy-review-lease'`).Scan(&leaseEnded); err != nil {
		t.Fatal(err)
	}
	if err := st.DB.QueryRowContext(ctx, `SELECT status FROM outbox WHERE job_id=?`, adoptedID).Scan(&outboxState); err != nil {
		t.Fatal(err)
	}
	if err := st.DB.QueryRowContext(ctx, `SELECT fired FROM timers WHERE id='legacy-review-timer'`).Scan(&timerFired); err != nil {
		t.Fatal(err)
	}
	if leaseID != "" || leaseEnded == "" || outboxState != "abandoned" || timerFired != 1 {
		t.Fatalf("legacy fence lease=%q ended=%q outbox=%q timer=%d", leaseID, leaseEnded, outboxState, timerFired)
	}
	events, err := st.LoadEvents(ctx, adoptedID)
	if err != nil || len(events) == 0 || events[len(events)-1].Kind != ledger.KindEpicAdoptAbsorbed {
		t.Fatalf("absorption events=%+v err=%v", events, err)
	}
	folded, err := ledger.Fold(events)
	if err != nil || folded.State != "cancelled" || folded.LeaseEpoch != leaseEpoch || folded.LeaseID != "" || folded.Verdict != nil {
		t.Fatalf("folded legacy state=%q epoch=%d lease=%q verdict=%+v err=%v", folded.State, folded.LeaseEpoch, folded.LeaseID, folded.Verdict, err)
	}

	clk.Advance(6 * time.Minute)
	rep, err := st.ReconcileEpicReviewHandoffs(ctx, clk.Now(), 5*time.Minute)
	if err != nil || rep.Dispatched != 1 {
		t.Fatalf("materialize absorbed job: rep=%+v err=%v", rep, err)
	}
	if err := st.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM jobs WHERE repo='russ' AND pr_number=4951`).Scan(&count); err != nil || count != 2 {
		t.Fatalf("materialization history/native count=%d err=%v", count, err)
	}
	var reviewJob, deliveryState string
	if err := st.DB.QueryRowContext(ctx, `SELECT review_job_id,state FROM epic_deliveries WHERE epic_id='collision'`).Scan(&reviewJob, &deliveryState); err != nil {
		t.Fatal(err)
	}
	if reviewJob == adoptedID || reviewJob == "" || deliveryState != "review_queued" {
		t.Fatalf("review job=%q state=%q must be separate from absorbed %q", reviewJob, deliveryState, adoptedID)
	}
	if candidates, err := st.ReviewPendingCandidates(ctx); err != nil || len(candidates) != 1 || candidates[0].JobID != reviewJob {
		t.Fatalf("materialized candidates=%+v err=%v", candidates, err)
	}
}

func TestV2OwnershipRejectsForkAndFlagOffPreservesLegacyAdoption(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 19, 9, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name, id       string
		flag, fork     bool
		headMissing    bool
		wantArtifactPR int
		wantJobs       int
	}{
		{name: "fork lookalike is not v2 intake", id: "fork", flag: true, fork: true, wantJobs: 0},
		{name: "same repo missing head identity", id: "missing", flag: true, headMissing: true, wantJobs: 0},
		{name: "flag off", id: "off", flag: false, wantJobs: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := testutil.NewStore(t)
			st.EnableEpicReviewHandoffV2 = tc.flag
			branch := "epic/" + tc.id
			seedOwnedEpic(t, st, tc.id, branch, now.Add(-time.Minute))
			f := gh.NewFake()
			headRefName := branch
			if tc.headMissing {
				headRefName = ""
			}
			f.SetPR(gh.PullRequest{
				Number: 5100, HeadRefName: headRefName, IsCrossRepository: tc.fork,
				HeadRefOid: "head", BaseRefOid: "base", CIRollup: gh.CISuccess,
				UpdatedAt: now, Labels: []string{"needs-claude"},
			})
			f.SetPRDiff(5100, "diff --git a/x b/x\n")
			rec := reconcile.NewForRepo("russ", st, f, clock.NewFake(now), nil)
			if _, err := rec.Sweep(ctx); err != nil {
				t.Fatal(err)
			}
			var boundPR, jobs int
			if err := st.DB.QueryRowContext(ctx, `SELECT COALESCE(pr_number,0) FROM epic_artifacts WHERE epic_id=?`, tc.id).Scan(&boundPR); err != nil {
				t.Fatal(err)
			}
			if err := st.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM jobs WHERE repo='russ' AND pr_number=5100`).Scan(&jobs); err != nil {
				t.Fatal(err)
			}
			if boundPR != tc.wantArtifactPR || jobs != tc.wantJobs {
				t.Fatalf("bound_pr=%d jobs=%d want %d/%d", boundPR, jobs, tc.wantArtifactPR, tc.wantJobs)
			}
		})
	}
}
