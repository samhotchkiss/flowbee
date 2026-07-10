package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/samhotchkiss/flowbee/internal/job"
	"github.com/samhotchkiss/flowbee/internal/store"
	"github.com/samhotchkiss/flowbee/internal/testutil"
)

// TestMergedWithoutCommitDefersDone: GitHub's GraphQL flips pullRequest.merged before
// mergeCommit.oid resolves, so a refetch can return Merged=true/MergeCommit="". The
// done transition must WAIT for the resolved commit, not fire on Merged alone —
// otherwise it (a) refreshes sibling bases to "" and (b) leaves the I-3 terminal-SHA
// freeze (keyed on merge_commit) unarmed, letting stale/replayed snapshots re-write a
// settled job. This pins both halves: defer on the empty window, settle + freeze once
// the commit resolves.
func TestMergedWithoutCommitDefersDone(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	seedBuildPR(t, st, "jw", 11)
	markMergeable(t, st, "jw")

	t1 := time.Unix(6000, 0)
	// the eventual-consistency window: merged=true, but the commit hasn't resolved.
	out, err := st.ApplyReconciledPR(ctx, "jw", store.ReconciledPR{
		Number: 11, UpdatedAt: t1, HeadSHA: "h", BaseSHA: "b", Merged: true, MergeCommit: "",
	}, t1)
	if err != nil {
		t.Fatalf("apply merged-no-commit: %v", err)
	}
	if out.Done {
		t.Fatalf("must NOT transition to done on Merged=true/MergeCommit=\"\": %+v", out)
	}
	if j, _ := st.GetJob(ctx, "jw"); j.State == job.StateDone {
		t.Fatalf("job settled to done without a resolved merge commit")
	}

	// next sweep: the commit resolved. NOW the job settles and the freeze arms.
	out2, err := st.ApplyReconciledPR(ctx, "jw", store.ReconciledPR{
		Number: 11, UpdatedAt: t1.Add(time.Minute), HeadSHA: "h", BaseSHA: "b", Merged: true, MergeCommit: "REALSHA", CIGreen: true,
	}, t1.Add(time.Minute))
	if err != nil {
		t.Fatalf("apply merged+commit: %v", err)
	}
	if !out2.Done {
		t.Fatalf("resolved merge commit must transition to done: %+v", out2)
	}

	// a later stale/replayed snapshot must now be frozen (the harm the empty window let through).
	out3, err := st.ApplyReconciledPR(ctx, "jw", store.ReconciledPR{
		Number: 11, UpdatedAt: t1.Add(time.Hour), HeadSHA: "h2", BaseSHA: "b", Merged: false, CIGreen: false,
	}, t1.Add(time.Hour))
	if err != nil {
		t.Fatalf("apply post-terminal: %v", err)
	}
	if !out3.Frozen || out3.Applied {
		t.Fatalf("terminal freeze must arm once the job is done with a commit: %+v", out3)
	}
}
