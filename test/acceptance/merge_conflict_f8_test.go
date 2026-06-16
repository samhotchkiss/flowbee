// F8 acceptance: merge conflicts — blast-radius reservations, the resolve_conflict
// job, and integrated-head re-review — proven end-to-end against real SQLite (a
// temp-file DB) and a real LOCAL git mirror (no GitHub, no network, no creds; the
// untrusted-data write path is F3/R4: Flowbee does the git writes).
//
// DONE-WHEN (each proven below by a real, non-skipped test):
//   - two overlapping builds are SERIALIZED by reservation (the scheduler withholds
//     the second while the first is in flight); wide-blast single-flights;
//   - a CONFLICTING rebase spawns a resolve_conflict job that goes back through
//     review (resolving_conflict -> review_pending -> code_review -> mergeable);
//   - the INTEGRATED head re-validates REVIEW (a clean rebase re-arms review + CI at
//     the new SHA, the prior SHA-bound verdict invalidated);
//   - a stacked descendant auto-rebases + re-arms when its parent PR merges.
package acceptance

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/samhotchkiss/flowbee/internal/content"
	"github.com/samhotchkiss/flowbee/internal/gitops"
	"github.com/samhotchkiss/flowbee/internal/job"
	"github.com/samhotchkiss/flowbee/internal/scheduler"
	"github.com/samhotchkiss/flowbee/internal/store"
	"github.com/samhotchkiss/flowbee/internal/testutil"
)

// f8WriteFile writes a file in the git fixture (the agent/main edits).
func f8WriteFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// ids folds candidate ids for assertion messages.
func ids(cands []scheduler.Candidate) []string {
	out := make([]string, len(cands))
	for i, c := range cands {
		out[i] = c.JobID
	}
	return out
}

// declaredJSON encodes a content.BlastRadius declaration for a build's write-set.
func declaredJSON(t *testing.T, paths []string, scope string) string {
	t.Helper()
	b, err := json.Marshal(content.BlastRadius{Paths: paths, Scope: scope})
	if err != nil {
		t.Fatalf("marshal blast radius: %v", err)
	}
	return string(b)
}

// seedBuildWithBlast seeds a ready build job and stamps its declared blast-radius so
// the scheduler can fold its write-set into a reservation.
func seedBuildWithBlast(t *testing.T, ctx context.Context, st *store.Store, id string, paths []string, scope string, now time.Time) {
	t.Helper()
	if _, err := st.SeedJob(ctx, store.SeedParams{
		ID: id, Kind: job.KindBuild, Flow: "build", Stage: "build",
		Role: job.RoleEngWorker, BaseSHA: "base0",
		RequiredCapabilities: []string{"role:eng_worker"}, Now: now,
	}); err != nil {
		t.Fatalf("seed %s: %v", id, err)
	}
	if _, err := st.DB.ExecContext(ctx,
		`UPDATE jobs SET declared_blast_radius = ? WHERE id = ?`, declaredJSON(t, paths, scope), id); err != nil {
		t.Fatalf("stamp blast radius %s: %v", id, err)
	}
}

// TestF8_OverlappingBuildsSerializedByReservation: two ready builds whose declared
// write-sets OVERLAP must not co-dispatch. With the first in flight (leased), the
// scheduler withholds the overlapping second; a DISJOINT third still dispatches.
func TestF8_OverlappingBuildsSerializedByReservation(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	now := time.Unix(1000, 0)

	// A reserves the internal/store/ tree; B touches a file UNDER it (overlap); C is
	// disjoint (internal/api/).
	seedBuildWithBlast(t, ctx, st, "A", []string{"internal/store"}, "", now)
	seedBuildWithBlast(t, ctx, st, "B", []string{"internal/store/queries.go"}, "", now)
	seedBuildWithBlast(t, ctx, st, "C", []string{"internal/api/server.go"}, "", now)

	// before anything is in flight, all three are leasable candidates.
	cands, err := st.ReadyCandidatesReserved(ctx)
	if err != nil {
		t.Fatalf("candidates0: %v", err)
	}
	if len(cands) != 3 {
		t.Fatalf("want 3 ready candidates initially, got %d", len(cands))
	}

	// dispatch A: claim it (now in flight) AND record its reservation.
	if _, err := st.ClaimReadyJob(ctx, store.ClaimParams{
		JobID: "A", LeaseID: "lA", Identity: "alice", ModelFamily: "codex",
		Role: job.RoleEngWorker, Attested: []string{"role:eng_worker"},
		TTL: time.Minute, Now: now,
	}); err != nil {
		t.Fatalf("claim A: %v", err)
	}
	if err := st.RecordReservation(ctx, "A"); err != nil {
		t.Fatalf("record reservation A: %v", err)
	}

	// now the scheduler must WITHHOLD B (overlaps A's internal/store/ write-set) but
	// still offer C (disjoint). This is the §E "avoid the conflict first" serialization.
	cands, err = st.ReadyCandidatesReserved(ctx)
	if err != nil {
		t.Fatalf("candidates1: %v", err)
	}
	got := map[string]bool{}
	for _, c := range cands {
		got[c.JobID] = true
	}
	if got["B"] {
		t.Fatalf("B overlaps the in-flight A and MUST be withheld (got candidates %v)", ids(cands))
	}
	if !got["C"] {
		t.Fatalf("C is disjoint and must still dispatch (got candidates %v)", ids(cands))
	}

	// the raw ranking (Order) would have offered B; the reservation filter is what
	// serializes it. Prove the reservation set folds A's declared write-set.
	resvs, err := st.ActiveReservations(ctx)
	if err != nil {
		t.Fatalf("active reservations: %v", err)
	}
	if len(resvs) != 1 || resvs[0].JobID != "A" {
		t.Fatalf("expected exactly A reserved, got %+v", resvs)
	}
}

