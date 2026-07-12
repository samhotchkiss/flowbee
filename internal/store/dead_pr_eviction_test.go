package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/samhotchkiss/flowbee/internal/job"
	"github.com/samhotchkiss/flowbee/internal/store"
	"github.com/samhotchkiss/flowbee/internal/testutil"
)

// parkNeedsHuman forces a PR-bound job into the needs_human sink with a given
// escalation reason, mirroring the raw-UPDATE seed style of markMergeable.
func parkNeedsHuman(t *testing.T, st *store.Store, id, reason string) {
	t.Helper()
	if _, err := st.DB.ExecContext(context.Background(),
		`UPDATE jobs SET state='needs_human', escalation_reason=? WHERE id=?`, reason, id); err != nil {
		t.Fatalf("park needs_human %s: %v", id, err)
	}
}

// TestNeedsHumanEvictedWhenPRMerges: a job parked in the needs_human sink whose PR is
// later MERGED (externally / by a human) self-drains to done — the human gate is moot
// once the PR reaches a terminal GitHub state. Prevents merged-PR zombies.
func TestNeedsHumanEvictedWhenPRMerges(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	seedBuildPR(t, st, "jmerge", 41)
	parkNeedsHuman(t, st, "jmerge", string(job.EscalationBounces))

	now := time.Unix(8000, 0)
	out, err := st.ApplyReconciledPR(ctx, "jmerge", store.ReconciledPR{
		Number: 41, UpdatedAt: now, HeadSHA: "h", BaseSHA: "b",
		Merged: true, MergeCommit: "merge-sha",
	}, now)
	if err != nil {
		t.Fatalf("apply merged: %v", err)
	}
	if !out.Done {
		t.Fatalf("a parked job whose PR merged must self-drain to done: %+v", out)
	}
	if j, _ := st.GetJob(ctx, "jmerge"); j.State != job.StateDone {
		t.Fatalf("state=%s, want done after the parked PR merged", j.State)
	}
}

// TestNeedsHumanEvictedWhenPRClosed: a job parked in the needs_human sink whose PR is
// CLOSED without merging is terminal-dead and self-drains to cancelled — the missing
// terminal exit the janitor deliberately won't supply (it refuses to requeue pr_closed).
func TestNeedsHumanEvictedWhenPRClosed(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	seedBuildPR(t, st, "jclosed", 42)
	parkNeedsHuman(t, st, "jclosed", string(job.EscalationPRClosed))

	now := time.Unix(8100, 0)
	out, err := st.ApplyReconciledPR(ctx, "jclosed", store.ReconciledPR{
		Number: 42, UpdatedAt: now, HeadSHA: "h", BaseSHA: "b",
		ClosedUnmerged: true,
	}, now)
	if err != nil {
		t.Fatalf("apply closed: %v", err)
	}
	if !out.Applied {
		t.Fatalf("a parked job whose PR closed-unmerged must be evicted: %+v", out)
	}
	if j, _ := st.GetJob(ctx, "jclosed"); j.State != job.StateCancelled {
		t.Fatalf("state=%s, want cancelled after the parked PR closed unmerged", j.State)
	}
}

// TestNeedsHumanNotEvictedWhilePROpen is the safety guard: an OPEN, still-undecided PR
// must NOT drain a parked job — the §12.6.1 human gate stays until GitHub reaches a
// terminal state. Eviction fires only on merged/closed, never on a live PR.
func TestNeedsHumanNotEvictedWhilePROpen(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	seedBuildPR(t, st, "jopen", 43)
	parkNeedsHuman(t, st, "jopen", string(job.EscalationBounces))

	now := time.Unix(8200, 0)
	out, err := st.ApplyReconciledPR(ctx, "jopen", store.ReconciledPR{
		Number: 43, UpdatedAt: now, HeadSHA: "h", BaseSHA: "b",
		// neither Merged nor ClosedUnmerged: the PR is open and undecided.
	}, now)
	if err != nil {
		t.Fatalf("apply open: %v", err)
	}
	if out.Done {
		t.Fatalf("an open PR must not complete a parked job: %+v", out)
	}
	if j, _ := st.GetJob(ctx, "jopen"); j.State != job.StateNeedsHuman {
		t.Fatalf("state=%s, want the job to stay parked while its PR is open", j.State)
	}
}

// TestNeedsHumanMergedWithoutCommitWaits guards the I-3 window: GitHub flips merged
// before mergeCommit.oid resolves, so a merged PR without a resolved commit must NOT
// evict yet (it would leave the terminal-SHA freeze unarmed) — it waits one sweep.
func TestNeedsHumanMergedWithoutCommitWaits(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	seedBuildPR(t, st, "jrace", 44)
	parkNeedsHuman(t, st, "jrace", string(job.EscalationBounces))

	now := time.Unix(8300, 0)
	out, err := st.ApplyReconciledPR(ctx, "jrace", store.ReconciledPR{
		Number: 44, UpdatedAt: now, HeadSHA: "h", BaseSHA: "b",
		Merged: true, MergeCommit: "", // resolved commit not yet available
	}, now)
	if err != nil {
		t.Fatalf("apply merged-no-commit: %v", err)
	}
	if out.Done {
		t.Fatalf("merged PR without a resolved commit must wait, not evict: %+v", out)
	}
	if j, _ := st.GetJob(ctx, "jrace"); j.State != job.StateNeedsHuman {
		t.Fatalf("state=%s, want the job to stay parked until the merge commit resolves", j.State)
	}
}
