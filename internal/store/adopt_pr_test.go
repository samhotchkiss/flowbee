package store_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/samhotchkiss/flowbee/internal/job"
	"github.com/samhotchkiss/flowbee/internal/lease"
	"github.com/samhotchkiss/flowbee/internal/ledger"
	"github.com/samhotchkiss/flowbee/internal/store"
	"github.com/samhotchkiss/flowbee/internal/testutil"
)

func claimAdoptedRepair(t *testing.T, ctx context.Context, st *store.Store, id string, now time.Time) int {
	t.Helper()
	if _, err := st.DB.ExecContext(ctx, `
		UPDATE jobs
		   SET state='ready', role='eng_worker', stage='build',
		       required_capabilities='["role:eng_worker"]',
		       enqueued_at=?
		 WHERE id=?`, now.Format(time.RFC3339Nano), id); err != nil {
		t.Fatalf("arm adopted repair: %v", err)
	}
	ls, err := st.ClaimReadyJob(ctx, store.ClaimParams{
		JobID: id, LeaseID: "repair-" + id + "-" + now.Format(time.RFC3339Nano), Identity: "builder",
		ModelFamily: "gpt", Role: job.RoleEngWorker,
		Attested: []string{"role:eng_worker"}, TTL: time.Minute, Now: now,
	})
	if err != nil {
		t.Fatalf("claim adopted repair: %v", err)
	}
	return ls.Epoch
}

// TestAdoptPRForReview covers the targeted single-PR adoption (`flowbee adopt <pr>`):
// a pre-existing PR Flowbee did not originate is imported as an opted-in adopted
// code_reviewer job in review_pending, with its Domain-B facts reconciled — and the
// import is idempotent.
func TestAdoptPRForReview(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	now := time.Unix(9000, 0)

	const patch = "diff --git a/x.go b/x.go\nindex 1111111..2222222 100644\n--- a/x.go\n+++ b/x.go\n@@ -1 +1 @@\n-old\n+new\n"
	id, rearmed, err := st.AdoptPRForReview(ctx, "russ", 4242, "base-sha", "head-sha", patch, false, false, true, false, now, now)
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
	if j.State != job.StateReviewPending {
		t.Fatalf("adopted PR state=%q, want review_pending", j.State)
	}
	if j.Role != job.RoleCodeReviewer {
		t.Fatalf("adopted PR role=%q, want code_reviewer", j.Role)
	}
	if j.PRNumber != 4242 {
		t.Fatalf("adopted PR number=%d, want 4242", j.PRNumber)
	}
	// repo MUST be set — else project-OUT's per-repo outbox drain strands the merge.
	if j.Repo != "russ" {
		t.Fatalf("adopted PR repo=%q, want russ (empty repo strands the merge in multi-repo)", j.Repo)
	}
	if d, err := st.JobPatchDiff(ctx, id); err != nil || d != patch {
		t.Fatalf("adopted PR patch_diff=%q err=%v, want authoritative patch", d, err)
	}
	if j.DiffEmpty {
		t.Fatal("nonempty adopted PR diff must not be marked empty")
	}
	// it must be opted-in (NOT quiescent) — project-out has to render it so the
	// reviewer is actually offered the work.
	quiescent, err := st.IsQuiescent(ctx, id)
	if err != nil {
		t.Fatalf("quiescent check: %v", err)
	}
	if quiescent {
		t.Fatal("an adopted-for-review PR must be opted in, not quiescent")
	}

	// idempotent: re-adopting the same PR is a no-op ("" id), no duplicate job.
	again, rearmed, err := st.AdoptPRForReview(ctx, "russ", 4242, "base-sha", "head-sha", patch, false, false, true, false, now, now)
	if err != nil {
		t.Fatalf("re-adopt: %v", err)
	}
	if again != "" || rearmed {
		t.Fatalf("re-adopting an unchanged tracked PR must be a no-op, got id=%q rearmed=%v", again, rearmed)
	}
}

