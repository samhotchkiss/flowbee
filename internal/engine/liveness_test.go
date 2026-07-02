package engine

import (
	"testing"

	"github.com/samhotchkiss/flowbee/internal/job"
	"github.com/samhotchkiss/flowbee/internal/ledger"
	"github.com/samhotchkiss/flowbee/internal/liveness"
)

// TestDecideLivenessNoKillWhenRung2Abstains: a soft-deadline crossing with Rung-2
// abstaining (no second un-gameable rung) must NOT revoke — the core M8 invariant.
func TestDecideLivenessNoKillWhenRung2Abstains(t *testing.T) {
	d := Decide(state(job.StateBuilding, 3), LivenessVerdict{
		Rungs: liveness.RungSet{Rung2: liveness.Rung2Abstain, Rung3: liveness.Rung3State{SoftCrossed: true}},
	})
	if d.Reject != nil {
		t.Fatalf("unexpected reject: %+v", d.Reject)
	}
	if len(d.Transitions) != 0 {
		t.Fatalf("a soft deadline + abstain must NOT kill, got %+v", d.Transitions)
	}
}

// TestDecideLivenessKillOnSoftPlusStalled: soft deadline + Rung-2 stalled => revoke
// to ready (re-dispatch), epoch bumped, governor counter incremented.
func TestDecideLivenessKillOnSoftPlusStalled(t *testing.T) {
	s := state(job.StateBuilding, 3)
	s.Job.MaxAttempts = 5
	d := Decide(s, LivenessVerdict{
		Rungs: liveness.RungSet{Rung1: liveness.Rung1Spinning, Rung2: liveness.Rung2Stalled, Rung3: liveness.Rung3State{SoftCrossed: true}},
	})
	if len(d.Transitions) != 1 {
		t.Fatalf("expected one revoke transition, got %+v", d.Transitions)
	}
	tr := d.Transitions[0]
	if tr.To != job.StateReady || tr.Kind != ledger.KindLeaseRevoked {
		t.Fatalf("expected revoke -> ready, got %s/%s", tr.To, tr.Kind)
	}
	if !tr.BumpEpoch || tr.StallRevocationsDelta != 1 || tr.AttemptsDelta != 1 {
		t.Fatalf("revoke must bump epoch + counters, got %+v", tr)
	}
	if tr.RevokeReason != "two_rung_stall" {
		t.Fatalf("reason=%q want two_rung_stall", tr.RevokeReason)
	}
}

// TestDecideLivenessAbsoluteCapUnilateral: the absolute cap revokes alone (no second
// rung, Rung-2 abstaining), routing to ready while attempts remain.
func TestDecideLivenessAbsoluteCapUnilateral(t *testing.T) {
	s := state(job.StateBuilding, 7)
	s.Job.MaxAttempts = 5
	d := Decide(s, LivenessVerdict{
		Rungs: liveness.RungSet{Rung2: liveness.Rung2Abstain, Rung3: liveness.Rung3State{AbsoluteCap: true}},
	})
	if len(d.Transitions) != 1 {
		t.Fatalf("absolute cap must revoke unilaterally, got %+v", d.Transitions)
	}
	if d.Transitions[0].To != job.StateReady || d.Transitions[0].RevokeReason != "absolute_cap" {
		t.Fatalf("got %+v", d.Transitions[0])
	}
}

// TestDecideLivenessHeartbeatStaleDistinctFromAbsoluteCap is the regression for the
// ledger mislabeling that made the conflict_resolver heartbeat bug take three
// independent ledger investigations to find (russ #3470/#3498/#3566): a worker gone
// SILENT (Rung3.HeartbeatStale, e.g. its agent CLI crashed / never started) is a
// unilateral kill just like the absolute cap, but it fires at ~4min instead of
// ~20min and for a completely different reason. Before the fix both collapsed to the
// SAME "absolute_cap" RevokeReason on the ledger; they must now be distinguishable.
func TestDecideLivenessHeartbeatStaleDistinctFromAbsoluteCap(t *testing.T) {
	s := state(job.StateResolvingConflict, 3)
	s.Job.MaxAttempts = 5
	d := Decide(s, LivenessVerdict{
		Rungs: liveness.RungSet{Rung2: liveness.Rung2Abstain, Rung3: liveness.Rung3State{HeartbeatStale: true}},
	})
	if len(d.Transitions) != 1 {
		t.Fatalf("heartbeat-stale must revoke unilaterally, got %+v", d.Transitions)
	}
	if got := d.Transitions[0].RevokeReason; got != "heartbeat_stale" {
		t.Fatalf("reason=%q want heartbeat_stale (must NOT collapse into absolute_cap)", got)
	}

	// the true absolute-cap kill is unaffected: still labeled absolute_cap.
	d2 := Decide(s, LivenessVerdict{
		Rungs: liveness.RungSet{Rung2: liveness.Rung2Abstain, Rung3: liveness.Rung3State{AbsoluteCap: true}},
	})
	if got := d2.Transitions[0].RevokeReason; got != "absolute_cap" {
		t.Fatalf("reason=%q want absolute_cap", got)
	}

	// when BOTH windows have been crossed (a job silent long enough to blow past the
	// absolute cap too), EvaluateKill's own precedence picks AbsoluteCap first — the
	// ledger label must match that, not just "whichever flag we see first".
	d3 := Decide(s, LivenessVerdict{
		Rungs: liveness.RungSet{Rung2: liveness.Rung2Abstain, Rung3: liveness.Rung3State{AbsoluteCap: true, HeartbeatStale: true}},
	})
	if got := d3.Transitions[0].RevokeReason; got != "absolute_cap" {
		t.Fatalf("reason=%q want absolute_cap when both windows crossed (matches EvaluateKill's precedence)", got)
	}
}

