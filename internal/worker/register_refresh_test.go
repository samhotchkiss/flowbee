package worker_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/samhotchkiss/flowbee/internal/testutil"
	"github.com/samhotchkiss/flowbee/internal/worker"
)

// TestReRegisterRefreshesCapsViaResolvedWorkerID reproduces the stale-roster bug and
// proves the fix. The workers table is UNIQUE(identity) but the registration upsert is
// keyed ON CONFLICT(worker_id). A re-registering worker that sends an empty worker_id
// gets a FRESH minted id, whose INSERT collides on identity and FAILS — freezing the
// stored capabilities at the first registration. Resolving the existing worker_id by
// identity first makes the upsert UPDATE the row, so a changed model_family refreshes.
func TestReRegisterRefreshesCapsViaResolvedWorkerID(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	reg := worker.NewRegistry(st, 300, 30, worker.OpenAllowlist())
	now := time.Unix(1000, 0)

	mustReg := func(workerID, family string) error {
		_, err := reg.Register(ctx, worker.Registration{
			WorkerID: workerID, Identity: "w1", Arch: "amd64", OS: "linux",
			Capabilities: []string{"role:eng_worker", "model_family:" + family, "arch:amd64", "os:linux"},
		}, now)
		return err
	}

	// first registration: succeeds, stores model_family:claude.
	if err := mustReg("wid1", "claude"); err != nil {
		t.Fatalf("first register: %v", err)
	}

	// THE BUG: a fresh worker_id (what the server mints for an empty WorkerID) collides
	// on UNIQUE(identity) and fails — so caps would stay frozen at claude.
	if err := mustReg("wid-fresh", "sonnet"); err == nil {
		t.Fatal("expected UNIQUE(identity) failure with a fresh worker_id (the bug) — got nil")
	}

	// THE FIX: resolve the existing worker_id for the identity, then re-register.
	wid, err := st.WorkerIDForIdentity(ctx, "w1")
	if err != nil || wid != "wid1" {
		t.Fatalf("WorkerIDForIdentity=%q err=%v, want wid1", wid, err)
	}
	if err := mustReg(wid, "sonnet"); err != nil {
		t.Fatalf("re-register with resolved worker_id: %v", err)
	}

	// the stored capabilities now reflect the NEW model_family — no stale claude.
	var caps string
	if err := st.DB.QueryRowContext(ctx,
		`SELECT attested_capabilities FROM workers WHERE identity='w1'`).Scan(&caps); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(caps, "model_family:sonnet") {
		t.Errorf("caps not refreshed to sonnet: %s", caps)
	}
	if strings.Contains(caps, "model_family:claude") {
		t.Errorf("stale model_family:claude still present: %s", caps)
	}

	// an unknown identity resolves to empty (so the handler mints a new id for it).
	if got, _ := st.WorkerIDForIdentity(ctx, "nobody"); got != "" {
		t.Errorf("WorkerIDForIdentity(nobody)=%q, want empty", got)
	}
}
