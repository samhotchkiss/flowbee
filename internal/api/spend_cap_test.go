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

func spendCapStore(t *testing.T) *store.Store {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(ctx, t.TempDir()+"/flowbee.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	if err := store.MigrateUp(ctx, st.DB); err != nil {
		t.Fatal(err)
	}
	return st
}

// TestLeaseGatedByAggregateSpendCap: once cumulative spend reaches the cap, an
// eng_worker (new build) lease returns 204 — the fleet pauses at intake.
func TestLeaseGatedByAggregateSpendCap(t *testing.T) {
	ctx := context.Background()
	st := spendCapStore(t)
	// a ready build job carrying $150 of metered spend (cap is $100).
	if _, err := st.SeedJob(ctx, store.SeedParams{
		ID: "j", Kind: job.KindBuild, Flow: "build", Stage: "build", Role: job.RoleEngWorker,
		RequiredCapabilities: []string{"role:eng_worker"}, Now: time.Unix(1000, 0),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB.ExecContext(ctx, `UPDATE jobs SET cost_micro_usd=150 WHERE id='j'`); err != nil {
		t.Fatal(err)
	}
	srv := api.New(st, clock.Real{}, ulid.NewMinter(nil), api.Config{
		SpendCapMicroUSD: 100, LongPollWait: 10 * time.Millisecond,
	}, "test")

	req := httptest.NewRequest(http.MethodGet, "/v1/lease?role=eng_worker&identity=w", nil)
	rec := httptest.NewRecorder()
	srv.PrivateHandler().ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("capped eng_worker lease = %d, want 204 (new work paused over the spend cap)", rec.Code)
	}
}

// TestSpendCapDisabledByDefault: with no cap (0), the breaker never trips, even with
// spend recorded — it is opt-in.
func TestSpendCapDisabledByDefault(t *testing.T) {
	ctx := context.Background()
	st := spendCapStore(t)
	if _, err := st.SeedJob(ctx, store.SeedParams{
		ID: "j", Kind: job.KindBuild, Flow: "build", Stage: "build", Role: job.RoleEngWorker, Now: time.Unix(1000, 0),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB.ExecContext(ctx, `UPDATE jobs SET cost_micro_usd=999999 WHERE id='j'`); err != nil {
		t.Fatal(err)
	}
	// cap unset (0) -> the /metrics breaker reads not-tripped.
	srv := api.New(st, clock.Real{}, ulid.NewMinter(nil), api.Config{}, "test")
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	rec := httptest.NewRecorder()
	srv.HealthHandler().ServeHTTP(rec, req)
	if !strings.Contains(rec.Body.String(), "flowbee_spend_capped 0") {
		t.Fatalf("with cap disabled, spend_capped must be 0:\n%s", rec.Body.String())
	}
}

// TestTotalSpendMicroUSDSums: the cumulative-spend query the breaker reads sums all jobs.
func TestTotalSpendMicroUSDSums(t *testing.T) {
	ctx := context.Background()
	st := spendCapStore(t)
	for i, c := range []int64{40, 60, 0} {
		id := string(rune('a' + i))
		if _, err := st.SeedJob(ctx, store.SeedParams{
			ID: id, Kind: job.KindBuild, Flow: "build", Stage: "build", Role: job.RoleEngWorker, Now: time.Unix(1000, 0),
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := st.DB.ExecContext(ctx, `UPDATE jobs SET cost_micro_usd=? WHERE id=?`, c, id); err != nil {
			t.Fatal(err)
		}
	}
	total, err := st.TotalSpendMicroUSD(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if total != 100 {
		t.Fatalf("TotalSpendMicroUSD=%d, want 100", total)
	}
}
