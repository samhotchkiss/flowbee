package job

import (
	"errors"
	"testing"
)

func TestNextHappyPath(t *testing.T) {
	cases := []struct {
		from State
		trig Trigger
		want State
	}{
		{StateReady, TriggerClaimed, StateLeased},
		{StateLeased, TriggerWorkStarted, StateBuilding},
		{StateBuilding, TriggerResultReceived, StateReviewPending},
		{StateLeased, TriggerReleased, StateReady},
		{StateBuilding, TriggerReleased, StateReady},
		{StateLeased, TriggerLeaseExpiredRetry, StateReady},
		{StateBuilding, TriggerLeaseExpiredExhaust, StateNeedsHuman},
	}
	for _, c := range cases {
		got, err := Next(c.from, c.trig)
		if err != nil {
			t.Fatalf("Next(%s,%s) err: %v", c.from, c.trig, err)
		}
		if got != c.want {
			t.Fatalf("Next(%s,%s)=%s want %s", c.from, c.trig, got, c.want)
		}
	}
}

// TestNextF4Edges: the F4 issue-review edges — amend in place completes the spec
// without an author bounce, a design fork escalates to needs_design, and the human
// resolves it back to spec_review. needs_design holds NO active lease.
func TestNextF4Edges(t *testing.T) {
	cases := []struct {
		from State
		trig Trigger
		want State
	}{
		{StateSpecReview, TriggerSpecAmended, StateDone},
		{StateSpecReview, TriggerSpecNeedsDesign, StateNeedsDesign},
		{StateNeedsDesign, TriggerDesignResolved, StateSpecReview},
	}
	for _, c := range cases {
		got, err := Next(c.from, c.trig)
		if err != nil {
			t.Fatalf("Next(%s,%s) err: %v", c.from, c.trig, err)
		}
		if got != c.want {
			t.Fatalf("Next(%s,%s)=%s want %s", c.from, c.trig, got, c.want)
		}
	}
	// needs_design and backlog are parked states — no active lease.
	if HasActiveLease(StateNeedsDesign) || HasActiveLease(StateBacklog) {
		t.Fatal("needs_design / backlog must NOT hold an active lease")
	}
}

func TestNextIllegal(t *testing.T) {
	if _, err := Next(StateReviewPending, TriggerClaimed); !errors.Is(err, ErrIllegalTransition) {
		t.Fatalf("expected ErrIllegalTransition, got %v", err)
	}
	if _, err := Next(StateDone, TriggerResultReceived); !errors.Is(err, ErrIllegalTransition) {
		t.Fatalf("expected ErrIllegalTransition, got %v", err)
	}
}

func TestActiveLeaseStatesMatchIndex(t *testing.T) {
	// The partial-unique-index predicate in 0002_m1_lease_thread.sql MUST equal
	// this set. Guard against drift.
	want := map[State]bool{
		StateLeased: true, StateBuilding: true, StateCodeReview: true,
		StateMerging: true, StateMergeHandoff: true,
		StateSpecAuthoring: true, StateSpecReview: true,
	}
	if len(want) != len(ActiveLeaseStates) {
		t.Fatalf("active-lease set size drift: %d vs %d", len(want), len(ActiveLeaseStates))
	}
	for s := range want {
		if !HasActiveLease(s) {
			t.Fatalf("state %s should hold an active lease", s)
		}
	}
	if HasActiveLease(StateReady) || HasActiveLease(StateReviewPending) {
		t.Fatalf("ready/review_pending must NOT hold an active lease")
	}
}