// TestDecideLivenessGovernorEscalates: at the Rung-4 governor ceiling a stall kill
// sticks in needs_human (anti-thrash) rather than re-dispatching.
func TestDecideLivenessGovernorEscalates(t *testing.T) {
	s := state(job.StateBuilding, 3)
	s.Job.MaxAttempts = 5
	d := Decide(s, LivenessVerdict{
		Rungs:                  liveness.RungSet{Rung1: liveness.Rung1Spinning, Rung2: liveness.Rung2Stalled, Rung3: liveness.Rung3State{SoftCrossed: true}},
		GovernorCeilingReached: true,
	})
	if len(d.Transitions) != 1 || d.Transitions[0].To != job.StateNeedsHuman {
		t.Fatalf("governor ceiling must route to needs_human, got %+v", d.Transitions)
	}
	if d.Transitions[0].Kind != ledger.KindStallEscalated {
		t.Fatalf("expected stall_escalated, got %s", d.Transitions[0].Kind)
	}
}

// TestDecideLivenessAbsoluteCapExhaustedEscalates: the absolute cap at the attempts
// ceiling sticks in needs_human (§6.7).
func TestDecideLivenessAbsoluteCapExhaustedEscalates(t *testing.T) {
	d := Decide(state(job.StateBuilding, 9), LivenessVerdict{
		Rungs:             liveness.RungSet{Rung2: liveness.Rung2Abstain, Rung3: liveness.Rung3State{AbsoluteCap: true}},
		AttemptsExhausted: true,
	})
	if len(d.Transitions) != 1 || d.Transitions[0].To != job.StateNeedsHuman {
		t.Fatalf("cap at attempts ceiling must route to needs_human, got %+v", d.Transitions)
	}
}

// TestDecideHeartbeatFastPathExited: the agent_exited_zombie fast-path -> failed +
// cancel directive, on its face (no ladder).
func TestDecideHeartbeatFastPathExited(t *testing.T) {
	d := Decide(state(job.StateBuilding, 4), Heartbeat{Epoch: 4, AgentExited: true})
	if d.Directive == nil || *d.Directive != DirectiveCancel {
		t.Fatalf("agent exited must cancel, got %+v", d.Directive)
	}
	if len(d.Transitions) != 1 || d.Transitions[0].To != job.StateFailed {
		t.Fatalf("agent exited must go to failed, got %+v", d.Transitions)
	}
	if !d.Transitions[0].BumpEpoch {
		t.Fatal("fast-path must bump the epoch (fence the zombie)")
	}
}

// TestDecideHeartbeatFastPathAwaitingInput: awaiting_input -> cancel + clean
// re-dispatch (ready), on its face.
func TestDecideHeartbeatFastPathAwaitingInput(t *testing.T) {
	d := Decide(state(job.StateBuilding, 4), Heartbeat{Epoch: 4, AwaitingInput: true})
	if d.Directive == nil || *d.Directive != DirectiveCancel {
		t.Fatalf("awaiting_input must cancel, got %+v", d.Directive)
	}
	if len(d.Transitions) != 1 || d.Transitions[0].To != job.StateReady {
		t.Fatalf("awaiting_input must re-dispatch to ready, got %+v", d.Transitions)
	}
}

// TestDecideLivenessStaleNonActive: a liveness verdict on a non-active job is a
// no-op reject (nothing to revoke).
func TestDecideLivenessStaleNonActive(t *testing.T) {
	d := Decide(state(job.StateReady, 1), LivenessVerdict{
		Rungs: liveness.RungSet{Rung3: liveness.Rung3State{AbsoluteCap: true}},
	})
	if d.Reject == nil {
		t.Fatal("liveness on a non-active job must reject")
	}
}

// TestDecideLivenessCodeReviewRevokeTarget: a code_review stall kill returns to
// review_pending (the build product still stands), not ready.
func TestDecideLivenessCodeReviewRevokeTarget(t *testing.T) {
	s := state(job.StateCodeReview, 2)
	s.Job.MaxAttempts = 5
	d := Decide(s, LivenessVerdict{
		Rungs: liveness.RungSet{Rung2: liveness.Rung2Abstain, Rung3: liveness.Rung3State{AbsoluteCap: true}},
	})
	if len(d.Transitions) != 1 || d.Transitions[0].To != job.StateReviewPending {
		t.Fatalf("code_review revoke must return to review_pending, got %+v", d.Transitions)
	}
}
