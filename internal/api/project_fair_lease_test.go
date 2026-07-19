package api_test

import (
	"context"
	"testing"
	"time"

	"github.com/samhotchkiss/flowbee/client"
	"github.com/samhotchkiss/flowbee/internal/store"
)

func assignProject(t *testing.T, st *store.Store, projectID string, jobIDs ...string) {
	t.Helper()
	for _, id := range jobIDs {
		if _, err := st.DB.ExecContext(context.Background(), `UPDATE jobs SET project_id=? WHERE id=?`, projectID, id); err != nil {
			t.Fatalf("assign %s to %s: %v", id, projectID, err)
		}
	}
}

func TestLeaseAPIProjectFairnessServesQuietProjectWithinBound(t *testing.T) {
	ctx := context.Background()
	st, c, clk := ctrlServer(t)
	old := clk.Now().Add(-16 * time.Minute)
	for _, p := range []store.PortfolioProject{
		{ID: "a-noisy", Name: "Noisy", State: "active", SchedulerWeight: 100},
		{ID: "z-quiet", Name: "Quiet", State: "active", SchedulerWeight: 1},
	} {
		if _, err := st.CreatePortfolioProject(ctx, p, old); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 8; i++ {
		id := "noisy-" + string(rune('a'+i))
		seedReady(t, st, id, "", old)
		assignProject(t, st, "a-noisy", id)
	}
	seedReady(t, st, "quiet", "", old)
	assignProject(t, st, "z-quiet", "quiet")

	first, ok, err := c.Lease(ctx, "w", "codex", "")
	if err != nil || !ok {
		t.Fatalf("first lease: ok=%v err=%v", ok, err)
	}
	if _, err := st.Result(ctx, store.ResultParams{JobID: first.JobID, Epoch: first.LeaseEpoch, Now: clk.Now()}); err != nil {
		t.Fatal(err)
	}
	second, ok, err := c.Lease(ctx, "w", "codex", "")
	if err != nil || !ok || second.JobID != "quiet" {
		t.Fatalf("quiet project starved behind noisy queue: first=%s second=%s ok=%v err=%v", first.JobID, second.JobID, ok, err)
	}
	turn, err := st.LastProjectSchedulerTurn(ctx, "build")
	if err != nil || !turn.ForcedByAge || turn.ProjectID != "z-quiet" {
		t.Fatalf("starvation-bound selection not explained: %+v err=%v", turn, err)
	}
}

func TestLeaseAPIProjectConcurrencyCapRoutesToAnotherProject(t *testing.T) {
	ctx := context.Background()
	st, c, clk := ctrlServer(t)
	if _, err := c.Register(ctx, client.Registration{WorkerID: "w2", Identity: "w2", Host: "h2", Capabilities: []string{"role:eng_worker", "model_family:codex"}}); err != nil {
		t.Fatal(err)
	}
	for _, p := range []store.PortfolioProject{
		{ID: "cap", Name: "Capped", State: "active", SchedulerWeight: 10, ConcurrencyCap: 1},
		{ID: "other", Name: "Other", State: "active", SchedulerWeight: 1},
	} {
		if _, err := st.CreatePortfolioProject(ctx, p, clk.Now()); err != nil {
			t.Fatal(err)
		}
	}
	for _, id := range []string{"cap-a", "cap-b"} {
		seedReady(t, st, id, "", clk.Now())
		assignProject(t, st, "cap", id)
	}
	seedReady(t, st, "other-a", "", clk.Now())
	assignProject(t, st, "other", "other-a")

	first, ok, err := c.Lease(ctx, "w", "codex", "")
	if err != nil || !ok || first.JobID != "cap-a" {
		t.Fatalf("first=%+v ok=%v err=%v", first, ok, err)
	}
	second, ok, err := c.Lease(ctx, "w2", "codex", "")
	if err != nil || !ok || second.JobID != "other-a" || second.ProjectID != "other" {
		t.Fatalf("cap did not route spare worker to another project: second=%+v ok=%v err=%v", second, ok, err)
	}
}
