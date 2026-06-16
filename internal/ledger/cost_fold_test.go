package ledger

import (
	"testing"
	"time"

	"github.com/samhotchkiss/flowbee/internal/job"
)

// Folding cost_metered events accumulates the per-job meter without changing state
// (§6.7, I-15); a terminal cost_escalated event revokes (epoch bump) and routes to
// needs_human + over_budget — replaying the ledger reconstructs the meter exactly.
func TestFoldCostMeteringAndEscalation(t *testing.T) {
	now := time.Unix(200, 0)
	events := []Event{
		{JobID: "J", JobSeq: 1, Kind: KindJobCreated, ToState: job.StateReady, Actor: "system", CreatedAt: now,
			Payload: Payload{Kind: job.KindBuild, Flow: "build", Stage: "build", Role: job.RoleEngWorker}},
		{JobID: "J", JobSeq: 2, Kind: KindLeaseClaimed, FromState: job.StateReady, ToState: job.StateLeased, LeaseEpoch: 1, Actor: "w1", CreatedAt: now,
			Payload: Payload{LeaseID: "L", BoundIdentity: "w1", BoundModelFamily: "codex"}},
		{JobID: "J", JobSeq: 3, Kind: KindWorkerStarted, FromState: job.StateLeased, ToState: job.StateBuilding, LeaseEpoch: 1, Actor: "w1", CreatedAt: now},
		{JobID: "J", JobSeq: 4, Kind: KindCostMetered, FromState: job.StateBuilding, ToState: job.StateBuilding, LeaseEpoch: 1, Actor: "w1", CreatedAt: now,
			Payload: Payload{CostTokensInDelta: 100, CostTokensOutDelta: 20, CostMicroUSDDelta: 400_000}},
		{JobID: "J", JobSeq: 5, Kind: KindCostMetered, FromState: job.StateBuilding, ToState: job.StateBuilding, LeaseEpoch: 1, Actor: "w1", CreatedAt: now,
			Payload: Payload{CostTokensInDelta: 50, CostTokensOutDelta: 10, CostMicroUSDDelta: 300_000}},
		// the report that trips the ceiling: escalate (epoch++ -> 2), to needs_human.
		{JobID: "J", JobSeq: 6, Kind: KindCostEscalated, FromState: job.StateBuilding, ToState: job.StateNeedsHuman, LeaseEpoch: 2, Actor: "w1", CreatedAt: now,
			Payload: Payload{CostTokensInDelta: 10, CostTokensOutDelta: 5, CostMicroUSDDelta: 400_000, EscalationReason: string(job.EscalationCost)}},
	}
	got, err := Fold(events)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != job.StateNeedsHuman {
		t.Fatalf("state=%s want needs_human", got.State)
	}
	if !got.OverBudget {
		t.Fatal("over_budget must be set after escalation")
	}
	if got.EscalationReason != string(job.EscalationCost) {
		t.Fatalf("escalation_reason=%q want cost", got.EscalationReason)
	}
	if got.LeaseEpoch != 2 {
		t.Fatalf("epoch=%d want 2 (fence bumped on escalation)", got.LeaseEpoch)
	}
	if got.CostTokensIn != 160 || got.CostTokensOut != 35 {
		t.Fatalf("token meter wrong: in=%d out=%d", got.CostTokensIn, got.CostTokensOut)
	}
	if got.CostMicroUSD != 1_100_000 {
		t.Fatalf("micro_usd meter=%d want 1_100_000", got.CostMicroUSD)
	}
	if got.LeaseID != "" || got.BoundIdentity != "" {
		t.Fatalf("lease must be cleared after escalation, got id=%q ident=%q", got.LeaseID, got.BoundIdentity)
	}
}
