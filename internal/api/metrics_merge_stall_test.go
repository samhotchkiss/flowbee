package api_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/samhotchkiss/flowbee/internal/api"
	"github.com/samhotchkiss/flowbee/internal/clock"
	"github.com/samhotchkiss/flowbee/internal/job"
	"github.com/samhotchkiss/flowbee/internal/store"
	"github.com/samhotchkiss/flowbee/internal/ulid"
)

// TestMergeStallAgeMetric: a change Flowbee approved but nobody merged must be
// OBSERVABLE by AGE, not just count. The plain flowbee_jobs{state="merge_handoff"}
// count fires on every (normal) handoff, so it can't distinguish a fresh handoff from
// one wedged 16h — which is exactly how a 15h+ merge stall sat silently. The
// flowbee_oldest_pending_merge_age_seconds gauge grows with the stall so a scraper
// pages at hour ~1, not hour 15. Mirrors flowbee_github_last_success_age_seconds.
func TestMergeStallAgeMetric(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, t.TempDir()+"/flowbee.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := store.MigrateUp(ctx, st.DB); err != nil {
		t.Fatal(err)
	}
	clk := clock.NewFake(time.Unix(1_700_000_000, 0))
	srv := api.New(st, clk, ulid.NewMinter(nil), api.Config{}, "v")

	body := func(path string) string {
		rec := httptest.NewRecorder()
		srv.HealthHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		return rec.Body.String()
	}

	// no pending-merge jobs yet: the gauge emits no series (absent == 0, correct).
	if m := body("/metrics"); strings.Contains(m, "flowbee_oldest_pending_merge_age_seconds") {
		t.Fatalf("no handoff yet -> no age series, got: %s", m)
	}

	// park a build in merge_handoff 16h ago (approved, nobody merged — the stall).
	if _, err := st.SeedJob(ctx, store.SeedParams{
		ID: "stuck-merge", Kind: job.KindBuild, Flow: "build", Stage: "build",
		Role: job.RoleEngWorker, Now: clk.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	stalledAt := clk.Now().Add(-16 * time.Hour).Format(time.RFC3339Nano)
	if _, err := st.DB.ExecContext(ctx,
		`UPDATE jobs SET state='merge_handoff', updated_at=? WHERE id='stuck-merge'`, stalledAt); err != nil {
		t.Fatal(err)
	}

	// 16h = 57600s: the AGE gauge must surface the stall (the count alone never would).
	if m := body("/metrics"); !strings.Contains(m, `flowbee_oldest_pending_merge_age_seconds{repo=""} 57600`) {
		t.Fatalf("merge-stall age gauge missing/wrong (want 57600s): %s", m)
	}
}

// TestRedMainMetric: a repo whose integration-branch CI is red surfaces on /metrics as
// flowbee_main_ci_red{repo}=1 (green-main stop-the-line, russ #214), and flips to 0 when
// main recovers.
func TestRedMainMetric(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, t.TempDir()+"/flowbee.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := store.MigrateUp(ctx, st.DB); err != nil {
		t.Fatal(err)
	}
	clk := clock.NewFake(time.Unix(1000, 0))
	srv := api.New(st, clk, ulid.NewMinter(nil), api.Config{}, "v")
	body := func() string {
		rec := httptest.NewRecorder()
		srv.HealthHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
		return rec.Body.String()
	}
	if err := st.RegisterRepo(ctx, store.Repo{ID: "russ", Owner: "o", Repo: "russ", DefaultBranch: "main", Active: true}); err != nil {
		t.Fatal(err)
	}

	if err := st.RecordMainCIRed(ctx, "russ", true); err != nil {
		t.Fatal(err)
	}
	if m := body(); !strings.Contains(m, `flowbee_main_ci_red{repo="russ"} 1`) {
		t.Fatalf("red main must surface as flowbee_main_ci_red=1: %s", m)
	}
	if err := st.RecordMainCIRed(ctx, "russ", false); err != nil {
		t.Fatal(err)
	}
	if m := body(); !strings.Contains(m, `flowbee_main_ci_red{repo="russ"} 0`) {
		t.Fatalf("recovered main must surface as flowbee_main_ci_red=0: %s", m)
	}
}
