package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/samhotchkiss/flowbee/internal/store"
	"github.com/samhotchkiss/flowbee/internal/testutil"
)

func TestDeliveryAgnosticBackstopSurfacesEveryOverdueState(t *testing.T) {
	st := testutil.NewStore(t)
	st.EnableEpicReviewHandoffV2 = true
	ctx := context.Background()
	entered := time.Date(2026, 7, 19, 1, 0, 0, 0, time.UTC)
	states := []string{"admitted", "building", "awaiting_artifact", "awaiting_ci", "merge_queued", "merging", "conflict_resolution", "merged", "cleanup_pending"}
	for _, state := range states {
		id := "backstop-" + state
		if err := st.AddEpicRun(ctx, store.EpicRun{ID: id, Repo: "repo", Branch: "epic/" + id}, 1, entered); err != nil {
			t.Fatal(err)
		}
		if _, err := st.DB.ExecContext(ctx, `UPDATE epic_deliveries SET state=?,state_version=1,
			state_entered_at=?,state_due_at=?,fact_progress_at=? WHERE epic_id=?`, state,
			entered.Format(time.RFC3339), entered.Add(time.Minute).Format(time.RFC3339), entered.Format(time.RFC3339), id); err != nil {
			t.Fatal(err)
		}
	}
	rep, err := st.ReconcileEpicDeliveryBackstops(ctx, entered.Add(time.Hour))
	if err != nil || rep.Alerted != len(states) {
		t.Fatalf("backstop=%+v err=%v", rep, err)
	}
	var alerts int
	if err := st.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM control_alerts WHERE state='pending'`).Scan(&alerts); err != nil || alerts != len(states) {
		t.Fatalf("alerts=%d err=%v", alerts, err)
	}
	// Repeat is idempotent for the same state/version.
	if rep, err = st.ReconcileEpicDeliveryBackstops(ctx, entered.Add(2*time.Hour)); err != nil || rep.Alerted != len(states) {
		t.Fatalf("repeat backstop=%+v err=%v", rep, err)
	}
	if err := st.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM control_alerts`).Scan(&alerts); err != nil || alerts != len(states) {
		t.Fatalf("repeat duplicated alerts=%d err=%v", alerts, err)
	}
}
