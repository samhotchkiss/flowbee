package reconcile_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/samhotchkiss/flowbee/internal/clock"
	gh "github.com/samhotchkiss/flowbee/internal/github"
	"github.com/samhotchkiss/flowbee/internal/job"
	"github.com/samhotchkiss/flowbee/internal/reconcile"
	"github.com/samhotchkiss/flowbee/internal/store"
	"github.com/samhotchkiss/flowbee/internal/testutil"
)

func seed(t *testing.T, st *store.Store, id string, pr int) {
	t.Helper()
	ctx := context.Background()
	if _, err := st.SeedJob(ctx, store.SeedParams{
		ID: id, Kind: job.KindBuild, Flow: "build", Stage: "build",
		Role: job.RoleEngWorker, BaseSHA: "base0",
		RequiredCapabilities: []string{"role:eng_worker"}, Now: time.Unix(1000, 0),
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := st.BindPRNumber(ctx, id, pr); err != nil {
		t.Fatalf("bind: %v", err)
	}
}

// TestSweepMatchesScriptedRepo: a sweep populates Domain-B columns to match the
// scripted fakeGitHub repo, and records the rate-limit gauge (I-14). The DONE-WHEN
// "sweep populates Domain-B columns to match a test repo".
func TestSweepMatchesScriptedRepo(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	seed(t, st, "jx", 100)
	seed(t, st, "jy", 101)

	f := gh.NewFake()
	f.SetPR(gh.PullRequest{Number: 100, HeadRefOid: "ha", BaseRefOid: "ba", CIRollup: gh.CISuccess, UpdatedAt: time.Unix(10, 0)})
	f.SetPR(gh.PullRequest{Number: 101, HeadRefOid: "hb", BaseRefOid: "bb", CIRollup: gh.CIFailure, UpdatedAt: time.Unix(11, 0)})
	f.SetRateLimit(gh.RateLimit{Limit: 5000, Remaining: 4321, ResetAt: time.Unix(99999, 0)})

	rec := reconcile.New(st, f, clock.NewFake(time.Unix(20, 0)), nil)
	if _, err := rec.Sweep(ctx); err != nil {
		t.Fatalf("sweep: %v", err)
	}

	src := store.DBFactSource{DB: st.DB}
	fa, _, _ := src.Facts(ctx, "jx")
	if fa.HeadSHA != "ha" || fa.BaseSHA != "ba" || !fa.CIGreen || !fa.PRExists {
		t.Fatalf("jx facts: %+v", fa)
	}
	fb, _, _ := src.Facts(ctx, "jy")
	if fb.HeadSHA != "hb" || fb.CIGreen {
		t.Fatalf("jy facts: %+v", fb)
	}
	g, _ := st.RateLimit(ctx)
	if g.Remaining != 4321 || g.Limit != 5000 {
		t.Fatalf("budget gauge not recorded: %+v", g)
	}
}

func TestRequiredChecksAuthorizeOnlyCompleteCurrentHead(t *testing.T) {
	ctx := context.Background()
	now := time.Unix(30, 0)

	t.Run("late required failure never reaches review or merge", func(t *testing.T) {
		st := testutil.NewStore(t)
		seed(t, st, "late-fail", 271)
		if _, err := st.DB.ExecContext(ctx, `
			UPDATE jobs
			   SET state='review_pending', required_capabilities='["role:code_reviewer"]',
			       head_sha='visible-head', base_sha='visible-base'
			 WHERE id='late-fail'`); err != nil {
			t.Fatal(err)
		}

		f := gh.NewFake()
		f.SetBranchProtection("main", gh.Protection{RequiredChecks: []string{"fast", "late"}})
		f.SetPR(gh.PullRequest{
			Number: 271, HeadRefOid: "visible-head", BaseRefOid: "visible-base",
			CIRollup: gh.CISuccess, CIHasRealSuccess: true,
			PassedChecks: []string{"fast"}, UpdatedAt: time.Unix(10, 0),
		})
		rec := reconcile.New(st, f, clock.NewFake(now), nil)
		if _, err := rec.Sweep(ctx); err != nil {
			t.Fatalf("sweep partial: %v", err)
		}
		if candidates, err := st.ReviewPendingCandidates(ctx); err != nil {
			t.Fatal(err)
		} else if len(candidates) != 0 {
			t.Fatalf("partial required-check rollup offered review candidates: %+v", candidates)
		}
		if j, _ := st.GetJob(ctx, "late-fail"); j.State == job.StateMergeable || j.State == job.StateMerging || j.State == job.StateMergeHandoff || j.State == job.StateDone {
			t.Fatalf("partial required-check rollup transitioned to %s", j.State)
		}

		f.SetPR(gh.PullRequest{
			Number: 271, HeadRefOid: "visible-head", BaseRefOid: "visible-base",
			CIRollup: gh.CIFailure, CIHasRealSuccess: true,
			PassedChecks: []string{"fast"}, FailingChecks: []string{"late"},
			UpdatedAt: time.Unix(20, 0),
		})
		if _, err := rec.Sweep(ctx); err != nil {
			t.Fatalf("sweep late failure: %v", err)
		}
		if j, _ := st.GetJob(ctx, "late-fail"); j.State != job.StateReady {
			t.Fatalf("late required failure state=%s, want ready rebuild without approval/merge", j.State)
		}
	})

	t.Run("complete terminal success at same visible head permits review", func(t *testing.T) {
		st := testutil.NewStore(t)
		seed(t, st, "all-green", 272)
		if _, err := st.DB.ExecContext(ctx, `
			UPDATE jobs
			   SET state='review_pending', required_capabilities='["role:code_reviewer"]',
			       head_sha='visible-head', base_sha='visible-base'
			 WHERE id='all-green'`); err != nil {
			t.Fatal(err)
		}

		f := gh.NewFake()
		f.SetBranchProtection("main", gh.Protection{RequiredChecks: []string{"fast", "late"}})
		f.SetPR(gh.PullRequest{
			Number: 272, HeadRefOid: "visible-head", BaseRefOid: "visible-base",
			CIRollup: gh.CIPending, CIHasRealSuccess: true,
			PassedChecks: []string{"fast"}, UpdatedAt: time.Unix(10, 0),
		})
		rec := reconcile.New(st, f, clock.NewFake(now), nil)
		if _, err := rec.Sweep(ctx); err != nil {
			t.Fatalf("sweep partial: %v", err)
		}
		if candidates, err := st.ReviewPendingCandidates(ctx); err != nil {
			t.Fatal(err)
		} else if len(candidates) != 0 {
			t.Fatalf("missing required check offered review candidates: %+v", candidates)
		}

		f.SetPR(gh.PullRequest{
			Number: 272, HeadRefOid: "visible-head", BaseRefOid: "visible-base",
			CIRollup: gh.CISuccess, CIHasRealSuccess: true,
			PassedChecks: []string{"fast", "late"}, UpdatedAt: time.Unix(20, 0),
		})
		if _, err := rec.Sweep(ctx); err != nil {
			t.Fatalf("sweep all green: %v", err)
		}
		candidates, err := st.ReviewPendingCandidates(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if len(candidates) != 1 || candidates[0].JobID != "all-green" {
			t.Fatalf("complete required-check rollup candidates=%+v, want all-green", candidates)
		}
	})
}

// TestSweepSupersedesOnNewCommit: a new commit (head SHA move) to an open PR whose
// job holds a SHA-bound verdict -> superseded + re-armed (the DONE-WHEN "new commit
// to an open PR -> job superseded + re-armed").
func TestSweepSupersedesOnNewCommit(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	seed(t, st, "jc", 200)

	f := gh.NewFake()
	f.SetPR(gh.PullRequest{Number: 200, HeadRefOid: "h1", BaseRefOid: "b1", CIRollup: gh.CISuccess, UpdatedAt: time.Unix(10, 0)})
	rec := reconcile.New(st, f, clock.NewFake(time.Unix(20, 0)), nil)
	if _, err := rec.Sweep(ctx); err != nil {
		t.Fatalf("sweep1: %v", err)
	}
	// move the job to mergeable with a verdict bound to (h1,b1).
	v := job.MintVerdict(job.VerdictApproved, job.DispositionHandoff, "h1", "b1")
	mustExec(t, st, `UPDATE jobs SET state='mergeable', verdict=?, head_sha='h1' WHERE id='jc'`, mustJSON(t, v))

	// a NEW commit lands: head moves to h2.
	f.SetPR(gh.PullRequest{Number: 200, HeadRefOid: "h2", BaseRefOid: "b1", CIRollup: gh.CIPending, UpdatedAt: time.Unix(30, 0)})
	outs, err := rec.Sweep(ctx)
	if err != nil {
		t.Fatalf("sweep2: %v", err)
	}
	var superseded bool
	for _, o := range outs {
		if o.JobID == "jc" && o.Superseded {
			superseded = true
		}
	}
	if !superseded {
		t.Fatalf("new commit did not supersede: outs=%+v", outs)
	}
	j, _ := st.GetJob(ctx, "jc")
	if j.State != job.StateReady || j.Verdict != nil || j.Role != job.RoleEngWorker {
		t.Fatalf("not re-armed: %+v", j)
	}
}

// TestRefetchReadsRealState: a targeted refetch reads the REAL scripted PR state.
// Even if a webhook CLAIMED a PR was approved/merged, the refetch reconciles the
// truth (here: still open, CI red) and cannot fast-track (§8.1.3).
func TestRefetchReadsRealState(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	seed(t, st, "jr", 300)

	f := gh.NewFake()
	// the REAL state: open, CI red, NOT merged.
	f.SetPR(gh.PullRequest{Number: 300, HeadRefOid: "h", BaseRefOid: "b", CIRollup: gh.CIFailure, UpdatedAt: time.Unix(5, 0)})
	rec := reconcile.New(st, f, clock.NewFake(time.Unix(10, 0)), nil)

	_, reconciled, err := rec.Refetch(ctx, 300)
	if err != nil || !reconciled {
		t.Fatalf("refetch reconciled=%v err=%v", reconciled, err)
	}
	fa, _, _ := store.DBFactSource{DB: st.DB}.Facts(ctx, "jr")
	if fa.Merged || fa.CIGreen {
		t.Fatalf("refetch fast-tracked a lie: facts=%+v", fa)
	}
	j, _ := st.GetJob(ctx, "jr")
	if j.State == job.StateDone {
		t.Fatalf("refetch merged an un-merged PR")
	}
}

func TestAdoptPRPersistsAuthoritativeDiff(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	f := gh.NewFake()
	const diff = "diff --git a/x.go b/x.go\nindex 1111111..2222222 100644\n--- a/x.go\n+++ b/x.go\n@@ -1 +1 @@\n-old\n+new\n"
	f.SetPR(gh.PullRequest{Number: 4078, HeadRefOid: "head-sha", BaseRefOid: "base-sha", CIRollup: gh.CISuccess, UpdatedAt: time.Unix(5, 0)})
	f.SetPRDiff(4078, diff)

	rec := reconcile.NewForRepo("russ", st, f, clock.NewFake(time.Unix(10, 0)), nil)
	id, rearmed, err := rec.AdoptPR(ctx, 4078)
	if err != nil {
		t.Fatalf("adopt: %v", err)
	}
	if rearmed {
		t.Fatal("first adopt must not report re-armed")
	}
	if id == "" {
		t.Fatal("expected adopted job")
	}
	j, _ := st.GetJob(ctx, id)
	if j.BaseSHA != "base-sha" || j.HeadSHA != "head-sha" || j.Repo != "russ" {
		t.Fatalf("adopted shas/repo: %+v", j)
	}
	if got, _ := st.JobPatchDiff(ctx, id); got != diff {
		t.Fatalf("patch_diff=%q, want authoritative diff", got)
	}
}

func TestAdoptPREmptyDiffIsExplicit(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	f := gh.NewFake()
	f.SetPR(gh.PullRequest{Number: 12, HeadRefOid: "same", BaseRefOid: "same", CIRollup: gh.CISuccess, UpdatedAt: time.Unix(5, 0)})
	f.SetPRDiff(12, "")

	rec := reconcile.NewForRepo("russ", st, f, clock.NewFake(time.Unix(10, 0)), nil)
	id, rearmed, err := rec.AdoptPR(ctx, 12)
	if err != nil {
		t.Fatalf("adopt empty: %v", err)
	}
	if rearmed {
		t.Fatal("first empty adopt must not report re-armed")
	}
	j, _ := st.GetJob(ctx, id)
	if !j.DiffEmpty {
		t.Fatal("empty adopted PR must be recorded explicitly")
	}
}

func TestAdoptPRFailsWithoutAuthoritativeDiff(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	f := gh.NewFake()
	f.SetPR(gh.PullRequest{Number: 4079, HeadRefOid: "head-sha", BaseRefOid: "base-sha", CIRollup: gh.CISuccess, UpdatedAt: time.Unix(5, 0)})

	rec := reconcile.NewForRepo("russ", st, f, clock.NewFake(time.Unix(10, 0)), nil)
	if _, _, err := rec.AdoptPR(ctx, 4079); err == nil {
		t.Fatal("adoption without an authoritative diff must fail")
	}
	if id, ok, err := st.JobIDForPRInRepo(ctx, "russ", 4079); err != nil || ok || id != "" {
		t.Fatalf("failed adoption must not create review job: id=%q ok=%v err=%v", id, ok, err)
	}
}

func mustExec(t *testing.T, st *store.Store, q string, args ...any) {
	t.Helper()
	if _, err := st.DB.ExecContext(context.Background(), q, args...); err != nil {
		t.Fatalf("exec: %v", err)
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(b)
}

// TestAdoptPRReadsRealStateAndImports covers the `flowbee adopt <pr>` reconcile edge:
// it fetches the PR's real state from GitHub (a fake here) and binds it to an opted-in
// adopted code_reviewer job in review_pending — idempotently, and refusing a
// merged/closed PR.
func TestAdoptPRReadsRealStateAndImports(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()

	f := gh.NewFake()
	f.SetPR(gh.PullRequest{Number: 900, HeadRefOid: "hh", BaseRefOid: "bb", CIRollup: gh.CISuccess, CIHasRealSuccess: true, UpdatedAt: time.Unix(10, 0)})
	f.SetPRDiff(900, "diff --git a/adopted b/adopted\n+review me\n")
	rec := reconcile.New(st, f, clock.NewFake(time.Unix(20, 0)), nil)

	id, rearmed, err := rec.AdoptPR(ctx, 900)
	if err != nil {
		t.Fatalf("adopt: %v", err)
	}
	if rearmed {
		t.Fatal("first adopt must not report re-armed")
	}
	if id == "" {
		t.Fatal("expected a new adopted job id")
	}
	j, err := st.GetJob(ctx, id)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if j.State != job.StateReviewPending || j.PRNumber != 900 {
		t.Fatalf("adopted job state=%q pr=%d, want review_pending / 900", j.State, j.PRNumber)
	}
	if got, _ := st.JobPatchDiff(ctx, id); got != "diff --git a/adopted b/adopted\n+review me\n" {
		t.Fatalf("adopted patch_diff=%q", got)
	}

	// idempotent
	again, rearmed, err := rec.AdoptPR(ctx, 900)
	if err != nil {
		t.Fatalf("re-adopt: %v", err)
	}
	if again != "" || rearmed {
		t.Fatalf("unchanged re-adopt must no-op, got id=%q rearmed=%v", again, rearmed)
	}

	// a merged PR is refused (nothing to review)
	f.SetPR(gh.PullRequest{Number: 901, HeadRefOid: "h1", BaseRefOid: "b1", Merged: true, UpdatedAt: time.Unix(12, 0)})
	if _, _, err := rec.AdoptPR(ctx, 901); err == nil {
		t.Fatal("adopting a merged PR must error")
	}

	// a non-existent PR is refused
	if _, _, err := rec.AdoptPR(ctx, 999); err == nil {
		t.Fatal("adopting a non-existent PR must error")
	}
}

// TestSweepAutoAdoptsNeedsClaudeLabeledPR covers the operational incident this
// closes: a long-running GPT "goal" session finishes a PR and labels it
// needs-claude, handing it off for Claude review. Before this, that PR sat
// stranded in the queue until a human noticed and ran `flowbee adopt <pr>` by
// hand. Now the floor sweep itself notices the label and adopts the PR through
// the same AdoptPR path the manual CLI uses — no human required.
func TestSweepAutoAdoptsNeedsClaudeLabeledPR(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()

	f := gh.NewFake()
	f.SetPR(gh.PullRequest{
		Number: 500, HeadRefOid: "hh", BaseRefOid: "bb",
		CIRollup: gh.CISuccess, CIHasRealSuccess: true, UpdatedAt: time.Unix(10, 0),
		Labels: []string{"needs-claude"},
	})
	f.SetPRDiff(500, "diff --git a/goal b/goal\n+finished by gpt\n")
	rec := reconcile.New(st, f, clock.NewFake(time.Unix(20, 0)), nil)

	if _, err := rec.Sweep(ctx); err != nil {
		t.Fatalf("sweep: %v", err)
	}

	id, ok, err := st.JobIDForPRInRepo(ctx, "", 500)
	if err != nil || !ok {
		t.Fatalf("expected PR #500 auto-adopted on sweep: ok=%v err=%v", ok, err)
	}
	j, err := st.GetJob(ctx, id)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if j.State != job.StateReviewPending {
		t.Fatalf("auto-adopted job state=%q, want review_pending", j.State)
	}
	if got, _ := st.JobPatchDiff(ctx, id); got != "diff --git a/goal b/goal\n+finished by gpt\n" {
		t.Fatalf("auto-adopted patch_diff=%q", got)
	}

	// re-sweeping the same unchanged PR must not duplicate the adopted job (the
	// AdoptPR idempotency the sweep reuses rather than reimplementing) — AND must
	// make ZERO adopt-path GitHub API calls (no PullRequest / PullRequestDiff).
	// The label never comes off, so without the local PRAdoptWouldAct gate the
	// steady state costs 2 API calls per labeled PR per sweep forever — the API
	// storm the gate exists to prevent. The only permitted new call is the
	// sweep's own BoardSweep read.
	before := len(f.Calls())
	if _, err := rec.Sweep(ctx); err != nil {
		t.Fatalf("second sweep: %v", err)
	}
	for _, call := range f.Calls()[before:] {
		if strings.HasPrefix(call, "PullRequest") {
			t.Fatalf("steady-state sweep made an adopt API call: %q", call)
		}
	}
	again, ok, err := st.JobIDForPRInRepo(ctx, "", 500)
	if err != nil || !ok || again != id {
		t.Fatalf("re-sweep must not disturb the adopted job: id=%q ok=%v err=%v", again, ok, err)
	}

	// a head move must still re-arm through the gate (the gate may only skip work
	// AdoptPRForReview would not have done — never a genuine re-arm).
	f.SetPR(gh.PullRequest{
		Number: 500, HeadRefOid: "hh2", BaseRefOid: "bb",
		CIRollup: gh.CISuccess, CIHasRealSuccess: true, UpdatedAt: time.Unix(30, 0),
		Labels: []string{"needs-claude"},
	})
	f.SetPRDiff(500, "diff --git a/goal b/goal\n+finished by gpt, take two\n")
	if _, err := rec.Sweep(ctx); err != nil {
		t.Fatalf("head-move sweep: %v", err)
	}
	j, err = st.GetJob(ctx, id)
	if err != nil {
		t.Fatalf("get re-armed job: %v", err)
	}
	if j.State != job.StateReviewPending || j.HeadSHA != "hh2" {
		t.Fatalf("moved head must re-arm: state=%q head=%q, want review_pending/hh2", j.State, j.HeadSHA)
	}
}

// TestSweepDoesNotAutoAdoptMergedOrClosedLabeledPR: the sweep's snapshot includes
// recently MERGED and CLOSED PRs (the merged->done backstop reads them), and a
// needs-claude label survives both. Neither may adopt — a merged PR has nothing
// left to review, and a closed-unmerged PR was rejected by a human. The guard also
// fires BEFORE any adopt API spend (terminal labeled PRs linger on the snapshot
// forever, so a post-fetch refusal would still be a per-sweep API storm).
func TestSweepDoesNotAutoAdoptMergedOrClosedLabeledPR(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()

	f := gh.NewFake()
	f.SetPR(gh.PullRequest{
		Number: 504, HeadRefOid: "hh", BaseRefOid: "bb", Merged: true,
		UpdatedAt: time.Unix(10, 0), Labels: []string{"needs-claude"},
	})
	f.SetPR(gh.PullRequest{
		Number: 505, HeadRefOid: "hh", BaseRefOid: "bb", ClosedUnmerged: true,
		UpdatedAt: time.Unix(10, 0), Labels: []string{"needs-claude"},
	})
	rec := reconcile.New(st, f, clock.NewFake(time.Unix(20, 0)), nil)

	if _, err := rec.Sweep(ctx); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	for _, pr := range []int{504, 505} {
		if _, ok, err := st.JobIDForPRInRepo(ctx, "", pr); err != nil || ok {
			t.Fatalf("terminal PR #%d must not be auto-adopted: ok=%v err=%v", pr, ok, err)
		}
	}
	// only the adopt path's two reads are forbidden — the sweep's own BoardSweep
	// and its one-time required-checks warm-up are legitimate infrastructure.
	for _, call := range f.Calls() {
		if strings.HasPrefix(call, "PullRequest") {
			t.Fatalf("terminal labeled PR triggered an adopt API call: %q", call)
		}
	}
}

// TestSweepDoesNotHijackFlowbeeOriginatedLabeledPR: someone labels a PR that a
// Flowbee-ORIGINATED job already tracks. The store's adopt path no-ops for
// non-adopted trackers, and the gate knows that locally — so the sweep must
// neither hijack/duplicate the job NOR pay AdoptPR's two API calls every sweep
// just to learn the no-op (the same steady-state storm as an adopted PR).
func TestSweepDoesNotHijackFlowbeeOriginatedLabeledPR(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	seed(t, st, "jorig", 506)

	f := gh.NewFake()
	f.SetPR(gh.PullRequest{
		Number: 506, HeadRefOid: "hh", BaseRefOid: "bb",
		CIRollup: gh.CISuccess, CIHasRealSuccess: true, UpdatedAt: time.Unix(10, 0),
		Labels: []string{"needs-claude"},
	})
	rec := reconcile.New(st, f, clock.NewFake(time.Unix(20, 0)), nil)

	if _, err := rec.Sweep(ctx); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	id, ok, err := st.JobIDForPRInRepo(ctx, "", 506)
	if err != nil || !ok || id != "jorig" {
		t.Fatalf("originated job must keep tracking its PR: id=%q ok=%v err=%v", id, ok, err)
	}
	var n, adopted int
	if err := st.DB.QueryRowContext(ctx,
		`SELECT COUNT(*), COALESCE(MAX(adopted),0) FROM jobs WHERE pr_number = 506`).Scan(&n, &adopted); err != nil || n != 1 || adopted != 0 {
		t.Fatalf("labeled originated PR must stay a single non-adopted job: n=%d adopted=%d err=%v", n, adopted, err)
	}
	for _, call := range f.Calls() {
		if strings.HasPrefix(call, "PullRequest") {
			t.Fatalf("originated tracked PR triggered an adopt API call: %q", call)
		}
	}
}

// TestSweepDoesNotAutoAdoptDraftNeedsClaudePR: a draft PR is still being worked —
// even with the needs-claude label already applied (e.g. scripted ahead of "ready
// for review"), the sweep must not pull it into the review pipeline early.
func TestSweepDoesNotAutoAdoptDraftNeedsClaudePR(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()

	f := gh.NewFake()
	f.SetPR(gh.PullRequest{
		Number: 501, HeadRefOid: "hh", BaseRefOid: "bb",
		CIRollup: gh.CISuccess, CIHasRealSuccess: true, UpdatedAt: time.Unix(10, 0),
		IsDraft: true, Labels: []string{"needs-claude"},
	})
	f.SetPRDiff(501, "diff --git a/wip b/wip\n+still working\n")
	rec := reconcile.New(st, f, clock.NewFake(time.Unix(20, 0)), nil)

	if _, err := rec.Sweep(ctx); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if _, ok, err := st.JobIDForPRInRepo(ctx, "", 501); err != nil || ok {
		t.Fatalf("draft PR must not be auto-adopted: ok=%v err=%v", ok, err)
	}
}

// TestSweepIgnoresUnlabeledPR: a plain PR with no adopt label is untouched by the
// sweep's auto-adopt (it is not Flowbee's to seize — an operator or a future
// labeled hand-off decides).
func TestSweepIgnoresUnlabeledPR(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()

	f := gh.NewFake()
	f.SetPR(gh.PullRequest{
		Number: 502, HeadRefOid: "hh", BaseRefOid: "bb",
		CIRollup: gh.CISuccess, CIHasRealSuccess: true, UpdatedAt: time.Unix(10, 0),
	})
	rec := reconcile.New(st, f, clock.NewFake(time.Unix(20, 0)), nil)

	if _, err := rec.Sweep(ctx); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if _, ok, err := st.JobIDForPRInRepo(ctx, "", 502); err != nil || ok {
		t.Fatalf("unlabeled PR must not be auto-adopted: ok=%v err=%v", ok, err)
	}
}

// TestSweepAutoAdoptsFlowbeeAdoptLabeledPR proves flowbee:adopt and needs-claude
// drive the exact same sweep-time adopt path (a shared label set, not per-label
// copy-pasted logic, §Task-A req 2) — flowbee:adopt gets identical treatment.
func TestSweepAutoAdoptsFlowbeeAdoptLabeledPR(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()

	f := gh.NewFake()
	f.SetPR(gh.PullRequest{
		Number: 503, HeadRefOid: "hh", BaseRefOid: "bb",
		CIRollup: gh.CISuccess, CIHasRealSuccess: true, UpdatedAt: time.Unix(10, 0),
		Labels: []string{"flowbee:adopt"},
	})
	f.SetPRDiff(503, "diff --git a/human b/human\n+opted in\n")
	rec := reconcile.New(st, f, clock.NewFake(time.Unix(20, 0)), nil)

	if _, err := rec.Sweep(ctx); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if _, ok, err := st.JobIDForPRInRepo(ctx, "", 503); err != nil || !ok {
		t.Fatalf("expected flowbee:adopt PR #503 auto-adopted on sweep: ok=%v err=%v", ok, err)
	}
}