// TestF8_WideBlastSingleFlights: a wide-blast (refactor) build single-flights the
// whole tree while in flight — every other candidate is withheld.
func TestF8_WideBlastSingleFlights(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	now := time.Unix(1000, 0)

	seedBuildWithBlast(t, ctx, st, "refactor", nil, "wide", now)
	seedBuildWithBlast(t, ctx, st, "feature", []string{"internal/api/server.go"}, "", now)

	if _, err := st.ClaimReadyJob(ctx, store.ClaimParams{
		JobID: "refactor", LeaseID: "lr", Identity: "alice", ModelFamily: "codex",
		Role: job.RoleEngWorker, Attested: []string{"role:eng_worker"},
		TTL: time.Minute, Now: now,
	}); err != nil {
		t.Fatalf("claim refactor: %v", err)
	}
	if err := st.RecordReservation(ctx, "refactor"); err != nil {
		t.Fatalf("record reservation refactor: %v", err)
	}

	cands, err := st.ReadyCandidatesReserved(ctx)
	if err != nil {
		t.Fatalf("candidates: %v", err)
	}
	if len(cands) != 0 {
		t.Fatalf("a wide-blast refactor must single-flight the tree, got candidates %v", ids(cands))
	}
}

// f8Mirror builds a local bare repo with one commit on main, plus a SECOND commit on
// main that conflicts with a patch, so a rebase onto the new head must conflict.
func f8Mirror(t *testing.T) (*gitops.Mirror, string) {
	t.Helper()
	root := t.TempDir()
	bare := filepath.Join(root, "mirror.git")
	m, err := gitops.InitBare(bare)
	if err != nil {
		t.Fatalf("init bare: %v", err)
	}
	work := filepath.Join(root, "seed")
	runGit(t, "", "clone", bare, work)
	f8WriteFile(t, filepath.Join(work, "shared.txt"), "line one\nline two\nline three\n")
	runGit(t, work, "-c", "user.email=t@t", "-c", "user.name=t", "add", "-A")
	runGit(t, work, "-c", "user.email=t@t", "-c", "user.name=t", "commit", "-m", "init")
	runGit(t, work, "branch", "-M", "main")
	runGit(t, work, "push", "origin", "main")
	base, err := m.HeadSHA("refs/heads/main")
	if err != nil {
		t.Fatalf("head sha: %v", err)
	}
	return m, base
}

