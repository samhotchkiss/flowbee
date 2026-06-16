package project

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/samhotchkiss/flowbee/internal/clock"
	gh "github.com/samhotchkiss/flowbee/internal/github"
	"github.com/samhotchkiss/flowbee/internal/gitops"
	"github.com/samhotchkiss/flowbee/internal/history"
	"github.com/samhotchkiss/flowbee/internal/job"
	"github.com/samhotchkiss/flowbee/internal/store"
	"github.com/samhotchkiss/flowbee/internal/testutil"
)

// newHistoryMirror builds a local bare mirror with one commit on main (no network,
// no GitHub) — the integration branch the dedicated post-merge history commit lands
// on. Returns the mirror and the base SHA of main.
func newHistoryMirror(t *testing.T) (*gitops.Mirror, string) {
	t.Helper()
	root := t.TempDir()
	bare := root + "/mirror.git"
	m, err := gitops.InitBare(bare)
	if err != nil {
		t.Fatalf("init bare: %v", err)
	}
	work := root + "/seed"
	mustRun(t, "", "git", "clone", bare, work)
	mustWrite(t, work+"/README.md", "hello\n")
	mustRun(t, work, "git", "-c", "user.email=t@t", "-c", "user.name=t", "add", "-A")
	mustRun(t, work, "git", "-c", "user.email=t@t", "-c", "user.name=t", "commit", "-m", "init")
	mustRun(t, work, "git", "branch", "-M", "main")
	mustRun(t, work, "git", "push", "origin", "main")
	base, err := m.HeadSHA("refs/heads/main")
	if err != nil {
		t.Fatalf("head sha: %v", err)
	}
	return m, base
}

