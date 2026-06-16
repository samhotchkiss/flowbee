package worker_test

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/samhotchkiss/flowbee/internal/api"
	"github.com/samhotchkiss/flowbee/internal/clock"
	"github.com/samhotchkiss/flowbee/internal/job"
	"github.com/samhotchkiss/flowbee/internal/store"
	"github.com/samhotchkiss/flowbee/internal/testutil"
	"github.com/samhotchkiss/flowbee/internal/ulid"
	"github.com/samhotchkiss/flowbee/internal/worker"
)

// TestStubRunOnce drives the §7.1 thin loop end-to-end over real HTTP: register,
// lease, heartbeat, echo result -> review_pending. Proves Mode-A is wired.
func TestStubRunOnce(t *testing.T) {
	st := testutil.NewStore(t)
	srv := api.New(st, clock.Real{}, ulid.NewMinter(nil), api.Config{
		LeaseTTL: time.Minute, LongPollWait: time.Second, LeaseTTLS: 300, HeartbeatIntervalS: 30,
	}, "test")
	ts := httptest.NewServer(srv.PrivateHandler())
	defer ts.Close()
	ctx := context.Background()

	jobID := ulid.New()
	if _, err := st.SeedJob(ctx, store.SeedParams{ID: jobID, Kind: job.KindBuild, Flow: "build", Stage: "build", Role: job.RoleEngWorker, BaseSHA: "abc", Now: time.Unix(1, 0)}); err != nil {
		t.Fatal(err)
	}

	out, err := worker.RunOnce(ctx, worker.StubConfig{BaseURL: ts.URL, Identity: "stub-1", ModelFamily: "codex"})
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if !out.Got || out.JobID != jobID {
		t.Fatalf("stub did not lease the seeded job: %+v", out)
	}
	if out.JobState != string(job.StateReviewPending) {
		t.Fatalf("final state=%s want review_pending", out.JobState)
	}

	// a second run finds no work and returns 204.
	out2, err := worker.RunOnce(ctx, worker.StubConfig{BaseURL: ts.URL, Identity: "stub-2", ModelFamily: "opus"})
	if err != nil {
		t.Fatalf("RunOnce2: %v", err)
	}
	if out2.Got {
		t.Fatalf("second stub should get 204, got %+v", out2)
	}
}
