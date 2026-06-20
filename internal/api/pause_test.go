package api_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/samhotchkiss/flowbee/client"
	"github.com/samhotchkiss/flowbee/internal/api"
	"github.com/samhotchkiss/flowbee/internal/clock"
	"github.com/samhotchkiss/flowbee/internal/job"
	"github.com/samhotchkiss/flowbee/internal/store"
	"github.com/samhotchkiss/flowbee/internal/testutil"
	"github.com/samhotchkiss/flowbee/internal/ulid"
)

// TestLeaseReturnsNoWorkWhenPaused: when the pause marker exists the lease
// endpoint returns 204 without touching the DB (no claim is attempted).
func TestLeaseReturnsNoWorkWhenPaused(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "paused")

	st := testutil.NewStore(t)
	ctx := context.Background()

	// seed a ready job so there IS something to claim normally.
	jobID := ulid.New()
	if _, err := st.SeedJob(ctx, store.SeedParams{
		ID: jobID, Kind: job.KindBuild, Flow: "build", Stage: "build",
		Role: job.RoleEngWorker, BaseSHA: "sha1", Now: time.Unix(1000, 0),
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	srv := api.New(st, clock.Real{}, ulid.NewMinter(nil), api.Config{
		LeaseTTL:        5 * time.Minute,
		LongPollWait:    100 * time.Millisecond,
		LeaseTTLS:       300,
		PauseMarkerPath: marker,
	}, "test")
	ts := httptest.NewServer(srv.PrivateHandler())
	defer ts.Close()

	c := client.New(ts.URL)

	// without marker: lease should succeed (200 with a grant).
	grant, ok, err := c.Lease(ctx, "worker1", "codex", "eng_worker")
	if err != nil {
		t.Fatalf("lease (no marker): %v", err)
	}
	if !ok {
		t.Error("lease (no marker): expected ok=true (204 means no work)")
	}
	_ = grant

	// create the pause marker.
	if err := os.WriteFile(marker, nil, 0o644); err != nil {
		t.Fatalf("create marker: %v", err)
	}

	// re-seed so there is another ready job (the first was claimed above).
	jobID2 := ulid.New()
	if _, err := st.SeedJob(ctx, store.SeedParams{
		ID: jobID2, Kind: job.KindBuild, Flow: "build", Stage: "build",
		Role: job.RoleEngWorker, BaseSHA: "sha2", Now: time.Unix(1001, 0),
	}); err != nil {
		t.Fatalf("seed2: %v", err)
	}

	// with marker: lease must return no-work (ok=false / 204).
	_, ok2, err := c.Lease(ctx, "worker1", "codex", "eng_worker")
	if err != nil {
		t.Fatalf("lease (paused): %v", err)
	}
	if ok2 {
		t.Error("lease (paused): expected ok=false (no new leases while paused)")
	}

	// remove marker: leasing resumes.
	if err := os.Remove(marker); err != nil {
		t.Fatalf("remove marker: %v", err)
	}

	_, ok3, err := c.Lease(ctx, "worker1", "codex", "eng_worker")
	if err != nil {
		t.Fatalf("lease (resumed): %v", err)
	}
	if !ok3 {
		t.Error("lease (resumed): expected ok=true after removing pause marker")
	}
}

// TestResumeClearsMarkerAndResumesLeases: the documented recovery path must work.
// pause (API) creates the marker AND sets the DB flag; resume (API) removes the
// marker AND clears the flag — so a marker can never wedge the fleet with no way
// back via `flowbee resume` (#216). Asserts the marker file is gone after resume
// and that leasing actually resumes.
func TestResumeClearsMarkerAndResumesLeases(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "paused")

	st := testutil.NewStore(t)
	ctx := context.Background()

	srv := api.New(st, clock.Real{}, ulid.NewMinter(nil), api.Config{
		LeaseTTL:        5 * time.Minute,
		LongPollWait:    100 * time.Millisecond,
		LeaseTTLS:       300,
		PauseMarkerPath: marker,
	}, "test")
	ts := httptest.NewServer(srv.PrivateHandler())
	defer ts.Close()

	c := client.New(ts.URL)

	// pause via the API: the marker must now exist (mirrors the DB flag).
	if err := c.Pause(ctx, ""); err != nil {
		t.Fatalf("pause: %v", err)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("after pause the marker must exist: %v", err)
	}

	// a ready job is NOT leased while paused.
	jobID := ulid.New()
	if _, err := st.SeedJob(ctx, store.SeedParams{
		ID: jobID, Kind: job.KindBuild, Flow: "build", Stage: "build",
		Role: job.RoleEngWorker, BaseSHA: "sha1", Now: time.Unix(1000, 0),
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, ok, err := c.Lease(ctx, "worker1", "codex", "eng_worker"); err != nil || ok {
		t.Fatalf("lease while paused: ok=%v err=%v (want ok=false)", ok, err)
	}

	// resume via the API: the marker must be GONE and leasing must resume.
	if err := c.Resume(ctx, ""); err != nil {
		t.Fatalf("resume: %v", err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("after resume the marker must be removed, stat err = %v", err)
	}
	if _, ok, err := c.Lease(ctx, "worker1", "codex", "eng_worker"); err != nil || !ok {
		t.Fatalf("lease after resume: ok=%v err=%v (want ok=true)", ok, err)
	}

	// resume again with no marker present must not error (idempotent).
	if err := c.Resume(ctx, ""); err != nil {
		t.Fatalf("idempotent resume: %v", err)
	}
}

// TestHeartbeatSucceedsWhilePaused: an already-leased job's heartbeat still
// succeeds while the pause marker is present — pausing only blocks NEW leases.
func TestHeartbeatSucceedsWhilePaused(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "paused")

	st := testutil.NewStore(t)
	ctx := context.Background()

	srv := api.New(st, clock.Real{}, ulid.NewMinter(nil), api.Config{
		LeaseTTL:           5 * time.Minute,
		LongPollWait:       2 * time.Second,
		LeaseTTLS:          300,
		HeartbeatIntervalS: 30,
		PauseMarkerPath:    marker,
	}, "test")
	ts := httptest.NewServer(srv.PrivateHandler())
	defer ts.Close()

	c := client.New(ts.URL)

	// seed and lease a job before pausing.
	jobID := ulid.New()
	if _, err := st.SeedJob(ctx, store.SeedParams{
		ID: jobID, Kind: job.KindBuild, Flow: "build", Stage: "build",
		Role: job.RoleEngWorker, BaseSHA: "sha1", Now: time.Unix(1000, 0),
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	grant, ok, err := c.Lease(ctx, "worker1", "codex", "eng_worker")
	if err != nil || !ok {
		t.Fatalf("lease: ok=%v err=%v", ok, err)
	}

	// now create the pause marker.
	if err := os.WriteFile(marker, nil, 0o644); err != nil {
		t.Fatalf("create marker: %v", err)
	}

	// heartbeat for the in-flight lease must still succeed.
	dir2, status, err := c.Heartbeat(ctx, grant.JobID, grant.LeaseEpoch)
	if err != nil {
		t.Fatalf("heartbeat while paused: %v", err)
	}
	if status != http.StatusOK {
		t.Errorf("heartbeat while paused: status = %d, want 200", status)
	}
	if dir2 == "" {
		t.Error("heartbeat while paused: expected a directive")
	}
}