func TestAdoptPRForReviewReadoptsAfterCancel(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	now := time.Unix(9000, 0)

	firstID, _, err := st.AdoptPRForReview(ctx, "russ", 4078, "base-1", "head-1", "diff --git a/old b/old\n", false, false, true, false, now, now)
	if err != nil || firstID == "" {
		t.Fatalf("first adopt: id=%q err=%v", firstID, err)
	}
	if _, err := st.CancelJob(ctx, firstID, false, now.Add(time.Minute)); err != nil {
		t.Fatalf("cancel first adopt: %v", err)
	}

	const newPatch = "diff --git a/new b/new\n"
	secondID, rearmed, err := st.AdoptPRForReview(ctx, "russ", 4078, "base-2", "head-2", newPatch, false, false, false, false, now.Add(2*time.Minute), now.Add(2*time.Minute))
	if err != nil {
		t.Fatalf("re-adopt: %v", err)
	}
	if rearmed {
		t.Fatal("re-adopt after cancel creates a replacement, not an in-place re-arm")
	}
	if secondID == "" || secondID == firstID {
		t.Fatalf("re-adopt after cancel must create a fresh job, first=%q second=%q", firstID, secondID)
	}

	first, err := st.GetJob(ctx, firstID)
	if err != nil {
		t.Fatalf("get cancelled job: %v", err)
	}
	if first.State != job.StateCancelled {
		t.Fatalf("historical job state=%q, want cancelled", first.State)
	}
	second, err := st.GetJob(ctx, secondID)
	if err != nil {
		t.Fatalf("get replacement job: %v", err)
	}
	if second.State != job.StateReviewPending || second.Role != job.RoleCodeReviewer {
		t.Fatalf("replacement state/role=%q/%q, want review_pending/code_reviewer", second.State, second.Role)
	}
	if second.BaseSHA != "base-2" || second.HeadSHA != "head-2" {
		t.Fatalf("replacement base/head=%q/%q, want base-2/head-2", second.BaseSHA, second.HeadSHA)
	}
	if diff, err := st.JobPatchDiff(ctx, secondID); err != nil || diff != newPatch {
		t.Fatalf("replacement diff=%q err=%v, want authoritative new diff", diff, err)
	}
}

