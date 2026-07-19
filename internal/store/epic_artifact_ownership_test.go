package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	gh "github.com/samhotchkiss/flowbee/internal/github"
	"github.com/samhotchkiss/flowbee/internal/store"
	"github.com/samhotchkiss/flowbee/internal/testutil"
)

func TestEpicDeliveryForRepoBranchIsExactAndFailsOnAmbiguity(t *testing.T) {
	ctx := context.Background()
	st := testutil.NewStore(t)
	now := time.Date(2026, 7, 19, 10, 0, 0, 0, time.UTC)
	if err := st.AddEpicRun(ctx, store.EpicRun{ID: "one", Repo: "russ", Branch: "epic/one"}, 1, now); err != nil {
		t.Fatal(err)
	}
	owner, ok, err := st.EpicDeliveryForRepoBranch(ctx, "russ", "epic/one")
	if err != nil || !ok || owner.EpicID != "one" {
		t.Fatalf("owner=%+v ok=%v err=%v", owner, ok, err)
	}
	for _, near := range []struct{ repo, branch string }{{"other", "epic/one"}, {"russ", "epic/one-more"}} {
		if _, ok, err := st.EpicDeliveryForRepoBranch(ctx, near.repo, near.branch); err != nil || ok {
			t.Fatalf("near match repo=%q branch=%q ok=%v err=%v", near.repo, near.branch, ok, err)
		}
	}
	if err := st.AddEpicRun(ctx, store.EpicRun{ID: "two", Repo: "russ", Branch: "epic/one"}, 1, now); err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.EpicDeliveryForRepoBranch(ctx, "russ", "epic/one"); !errors.Is(err, store.ErrEpicArtifactOwnershipAmbiguous) {
		t.Fatalf("ambiguous owner err=%v", err)
	}
}

func TestEpicArtifactBindingCannotJumpToAnotherPR(t *testing.T) {
	ctx := context.Background()
	st := testutil.NewStore(t)
	now := time.Date(2026, 7, 19, 10, 0, 0, 0, time.UTC)
	if err := st.AddEpicRun(ctx, store.EpicRun{ID: "bound", Repo: "russ", Branch: "epic/bound"}, 1, now); err != nil {
		t.Fatal(err)
	}
	fact := store.EpicArtifactFact{
		EpicID: "bound", Repo: "russ", Branch: "epic/bound", PRNumber: 10,
		PROpen: true, HeadSHA: "head", BaseSHA: "base", CIState: "pending",
	}
	if err := st.ObserveEpicArtifactFact(ctx, fact, now); err != nil {
		t.Fatal(err)
	}
	fact.PRNumber = 11
	if err := st.ObserveEpicArtifactFact(ctx, fact, now.Add(time.Minute)); !errors.Is(err, store.ErrEpicArtifactPRConflict) {
		t.Fatalf("replacement PR err=%v", err)
	}
	var bound int
	if err := st.DB.QueryRowContext(ctx, `SELECT pr_number FROM epic_artifacts WHERE epic_id='bound'`).Scan(&bound); err != nil || bound != 10 {
		t.Fatalf("bound PR=%d err=%v", bound, err)
	}
}

func TestLegacyAdoptSweepFencesOwnedBranchOnlyUnderV2Flag(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 19, 10, 0, 0, 0, time.UTC)
	st := testutil.NewStore(t)
	if err := st.AddEpicRun(ctx, store.EpicRun{ID: "owned", Repo: "russ", Branch: "epic/owned"}, 1, now); err != nil {
		t.Fatal(err)
	}
	st.EnableEpicReviewHandoffV2 = true
	snap := gh.BoardSnapshot{PullRequests: []gh.PullRequest{
		{Number: 20, HeadRefName: "epic/owned", HeadRefOid: "owned-head", BaseRefOid: "base", UpdatedAt: now, Labels: []string{"flowbee:adopt"}},
		{Number: 21, HeadRefName: "epic/owned", IsCrossRepository: true, HeadRefOid: "fork-head", BaseRefOid: "base", UpdatedAt: now, Labels: []string{"flowbee:adopt"}},
	}}
	ids, err := st.AdoptSweepForRepo(ctx, "russ", snap, time.Unix(1, 0), now)
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 1 {
		t.Fatalf("adopted=%v want fork only", ids)
	}
	var owned, fork int
	if err := st.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM jobs WHERE pr_number=20`).Scan(&owned); err != nil {
		t.Fatal(err)
	}
	if err := st.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM jobs WHERE pr_number=21`).Scan(&fork); err != nil {
		t.Fatal(err)
	}
	if owned != 0 || fork != 1 {
		t.Fatalf("owned jobs=%d fork jobs=%d", owned, fork)
	}
}

