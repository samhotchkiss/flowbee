package acceptance

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/samhotchkiss/flowbee/client"
	"github.com/samhotchkiss/flowbee/internal/job"
	"github.com/samhotchkiss/flowbee/internal/store"
)

// rebuildToReviewPending re-leases a bounced (`ready`) job as the eng_worker and
// posts a fresh build result, returning it to review_pending for re-review.
func rebuildToReviewPending(t *testing.T, ctx context.Context, st *store.Store, url, jobID string, attempt int) {
	t.Helper()
	if j, _ := st.GetJob(ctx, jobID); j.State != job.StateReady {
		t.Fatalf("rebuild #%d: job=%s want ready", attempt, j.State)
	}
	builder := registerWorker(t, ctx, url, "builder-alice", "codex")
	g, ok, err := builder.Lease(ctx, "builder-alice", "codex", "")
	if err != nil || !ok || g.JobID != jobID {
		t.Fatalf("rebuild #%d lease ok=%v err=%v job=%s", attempt, ok, err, g.JobID)
	}
	if _, _, err := builder.Result(ctx, jobID, g.LeaseEpoch, "rebuild-"+itoa(attempt), map[string]any{"kind": "patch", "base_sha": "base1"}); err != nil {
		t.Fatalf("rebuild #%d result: %v", attempt, err)
	}
	if j, _ := st.GetJob(ctx, jobID); j.State != job.StateReviewPending {
		t.Fatalf("rebuild #%d -> state=%s want review_pending", attempt, j.State)
	}
	// Every accepted build result invalidates the prior head's green CI. Model the
	// reconcile/CI pass for this distinct rebuild before asking for re-review; otherwise
	// the helper tries to reuse the pre-bounce head's authorization and is correctly
	// withheld by ReviewPendingCandidates. A unique head per attempt makes this guard
	// deterministic under both normal and -race test scheduling.
	if err := st.UpsertDomainBFacts(ctx, jobID, job.DomainBFacts{
		PRExists: true, PRNumber: 42, HeadSHA: "head-rebuild-" + itoa(attempt),
		BaseSHA: "base1", CIGreen: true,
	}); err != nil {
		t.Fatalf("rebuild #%d reconcile green facts: %v", attempt, err)
	}
}

type reviewLease struct {
	cl    *client.Client
	epoch int
}

// rereview re-leases a review_pending job as the code_reviewer and returns the
// client + epoch so the caller can post the next verdict claim.
func rereview(t *testing.T, ctx context.Context, st *store.Store, url, jobID string) reviewLease {
	t.Helper()
	reviewer := client.New(url)
	// reviewer-bob is already enrolled (from driveToCodeReview); re-register under a
	// stable worker_id so the ON CONFLICT(worker_id) upsert path refreshes it
	// rather than colliding on the UNIQUE(identity) constraint.
	if _, err := reviewer.Register(ctx, client.Registration{
		WorkerID: "wk-reviewer-bob", Identity: "reviewer-bob", Host: "t",
		Capabilities: []string{"role:code_reviewer", "model_family:opus"},
	}); err != nil {
		t.Fatalf("rereview register: %v", err)
	}
	rg, ok, err := reviewer.Lease(ctx, "reviewer-bob", "opus", string(job.RoleCodeReviewer))
	if err != nil || !ok {
		t.Fatalf("rereview lease ok=%v err=%v", ok, err)
	}
	if rg.JobID != jobID {
		t.Fatalf("rereview leased %s want %s", rg.JobID, jobID)
	}
	return reviewLease{cl: reviewer, epoch: rg.LeaseEpoch}
}

// readDefaultFlows locates the shipped flows/flows.yaml from the test's working
// directory (test/acceptance) up to the repo root.
func readDefaultFlows() ([]byte, error) {
	candidates := []string{
		filepath.Join("..", "..", "flows", "flows.yaml"),
		filepath.Join("flows", "flows.yaml"),
	}
	var lastErr error
	for _, c := range candidates {
		b, err := os.ReadFile(c)
		if err == nil {
			return b, nil
		}
		lastErr = err
	}
	return nil, lastErr
}

func itoa(n int) string { return strconv.Itoa(n) }
