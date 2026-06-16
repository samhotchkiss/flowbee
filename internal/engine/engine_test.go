package engine

import (
	"testing"
	"time"

	"github.com/samhotchkiss/flowbee/internal/job"
	"github.com/samhotchkiss/flowbee/internal/ledger"
)

func state(s job.State, epoch int) EngineState {
	return EngineState{Job: job.Job{State: s, LeaseEpoch: epoch}, Now: time.Unix(0, 0), Epoch: epoch}
}

func TestDecideHeartbeatStaleEpoch(t *testing.T) {
	d := Decide(state(job.StateBuilding, 5), Heartbeat{Epoch: 4})
	if d.Reject == nil {
		t.Fatal("stale heartbeat must be rejected (409)")
	}
}

func TestDecideHeartbeatContinue(t *testing.T) {
	d := Decide(state(job.StateBuilding, 5), Heartbeat{Epoch: 5})
	if d.Reject != nil {
		t.Fatalf("current heartbeat must continue, got reject %+v", d.Reject)
	}
	if d.Directive == nil || *d.Directive != DirectiveContinue {
		t.Fatalf("expected continue directive, got %+v", d.Directive)
	}
}

func TestDecideResultFromBuilding(t *testing.T) {
	d := Decide(state(job.StateBuilding, 2), WorkResult{Epoch: 2})
	if d.Reject != nil {
		t.Fatalf("unexpected reject: %+v", d.Reject)
	}
	if len(d.Transitions) != 1 {
		t.Fatalf("expected 1 transition, got %d", len(d.Transitions))
	}
	tr := d.Transitions[0]
	if tr.From != job.StateBuilding || tr.To != job.StateReviewPending || tr.Kind != ledger.KindResultAccepted {
		t.Fatalf("bad transition: %+v", tr)
	}
}

func TestDecideResultFromLeasedEmitsStartThenResult(t *testing.T) {
	d := Decide(state(job.StateLeased, 1), WorkResult{Epoch: 1})
	if d.Reject != nil {
		t.Fatalf("unexpected reject: %+v", d.Reject)
	}
	if len(d.Transitions) != 2 {
		t.Fatalf("expected start+result transitions, got %d", len(d.Transitions))
	}
	if d.Transitions[0].Kind != ledger.KindWorkerStarted {
		t.Fatalf("first transition should be worker_started, got %s", d.Transitions[0].Kind)
	}
	if d.Transitions[1].To != job.StateReviewPending {
		t.Fatalf("final state should be review_pending, got %s", d.Transitions[1].To)
	}
}

func TestDecideResultStaleEpoch(t *testing.T) {
	d := Decide(state(job.StateBuilding, 9), WorkResult{Epoch: 8})
	if d.Reject == nil {
		t.Fatal("stale result must be rejected (409)")
	}
}

func TestDecideDeterministic(t *testing.T) {
	s := state(job.StateBuilding, 3)
	a := Decide(s, WorkResult{Epoch: 3})
	b := Decide(s, WorkResult{Epoch: 3})
	if len(a.Transitions) != len(b.Transitions) {
		t.Fatal("Decide must be deterministic")
	}
	for i := range a.Transitions {
		if a.Transitions[i] != b.Transitions[i] {
			t.Fatalf("non-deterministic transition at %d", i)
		}
	}
}