func TestTargetedAdoptRechecksEpicOwnershipInsideInsertTransaction(t *testing.T) {
	ctx := context.Background()
	st := testutil.NewStore(t)
	now := time.Date(2026, 7, 19, 11, 0, 0, 0, time.UTC)
	if err := st.AddEpicRun(ctx, store.EpicRun{ID: "txn-owner", Repo: "russ", Branch: "epic/txn-owner"}, 1, now); err != nil {
		t.Fatal(err)
	}
	st.EnableEpicReviewHandoffV2 = true
	id, rearmed, err := st.AdoptPRForReviewWithHeadRef(ctx, "russ", 30, "epic/txn-owner",
		"base", "head", "diff --git a/x b/x\n", false, false, true, false, now, now)
	if err != nil || id != "" || rearmed {
		t.Fatalf("owned targeted adopt id=%q rearmed=%v err=%v", id, rearmed, err)
	}
	var jobs int
	if err := st.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM jobs WHERE repo='russ' AND pr_number=30`).Scan(&jobs); err != nil {
		t.Fatal(err)
	}
	if jobs != 0 {
		t.Fatalf("transaction-level owner fence inserted %d jobs", jobs)
	}
	if _, _, err := st.AdoptPRForReviewWithHeadRef(ctx, "russ", 32, "",
		"base", "head", "diff --git a/z b/z\n", false, false, true, false, now, now); err == nil {
		t.Fatal("same-repository adoption with unknown head branch must fail closed")
	}

	// Exact identity only: a near branch remains ordinary adoptable work.
	id, _, err = st.AdoptPRForReviewWithHeadRef(ctx, "russ", 31, "epic/txn-owner-fork",
		"base", "head", "diff --git a/y b/y\n", false, false, true, false, now, now)
	if err != nil || id == "" {
		t.Fatalf("near branch adopt id=%q err=%v", id, err)
	}
}

func TestLegacyAdoptSweepForRepoScopesIDsLookupAndPersistedRepo(t *testing.T) {
	ctx := context.Background()
	st := testutil.NewStore(t)
	now := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	snap := gh.BoardSnapshot{PullRequests: []gh.PullRequest{{
		Number: 40, HeadRefName: "dev/shared", HeadRefOid: "head", BaseRefOid: "base",
		UpdatedAt: now, Labels: []string{"flowbee:adopt"},
	}}}
	core, err := st.AdoptSweepForRepo(ctx, "core", snap, time.Unix(1, 0), now)
	if err != nil {
		t.Fatal(err)
	}
	web, err := st.AdoptSweepForRepo(ctx, "web", snap, time.Unix(1, 0), now)
	if err != nil {
		t.Fatal(err)
	}
	if len(core) != 1 || len(web) != 1 || core[0] == web[0] {
		t.Fatalf("repo-scoped ids core=%v web=%v", core, web)
	}
	var coreRepo, webRepo string
	if err := st.DB.QueryRowContext(ctx, `SELECT repo FROM jobs WHERE id=?`, core[0]).Scan(&coreRepo); err != nil {
		t.Fatal(err)
	}
	if err := st.DB.QueryRowContext(ctx, `SELECT repo FROM jobs WHERE id=?`, web[0]).Scan(&webRepo); err != nil {
		t.Fatal(err)
	}
	if coreRepo != "core" || webRepo != "web" {
		t.Fatalf("persisted repos core=%q web=%q", coreRepo, webRepo)
	}
}