// TestAdoptPRForReviewSkipsOriginatedPR: a PR Flowbee ALREADY originated (its own
// build job carries the pr_number) must not be re-adopted — adoption is only for
// foreign PRs. The idempotency guard keys on pr_number regardless of how the job
// was created, within the same repo scope.
func TestAdoptPRForReviewSkipsOriginatedPR(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	now := time.Unix(9000, 0)

	// seed a normal build job and stamp it with a PR number (as project-out does on
	// PR creation), simulating a Flowbee-originated PR.
	issue := 77
	if _, err := st.SeedJob(ctx, store.SeedParams{
		ID: "orig-1", Kind: job.KindBuild, Flow: "build", Stage: "build",
		Role: job.RoleEngWorker, IssueNumber: &issue, Repo: "russ", Now: now,
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := st.StampPRNumber(ctx, "orig-1", 555, "head", "base", now); err != nil {
		t.Fatalf("stamp pr: %v", err)
	}

	id, _, err := st.AdoptPRForReview(ctx, "russ", 555, "base", "head", "diff --git a/x b/x\n", false, false, true, false, now, now)
	if err != nil {
		t.Fatalf("adopt: %v", err)
	}
	if id != "" {
		t.Fatalf("must not adopt a PR Flowbee already originated, got id %q", id)
	}
}

func TestAdoptPRForReviewRefreshesLegacyAndHeadMove(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	now := time.Unix(9000, 0)

	id, _, err := st.AdoptPRForReview(ctx, "russ", 99, "base1", "head1", "", false, false, true, false, now, now)
	if err != nil || id == "" {
		t.Fatalf("legacy seed adopt id=%q err=%v", id, err)
	}
	if d, _ := st.JobPatchDiff(ctx, id); d != "" {
		t.Fatalf("legacy setup patch=%q, want empty", d)
	}
	again, rearmed, err := st.AdoptPRForReview(ctx, "russ", 99, "base1", "head1", "diff --git a/a b/a\n", false, false, true, false, now, now)
	if err != nil {
		t.Fatalf("backfill adopt: %v", err)
	}
	if again != "" || rearmed {
		t.Fatalf("backfill should not duplicate or re-arm, got id=%q rearmed=%v", again, rearmed)
	}
	if d, _ := st.JobPatchDiff(ctx, id); d != "diff --git a/a b/a\n" {
		t.Fatalf("backfilled patch=%q", d)
	}

	again, rearmed, err = st.AdoptPRForReview(ctx, "russ", 99, "base1", "head2", "diff --git a/b b/b\n", false, false, true, false, now, now)
	if err != nil {
		t.Fatalf("head refresh adopt: %v", err)
	}
	if again != id || !rearmed {
		t.Fatalf("head refresh should re-arm existing job, got id=%q rearmed=%v", again, rearmed)
	}
	j, _ := st.GetJob(ctx, id)
	if j.HeadSHA != "head2" {
		t.Fatalf("head_sha=%q, want head2", j.HeadSHA)
	}
	if d, _ := st.JobPatchDiff(ctx, id); d != "diff --git a/b b/b\n" {
		t.Fatalf("refreshed patch=%q", d)
	}
}

func TestAdoptPRForReviewHeadMoveRearmsReviewAndInvalidatesStaleAuthorization(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	now := time.Unix(9000, 0)

	oldDiff := "diff --git a/app.go b/app.go\n@@ -1 +1 @@\n-old\n+reviewed\n"
	id, _, err := st.AdoptPRForReview(ctx, "russ", 4153, "base-old", "head-old", oldDiff, false, false, true, false, now, now)
	if err != nil || id == "" {
		t.Fatalf("adopt: id=%q err=%v", id, err)
	}
	v := job.MintVerdict(job.VerdictApproved, job.DispositionSelfMerge, "head-old", "base-old")
	if _, err := st.DB.ExecContext(ctx, `
		UPDATE jobs
		   SET state='merge_handoff', verdict=?, lease_epoch=7, lease_id='lease-old',
		       bound_identity='reviewer-old', bound_model_family='opus',
		       lease_deadline=?, lease_hb_due=?, phase_deadline_at=?
		 WHERE id=?`,
		mustJSON(t, v), now.Add(time.Hour).Format(time.RFC3339),
		now.Add(time.Minute).Format(time.RFC3339), now.Add(30*time.Minute).Format(time.RFC3339), id); err != nil {
		t.Fatalf("seed stale handoff: %v", err)
	}
	if _, err := st.DB.ExecContext(ctx, `
		INSERT INTO leases (lease_id, job_id, lease_epoch, identity, model_family, granted_at, ttl_s, deadline)
		VALUES ('lease-old', ?, 7, 'reviewer-old', 'opus', ?, 3600, ?)`,
		id, now.Format(time.RFC3339), now.Add(time.Hour).Format(time.RFC3339)); err != nil {
		t.Fatalf("seed lease: %v", err)
	}
	if err := st.EnqueueOutbox(ctx, store.OutboxRow{JobID: id, Action: store.ActionEnqueueMerge, HeadSHA: "head-old"}); err != nil {
		t.Fatalf("seed stale merge outbox: %v", err)
	}

	newDiff := "diff --git a/app.go b/app.go\n@@ -1 +1 @@\n-reviewed\n+fixed\n"
	gotID, rearmed, err := st.AdoptPRForReview(ctx, "russ", 4153, "base-new", "head-new", newDiff, false, false, true, false, now.Add(time.Minute), now.Add(time.Minute))
	if err != nil {
		t.Fatalf("re-adopt moved head: %v", err)
	}
	if gotID != id || !rearmed {
		t.Fatalf("moved head must re-arm existing job, got id=%q rearmed=%v", gotID, rearmed)
	}

	j, err := st.GetJob(ctx, id)
	if err != nil {
		t.Fatalf("get re-armed job: %v", err)
	}
	if j.State != job.StateReviewPending || j.Role != job.RoleCodeReviewer || j.Stage != "review" {
		t.Fatalf("state/role/stage=%s/%s/%s, want review_pending/code_reviewer/review", j.State, j.Role, j.Stage)
	}
	if j.BaseSHA != "base-new" || j.HeadSHA != "head-new" {
		t.Fatalf("base/head=%q/%q, want refreshed base-new/head-new", j.BaseSHA, j.HeadSHA)
	}
	if j.Verdict != nil {
		t.Fatalf("stale verdict survived re-arm: %+v", j.Verdict)
	}
	if j.LeaseEpoch != 8 || j.LeaseID != "" || j.BoundIdentity != "" {
		t.Fatalf("lease fence not reset: epoch=%d lease=%q identity=%q", j.LeaseEpoch, j.LeaseID, j.BoundIdentity)
	}
	if diff, err := st.JobPatchDiff(ctx, id); err != nil || diff != newDiff {
		t.Fatalf("review diff=%q err=%v, want refreshed diff", diff, err)
	}
	cands, err := st.ReviewPendingCandidates(ctx)
	if err != nil {
		t.Fatalf("review candidates: %v", err)
	}
	var found bool
	for _, c := range cands {
		if c.JobID == id {
			found = true
			if !c.CIReady {
				t.Fatal("re-armed review candidate must carry refreshed green CI facts")
			}
		}
	}
	if !found {
		t.Fatalf("re-armed job is not an active review candidate: %+v", cands)
	}

	var outboxStatus string
	if err := st.DB.QueryRowContext(ctx, `
		SELECT status FROM outbox WHERE job_id=? AND action=? AND head_sha='head-old'`,
		id, store.ActionEnqueueMerge).Scan(&outboxStatus); err != nil {
		t.Fatalf("read stale outbox: %v", err)
	}
	if outboxStatus != "abandoned" {
		t.Fatalf("stale merge outbox status=%q, want abandoned", outboxStatus)
	}
	var openLeaseEnded, openLeaseReason string
	if err := st.DB.QueryRowContext(ctx, `
		SELECT COALESCE(ended_at,''), COALESCE(end_reason,'') FROM leases WHERE lease_id='lease-old'`).
		Scan(&openLeaseEnded, &openLeaseReason); err != nil {
		t.Fatalf("read lease audit: %v", err)
	}
	if openLeaseEnded == "" || openLeaseReason != "superseded" {
		t.Fatalf("lease audit ended_at=%q reason=%q, want superseded closure", openLeaseEnded, openLeaseReason)
	}
	var leaseDeadline string
	if err := st.DB.QueryRowContext(ctx, `SELECT COALESCE(lease_deadline,'') FROM jobs WHERE id=?`, id).Scan(&leaseDeadline); err != nil {
		t.Fatalf("read lease deadline: %v", err)
	}
	if leaseDeadline != "" {
		t.Fatalf("stale lease_deadline=%q, want cleared", leaseDeadline)
	}
	events, err := st.LoadEvents(ctx, id)
	if err != nil {
		t.Fatalf("load events: %v", err)
	}
	if len(events) != 2 || events[0].Kind != ledger.KindAdopted || events[1].Kind != ledger.KindAdoptRearmed {
		t.Fatalf("audit history should preserve adopt and append re-arm, got %+v", events)
	}
	folded, err := ledger.Fold(events)
	if err != nil {
		t.Fatalf("fold: %v", err)
	}
	if folded.State != j.State || folded.Role != j.Role || folded.BaseSHA != j.BaseSHA ||
		folded.HeadSHA != j.HeadSHA || folded.Verdict != nil || folded.LeaseEpoch != j.LeaseEpoch {
		t.Fatalf("fold != projection:\n fold=%+v\n proj=%+v", folded, j)
	}
}

func TestAdoptPRForReviewScopesPRNumbersByRepo(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	now := time.Unix(9000, 0)

	idA, _, err := st.AdoptPRForReview(ctx, "core", 4078, "base-a", "head-a", "diff --git a/core b/core\n", false, false, true, false, now, now)
	if err != nil {
		t.Fatalf("adopt core: %v", err)
	}
	idB, _, err := st.AdoptPRForReview(ctx, "web", 4078, "base-b", "head-b", "diff --git a/web b/web\n", false, false, true, false, now, now)
	if err != nil {
		t.Fatalf("adopt web: %v", err)
	}
	if idA == "" || idB == "" || idA == idB {
		t.Fatalf("repo-scoped PRs should create distinct jobs, got %q and %q", idA, idB)
	}
	if d, _ := st.JobPatchDiff(ctx, idA); d != "diff --git a/core b/core\n" {
		t.Fatalf("core patch=%q", d)
	}
	if d, _ := st.JobPatchDiff(ctx, idB); d != "diff --git a/web b/web\n" {
		t.Fatalf("web patch=%q", d)
	}
}

func TestAdoptPRForReviewJobIDUsesCollisionFreeRepoEncoding(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	now := time.Unix(9000, 0)

	idA, _, err := st.AdoptPRForReview(ctx, "owner/repo", 4078, "base-a", "head-a", "diff --git a/slash b/slash\n", false, false, true, false, now, now)
	if err != nil {
		t.Fatalf("adopt owner/repo: %v", err)
	}
	idB, _, err := st.AdoptPRForReview(ctx, "owner-repo", 4078, "base-b", "head-b", "diff --git a/dash b/dash\n", false, false, true, false, now, now)
	if err != nil {
		t.Fatalf("adopt owner-repo: %v", err)
	}
	if idA == "" || idB == "" || idA == idB {
		t.Fatalf("repo identities must not collide in adopted job ids, got %q and %q", idA, idB)
	}
	if d, _ := st.JobPatchDiff(ctx, idA); d != "diff --git a/slash b/slash\n" {
		t.Fatalf("owner/repo patch=%q", d)
	}
	if d, _ := st.JobPatchDiff(ctx, idB); d != "diff --git a/dash b/dash\n" {
		t.Fatalf("owner-repo patch=%q", d)
	}
}

func TestAdoptPRForReviewRecordsExplicitEmptyDiff(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	now := time.Unix(9000, 0)

	id, _, err := st.AdoptPRForReview(ctx, "russ", 12, "same", "same", "", true, false, true, false, now, now)
	if err != nil {
		t.Fatalf("adopt empty: %v", err)
	}
	j, err := st.GetJob(ctx, id)
	if err != nil {
		t.Fatalf("get empty job: %v", err)
	}
	if !j.DiffEmpty {
		t.Fatal("explicit empty adopted PR must set DiffEmpty")
	}
	if d, _ := st.JobPatchDiff(ctx, id); d != "" {
		t.Fatalf("empty patch_diff=%q, want empty", d)
	}
}

func TestAdoptedPRMissingDiffIsNotReviewCandidate(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	now := time.Unix(9000, 0)

	id, _, err := st.AdoptPRForReview(ctx, "russ", 13, "base", "head", "", false, false, true, false, now, now)
	if err != nil {
		t.Fatalf("adopt legacy missing diff: %v", err)
	}
	cands, err := st.ReviewPendingCandidates(ctx)
	if err != nil {
		t.Fatalf("review candidates: %v", err)
	}
	for _, c := range cands {
		if c.JobID == id {
			t.Fatalf("legacy adopted PR with missing diff must be withheld from review candidates: %+v", cands)
		}
	}

	if _, _, err := st.AdoptPRForReview(ctx, "russ", 13, "base", "head", "diff --git a/x b/x\n", false, false, true, false, now, now); err != nil {
		t.Fatalf("backfill diff: %v", err)
	}
	cands, err = st.ReviewPendingCandidates(ctx)
	if err != nil {
		t.Fatalf("review candidates after backfill: %v", err)
	}
	for _, c := range cands {
		if c.JobID == id {
			return
		}
	}
	t.Fatalf("backfilled adopted PR should become review candidate: %+v", cands)
}

func TestAdoptedRepairRejectsDroppedOriginalPaths(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	now := time.Unix(9000, 0)

	original := "diff --git a/a.go b/a.go\n--- a/a.go\n+++ b/a.go\n@@ -1 +1 @@\n-a\n+b\n" +
		"diff --git a/b.go b/b.go\n--- a/b.go\n+++ b/b.go\n@@ -1 +1 @@\n-a\n+b\n"
	id, _, err := st.AdoptPRForReview(ctx, "russ", 4182, "base", "head", original, false, false, true, false, now, now)
	if err != nil {
		t.Fatalf("adopt: %v", err)
	}
	epoch := claimAdoptedRepair(t, ctx, st, id, now.Add(time.Minute))

	repairOnly := "diff --git a/a.go b/a.go\n--- a/a.go\n+++ b/a.go\n@@ -1 +1,2 @@\n b\n+limit fix\n"
	_, err = st.Result(ctx, store.ResultParams{
		JobID: id, Epoch: epoch, Now: now.Add(2 * time.Minute),
		PushedSHA: "head", PatchDiff: repairOnly,
	})
	if err == nil || !strings.Contains(err.Error(), "dropped original changed paths") {
		t.Fatalf("result err=%v, want dropped-path rejection", err)
	}
	j, _ := st.GetJob(ctx, id)
	if j.State == job.StateReviewPending {
		t.Fatal("dropped-path adopted repair must fail closed before review")
	}
	if diff, err := st.JobPatchDiff(ctx, id); err != nil || diff != original {
		t.Fatalf("original patch not retained after rejected repair: diff=%q err=%v", diff, err)
	}
}

func TestAdoptedRepairRejectsSamePathDroppedOriginalPatch(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	now := time.Unix(9000, 0)

	original := "diff --git a/a.go b/a.go\n--- a/a.go\n+++ b/a.go\n@@ -1,2 +1,2 @@\n stable\n-old contract\n+new contract\n"
	id, _, err := st.AdoptPRForReview(ctx, "russ", 4185, "base", "old-pr-head", original, false, false, true, false, now, now)
	if err != nil {
		t.Fatalf("adopt: %v", err)
	}
	epoch := claimAdoptedRepair(t, ctx, st, id, now.Add(time.Minute))

	samePathButInverted := "diff --git a/a.go b/a.go\n--- a/a.go\n+++ b/a.go\n@@ -1,2 +1,3 @@\n stable\n-old contract\n+base contract\n+limit fix\n"
	_, err = st.Result(ctx, store.ResultParams{
		JobID: id, Epoch: epoch, Now: now.Add(2 * time.Minute),
		PushedSHA: "old-pr-head", PatchDiff: samePathButInverted,
	})
	if err == nil || !strings.Contains(err.Error(), "dropped original patch lines") {
		t.Fatalf("result err=%v, want dropped-patch-line rejection", err)
	}
	j, _ := st.GetJob(ctx, id)
	if j.State == job.StateReviewPending {
		t.Fatal("same-path patch loss must fail closed before review")
	}
	if diff, err := st.JobPatchDiff(ctx, id); err != nil || diff != original {
		t.Fatalf("original patch not retained after rejected same-path repair: diff=%q err=%v", diff, err)
	}
}

func TestAdoptedRepairWorkerSHACannotBecomeAuthoritativeBeforeReconcile(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	now := time.Unix(9000, 0)

	original := "diff --git a/a.go b/a.go\n--- a/a.go\n+++ b/a.go\n@@ -1 +1 @@\n-a\n+b\n"
	id, _, err := st.AdoptPRForReviewWithHeadRef(ctx, "russ", 4188, "base", "github-head", "hotfix/mail-temporal-red-main", original, false, false, true, false, now, now)
	if err != nil {
		t.Fatalf("adopt: %v", err)
	}
	epoch := claimAdoptedRepair(t, ctx, st, id, now.Add(time.Minute))

	cumulative := original + "diff --git a/limit_test.go b/limit_test.go\n--- a/limit_test.go\n+++ b/limit_test.go\n@@ -0,0 +1 @@\n+zero limit\n"
	resp, err := st.Result(ctx, store.ResultParams{
		JobID: id, Epoch: epoch, Now: now.Add(2 * time.Minute),
		PushedSHA: "flowbee-hidden-head", PushedBranch: "hotfix/mail-temporal-red-main",
		PatchDiff: cumulative,
	})
	if err != nil || !resp.Accepted {
		t.Fatalf("pending materialization result=%+v err=%v", resp, err)
	}
	j, _ := st.GetJob(ctx, id)
	if j.HeadSHA == "flowbee-hidden-head" {
		t.Fatalf("worker-reported SHA became authoritative before GitHub: state=%s head=%q", j.State, j.HeadSHA)
	}
	cands, err := st.ReviewPendingCandidates(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range cands {
		if c.JobID == id {
			t.Fatal("worker-reported SHA became code-review input before GitHub observed it")
		}
	}
	var factHead string
	if err := st.DB.QueryRowContext(ctx, `SELECT head_sha FROM domain_b_facts WHERE job_id=?`, id).Scan(&factHead); err != nil {
		t.Fatalf("read facts: %v", err)
	}
	if factHead != "github-head" {
		t.Fatalf("worker result rewrote reconciled facts to %q, want authoritative github-head", factHead)
	}
}

func TestAdoptedRepairRejectsCorrectionAtUnchangedGitHubHeadAfterRearm(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	now := time.Unix(9000, 0)

	original := "diff --git a/a.go b/a.go\n--- a/a.go\n+++ b/a.go\n@@ -1 +1 @@\n-a\n+b\n"
	id, _, err := st.AdoptPRForReview(ctx, "russ", 4191, "base", "old-github-head", original, false, false, true, false, now, now)
	if err != nil {
		t.Fatalf("adopt: %v", err)
	}
	if out, err := st.ApplyReconciledPR(ctx, id, store.ReconciledPR{
		Number: 4191, BaseSHA: "base", HeadSHA: "current-github-head", CIGreen: false,
		UpdatedAt: now.Add(time.Minute),
	}, now.Add(time.Minute)); err != nil {
		t.Fatalf("reconcile moved head: %v", err)
	} else if !out.Superseded {
		t.Fatalf("head movement should re-arm adopted repair, got %+v", out)
	}

	rearmed, err := st.GetJob(ctx, id)
	if err != nil {
		t.Fatalf("get rearmed job: %v", err)
	}
	if rearmed.State != job.StateReady || rearmed.HeadSHA != "" {
		t.Fatalf("setup state/head=%s/%q, want ready with cleared job head", rearmed.State, rearmed.HeadSHA)
	}
	epoch := claimAdoptedRepair(t, ctx, st, id, now.Add(2*time.Minute))

	cumulativeWithCorrection := original + "diff --git a/limit_test.go b/limit_test.go\n--- a/limit_test.go\n+++ b/limit_test.go\n@@ -0,0 +1 @@\n+zero limit\n"
	_, err = st.Result(ctx, store.ResultParams{
		JobID: id, Epoch: epoch, Now: now.Add(3 * time.Minute),
		PushedSHA: "current-github-head", PatchDiff: cumulativeWithCorrection,
	})
	if err == nil || !strings.Contains(err.Error(), "unchanged GitHub-visible PR head") {
		t.Fatalf("result err=%v, want unchanged-head correction rejection", err)
	}
	j, _ := st.GetJob(ctx, id)
	if j.State == job.StateReviewPending || j.HeadSHA == "current-github-head" {
		t.Fatalf("unchanged-head correction became reviewable: state=%s head=%q", j.State, j.HeadSHA)
	}
}

func TestAdoptedRepairFastForwardWaitsForReconciledVisiblePRHead(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	now := time.Unix(9000, 0)

	original := "diff --git a/a.go b/a.go\n--- a/a.go\n+++ b/a.go\n@@ -1 +1 @@\n-a\n+b\n" +
		"diff --git a/b.go b/b.go\n--- a/b.go\n+++ b/b.go\n@@ -1 +1 @@\n-a\n+b\n" +
		"diff --git a/c.go b/c.go\n--- a/c.go\n+++ b/c.go\n@@ -1 +1 @@\n-a\n+b\n" +
		"diff --git a/d.go b/d.go\n--- a/d.go\n+++ b/d.go\n@@ -1 +1 @@\n-a\n+b\n" +
		"diff --git a/e.go b/e.go\n--- a/e.go\n+++ b/e.go\n@@ -1 +1 @@\n-a\n+b\n" +
		"diff --git a/f.go b/f.go\n--- a/f.go\n+++ b/f.go\n@@ -1 +1 @@\n-a\n+b\n" +
		"diff --git a/g.go b/g.go\n--- a/g.go\n+++ b/g.go\n@@ -1 +1 @@\n-a\n+b\n" +
		"diff --git a/h.go b/h.go\n--- a/h.go\n+++ b/h.go\n@@ -1 +1 @@\n-a\n+b\n"
	id, _, err := st.AdoptPRForReviewWithHeadRef(ctx, "russ", 4187, "base", "old-pr-head", "hotfix/mail-temporal-red-main", original, false, false, true, false, now, now)
	if err != nil {
		t.Fatalf("adopt: %v", err)
	}
	epoch := claimAdoptedRepair(t, ctx, st, id, now.Add(time.Minute))

	cumulativeWithCorrection := original + "diff --git a/limit_test.go b/limit_test.go\n--- a/limit_test.go\n+++ b/limit_test.go\n@@ -0,0 +1 @@\n+zero limit\n"
	resp, err := st.Result(ctx, store.ResultParams{
		JobID: id, Epoch: epoch, Now: now.Add(2 * time.Minute),
		PushedSHA: "new-pr-head", PushedBranch: "hotfix/mail-temporal-red-main",
		PatchDiff: cumulativeWithCorrection,
	})
	if err != nil || !resp.Accepted || resp.JobState != string(job.StateReviewPending) {
		t.Fatalf("fast-forward result=%+v err=%v, want safely accepted pending visibility", resp, err)
	}
	j, err := st.GetJob(ctx, id)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if j.HeadSHA == "new-pr-head" || j.State != job.StateReviewPending {
		t.Fatalf("pending repair state/head=%s/%q, worker SHA must not yet be authoritative", j.State, j.HeadSHA)
	}
	cands, err := st.ReviewPendingCandidates(ctx)
	if err != nil {
		t.Fatalf("pre-reconcile review candidates: %v", err)
	}
	for _, c := range cands {
		if c.JobID == id {
			t.Fatal("fast-forwarded repair must not be reviewed before GitHub reports its SHA")
		}
	}

	if _, err := st.ApplyReconciledPR(ctx, id, store.ReconciledPR{
		Number: 4187, HeadSHA: "new-pr-head", BaseSHA: "base", CIGreen: true,
		UpdatedAt: now.Add(3 * time.Minute),
	}, now.Add(3*time.Minute)); err != nil {
		t.Fatalf("reconcile fast-forwarded PR head: %v", err)
	}
	cands, err = st.ReviewPendingCandidates(ctx)
	if err != nil {
		t.Fatalf("post-reconcile review candidates: %v", err)
	}
	found := false
	for _, c := range cands {
		found = found || c.JobID == id
	}
	if !found {
		t.Fatal("repair should become reviewable after GitHub reports the exact head and green CI")
	}
	if diff, err := st.JobPatchDiff(ctx, id); err != nil || diff != cumulativeWithCorrection {
		t.Fatalf("stored diff=%q err=%v, want eight-file patch plus correction", diff, err)
	}
}

func TestAdoptedRepairOriginalHeadMoveRevokesRepairLease(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	now := time.Unix(9000, 0)

	original := "diff --git a/a.go b/a.go\n--- a/a.go\n+++ b/a.go\n@@ -1 +1 @@\n-a\n+b\n"
	id, _, err := st.AdoptPRForReviewWithHeadRef(ctx, "russ", 4189, "base", "old-pr-head", "hotfix/mail-temporal-red-main", original, false, false, true, false, now, now)
	if err != nil {
		t.Fatalf("adopt: %v", err)
	}
	epoch := claimAdoptedRepair(t, ctx, st, id, now.Add(time.Minute))

	out, err := st.ApplyReconciledPR(ctx, id, store.ReconciledPR{
		Number: 4189, BaseSHA: "base", HeadSHA: "foreign-head", CIGreen: false,
		UpdatedAt: now.Add(90 * time.Second),
	}, now.Add(90*time.Second))
	if err != nil {
		t.Fatalf("reconcile moved head: %v", err)
	}
	if !out.Superseded {
		t.Fatalf("concurrent original-head movement must supersede active repair, got %+v", out)
	}

	cumulativeWithCorrection := original + "diff --git a/limit_test.go b/limit_test.go\n--- a/limit_test.go\n+++ b/limit_test.go\n@@ -0,0 +1 @@\n+zero limit\n"
	_, err = st.Result(ctx, store.ResultParams{
		JobID: id, Epoch: epoch, Now: now.Add(2 * time.Minute),
		PushedSHA: "new-pr-head", PushedBranch: "hotfix/mail-temporal-red-main",
		PatchDiff: cumulativeWithCorrection,
	})
	if !errors.Is(err, lease.ErrStaleEpoch) {
		t.Fatalf("stale repair result err=%v, want stale epoch after head-move re-arm", err)
	}
	j, err := st.GetJob(ctx, id)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if j.State != job.StateReady || j.Role != job.RoleEngWorker || j.HeadSHA != "" {
		t.Fatalf("moved-head repair state/role/head=%s/%s/%q, want ready/eng_worker/empty", j.State, j.Role, j.HeadSHA)
	}
	if diff, err := st.JobPatchDiff(ctx, id); err != nil || diff != original {
		t.Fatalf("original patch not retained for re-armed repair: diff=%q err=%v", diff, err)
	}
}

func TestAdoptedRepairReplacementBranchClearsOldPRBinding(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	now := time.Unix(9000, 0)

	original := "diff --git a/a.go b/a.go\n--- a/a.go\n+++ b/a.go\n@@ -1 +1 @@\n-a\n+b\n"
	id, _, err := st.AdoptPRForReviewWithHeadRef(ctx, "russ", 4190, "base", "old-pr-head", "foreign/hotfix", original, false, false, true, false, now, now)
	if err != nil {
		t.Fatalf("adopt: %v", err)
	}
	epoch := claimAdoptedRepair(t, ctx, st, id, now.Add(time.Minute))

	cumulativeWithCorrection := original + "diff --git a/limit_test.go b/limit_test.go\n--- a/limit_test.go\n+++ b/limit_test.go\n@@ -0,0 +1 @@\n+zero limit\n"
	resp, err := st.Result(ctx, store.ResultParams{
		JobID: id, Epoch: epoch, Now: now.Add(2 * time.Minute),
		PushedSHA: "replacement-head", PushedBranch: store.PRBranch(id),
		PatchDiff: cumulativeWithCorrection,
	})
	if err != nil {
		t.Fatalf("replacement result: %v", err)
	}
	if !resp.Accepted || resp.JobState != string(job.StateReviewPending) {
		t.Fatalf("replacement response=%+v, want accepted review_pending", resp)
	}
	j, err := st.GetJob(ctx, id)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if j.PRNumber != 0 || j.HeadSHA != "" || j.HeadRef != store.PRBranch(id) || j.PendingRepairHeadSHA != "replacement-head" {
		t.Fatalf("replacement repair binding pr/head/ref/pending=%d/%q/%q/%q, want unbound/empty/%q/replacement-head",
			j.PRNumber, j.HeadSHA, j.HeadRef, j.PendingRepairHeadSHA, store.PRBranch(id))
	}
	events, err := st.LoadEvents(ctx, id)
	if err != nil {
		t.Fatalf("load events: %v", err)
	}
	folded, err := ledger.Fold(events)
	if err != nil {
		t.Fatalf("fold replacement result: %v", err)
	}
	if folded.PRNumber != j.PRNumber || folded.HeadSHA != j.HeadSHA || folded.HeadRef != j.HeadRef ||
		folded.PendingRepairHeadSHA != j.PendingRepairHeadSHA {
		t.Fatalf("replacement result fold != projection:\nfold=%+v\nprojection=%+v", folded, j)
	}
	cands, err := st.ReviewPendingCandidates(ctx)
	if err != nil {
		t.Fatalf("review candidates: %v", err)
	}
	for _, c := range cands {
		if c.JobID == id {
			t.Fatal("replacement repair must wait for replacement-PR reconciliation before review")
		}
	}
}
