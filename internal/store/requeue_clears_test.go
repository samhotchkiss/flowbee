package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/samhotchkiss/flowbee/internal/job"
	"github.com/samhotchkiss/flowbee/internal/store"
	"github.com/samhotchkiss/flowbee/internal/testutil"
)

// TestRequeueClearsOverBudgetAndEscalation: a requeued build is ACTIVE again, so it must
// drop its over_budget + escalation_reason flags (else it keeps counting in the
// over-budget metric / shows a stale escalation reason) and re-arm to ready/eng_worker.
func TestRequeueClearsOverBudgetAndEscalation(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	now := time.Unix(1000, 0)

	if _, err := st.SeedJob(ctx, store.SeedParams{
		ID: "b", Kind: job.KindBuild, Flow: "build", Stage: "build", Role: job.RoleEngWorker, Now: now,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB.ExecContext(ctx,
		`UPDATE jobs SET state='needs_human', over_budget=1, escalation_reason='cost' WHERE id='b'`); err != nil {
		t.Fatal(err)
	}
	final, err := st.RequeueJob(ctx, "b", now.Add(time.Second))
	if err != nil {
		t.Fatalf("requeue: %v", err)
	}
	if final != job.StateReady {
		t.Fatalf("requeued build state=%s, want ready", final)
	}
	var over int
	var reason, role string
	if err := st.DB.QueryRowContext(ctx,
		`SELECT over_budget, escalation_reason, role FROM jobs WHERE id='b'`).Scan(&over, &reason, &role); err != nil {
		t.Fatal(err)
	}
	if over != 0 || reason != "" {
		t.Fatalf("requeue left over_budget=%d reason=%q, want both cleared", over, reason)
	}
	if role != "eng_worker" {
		t.Fatalf("requeued build role=%s, want eng_worker", role)
	}
}

// TestRequeueSpecJobReArmsToSpecAuthoring: a SPEC job that escalated must re-arm to
// spec_authoring (it has no build to rebuild) — not to a build, which would run with no
// spec and fail again.
func TestRequeueSpecJobReArmsToSpecAuthoring(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	now := time.Unix(1000, 0)

	if _, err := st.SeedJob(ctx, store.SeedParams{
		ID: "s", Kind: job.KindSpec, Flow: "spec", Stage: "spec", Role: job.RoleSpecAuthor, Now: now,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB.ExecContext(ctx, `UPDATE jobs SET state='needs_human' WHERE id='s'`); err != nil {
		t.Fatal(err)
	}
	final, err := st.RequeueJob(ctx, "s", now.Add(time.Second))
	if err != nil {
		t.Fatalf("requeue: %v", err)
	}
	if final != job.StateSpecAuthoring {
		t.Fatalf("requeued spec state=%s, want spec_authoring (not a build)", final)
	}
	j, _ := st.GetJob(ctx, "s")
	if j.Role != job.RoleSpecAuthor {
		t.Fatalf("requeued spec role=%s, want spec_author", j.Role)
	}
}
