package store

import (
	"context"
	"testing"
	"time"

	"github.com/samhotchkiss/flowbee/internal/job"
	"github.com/samhotchkiss/flowbee/internal/lease"
)

// seedLeasedCost seeds a build job (optional $ ceiling + flow_id) and claims it,
// returning the live epoch.
func seedLeasedCost(t *testing.T, st *Store, jobID, flowID string, ceiling *int64) int {
	t.Helper()
	ctx := context.Background()
	now := time.Unix(1000, 0)
	if _, err := st.SeedJob(ctx, SeedParams{
		ID: jobID, Kind: job.KindBuild, Flow: "build", Stage: "build",
		Role: job.RoleEngWorker, BaseSHA: "b0", FlowID: flowID,
		CostCeilingMicroUSD: ceiling, Now: now,
	}); err != nil {
		t.Fatalf("seed %s: %v", jobID, err)
	}
	ls, err := st.ClaimReadyJob(ctx, ClaimParams{
		JobID: jobID, LeaseID: "L-" + jobID, Identity: "w-" + jobID, ModelFamily: "codex",
		Role: job.RoleEngWorker, Attested: []string{"role:eng_worker", "model_family:codex"},
		TTL: time.Hour, Now: now,
	})
	if err != nil {
		t.Fatalf("claim %s: %v", jobID, err)
	}
	return ls.Epoch
}

func TestRecordCostAccumulatesUnderCeiling(t *testing.T) {
	st := newLiveStore(t)
	ctx := context.Background()
	ceiling := int64(10_000_000)
	epoch := seedLeasedCost(t, st, "j", "f", &ceiling)

	res, err := st.RecordCost(ctx, CostParams{
		JobID: "j", Epoch: epoch, Now: time.Unix(1001, 0),
		TokensInDelta: 100, TokensOutDelta: 20, MicroUSDDelta: 3_000_000,
	})
	if err != nil {
		t.Fatalf("record cost: %v", err)
	}
	if res.Escalated || res.Directive != "continue" {
		t.Fatalf("under ceiling: escalated=%v dir=%q", res.Escalated, res.Directive)
	}
	j, _ := st.GetJob(ctx, "j")
	if j.CostMicroUSD != 3_000_000 || j.CostTokensIn != 100 || j.CostTokensOut != 20 {
		t.Fatalf("meter not accumulated: %+v", j)
	}
	if !job.HasActiveLease(j.State) {
		t.Fatalf("job must keep its lease, got %s", j.State)
	}
}

func TestRecordCostEscalatesOverCeiling(t *testing.T) {
	st := newLiveStore(t)
	ctx := context.Background()
	ceiling := int64(5_000_000)
	epoch := seedLeasedCost(t, st, "j", "f", &ceiling)

	res, err := st.RecordCost(ctx, CostParams{
		JobID: "j", Epoch: epoch, Now: time.Unix(1001, 0),
		TokensInDelta: 1, TokensOutDelta: 1, MicroUSDDelta: 6_000_000,
	})
	if err != nil {
		t.Fatalf("record cost: %v", err)
	}
	if !res.Escalated || res.Directive != "cancel" {
		t.Fatalf("over ceiling must escalate+cancel: escalated=%v dir=%q", res.Escalated, res.Directive)
	}
	j, _ := st.GetJob(ctx, "j")
	if j.State != job.StateNeedsHuman || !j.OverBudget {
		t.Fatalf("over budget: state=%s over=%v", j.State, j.OverBudget)
	}
	if j.EscalationReason != string(job.EscalationCost) {
		t.Fatalf("escalation_reason=%q want cost", j.EscalationReason)
	}
	if j.LeaseEpoch != epoch+1 {
		t.Fatalf("escalation must bump the epoch: %d -> %d", epoch, j.LeaseEpoch)
	}
	// the fenced worker's next stale call -> ErrStaleEpoch (409).
	if _, err := st.RecordCost(ctx, CostParams{
		JobID: "j", Epoch: epoch, Now: time.Unix(1002, 0), MicroUSDDelta: 1,
	}); err != lease.ErrStaleEpoch {
		t.Fatalf("stale cost report want ErrStaleEpoch, got %v", err)
	}
}

// TestRecordCostDefaultCeilingEngages proves the operator-configured fleet-wide
// default ceiling (Store.DefaultCostCeilingMicroUSD) caps a job that carries NO
// per-job ceiling of its own — the §6.7 circuit-breaker an operator arms via
// FLOWBEE_COST_CEILING_USD without seeding a ceiling on every job.
func TestRecordCostDefaultCeilingEngages(t *testing.T) {
	st := newLiveStore(t)
	st.DefaultCostCeilingMicroUSD = 5_000_000
	ctx := context.Background()
	epoch := seedLeasedCost(t, st, "j", "f", nil) // nil per-job ceiling

	res, err := st.RecordCost(ctx, CostParams{
		JobID: "j", Epoch: epoch, Now: time.Unix(1001, 0),
		MicroUSDDelta: 6_000_000, // over the default
	})
	if err != nil {
		t.Fatalf("record cost: %v", err)
	}
	if !res.Escalated || res.Directive != "cancel" {
		t.Fatalf("over default ceiling must escalate+cancel: escalated=%v dir=%q", res.Escalated, res.Directive)
	}
	j, _ := st.GetJob(ctx, "j")
	if j.State != job.StateNeedsHuman || !j.OverBudget {
		t.Fatalf("over budget: state=%s over=%v", j.State, j.OverBudget)
	}
	if j.EscalationReason != string(job.EscalationCost) {
		t.Fatalf("escalation_reason=%q want cost", j.EscalationReason)
	}
	// the default is NOT persisted onto the job — it applies per-decision only.
	if j.CostCeilingMicroUSD != nil {
		t.Fatalf("default ceiling must not persist onto the job, got %v", *j.CostCeilingMicroUSD)
	}
}

