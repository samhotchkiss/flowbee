package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/samhotchkiss/flowbee/internal/job"
	"github.com/samhotchkiss/flowbee/internal/store"
	"github.com/samhotchkiss/flowbee/internal/testutil"
)

type staleGreenFacts struct {
	facts job.DomainBFacts
}

func (s staleGreenFacts) Facts(context.Context, string) (job.DomainBFacts, bool, error) {
	return s.facts, true, nil
}

func seedMergeableWithVerdict(t *testing.T, st *store.Store, id string, pr int, disposition job.Disposition) {
	t.Helper()
	ctx := context.Background()
	seedBuildPR(t, st, id, pr)
	if err := st.SetReconciledFacts(ctx, id, store.ReconciledPR{
		Number: pr, HeadSHA: "h", BaseSHA: "b", CIGreen: true,
	}); err != nil {
		t.Fatal(err)
	}
	v := job.MintVerdict(job.VerdictApproved, disposition, "h", "b")
	if _, err := st.DB.ExecContext(ctx, `
		UPDATE jobs SET state='mergeable', head_sha='h', base_sha='b', verdict=?
		 WHERE id=?`, mustJSON(t, v), id); err != nil {
		t.Fatal(err)
	}
}

func TestDispatchMergeDoesNotLeaveMergeableWhileRequiredCheckPending(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	seedMergeableWithVerdict(t, st, "pending-merge", 88, job.DispositionSelfMerge)

	if err := st.SetReconciledFacts(ctx, "pending-merge", store.ReconciledPR{
		Number: 88, HeadSHA: "h", BaseSHA: "b", CIGreen: false,
	}); err != nil {
		t.Fatal(err)
	}

	staleSrc := staleGreenFacts{facts: job.DomainBFacts{
		PRExists: true, PRNumber: 88, HeadSHA: "h", BaseSHA: "b", CIGreen: true,
	}}
	final, err := st.DispatchMerge(ctx, staleSrc, job.Policy{AllowSelfMerge: true},
		store.DispatchMergeParams{JobID: "pending-merge", Now: time.Unix(9000, 0)})
	if err != nil {
		t.Fatal(err)
	}
	if final != job.StateMergeable {
		t.Fatalf("pending required check moved to %s, want mergeable", final)
	}
	if j, _ := st.GetJob(ctx, "pending-merge"); j.State != job.StateMergeable {
		t.Fatalf("persisted state=%s, want mergeable", j.State)
	}
	enqueued, err := st.EnqueueMergeForJob(ctx, "pending-merge", time.Unix(9001, 0))
	if err != nil {
		t.Fatal(err)
	}
	if enqueued {
		t.Fatal("pending required check must not enqueue a merge")
	}

	if err := st.SetReconciledFacts(ctx, "pending-merge", store.ReconciledPR{
		Number: 88, HeadSHA: "h", BaseSHA: "b", CIGreen: true,
	}); err != nil {
		t.Fatal(err)
	}
	final, err = st.DispatchMerge(ctx, staleSrc, job.Policy{AllowSelfMerge: true},
		store.DispatchMergeParams{JobID: "pending-merge", Now: time.Unix(9010, 0)})
	if err != nil {
		t.Fatal(err)
	}
	if final != job.StateMerging {
		t.Fatalf("green required checks moved to %s, want merging", final)
	}
}

func TestDispatchMergeDoesNotHandoffWhileRequiredCheckPending(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	seedMergeableWithVerdict(t, st, "pending-handoff", 89, job.DispositionHandoff)

	if err := st.SetReconciledFacts(ctx, "pending-handoff", store.ReconciledPR{
		Number: 89, HeadSHA: "h", BaseSHA: "b", CIGreen: false,
	}); err != nil {
		t.Fatal(err)
	}
	final, err := st.DispatchMerge(ctx, staleGreenFacts{}, job.Policy{},
		store.DispatchMergeParams{JobID: "pending-handoff", Now: time.Unix(9100, 0)})
	if err != nil {
		t.Fatal(err)
	}
	if final != job.StateMergeable {
		t.Fatalf("pending required check moved to %s, want mergeable", final)
	}
}

func TestDispatchMergeBlocksUnknownMergeableState(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	seedMergeableWithVerdict(t, st, "unknown-mergeable", 90, job.DispositionSelfMerge)

	if err := st.SetReconciledFacts(ctx, "unknown-mergeable", store.ReconciledPR{
		Number: 90, HeadSHA: "h", BaseSHA: "b", CIGreen: true, MergeableState: "UNKNOWN",
	}); err != nil {
		t.Fatal(err)
	}

	final, err := st.DispatchMerge(ctx, staleGreenFacts{}, job.Policy{AllowSelfMerge: true},
		store.DispatchMergeParams{JobID: "unknown-mergeable", Now: time.Unix(9200, 0)})
	if err != nil {
		t.Fatal(err)
	}
	if final != job.StateMergeable {
		t.Fatalf("unknown GitHub mergeable state moved to %s, want mergeable", final)
	}
	if enqueued, err := st.EnqueueMergeForJob(ctx, "unknown-mergeable", time.Unix(9201, 0)); err != nil {
		t.Fatal(err)
	} else if enqueued {
		t.Fatal("unknown GitHub mergeable state must not enqueue a merge")
	}

	if err := st.SetReconciledFacts(ctx, "unknown-mergeable", store.ReconciledPR{
		Number: 90, HeadSHA: "h", BaseSHA: "b", CIGreen: true, MergeableState: "CLEAN",
	}); err != nil {
		t.Fatal(err)
	}
	final, err = st.DispatchMerge(ctx, staleGreenFacts{}, job.Policy{AllowSelfMerge: true},
		store.DispatchMergeParams{JobID: "unknown-mergeable", Now: time.Unix(9210, 0)})
	if err != nil {
		t.Fatal(err)
	}
	if final != job.StateMerging {
		t.Fatalf("known-clean mergeable state moved to %s, want merging", final)
	}
}
