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

// TestRepoRegistryCRUD: the F9 repos registry round-trips: register (idempotent
// upsert), get, list (active filter), park/resume.
func TestRepoRegistryCRUD(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()

	if err := st.RegisterRepo(ctx, store.Repo{ID: "core", Owner: "acme", Repo: "core", DefaultBranch: "main", Active: true}); err != nil {
		t.Fatalf("register core: %v", err)
	}
	// empty default_branch defaults to main.
	if err := st.RegisterRepo(ctx, store.Repo{ID: "web", Owner: "acme", Repo: "web", Active: true}); err != nil {
		t.Fatalf("register web: %v", err)
	}
	r, err := st.GetRepo(ctx, "web")
	if err != nil {
		t.Fatalf("get web: %v", err)
	}
	if r.DefaultBranch != "main" {
		t.Fatalf("default branch not defaulted: %q", r.DefaultBranch)
	}

	// idempotent upsert: re-register core with a changed branch.
	if err := st.RegisterRepo(ctx, store.Repo{ID: "core", Owner: "acme", Repo: "core", DefaultBranch: "trunk", Active: true}); err != nil {
		t.Fatalf("re-register core: %v", err)
	}
	r, _ = st.GetRepo(ctx, "core")
	if r.DefaultBranch != "trunk" {
		t.Fatalf("upsert did not update branch: %q", r.DefaultBranch)
	}

	repos, err := st.ListRepos(ctx, true)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(repos) != 2 || repos[0].ID != "core" || repos[1].ID != "web" {
		t.Fatalf("list mismatch: %+v", repos)
	}

	// park web => omitted from the active list.
	if err := st.SetRepoActive(ctx, "web", false); err != nil {
		t.Fatalf("park web: %v", err)
	}
	active, _ := st.ListRepos(ctx, true)
	if len(active) != 1 || active[0].ID != "core" {
		t.Fatalf("parked repo still active: %+v", active)
	}
	all, _ := st.ListRepos(ctx, false)
	if len(all) != 2 {
		t.Fatalf("parked repo missing from full list: %+v", all)
	}

	if _, err := st.GetRepo(ctx, "nope"); !errors.Is(err, store.ErrRepoNotFound) {
		t.Fatalf("expected ErrRepoNotFound, got %v", err)
	}
	if err := st.SetRepoActive(ctx, "nope", true); !errors.Is(err, store.ErrRepoNotFound) {
		t.Fatalf("expected ErrRepoNotFound on park, got %v", err)
	}
}

// TestJobIDForPRInRepoScoping: the keystone scoping invariant. The SAME PR number
// in two different repos binds to two different jobs — a swept PR in repo A never
// cross-binds to repo B's job.
func TestJobIDForPRInRepoScoping(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	now := time.Unix(1000, 0)

	if _, err := st.SeedJob(ctx, store.SeedParams{
		ID: "jA", Kind: job.KindBuild, Flow: "build", Stage: "build",
		Role: job.RoleEngWorker, BaseSHA: "a0", Repo: "core", Now: now,
	}); err != nil {
		t.Fatalf("seed jA: %v", err)
	}
	if _, err := st.SeedJob(ctx, store.SeedParams{
		ID: "jB", Kind: job.KindBuild, Flow: "build", Stage: "build",
		Role: job.RoleEngWorker, BaseSHA: "b0", Repo: "web", Now: now,
	}); err != nil {
		t.Fatalf("seed jB: %v", err)
	}
	// both jobs get PR #1000 (collision across repos).
	if err := st.BindPRNumber(ctx, "jA", 1000); err != nil {
		t.Fatalf("bind jA: %v", err)
	}
	if err := st.BindPRNumber(ctx, "jB", 1000); err != nil {
		t.Fatalf("bind jB: %v", err)
	}

	idA, ok, err := st.JobIDForPRInRepo(ctx, "core", 1000)
	if err != nil || !ok || idA != "jA" {
		t.Fatalf("repo core PR 1000 -> %q ok=%v err=%v (want jA)", idA, ok, err)
	}
	idB, ok, err := st.JobIDForPRInRepo(ctx, "web", 1000)
	if err != nil || !ok || idB != "jB" {
		t.Fatalf("repo web PR 1000 -> %q ok=%v err=%v (want jB)", idB, ok, err)
	}
	// an unmanaged repo has no binding.
	if _, ok, _ := st.JobIDForPRInRepo(ctx, "ghost", 1000); ok {
		t.Fatalf("ghost repo should have no binding")
	}

	// the repo scope round-trips onto the job projection.
	jA, _ := st.GetJob(ctx, "jA")
	if jA.Repo != "core" {
		t.Fatalf("job repo not persisted: %q", jA.Repo)
	}
}
