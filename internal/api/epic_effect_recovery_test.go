package api_test

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/samhotchkiss/flowbee/internal/api"
	"github.com/samhotchkiss/flowbee/internal/clock"
	"github.com/samhotchkiss/flowbee/internal/store"
	"github.com/samhotchkiss/flowbee/internal/testutil"
	"github.com/samhotchkiss/flowbee/internal/ulid"
)

func TestEpicEffectRecoveryAPIRearmsExactDeadLetter(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 19, 18, 0, 0, 0, time.UTC)
	st := testutil.NewStore(t)
	st.EnableEpicReviewHandoffV2 = true
	if err := st.AddEpicRun(ctx, store.EpicRun{ID: "recover-api", ProjectID: "default", Repo: "russ",
		Branch: "epic/recover-api", FilePath: "epics/recover-api.md"}, 1, now); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB.ExecContext(ctx, `UPDATE epic_deliveries SET state='merge_queued',head_sha='h1',base_sha='b1' WHERE epic_id='recover-api'`); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB.ExecContext(ctx, `INSERT INTO epic_actions
		(id,project_id,epic_id,kind,state,dedup_key,head_sha,base_sha,recovery_count,dead_lettered_at,created_at,updated_at)
		VALUES ('merge-recover-api','default','recover-api','merge_dispatch','dead_letter','recover-api:merge:h1:b1','h1','b1',2,?,?,?)`,
		now.Format(time.RFC3339), now.Format(time.RFC3339), now.Format(time.RFC3339)); err != nil {
		t.Fatal(err)
	}
	srv := api.New(st, clock.NewFake(now.Add(time.Minute)), ulid.NewMinter(nil), api.Config{}, "test")
	ts := httptest.NewServer(srv.PrivateHandler())
	defer ts.Close()
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/epics/recover-api/effect-recovery",
		bytes.NewBufferString(`{"head_sha":"h1","recovery_code":"merge_dispatch_stalled"}`))
	req.Header.Set("Content-Type", "application/json")
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	var state string
	var recovery int
	if err := st.DB.QueryRowContext(ctx, `SELECT state,recovery_count FROM epic_actions WHERE id='merge-recover-api'`).Scan(&state, &recovery); err != nil {
		t.Fatal(err)
	}
	if state != "pending" || recovery != 0 {
		t.Fatalf("state=%q recovery=%d", state, recovery)
	}
}