// TestF11HistoryArchiveOnMerge is the F11 acceptance test (build-list §F): on merge,
// Flowbee writes docs/history/<id>.md (a curated card: status, attempts, verdicts,
// linked PR, lessons) + a generated TOC, as a read-model FOLD over the event ledger,
// Flowbee the sole writer (a dedicated post-merge commit; not entangled with the
// feature PR), reconstructable from job_events.
func TestF11HistoryArchiveOnMerge(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	mirror, base := newHistoryMirror(t)

	// the project-OUT sender, wired with the LOCAL-git history writer (the §F sole
	// writer) landing dedicated commits on main.
	clk := clock.NewFake(time.Unix(5000, 0))
	sender := New(st, gh.NewFake(), clk, nil).WithHistory(mirror, "main")

	const id = "build-healthz"
	const head = "headSHA-aaaaaaaaaaaa"

	// ── drive a REAL build lifecycle through the store: seed -> claim -> result ->
	// bind facts -> claim review -> bounce -> re-build -> approve (mint verdict). The
	// events this produces are the canonical ledger the card folds from.
	now := time.Unix(1000, 0)
	if _, err := st.SeedJob(ctx, store.SeedParams{
		ID: id, Kind: job.KindBuild, Flow: "build", Stage: "build", Role: job.RoleEngWorker,
		BaseSHA: base, Now: now,
		TaskText: "Add a /healthz endpoint\nReturns 200 with build info.",
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	src := store.DBFactSource{DB: st.DB}
	policy := job.Policy{AllowSelfMerge: true} // Branch B production posture

	// attempt 1: build -> review -> BOUNCE (records a lesson + a bounce counter).
	buildAndReview(t, st, src, policy, id, base, head, 1,
		job.VerdictChangesRequested, job.DispositionHandoff, "k1")
	// attempt 2: re-build -> review -> APPROVE self_merge (mints a SHA-bound verdict).
	buildAndReview(t, st, src, policy, id, base, head, 2,
		job.VerdictApproved, job.DispositionSelfMerge, "k2")

	// bind the PR number the way the §7.3 PR-open path would (Flowbee owns it).
	mergeNow := time.Unix(9000, 0)
	if err := st.StampPRNumber(ctx, id, 42, head, base, mergeNow); err != nil {
		t.Fatalf("stamp pr: %v", err)
	}

	// ── the MERGE: reconcile-IN sees the PR merged. This is "completing a job" — it
	// transitions the job to done AND enqueues the dedicated post-merge history write.
	out, err := st.ApplyReconciledPR(ctx, id, store.ReconciledPR{
		Number: 42, UpdatedAt: mergeNow, HeadSHA: head, BaseSHA: base,
		Merged: true, MergeCommit: "mergecommit-abc123", CIGreen: true,
	}, mergeNow)
	if err != nil {
		t.Fatalf("reconcile merged: %v", err)
	}
	if !out.Done {
		t.Fatalf("merge did not complete the job: %+v", out)
	}

	// the history write is now a pending outbox row (enqueued atomically with done).
	row, ok, err := st.NextPendingOutbox(ctx)
	if err != nil || !ok {
		t.Fatalf("history write not enqueued: ok=%v err=%v", ok, err)
	}
	if row.Action != store.ActionWriteHistory || row.JobID != id {
		t.Fatalf("unexpected pending row: %+v", row)
	}

	// ── drain the sender: Flowbee (sole writer) lands the dedicated post-merge commit.
	n, err := sender.DrainOnce(ctx)
	if err != nil || n != 1 {
		t.Fatalf("drain history: n=%d err=%v", n, err)
	}

	// ── PROVE the archive on disk: the card + the TOC are committed on main.
	cardPath := history.CardPath(id)
	card, found, err := mirror.ReadFileAtRef("refs/heads/main", cardPath)
	if err != nil || !found {
		t.Fatalf("history card not committed at %s: found=%v err=%v", cardPath, found, err)
	}
	for _, want := range []string{
		"# Add a /healthz endpoint",         // curated title
		"**Status:** done",                  // status
		"**PR:** #42",                       // linked PR
		"**Merge commit:** `mergecommit-abc123`",
		"**Attempts:**",                     // attempts
		"## Verdicts",                       // verdicts section
		"approved",                          // the minted verdict
		"## Lessons",                        // curated lessons
		"bounced",                           // the bounce lesson (precedent)
		"## Timeline",                       // institutional timeline
	} {
		if !strings.Contains(card, want) {
			t.Fatalf("history card missing %q:\n%s", want, card)
		}
	}

	toc, found, err := mirror.ReadFileAtRef("refs/heads/main", history.TOCPath)
	if err != nil || !found {
		t.Fatalf("history TOC not committed: found=%v err=%v", found, err)
	}
	if !strings.Contains(toc, id) || !strings.Contains(toc, "Add a /healthz endpoint") {
		t.Fatalf("TOC does not index the completed job:\n%s", toc)
	}
	if !strings.Contains(toc, "[`"+id+"`](./"+id+".md)") {
		t.Fatalf("TOC missing card link for %s:\n%s", id, toc)
	}

	// ── PROVE the dedicated commit is NOT entangled with the feature PR: its parent
	// is the prior main tip (the init commit), it touches ONLY docs/history/*, and it
	// is authored by Flowbee.
	headSHA, _ := mirror.RefSHA("refs/heads/main")
	if headSHA == base {
		t.Fatalf("history commit did not advance main")
	}
	parent := revParse(t, mirror, headSHA+"^")
	if parent != base {
		t.Fatalf("history commit parent %s != prior main %s (entangled with other work)", parent, base)
	}
	changed := commitFiles(t, mirror, headSHA)
	for _, f := range changed {
		if !strings.HasPrefix(f, "docs/history/") {
			t.Fatalf("history commit touched a non-archive file %q (entangled): %v", f, changed)
		}
	}
	if author := commitAuthor(t, mirror, headSHA); author != "flowbee" {
		t.Fatalf("history commit not authored by Flowbee (sole writer): %q", author)
	}

	// ── PROVE reconstructable from job_events: independently fold the ledger and
	// assert it renders byte-identical to what was committed.
	events, err := st.LoadEvents(ctx, id)
	if err != nil {
		t.Fatalf("load events: %v", err)
	}
	rebuilt, err := history.Fold(events)
	if err != nil {
		t.Fatalf("refold: %v", err)
	}
	if history.Render(rebuilt) != card {
		t.Fatalf("committed card is NOT reconstructable from job_events:\n--committed--\n%s\n--refolded--\n%s",
			card, history.Render(rebuilt))
	}
	if rebuilt.PRNumber != 42 || rebuilt.Status != job.StateDone || rebuilt.Bounces < 1 {
		t.Fatalf("refolded card facts wrong: %+v", rebuilt)
	}

	// ── PROVE the precedent gate can query it: the store's read-model returns the
	// same card directly from job_events (zero-tooling grounding for a hook).
	queried, err := st.HistoryCardForJob(ctx, id)
	if err != nil {
		t.Fatalf("precedent query: %v", err)
	}
	if queried.JobID != id || len(queried.Verdicts) == 0 {
		t.Fatalf("precedent query card wrong: %+v", queried)
	}

	// ── idempotent re-drain: a re-enqueue + re-drain does NOT author a second commit
	// (the §8.2 dedupe key + the no-change guard).
	if err := st.EnqueueOutbox(ctx, store.OutboxRow{JobID: id, Action: store.ActionWriteHistory, HeadSHA: id}); err != nil {
		t.Fatalf("re-enqueue: %v", err)
	}
	beforeSHA, _ := mirror.RefSHA("refs/heads/main")
	if _, err := sender.DrainOnce(ctx); err != nil {
		t.Fatalf("re-drain: %v", err)
	}
	afterSHA, _ := mirror.RefSHA("refs/heads/main")
	if beforeSHA != afterSHA {
		t.Fatalf("re-drain authored a duplicate history commit: %s -> %s", beforeSHA, afterSHA)
	}
}

// buildAndReview drives one build attempt to a code-review verdict using the REAL
// store pipeline: claim build -> result -> bind reconciled head -> claim review ->
// post the review claim (the engine mints/bounces over the reconciled facts).
func buildAndReview(t *testing.T, st *store.Store, src store.FactSource, policy job.Policy,
	id, base, head string, attempt int, claim job.VerdictValue, disp job.Disposition, idemp string) {
	t.Helper()
	ctx := context.Background()
	suffix := string(rune('a' + attempt))

	ls, err := st.ClaimReadyJob(ctx, store.ClaimParams{
		JobID: id, LeaseID: "bl-" + suffix, Identity: "go-developer", ModelFamily: "codex",
		Role: job.RoleEngWorker, Attested: []string{"role:eng_worker", "model_family:codex"},
		TTL: time.Minute, Now: time.Unix(int64(1000+attempt*100), 0),
	})
	if err != nil {
		t.Fatalf("claim build attempt %d: %v", attempt, err)
	}
	if _, err := st.Result(ctx, store.ResultParams{
		JobID: id, Epoch: ls.Epoch, Now: time.Unix(int64(1050+attempt*100), 0),
	}); err != nil {
		t.Fatalf("build result attempt %d: %v", attempt, err)
	}
	// reconcile the head + green CI the gate judges (a self_merge needs green facts).
	if err := st.SetReconciledFacts(ctx, id, store.ReconciledPR{
		Number: 0, HeadSHA: head, BaseSHA: base, CIGreen: true,
	}); err != nil {
		t.Fatalf("set facts attempt %d: %v", attempt, err)
	}
	rls, err := st.ClaimReviewJob(ctx, store.ClaimReviewParams{
		JobID: id, LeaseID: "rl-" + suffix, Identity: "senior-code-reviewer", ModelFamily: "opus",
		Attested: []string{"role:code_reviewer", "model_family:opus"},
		TTL: time.Minute, Now: time.Unix(int64(1080+attempt*100), 0),
	})
	if err != nil {
		t.Fatalf("claim review attempt %d: %v", attempt, err)
	}
	if _, err := st.ReviewResult(ctx, src, policy, store.ReviewResultParams{
		JobID: id, Epoch: rls.Epoch, Claim: claim, Disposition: disp,
		IdempotencyKey: idemp, Now: time.Unix(int64(1090+attempt*100), 0),
	}); err != nil {
		t.Fatalf("review result attempt %d: %v", attempt, err)
	}
}

