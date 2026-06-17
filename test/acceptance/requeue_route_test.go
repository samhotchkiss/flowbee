package acceptance

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/samhotchkiss/flowbee/internal/api"
	"github.com/samhotchkiss/flowbee/internal/clock"
	"github.com/samhotchkiss/flowbee/internal/job"
	"github.com/samhotchkiss/flowbee/internal/store"
	"github.com/samhotchkiss/flowbee/internal/testutil"
	"github.com/samhotchkiss/flowbee/internal/ulid"
)

// TestRequeueRouteReArmsNeedsHuman exercises the real HTTP route end-to-end —
// catching the #38 regression (handler registered on the worker submux but not
// mounted on the main mux -> 405). A needs_human job must re-arm to ready over
// POST /v1/jobs/{id}/requeue.
func TestRequeueRouteReArmsNeedsHuman(t *testing.T) {
	st := testutil.NewStore(t)
	srv := api.New(st, clock.Real{}, ulid.NewMinter(nil), api.Config{
		LeaseTTL: 5 * time.Minute, LongPollWait: time.Second,
		LeaseTTLS: 300, HeartbeatIntervalS: 30,
	}, "test")
	ts := httptest.NewServer(srv.PrivateHandler())
	defer ts.Close()
	ctx := context.Background()

	id := ulid.New()
	if _, err := st.SeedJob(ctx, store.SeedParams{
		ID: id, Kind: job.KindBuild, Flow: "build", Stage: "build",
		Role: job.RoleEngWorker, BaseSHA: "base", Now: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB.ExecContext(ctx,
		`UPDATE jobs SET state='needs_human', attempts=3 WHERE id=?`, id); err != nil {
		t.Fatal(err)
	}

	resp, err := http.Post(ts.URL+"/v1/jobs/"+id+"/requeue", "application/json", nil)
	if err != nil {
		t.Fatalf("POST requeue: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("requeue route returned %d (want 200) — is it mounted on the main mux?", resp.StatusCode)
	}

	j, _ := st.GetJob(ctx, id)
	if j.State != job.StateReady {
		t.Fatalf("after requeue state=%s, want ready", j.State)
	}
	if j.Attempts != 0 {
		t.Fatalf("after requeue attempts=%d, want 0", j.Attempts)
	}
}
