package job

import "errors"

// Trigger names a state-machine edge (M1 subset; the full table lands in M3).
type Trigger string

const (
	TriggerClaimed             Trigger = "claimed"
	TriggerWorkStarted         Trigger = "work_started"
	TriggerResultReceived      Trigger = "result_received"
	TriggerReleased            Trigger = "released"
	TriggerLeaseExpiredRetry   Trigger = "lease_expired_retry"
	TriggerLeaseExpiredExhaust Trigger = "lease_expired_exhausted"
)

// ErrIllegalTransition is returned for any (state, trigger) pair not in the table.
var ErrIllegalTransition = errors.New("illegal state transition")

// transitionKey is the lookup key into the pure transition table.
type transitionKey struct {
	from    State
	trigger Trigger
}

// transitions is the pure, total §6.2 state machine (M1 subset). The
// attempts<max decision is resolved by the runtime (which picks the retry vs
// exhausted trigger), keeping Next pure and table-driven.
var transitions = map[transitionKey]State{
	{StateReady, TriggerClaimed}:           StateLeased,
	{StateLeased, TriggerWorkStarted}:      StateBuilding,
	{StateBuilding, TriggerResultReceived}: StateReviewPending,

	{StateLeased, TriggerReleased}:   StateReady,
	{StateBuilding, TriggerReleased}: StateReady,

	{StateLeased, TriggerLeaseExpiredRetry}:   StateReady,
	{StateBuilding, TriggerLeaseExpiredRetry}: StateReady,

	{StateLeased, TriggerLeaseExpiredExhaust}:   StateNeedsHuman,
	{StateBuilding, TriggerLeaseExpiredExhaust}: StateNeedsHuman,
}

// Next is the pure §6.2 state machine: (state, trigger) -> next state. It is a
// total function over the table; any pair not present returns ErrIllegalTransition.
// No side effects, no clock, no I/O.
func Next(from State, t Trigger) (State, error) {
	to, ok := transitions[transitionKey{from: from, trigger: t}]
	if !ok {
		return from, ErrIllegalTransition
	}
	return to, nil
}
