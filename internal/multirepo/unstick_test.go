package multirepo_test

import (
	"context"
	"testing"
	"time"

	"github.com/samhotchkiss/flowbee/internal/clock"
	gh "github.com/samhotchkiss/flowbee/internal/github"
	"github.com/samhotchkiss/flowbee/internal/job"
	"github.com/samhotchkiss/flowbee/internal/multirepo"
	"github.com/samhotchkiss/flowbee/internal/store"
	"github.com/samhotchkiss/flowbee/internal/testutil"
)

// seedHandoff parks a build job in merge_handoff with an open PR (the #214 rot shape).
func seedHandoff(t *testing.T, st *store.Store, id, repo string, pr int, now time.Time) {
	t.Helper()
	ctx := context.Background()
	if _, err := st.SeedJob(ctx, store.SeedParams{
		ID: id, Kind: job.KindBuild, Flow: "build", Stage: "build",
		Role: job.RoleEngWorker, Repo: repo, Now: now,
	}); err != nil {
		t.Fatalf("seed %s: %v", id, err)
	}
	if _, err := st.DB.ExecContext(ctx,
		`UPDATE jobs SET state='merge_handoff', issue_number=? WHERE id=?`, pr, id); err != nil {
		t.Fatalf("park %s: %v", id, err)
	}
	if _, err := st.DB.ExecContext(ctx, `
		INSERT INTO domain_b_facts (job_id, pr_exists, pr_number, ci_green, merged, updated_at)
		VALUES (?, 1, ?, 1, 0, ?)`, id, pr, now.Format(time.RFC3339Nano)); err != nil {
		t.Fatalf("facts %s: %v", id, err)
	}
}

// TestUnstickAllFastForwardsBehindPRs: UnstickAll update-branches a merge_handoff PR that is
// BEHIND (and only that one — a clean PR is left alone), and is idempotent (a PR no longer
// behind after the FF isn't re-updated). It NEVER merges.
func TestUnstickAllFastForwardsBehindPRs(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	now := time.Unix(10_000, 0)

	if err := st.RegisterRepo(ctx, store.Repo{ID: "core", Owner: "acme", Repo: "core", DefaultBranch: "main", Active: true}); err != nil {
		t.Fatal(err)
	}
	fake := gh.NewFake()
	mgr, err := multirepo.New(ctx, st, clock.NewFake(now), nil,
		func(store.Repo) (gh.Client, gh.Writer, error) { return fake, fake, nil })
	if err != nil {
		t.Fatal(err)
	}

	seedHandoff(t, st, "behind", "core", 77, now) // rotting behind base
	seedHandoff(t, st, "clean", "core", 88, now)  // up to date already
	fake.SetMergeableState(77, "behind")
	fake.SetMergeableState(88, "clean")

	counts, err := mgr.UnstickAll(ctx)
	if err != nil {
		t.Fatalf("unstick: %v", err)
	}
	if counts["core"] != 1 {
		t.Fatalf("core un-stuck count = %d, want 1 (only the behind PR)", counts["core"])
	}
	got := fake.UpdatedBranches()
	if len(got) != 1 || got[0] != 77 {
		t.Fatalf("UpdateBranch called for %v, want [77] (the behind PR, never the clean one)", got)
	}
	// the fake FF'd #77 to clean, so a second pass is a no-op (idempotent — no spam).
	counts2, _ := mgr.UnstickAll(ctx)
	if counts2["core"] != 0 {
		t.Fatalf("second pass un-stuck %d, want 0 (the FF'd PR is no longer behind)", counts2["core"])
	}
	// and it NEVER merged.
	for _, c := range fake.Calls() {
		if c == "EnqueueMergeQueue(77)" {
			t.Fatal("un-stick must never merge a PR")
		}
	}
}

// TestUnstickAllSkipsConflictAndUnknown: a real conflict (UpdateBranch 422) and an async
// "unknown" mergeable_state are both left for a later pass / a human — never force-merged,
// never fatal.
func TestUnstickAllSkipsConflictAndUnknown(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	now := time.Unix(10_000, 0)

	if err := st.RegisterRepo(ctx, store.Repo{ID: "core", Owner: "acme", Repo: "core", DefaultBranch: "main", Active: true}); err != nil {
		t.Fatal(err)
	}
	fake := gh.NewFake()
	mgr, err := multirepo.New(ctx, st, clock.NewFake(now), nil,
		func(store.Repo) (gh.Client, gh.Writer, error) { return fake, fake, nil })
	if err != nil {
		t.Fatal(err)
	}

	seedHandoff(t, st, "unknown", "core", 90, now)
	seedHandoff(t, st, "conflict", "core", 91, now)
	fake.SetMergeableState(90, "unknown") // GitHub hasn't computed it yet
	fake.SetMergeableState(91, "behind")
	fake.FailNextWriteWith(&gh.ErrGitHub{StatusCode: 422, Method: "PUT", Path: "/pulls/91/update-branch", Body: "merge conflict"})

	counts, err := mgr.UnstickAll(ctx)
	if err != nil {
		t.Fatalf("unstick must not be fatal on a per-PR error: %v", err)
	}
	if counts["core"] != 0 {
		t.Fatalf("unknown is skipped and the conflict FF failed, so count = %d, want 0", counts["core"])
	}
	if len(fake.UpdatedBranches()) != 0 {
		t.Fatalf("no branch should be recorded updated (90 unknown, 91 conflicted), got %v", fake.UpdatedBranches())
	}
}
