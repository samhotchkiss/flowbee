package engine

import (
	"testing"
	"time"

	"github.com/samhotchkiss/flowbee/internal/job"
	"github.com/samhotchkiss/flowbee/internal/ledger"
)

func ptr(v int64) *int64 { return &v }

// costState builds an active-lease build job with the given accumulated meter +
// ceiling, at the live epoch.
func costState(costMicroUSD int64, ceiling *int64, epoch int) EngineState {
	return EngineState{
		Job: job.Job{
			State: job.StateBuilding, LeaseEpoch: epoch,
			CostMicroUSD: costMicroUSD, CostCeilingMicroUSD: ceiling,
		},
		Now: time.Unix(0, 0), Epoch: epoch,
	}
}

func TestCostMeterStaleEpoch(t *testing.T) {
	d := Decide(costState(0, ptr(1_000_000), 5), CostMeter{Epoch: 4})
	if d.Reject == nil {
		t.Fatal("stale cost report must be rejected (409)")
	}
}

func TestCostMeterUnderCeilingContinues(t *testing.T) {
	d := Decide(costState(500_000, ptr(1_000_000), 3), CostMeter{Epoch: 3})
	if d.Reject != nil {
		t.Fatalf("unexpected reject: %+v", d.Reject)
	}
	if len(d.Transitions) != 0 {
		t.Fatalf("under ceiling must not transition, got %d", len(d.Transitions))
	}
	if d.Directive == nil || *d.Directive != DirectiveContinue {
		t.Fatalf("under ceiling expected continue, got %+v", d.Directive)
	}
}

func TestCostMeterNoCeilingNeverEscalates(t *testing.T) {
	// a huge meter with NO ceiling still continues (the meter only rolls up).
	d := Decide(costState(999_000_000, nil, 1), CostMeter{Epoch: 1})
	if len(d.Transitions) != 0 || d.Directive == nil || *d.Directive != DirectiveContinue {
		t.Fatalf("no ceiling must continue, got transitions=%d dir=%+v", len(d.Transitions), d.Directive)
	}
}

func TestCostMeterAtCeilingEscalates(t *testing.T) {
	// exactly at the ceiling escalates (>=), I-15: never silently overspend.
	d := Decide(costState(1_000_000, ptr(1_000_000), 7), CostMeter{Epoch: 7})
	if d.Reject != nil {
		t.Fatalf("unexpected reject: %+v", d.Reject)
	}
	if d.Directive == nil || *d.Directive != DirectiveCancel {
		t.Fatalf("over budget expected cancel directive, got %+v", d.Directive)
	}
	if len(d.Transitions) != 1 {
		t.Fatalf("expected 1 escalation transition, got %d", len(d.Transitions))
	}
	tr := d.Transitions[0]
	if tr.To != job.StateNeedsHuman {
		t.Fatalf("cost escalation must route to needs_human, got %s", tr.To)
	}
	if tr.Kind != ledger.KindCostEscalated {
		t.Fatalf("expected cost_escalated kind, got %s", tr.Kind)
	}
	if !tr.BumpEpoch {
		t.Fatal("cost escalation must revoke the lease (bump epoch)")
	}
	if tr.RevokeReason != string(job.EscalationCost) {
		t.Fatalf("expected cost reason, got %q", tr.RevokeReason)
	}
}

func TestCostMeterNonActiveRejected(t *testing.T) {
	s := EngineState{
		Job: job.Job{State: job.StateReviewPending, LeaseEpoch: 2, CostCeilingMicroUSD: ptr(10)},
		Now: time.Unix(0, 0), Epoch: 2,
	}
	d := Decide(s, CostMeter{Epoch: 2})
	if d.Reject == nil {
		t.Fatal("cost report on a non-active-lease job must be rejected")
	}
}

// An over-budget job that is somehow still active gets a cancel directive on a
// plain heartbeat (the standing "stop" until the escalation transition lands).
func TestHeartbeatOverBudgetCancels(t *testing.T) {
	s := EngineState{
		Job: job.Job{State: job.StateBuilding, LeaseEpoch: 4, OverBudget: true},
		Now: time.Unix(0, 0), Epoch: 4,
	}
	d := Decide(s, Heartbeat{Epoch: 4})
	if d.Directive == nil || *d.Directive != DirectiveCancel {
		t.Fatalf("over-budget heartbeat expected cancel, got %+v", d.Directive)
	}
}
