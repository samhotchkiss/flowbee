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
	// M2 scheduler/DAG triggers.
	TriggerDepsCleared Trigger = "deps_cleared" // blocked -> ready (all blocked_by done)
	TriggerCompleted   Trigger = "completed"    // review_pending -> done (M2: hand-driven completion)
	// M3 build-flow gate triggers (§6.2).
	TriggerReviewClaimed   Trigger = "review_claimed"   // review_pending -> code_review (reviewer leases)
	TriggerReviewStarted   Trigger = "review_started"   // code_review lease -> work begins (no state move)
	TriggerApproved        Trigger = "approved"         // code_review -> mergeable (gate minted a verdict)
	TriggerBounce          Trigger = "bounce"           // code_review -> building (changes_requested; bounce++)
	TriggerBounceExhausted Trigger = "bounce_exhausted" // code_review -> needs_human (max_bounces)
	TriggerHandoff         Trigger = "handoff"          // mergeable -> merge_handoff (distinct merger arm)
	TriggerSelfMerge       Trigger = "self_merge"       // mergeable -> merging (reviewer-attributed)
	// M7 spec-flow gate triggers (§11).
	TriggerSpecReviewClaimed Trigger = "spec_review_claimed" // spec_authoring -> spec_review (reviewer leases)
	TriggerSpecSignedOff     Trigger = "spec_signed_off"     // spec_review -> done (sign-off minted; issue materialized)
	TriggerSpecSuperseded    Trigger = "spec_superseded"     // spec edit voided the sign-off; re-arm the gate
	TriggerSpecAuthored      Trigger = "spec_authored"       // spec_authoring -> spec_review (author submitted draft)

	// F4 issue-review edges. Issue-review AMENDS in place (commits the amended spec,
	// mints a sign-off on the amended hash -> done) and never bounces to the author; a
	// design fork escalates to needs_design (surfaced on /v1/needs-input) and resumes
	// to spec_review when a human supplies the design decision.
	TriggerSpecAmended     Trigger = "spec_amended"      // spec_review -> done (amended in place + signed off)
	TriggerSpecNeedsDesign Trigger = "spec_needs_design" // spec_review -> needs_design (design fork)
	TriggerDesignResolved  Trigger = "design_resolved"   // needs_design -> spec_review (human answered)

	// M8 liveness triggers (§10.7). A "kill" is a lease REVOCATION (epoch++ +
	// compensation), routed to `ready` for re-dispatch unless the Rung-4 governor
	// ceiling holds it in `needs_human` (anti-thrash). The absolute-cap kill and a
	// two-rung stall kill share these edges; the runtime picks retry-vs-exhausted by
	// the governor / attempts counters, keeping Next pure.
	TriggerStallRevokedRetry   Trigger = "stall_revoked_retry"     // active -> ready (revoke; re-dispatch)
	TriggerStallRevokedExhaust Trigger = "stall_revoked_exhausted" // active -> needs_human (governor ceiling)
	TriggerLeaseCappedRetry    Trigger = "lease_capped_retry"      // active -> ready (absolute cap; re-dispatch)
	TriggerLeaseCappedExhaust  Trigger = "lease_capped_exhausted"  // active -> needs_human (max_attempts at cap)
	TriggerAgentExited         Trigger = "agent_exited"            // active -> failed (zombie fast-path, §10.6)
	TriggerAwaitingInputCancel Trigger = "awaiting_input_cancel"   // active -> ready (awaiting-input fast-path, §10.6)
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

	// M2 DAG/scheduler edges.
	{StateBlocked, TriggerDepsCleared}:     StateReady,
	{StateReviewPending, TriggerCompleted}: StateDone,

	// M3 build-flow gate edges (§6.2). review_pending holds no active lease;
	// claiming it as a code_reviewer enters code_review (an active-lease state).
	{StateReviewPending, TriggerReviewClaimed}: StateCodeReview,
	// the code_review gate, driven by reconciled facts + a minted verdict (I-9).
	// A bounce returns the job to `ready` (re-leasable by an eng_worker to rebuild
	// against the same base) — NOT `building`, which is an active-lease state with
	// no worker. This matches the §6.2.2 diagram (the bounce arrow re-arms `ready`)
	// and keeps the one_active_lease_per_job index honest.
	{StateCodeReview, TriggerApproved}:        StateMergeable,
	{StateCodeReview, TriggerBounce}:          StateReady,
	{StateCodeReview, TriggerBounceExhausted}: StateNeedsHuman,
	// a code_review lease can expire/release back to review_pending (not ready):
	// the review attempt failed but the build product still stands.
	{StateCodeReview, TriggerReleased}:          StateReviewPending,
	{StateCodeReview, TriggerLeaseExpiredRetry}: StateReviewPending,
	// the branch point after a passing gate (§5.4):
	{StateMergeable, TriggerHandoff}:   StateMergeHandoff,
	{StateMergeable, TriggerSelfMerge}: StateMerging,

	// M7 spec-flow edges (§11). The spec_author drafts in spec_authoring; on
	// submit the gate stage spec_review opens; a minted sign-off completes the
	// spec job (-> done, having materialized the issue); changes_requested bounces
	// back to spec_authoring; a spec edit supersedes a sign-off and re-arms.
	{StateSpecAuthoring, TriggerSpecAuthored}:   StateSpecReview,
	{StateSpecReview, TriggerSpecSignedOff}:     StateDone,
	{StateSpecReview, TriggerBounce}:            StateSpecAuthoring,
	{StateSpecReview, TriggerBounceExhausted}:   StateNeedsHuman,
	{StateSpecReview, TriggerSpecSuperseded}:    StateSpecAuthoring,
	{StateSpecReview, TriggerReleased}:          StateSpecAuthoring,
	{StateSpecReview, TriggerLeaseExpiredRetry}: StateSpecAuthoring,

	// F4 issue-review: AMEND in place completes the spec (-> done) without an author
	// bounce; a design fork escalates to needs_design and resumes to spec_review.
	{StateSpecReview, TriggerSpecAmended}:     StateDone,
	{StateSpecReview, TriggerSpecNeedsDesign}: StateNeedsDesign,
	{StateNeedsDesign, TriggerDesignResolved}: StateSpecReview,

	// M8 liveness edges (§10.7). Every active-lease state can be killed (revoked) by
	// the two-rung rule or the absolute cap, or fast-pathed to failed/cancelled. A
	// revoke routes to `ready` for re-dispatch (anti-affinity still holds) unless the
	// governor ceiling holds it in `needs_human`. A build-stage revoke returns to
	// `ready`; a review-stage (code_review) revoke returns to `review_pending` (the
	// build product still stands), mirroring the release/expiry edges above.
	{StateLeased, TriggerStallRevokedRetry}:        StateReady,
	{StateBuilding, TriggerStallRevokedRetry}:      StateReady,
	{StateCodeReview, TriggerStallRevokedRetry}:    StateReviewPending,
	{StateSpecAuthoring, TriggerStallRevokedRetry}: StateSpecAuthoring,
	{StateSpecReview, TriggerStallRevokedRetry}:    StateSpecAuthoring,

	{StateLeased, TriggerStallRevokedExhaust}:        StateNeedsHuman,
	{StateBuilding, TriggerStallRevokedExhaust}:      StateNeedsHuman,
	{StateCodeReview, TriggerStallRevokedExhaust}:    StateNeedsHuman,
	{StateSpecAuthoring, TriggerStallRevokedExhaust}: StateNeedsHuman,
	{StateSpecReview, TriggerStallRevokedExhaust}:    StateNeedsHuman,

	{StateLeased, TriggerLeaseCappedRetry}:        StateReady,
	{StateBuilding, TriggerLeaseCappedRetry}:      StateReady,
	{StateCodeReview, TriggerLeaseCappedRetry}:    StateReviewPending,
	{StateSpecAuthoring, TriggerLeaseCappedRetry}: StateSpecAuthoring,
	{StateSpecReview, TriggerLeaseCappedRetry}:    StateSpecAuthoring,

	{StateLeased, TriggerLeaseCappedExhaust}:        StateNeedsHuman,
	{StateBuilding, TriggerLeaseCappedExhaust}:      StateNeedsHuman,
	{StateCodeReview, TriggerLeaseCappedExhaust}:    StateNeedsHuman,
	{StateSpecAuthoring, TriggerLeaseCappedExhaust}: StateNeedsHuman,
	{StateSpecReview, TriggerLeaseCappedExhaust}:    StateNeedsHuman,

	// agent_exited_zombie fast-path (§10.6): straight to `failed` (terminal for the
	// attempt; compensation re-queues subject to max_attempts).
	{StateLeased, TriggerAgentExited}:        StateFailed,
	{StateBuilding, TriggerAgentExited}:      StateFailed,
	{StateCodeReview, TriggerAgentExited}:    StateFailed,
	{StateSpecAuthoring, TriggerAgentExited}: StateFailed,
	{StateSpecReview, TriggerAgentExited}:    StateFailed,

	// awaiting_input fast-path (§10.6): clean cancel, route per policy (re-dispatch).
	{StateLeased, TriggerAwaitingInputCancel}:        StateReady,
	{StateBuilding, TriggerAwaitingInputCancel}:      StateReady,
	{StateCodeReview, TriggerAwaitingInputCancel}:    StateReviewPending,
	{StateSpecAuthoring, TriggerAwaitingInputCancel}: StateSpecAuthoring,
	{StateSpecReview, TriggerAwaitingInputCancel}:    StateSpecAuthoring,

	// re-dispatch from a needs_human stall after deliberate human resume: stays in
	// `failed` -> ready edge for the zombie re-queue path.
	{StateFailed, TriggerLeaseExpiredRetry}: StateReady,
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