// TestF8_CleanRebaseReValidatesReviewAtIntegratedHead: a build that already passed
// review (a SHA-bound verdict) sees its base move; a CLEAN rebase re-arms it back
// through REVIEW at the integrated head (not just CI), the prior verdict invalidated.
// This is the merge-queue integrated-head re-validation: the verdict re-arms at the
// integrated SHA.
func TestF8_CleanRebaseReValidatesReviewAtIntegratedHead(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	now := time.Unix(1000, 0)

	// a build that reached `mergeable` with a verdict bound to (head=h1, base=base0).
	if _, err := st.SeedJob(ctx, store.SeedParams{
		ID: "j", Kind: job.KindBuild, Flow: "build", Stage: "build",
		Role: job.RoleEngWorker, BaseSHA: "base0",
		RequiredCapabilities: []string{"role:eng_worker"}, Now: now,
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	v := job.MintVerdict(job.VerdictApproved, job.DispositionHandoff, "h1", "base0")
	vb, _ := json.Marshal(v)
	if _, err := st.DB.ExecContext(ctx,
		`UPDATE jobs SET state='mergeable', verdict=?, head_sha='h1', lease_epoch=2 WHERE id='j'`, string(vb)); err != nil {
		t.Fatalf("set mergeable: %v", err)
	}

	// the base moved to base1: a CLEAN rebase (no mirror, ForceConflict=false) re-arms
	// the job back through review at the new integrated head.
	res, err := st.RebaseOnto(ctx, nil, store.RebaseOntoParams{
		JobID: "j", NewBaseSHA: "base1", Now: time.Unix(1100, 0),
	})
	if err != nil {
		t.Fatalf("rebase onto: %v", err)
	}
	if !res.Clean {
		t.Fatalf("expected a clean rebase, got %+v", res)
	}
	j, _ := st.GetJob(ctx, "j")
	if j.State != job.StateReviewPending {
		t.Fatalf("clean rebase must re-arm REVIEW (review_pending), got %s", j.State)
	}
	if j.Verdict != nil {
		t.Fatalf("the SHA-bound verdict must be invalidated at the new integrated head")
	}
	if j.BaseSHA != "base1" {
		t.Fatalf("base not advanced to the integrated head: %s", j.BaseSHA)
	}
	if j.LeaseEpoch != 3 {
		t.Fatalf("epoch not bumped on rebase (a still-running worker must be fenced): %d", j.LeaseEpoch)
	}
	// the re-armed job is leasable by a code_reviewer (caps flipped), not an eng_worker.
	want := []string{"role:code_reviewer"}
	if len(j.RequiredCapabilities) != 1 || j.RequiredCapabilities[0] != want[0] {
		t.Fatalf("re-armed caps=%v want %v (re-review at the integrated SHA)", j.RequiredCapabilities, want)
	}
}

// TestF8_RealConflictSpawnsResolveJobThenReReview: a REAL conflict (proven by a real
// git rebase against a conflicting new base) spawns a resolve_conflict job. A
// conflict_resolver claims it, returns the resolved diff, and the job goes BACK
// THROUGH REVIEW (review_pending -> code_review -> mergeable) — resolution is just
// another job, gated like any build.
func TestF8_RealConflictSpawnsResolveJobThenReReview(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	now := time.Unix(1000, 0)

	m, base := f8Mirror(t)

	// a build whose patch edits shared.txt's middle line.
	patch := "" +
		"diff --git a/shared.txt b/shared.txt\n" +
		"index 0000000..1111111 100644\n" +
		"--- a/shared.txt\n" +
		"+++ b/shared.txt\n" +
		"@@ -1,3 +1,3 @@\n" +
		" line one\n" +
		"-line two\n" +
		"+line two EDITED BY BUILD\n" +
		" line three\n"

	if _, err := st.SeedJob(ctx, store.SeedParams{
		ID: "cj", Kind: job.KindBuild, Flow: "build", Stage: "build",
		Role: job.RoleEngWorker, BaseSHA: base,
		RequiredCapabilities: []string{"role:eng_worker"}, Now: now,
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	// stamp the build's patch + a verdict (it had passed review), plus the builder
	// identity (so the resolver anti-affinity has a sibling to exclude).
	v := job.MintVerdict(job.VerdictApproved, job.DispositionHandoff, "h1", base)
	vb, _ := json.Marshal(v)
	if _, err := st.DB.ExecContext(ctx, `
		UPDATE jobs SET state='mergeable', verdict=?, head_sha='h1', lease_epoch=2,
		       patch_diff=?, builder_identity='builder-codex', builder_model_family='codex',
		       eng_worker_job=id
		 WHERE id='cj'`, string(vb), patch); err != nil {
		t.Fatalf("stamp build: %v", err)
	}

	// main moves: a SECOND commit rewrites the SAME middle line, so the build's patch
	// cannot replay cleanly — a REAL conflict.
	work := filepath.Join(t.TempDir(), "advance")
	runGit(t, "", "clone", m.Path, work)
	runGit(t, work, "checkout", "main")
	f8WriteFile(t, filepath.Join(work, "shared.txt"), "line one\nline two CHANGED ON MAIN\nline three\n")
	runGit(t, work, "-c", "user.email=t@t", "-c", "user.name=t", "add", "-A")
	runGit(t, work, "-c", "user.email=t@t", "-c", "user.name=t", "commit", "-m", "main moved")
	runGit(t, work, "push", "origin", "main")
	newBase, err := m.HeadSHA("refs/heads/main")
	if err != nil {
		t.Fatalf("new base: %v", err)
	}

	// the real rebase onto the moved main CONFLICTS -> a resolve_conflict job.
	res, err := st.RebaseOnto(ctx, m, store.RebaseOntoParams{
		JobID: "cj", NewBaseSHA: newBase, Now: time.Unix(1100, 0),
	})
	if err != nil {
		t.Fatalf("rebase onto: %v", err)
	}
	if res.Clean || !res.ResolverNeeded || res.ConflictJob != "cj" {
		t.Fatalf("a real conflict must spawn a resolve_conflict job, got %+v", res)
	}
	j, _ := st.GetJob(ctx, "cj")
	if j.State != job.StateResolvingConflict {
		t.Fatalf("conflict must route to resolving_conflict, got %s", j.State)
	}
	if j.Role != job.RoleConflictResolver {
		t.Fatalf("conflict job role=%s want conflict_resolver", j.Role)
	}
	if j.Verdict != nil {
		t.Fatalf("the prior verdict must be invalidated by the conflict")
	}

	// the resolve_conflict job is a leasable candidate for a conflict_resolver.
	cc, err := st.ResolvingConflictCandidates(ctx)
	if err != nil {
		t.Fatalf("conflict candidates: %v", err)
	}
	if len(cc) != 1 || cc[0].JobID != "cj" {
		t.Fatalf("expected cj as a resolve_conflict candidate, got %v", ids(cc))
	}

	// a DIFFERENT identity (anti-affinity: not the builder, not its model family)
	// claims the resolve_conflict job.
	rl, err := st.ClaimConflictJob(ctx, store.ClaimConflictParams{
		JobID: "cj", LeaseID: "lr", Identity: "resolver-opus", ModelFamily: "opus",
		Attested: []string{"role:conflict_resolver"}, TTL: time.Minute, Now: time.Unix(1200, 0),
	})
	if err != nil || rl == nil {
		t.Fatalf("claim resolve_conflict: err=%v lease=%v", err, rl)
	}
	jc, _ := st.GetJob(ctx, "cj")
	if jc.BoundIdentity != "resolver-opus" {
		t.Fatalf("resolver not bound: %s", jc.BoundIdentity)
	}

	// the builder's OWN identity must NOT win the resolution (anti-affinity).
	if _, err := st.ClaimConflictJob(ctx, store.ClaimConflictParams{
		JobID: "cj", LeaseID: "lr2", Identity: "builder-codex", ModelFamily: "codex",
		Attested: []string{"role:conflict_resolver"}, TTL: time.Minute, Now: time.Unix(1201, 0),
	}); err == nil {
		t.Fatalf("the builder must not win its own conflict resolution (anti-affinity)")
	}

	// the resolver returns the RESOLVED diff (a clean patch against the new base).
	resolved := "" +
		"diff --git a/shared.txt b/shared.txt\n" +
		"index 2222222..3333333 100644\n" +
		"--- a/shared.txt\n" +
		"+++ b/shared.txt\n" +
		"@@ -1,3 +1,3 @@\n" +
		" line one\n" +
		"-line two CHANGED ON MAIN\n" +
		"+line two EDITED BY BUILD + MAIN MERGED\n" +
		" line three\n"
	if _, err := st.ResolveConflictResult(ctx, store.ResolveConflictParams{
		JobID: "cj", Epoch: rl.Epoch, ResolvedDiff: resolved,
		DeclaredBlastRadius: declaredJSON(t, []string{"shared.txt"}, ""),
		Now:                 time.Unix(1300, 0),
	}); err != nil {
		t.Fatalf("resolve conflict result: %v", err)
	}

	// the resolved diff goes BACK THROUGH REVIEW: review_pending, the resolved patch
	// stored as the untrusted product to be re-gated, the verdict still clear.
	jr, _ := st.GetJob(ctx, "cj")
	if jr.State != job.StateReviewPending {
		t.Fatalf("resolution must re-enter review (review_pending), got %s", jr.State)
	}
	if jr.Verdict != nil {
		t.Fatalf("the resolved diff must be re-judged (no carried verdict)")
	}
	storedDiff := ""
	if err := st.DB.QueryRowContext(ctx, `SELECT patch_diff FROM jobs WHERE id='cj'`).Scan(&storedDiff); err != nil {
		t.Fatalf("read stored diff: %v", err)
	}
	if storedDiff != resolved {
		t.Fatalf("resolved diff not stored as the re-gated product")
	}

	// re-review the resolved product to mergeable: a reviewer (still not the builder)
	// claims, the gate re-runs over reconciled-green facts at the new head + base.
	if err := st.UpsertDomainBFacts(ctx, "cj", job.DomainBFacts{
		PRExists: true, PRNumber: 4242, HeadSHA: "h2", BaseSHA: newBase, CIGreen: true,
	}); err != nil {
		t.Fatalf("upsert facts: %v", err)
	}
	rv, err := st.ClaimReviewJob(ctx, store.ClaimReviewParams{
		JobID: "cj", LeaseID: "lrev", Identity: "reviewer-opus2", ModelFamily: "opus",
		Attested: []string{"role:code_reviewer"}, TTL: time.Minute, Now: time.Unix(1400, 0),
	})
	if err != nil || rv == nil {
		t.Fatalf("claim re-review: err=%v lease=%v", err, rv)
	}
	resp, err := st.ReviewResult(ctx, store.DBFactSource{DB: st.DB}, job.Policy{}, store.ReviewResultParams{
		JobID: "cj", Epoch: rv.Epoch, Claim: job.VerdictApproved, Disposition: job.DispositionHandoff,
		Now: time.Unix(1500, 0),
	})
	if err != nil {
		t.Fatalf("re-review result: %v", err)
	}
	if !resp.Minted || resp.JobState != string(job.StateMergeable) {
		t.Fatalf("re-review of the resolved diff must mint + reach mergeable, got %+v", resp)
	}
	jm, _ := st.GetJob(ctx, "cj")
	if jm.Verdict == nil || !jm.Verdict.Verify("h2", newBase) {
		t.Fatalf("re-CI'd resolution must mint a fresh SHA-bound verdict at the integrated head")
	}
}

// TestF8_StackedDescendantAutoRebasesOnParentMerge: a job stacked on a parent PR
// auto-rebases + re-arms (review re-validates at the new base) when the parent merges.
func TestF8_StackedDescendantAutoRebasesOnParentMerge(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	now := time.Unix(1000, 0)

	// a descendant build stacked on parent PR #77, already at mergeable with a verdict.
	if _, err := st.SeedJob(ctx, store.SeedParams{
		ID: "child", Kind: job.KindBuild, Flow: "build", Stage: "build",
		Role: job.RoleEngWorker, BaseSHA: "parent-head",
		RequiredCapabilities: []string{"role:eng_worker"}, Now: now,
	}); err != nil {
		t.Fatalf("seed child: %v", err)
	}
	v := job.MintVerdict(job.VerdictApproved, job.DispositionHandoff, "child-h1", "parent-head")
	vb, _ := json.Marshal(v)
	if _, err := st.DB.ExecContext(ctx,
		`UPDATE jobs SET state='mergeable', verdict=?, head_sha='child-h1', lease_epoch=2 WHERE id='child'`, string(vb)); err != nil {
		t.Fatalf("set child mergeable: %v", err)
	}
	if err := st.MarkStackedOn(ctx, "child", 77); err != nil {
		t.Fatalf("mark stacked: %v", err)
	}

	// the parent PR #77 merges; main is now newmain. Every descendant auto-rebases.
	rearmed, err := st.RearmStackedDescendants(ctx, nil, 77, "newmain", time.Unix(1100, 0))
	if err != nil {
		t.Fatalf("rearm stacked: %v", err)
	}
	if len(rearmed) != 1 || rearmed[0] != "child" {
		t.Fatalf("the stacked descendant must re-arm, got %v", rearmed)
	}
	jc, _ := st.GetJob(ctx, "child")
	if jc.State != job.StateReviewPending {
		t.Fatalf("descendant must re-validate review at the new base, got %s", jc.State)
	}
	if jc.Verdict != nil {
		t.Fatalf("the descendant's SHA-bound verdict must invalidate on the parent merge")
	}
	if jc.BaseSHA != "newmain" {
		t.Fatalf("descendant not rebased onto the new main: %s", jc.BaseSHA)
	}
	// the stack pointer is cleared (the parent is gone).
	var stacked int
	if err := st.DB.QueryRowContext(ctx, `SELECT stacked_on_pr FROM jobs WHERE id='child'`).Scan(&stacked); err != nil {
		t.Fatalf("read stacked: %v", err)
	}
	if stacked != 0 {
		t.Fatalf("stack pointer not cleared after parent merge: %d", stacked)
	}
}