// TestRecordCostNoDefaultNeverCaps proves the shipped posture: with no default
// and no per-job ceiling, an arbitrarily large meter accumulates and never caps.
func TestRecordCostNoDefaultNeverCaps(t *testing.T) {
	st := newLiveStore(t) // DefaultCostCeilingMicroUSD == 0
	ctx := context.Background()
	epoch := seedLeasedCost(t, st, "j", "f", nil)

	res, err := st.RecordCost(ctx, CostParams{
		JobID: "j", Epoch: epoch, Now: time.Unix(1001, 0), MicroUSDDelta: 999_000_000,
	})
	if err != nil {
		t.Fatalf("record cost: %v", err)
	}
	if res.Escalated || res.Directive != "continue" {
		t.Fatalf("no ceiling must never cap: escalated=%v dir=%q", res.Escalated, res.Directive)
	}
}

// TestRecordCostPerJobCeilingOverridesDefault proves a deliberately-seeded per-job
// ceiling wins over the fleet default (e.g. a costly epic granted more headroom):
// a meter under the per-job ceiling but over the default still continues.
func TestRecordCostPerJobCeilingOverridesDefault(t *testing.T) {
	st := newLiveStore(t)
	st.DefaultCostCeilingMicroUSD = 5_000_000
	ctx := context.Background()
	high := int64(100_000_000)
	epoch := seedLeasedCost(t, st, "j", "f", &high) // per-job ceiling >> default

	res, err := st.RecordCost(ctx, CostParams{
		JobID: "j", Epoch: epoch, Now: time.Unix(1001, 0), MicroUSDDelta: 6_000_000,
	})
	if err != nil {
		t.Fatalf("record cost: %v", err)
	}
	if res.Escalated || res.Directive != "continue" {
		t.Fatalf("per-job ceiling must override default: escalated=%v dir=%q", res.Escalated, res.Directive)
	}
}

func TestRecordCostStaleEpoch(t *testing.T) {
	st := newLiveStore(t)
	ctx := context.Background()
	ceiling := int64(10_000_000)
	epoch := seedLeasedCost(t, st, "j", "f", &ceiling)
	if _, err := st.RecordCost(ctx, CostParams{
		JobID: "j", Epoch: epoch - 1, Now: time.Unix(1001, 0), MicroUSDDelta: 1,
	}); err != lease.ErrStaleEpoch {
		t.Fatalf("stale epoch want ErrStaleEpoch, got %v", err)
	}
}

func TestFlowCostRollupSumsAcrossJobs(t *testing.T) {
	st := newLiveStore(t)
	ctx := context.Background()
	ceiling := int64(100_000_000)
	for i, id := range []string{"a", "b", "c"} {
		epoch := seedLeasedCost(t, st, id, "feat", &ceiling)
		usd := int64((i + 1) * 1_000_000)
		if _, err := st.RecordCost(ctx, CostParams{
			JobID: id, Epoch: epoch, Now: time.Unix(1001, 0),
			TokensInDelta: int64((i + 1) * 1000), TokensOutDelta: int64((i + 1) * 100), MicroUSDDelta: usd,
		}); err != nil {
			t.Fatalf("cost %s: %v", id, err)
		}
	}
	roll, err := st.FlowCostRollup(ctx, "feat")
	if err != nil {
		t.Fatalf("rollup: %v", err)
	}
	if len(roll.Jobs) != 3 {
		t.Fatalf("rollup jobs=%d want 3", len(roll.Jobs))
	}
	if roll.TotalMicroUSD != 6_000_000 {
		t.Fatalf("total micro_usd=%d want 6_000_000", roll.TotalMicroUSD)
	}
	if roll.TotalTokensIn != 6000 || roll.TotalTokensOut != 600 {
		t.Fatalf("token totals wrong: in=%d out=%d", roll.TotalTokensIn, roll.TotalTokensOut)
	}
}

func TestNeedsHumanViewClassifiesTriggers(t *testing.T) {
	tests := []struct {
		reason string
		over   int
		att, maxA, bnc, maxB, stall int
		want   string
	}{
		{"cost", 1, 0, 5, 0, 3, 0, "cost"},
		{"", 0, 5, 5, 0, 3, 0, "attempts"},
		{"", 0, 0, 5, 3, 3, 0, "bounces"},
		{"", 0, 0, 5, 0, 3, 2, "stall"},
		{"absolute_cap", 0, 5, 5, 0, 3, 1, "attempts"}, // attempts exhausted at the cap
		{"two_rung_stall", 0, 0, 5, 0, 3, 2, "stall"},  // governor, attempts not spent
	}
	for _, tc := range tests {
		got := classifyEscalation(tc.reason, tc.over, tc.att, tc.maxA, tc.bnc, tc.maxB, tc.stall)
		if got != tc.want {
			t.Fatalf("classify(reason=%q over=%d att=%d/%d bnc=%d/%d stall=%d)=%q want %q",
				tc.reason, tc.over, tc.att, tc.maxA, tc.bnc, tc.maxB, tc.stall, got, tc.want)
		}
	}
}
