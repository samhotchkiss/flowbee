package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/samhotchkiss/flowbee/internal/job"
	"github.com/samhotchkiss/flowbee/internal/ledger"
	"github.com/samhotchkiss/flowbee/internal/store"
	"github.com/samhotchkiss/flowbee/internal/testutil"
)

// TestSeedJobCarriesTask proves F1's persistence: a job seeded with a
// task/spec/acceptance round-trips through GetJob AND folds identically from the
// ledger (the projection == Fold(events) invariant holds for the new fields).
func TestSeedJobCarriesTask(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()

	const (
		task   = "implement /healthz returning 200"
		spec   = "wire it into the router; cover with a test"
		accept = "- GET /healthz returns 200\n- test proves it"
	)
	seeded, err := st.SeedJob(ctx, store.SeedParams{
		ID: "job-task-1", Kind: job.KindBuild, Flow: "build", Stage: "build",
		Role: job.RoleEngWorker, BaseSHA: "base-1", Now: time.Unix(1000, 0),
		TaskText: task, SpecText: spec, AcceptanceCriteria: accept,
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	if seeded.TaskText != task || seeded.SpecText != spec || seeded.AcceptanceCriteria != accept {
		t.Fatalf("seeded job missing task fields: %+v", seeded)
	}

	got, err := st.GetJob(ctx, "job-task-1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.TaskText != task || got.SpecText != spec || got.AcceptanceCriteria != accept {
		t.Fatalf("GetJob lost task fields: task=%q spec=%q accept=%q", got.TaskText, got.SpecText, got.AcceptanceCriteria)
	}

	// the projection MUST equal Fold(events) for the new fields (replayability).
	events, err := st.LoadEvents(ctx, "job-task-1")
	if err != nil {
		t.Fatalf("load events: %v", err)
	}
	folded, err := ledger.Fold(events)
	if err != nil {
		t.Fatalf("fold: %v", err)
	}
	if folded.TaskText != task || folded.SpecText != spec || folded.AcceptanceCriteria != accept {
		t.Fatalf("Fold(events) != projection for task fields: %+v", folded)
	}
}
