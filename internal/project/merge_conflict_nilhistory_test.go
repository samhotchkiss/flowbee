package project

import (
	"context"
	"testing"

	"github.com/samhotchkiss/flowbee/internal/job"
	"github.com/samhotchkiss/flowbee/internal/store"
)

// TestMergeConflictNilHistoryNoPanic: a persistent merge conflict on a Sender that has
// NO history writer wired (the legacy New() path, or a repo whose history factory returned
// nil in multirepo.go) must not PANIC. The conflict route at project.go:249 dereferences
// s.history.FetchBranch unconditionally; with s.history == nil that is a nil-interface
// deref that crashes the WHOLE drain goroutine — wedging every other GitHub write for the
// repo (the §8.2.4 single serialized sender). It should fall through to the transient/
// dead-letter path instead.
func TestMergeConflictNilHistoryNoPanic(t *testing.T) {
	st, fake, sender, clk := newSender(t) // New() => s.history is nil
	ctx := context.Background()

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

	// drain past the mergeability retries; the post-retry attempt hits the conflict route
	// which dereferences s.history.FetchBranch -> panic with nil history.
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("drain PANICKED on nil history during merge-conflict route: %v", r)
		}
	}()
	for i := 0; i < mergeMergeabilityRetries+1; i++ {
		_, _ = sender.DrainOnce(ctx)
	}
}
