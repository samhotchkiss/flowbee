package project

import (
	"context"
	"testing"

	"github.com/samhotchkiss/flowbee/internal/gitops"
	"github.com/samhotchkiss/flowbee/internal/job"
	"github.com/samhotchkiss/flowbee/internal/store"
)

// fakeHistory records FetchBranch calls and serves a fixed main tip, so a test can
// assert the merge-conflict router fetches the post-merge main BEFORE resolving the
// resolver's base.
type fakeHistory struct {
	fetched []string
	tip     string
}

func (f *fakeHistory) CommitHistory(branch, message string, files []gitops.HistoryFile) (string, bool, error) {
	return "", true, nil
}
func (f *fakeHistory) HeadSHA(ref string) (string, error) { return f.tip, nil }
func (f *fakeHistory) FetchBranch(branch string) error {
	f.fetched = append(f.fetched, branch)
	return nil
}

// TestMergeConflictRoutesToResolverAfterFetch: when a merge returns ErrMergeConflict,
// the sender fetches the current main (so the resolver's base actually has the sibling
// merge), routes the job to resolving_conflict at that tip, and CONSUMES the merge row
// — it does not return an error (which would leave the row pending to retry forever).
func TestMergeConflictRoutesToResolverAfterFetch(t *testing.T) {
	st, fake, sender, clk := newSender(t)
	ctx := context.Background()
	hist := &fakeHistory{tip: "postmerge-main-sha"}
	sender.WithHistory(hist, "main")

	if _, err := st.SeedJob(ctx, store.SeedParams{
		ID: "j", Kind: job.KindBuild, Flow: "build", Stage: "build", Role: job.RoleEngWorker, Now: clk.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB.ExecContext(ctx, `UPDATE jobs SET state='merging' WHERE id='j'`); err != nil {
		t.Fatal(err)
	}
	fake.SetMergeConflict(78)
	if err := st.EnqueueOutbox(ctx, store.OutboxRow{
		JobID: "j", Action: store.ActionEnqueueMerge, Payload: `{"pr_number":78}`,
	}); err != nil {
		t.Fatal(err)
	}

	// a "not mergeable" 405 is first RETRIED a few drains (it may be GitHub recomputing
	// mergeability after a sibling merge, not a real conflict). A PERSISTENT conflict —
	// the fake 405s every attempt — routes to the resolver once the retries are spent.
	for i := 0; i < mergeMergeabilityRetries; i++ {
		_, _ = sender.DrainOnce(ctx) // early attempts return the conflict err (transient retry)
	}

	// fetched main (so the resolver base is the real post-merge main, not the stale mirror)
	if len(hist.fetched) == 0 || hist.fetched[0] != "main" {
		t.Fatalf("FetchBranch(main) not called before routing: %v", hist.fetched)
	}
	// routed to the resolver at the fetched tip
	j, _ := st.GetJob(ctx, "j")
	if j.State != job.StateResolvingConflict {
		t.Fatalf("state=%s, want resolving_conflict", j.State)
	}
	if j.BaseSHA != "postmerge-main-sha" {
		t.Fatalf("base=%s, want the fetched post-merge main (resolve against the sibling's merge)", j.BaseSHA)
	}
	// the merge row was consumed, not left pending to loop forever.
	row, ok, err := st.NextPendingOutbox(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatalf("a pending outbox row remains (%s) — the merge should be consumed", row.Action)
	}
}
