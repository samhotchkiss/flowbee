package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/samhotchkiss/flowbee/internal/job"
	"github.com/samhotchkiss/flowbee/internal/store"
	"github.com/samhotchkiss/flowbee/internal/testutil"
)

// TestStaleReviewBuilds: only review_pending builds that are BEHIND the integration
// tip AND carry a stored patch are reported for rebase-before-review. A build already
// on the tip, a build with no patch, and a non-review job are all skipped.
func TestStaleReviewBuilds(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	now := time.Unix(1000, 0)

	seed := func(id, base, patch, state string) {
		if _, err := st.SeedJob(ctx, store.SeedParams{
			ID: id, Kind: job.KindBuild, Flow: "build", Stage: "build",
			Role: job.RoleEngWorker, BaseSHA: base, Now: now,
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := st.DB.ExecContext(ctx,
			`UPDATE jobs SET state=?, patch_diff=? WHERE id=?`, state, patch, id); err != nil {
			t.Fatal(err)
		}
	}

	mainTip := "tip999"
	seed("stale", "old111", "diff --git a/f b/f\n+x", "review_pending")  // behind + has patch -> stale
	seed("current", mainTip, "diff --git a/f b/f\n+y", "review_pending") // already on tip -> skip
	seed("nopatch", "old111", "", "review_pending")                     // behind but no patch -> skip
	seed("building", "old111", "diff --git a/f b/f\n+z", "building")     // not review_pending -> skip

	ids, err := st.StaleReviewBuilds(ctx, "", mainTip)
	if err != nil {
		t.Fatalf("StaleReviewBuilds: %v", err)
	}
	if len(ids) != 1 || ids[0] != "stale" {
		t.Fatalf("stale review builds = %v, want exactly [stale]", ids)
	}

	// an empty tip (no integration ref yet) reports nothing — never rebase blind.
	if ids, err := st.StaleReviewBuilds(ctx, "", ""); err != nil || len(ids) != 0 {
		t.Fatalf("empty tip = %v,%v want none", ids, err)
	}

	// F9 repo scoping: a stale job in repo "other" is NOT returned when scoping to a
	// different repo (never compare/rebase across repos).
	if _, err := st.DB.ExecContext(ctx,
		`UPDATE jobs SET repo='other' WHERE id='stale'`); err != nil {
		t.Fatal(err)
	}
	if ids, _ := st.StaleReviewBuilds(ctx, "myrepo", mainTip); len(ids) != 0 {
		t.Fatalf("scoping to myrepo must exclude repo 'other': %v", ids)
	}
	if ids, _ := st.StaleReviewBuilds(ctx, "other", mainTip); len(ids) != 1 || ids[0] != "stale" {
		t.Fatalf("scoping to 'other' must include it: %v", ids)
	}
}
