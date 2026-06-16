// Package engine is the deterministic CORE (DESIGN §1.2, §3.4): a pure function
// Decide(state, event) -> Decision. It performs zero I/O, reads no clock,
// generates no IDs — everything is injected via EngineState/Event values. An
// outer runtime (internal/store + internal/api) applies the returned Decision
// transactionally. This is what makes replay possible: same (state, event) ->
// same Decision, always. archcheck forbids clock/rand/ULID/GitHub imports here.
package engine

import (
	"time"

	"github.com/samhotchkiss/flowbee/internal/content"
	"github.com/samhotchkiss/flowbee/internal/job"
	"github.com/samhotchkiss/flowbee/internal/ledger"
	"github.com/samhotchkiss/flowbee/internal/liveness"
)

// EngineState is an immutable snapshot folded from persisted facts ONLY. The core
// reads nothing else. M3 adds the reconciled Domain-B facts + policy the build-flow
// gate consumes — both passed IN as values (the engine never fetches them, I-9).
type EngineState struct {
	Job    job.Job
	Now    time.Time        // injected clock reading — passed IN, never read by the core
	Epoch  int              // the live lease epoch of the job (the fence in force)
	GitHub job.DomainBFacts // reconciled-IN facts (M3: from a stubbed FactSource)
	Policy job.Policy       // THE ONE DECISION (§14): AllowSelfMerge
	Spec   SpecState        // spec-flow inputs (M7): the CURRENT content hash + lenses
	// Content is the deterministic content-integrity Result (§9.2, I-11), computed
	// by the runtime from the stored patch + declared blast-radius and passed IN as
	// a value (the engine never runs the checks itself — it stays a pure function of
	// persisted facts). nil means the gate did not run: the SAFE default is "not
	// self_merge-eligible" (handoff forced). The §5.4 conditions 2–4 read it.
	Content *content.Result
}

// SpecState carries the spec-flow ground truth the gate reasons over (§11.5):
// the spec branch's CURRENT content hash (the authority), its version, and the
// author/reviewer lenses (the §5.5 distinct-lens term). Resolved by the runtime
// from the persisted spec job and passed IN — the engine never reads the bytes.
type SpecState struct {
	CurrentHash  string
	Version      int
	AuthorLens   string
	ReviewerLens string
}

// Event is the closed set of triggers the core understands (M1 subset).
type Event interface{ isEngineEvent() }

// Heartbeat is a fenced liveness ping. Epoch is the caller's claimed fence. The
// observations feed the lower, gameable rungs (§10.2) — explicitly HINTS — except
// the two locally-provable fast-paths (§10.6): agent_exited_zombie / awaiting_input,
// which yield a `cancel` directive on their face (a dead/blocked agent, not a kill).
type Heartbeat struct {
	Epoch int
	// Health is the Rung-0 worker-local supervisor enum (hint). HealthZombie /
	// AgentExited drive the agent_exited fast-path -> failed.
	Health liveness.AgentHealth
	// AwaitingInput is the §10.6 awaiting_input fast-path: the agent is blocked on
	// human/interactive input that will never come -> directive: cancel.
	AwaitingInput bool
	// AgentExited is the Rung-0 locally-provable exit (supervisor waitpid'd, agent PID
	// died) -> directive: cancel / state failed. The richest standing for a Rung-0
	// signal precisely because the worker proved it on its own machine.
	AgentExited bool
}

// LivenessVerdict is the runtime's folded rung snapshot for an ACTIVE-lease job,
// driven by a Rung-3 clock comparison (done by the runtime against Flowbee's clock)
// + the last Rung-2 sweep verdict + the last heartbeat's Rung-0/1 hints. The engine
// runs the PURE two-rung kill rule over it (I-13) and returns either a revoke
// transition (re-dispatch, or needs_human at the governor ceiling) or a no-op. The
// runtime resolves the retry-vs-exhausted arm via the governor ceiling, passed in.
type LivenessVerdict struct {
	Rungs liveness.RungSet
	// GovernorCeilingReached is the Rung-4 anti-thrash decision (§10.7), resolved by
	// the runtime from stall_revocations vs the ceiling: when true, a stall kill
	// routes to needs_human (held against thrash) rather than re-dispatching.
	GovernorCeilingReached bool
	// AttemptsExhausted is the §6.7 max_attempts decision for an absolute-cap kill,
	// resolved by the runtime: when true, the cap routes to needs_human.
	AttemptsExhausted bool
}

// WorkResult is a fenced work-product CLAIM (never a verdict, I-9). Epoch is the
// caller's claimed fence.
type WorkResult struct {
	Epoch int
}

// Release is a fenced voluntary release of the lease back to `ready`.
type Release struct{ Epoch int }

// ReviewClaim is a fenced code-review work-product CLAIM (I-9). The verdict Value
// and Disposition are the reviewer's *claim* — never authoritative. The engine
// runs the pure gate over the reconciled facts in EngineState and either mints a
// SHA-bound verdict (approved) or bounces (changes_requested / failed reconcile).
type ReviewClaim struct {
	Epoch       int
	Value       job.VerdictValue
	Disposition job.Disposition
}

// MergeDispatch advances a `mergeable` job onto its branch-point arm (§5.4),
// decided by the minted verdict's disposition under policy. Not a worker call —
// the runtime fires it after the gate mints an approval.
type MergeDispatch struct{}

// CostMeter is a fenced cost report (§6.7, I-15). The runtime has ALREADY folded
// the worker-reported {tokens_in, tokens_out, $} delta into the job's meter and
// passes the NEW accumulated micro-USD total in s.Job.CostMicroUSD; the engine
// runs the PURE ceiling predicate (job.CostExceeded) and either escalates the job
// to needs_human (revoke + `cancel` directive + the over-budget label rendering,
// I-15) or returns continue. The dollar arithmetic is exact integer micro-USD —
// never a float ceiling comparison. Like every other fenced event a stale epoch
// short-circuits to a 409 reject.
type CostMeter struct{ Epoch int }

// SpecReviewClaim is a fenced spec-review work-product CLAIM (§11, I-9). The
// reviewer's decision + the two sub-checks are a CLAIM only — never authoritative.
// The engine runs the pure spec gate (over the CURRENT content hash, the bytes are
// the ground truth, not a GitHub fact) and either mints a content-hash-bound
// sign-off or bounces/supersedes. The runtime resolves CurrentSpecHash + lenses
// from the persisted spec job and passes them in EngineState.Spec.
type SpecReviewClaim struct {
	Epoch             int
	Claim             job.VerdictValue
	ClaimBindsTo      string // the spec_content_hash the worker judged
	MeetsStyle        bool
	MeetsRequirements bool
}

func (Heartbeat) isEngineEvent()       {}
func (LivenessVerdict) isEngineEvent() {}
func (WorkResult) isEngineEvent()      {}
func (Release) isEngineEvent()         {}
func (ReviewClaim) isEngineEvent()     {}
func (MergeDispatch) isEngineEvent()   {}
func (SpecReviewClaim) isEngineEvent() {}
func (CostMeter) isEngineEvent()       {}

// Directive is the continue|cancel reply to a heartbeat.
type Directive string

const (
	DirectiveContinue Directive = "continue"
	DirectiveCancel   Directive = "cancel"
)

// RejectReason describes a rejected fenced call (mapped to HTTP 409).
type RejectReason struct{ Reason string }

// Transition is a state move plus the ledger event kind to append for it. The
// optional deltas/verdict ride along so the runtime applies the projection
// mutation atomically with the state move.
type Transition struct {
	From          job.State
	To            job.State
	Kind          ledger.EventKind
	BouncesDelta  int              // increment applied to the bounces counter
	AttemptsDelta int              // increment applied to the attempts counter
	Verdict       *job.Verdict     // the minted, tamper-evident sign-off (I-9); nil if none
	SpecSignoff   *job.SpecSignoff // the minted, content-hash-bound spec sign-off (§11.5); nil if none
	// M8 liveness deltas. StallRevocationsDelta increments the Rung-4 governor
	// counter on a stall kill; BumpEpoch tells the runtime this transition revokes
	// the lease (epoch++, the zombie's fence) and fires compensation. RevokeReason
	// records WHY (absolute_cap / two_rung_stall / awaiting_input / agent_exited).
	StallRevocationsDelta int
	BumpEpoch             bool
	RevokeReason          string
}

// Decision is DATA describing intent. The runtime applies it; the core never acts.
type Decision struct {
	Transitions []Transition
	// VerdictMint is the tamper-evident verdict the gate minted from reconciled
	// facts (I-9), if any. The runtime persists it into jobs.verdict. It is also
	// carried on the minting transition so a single apply covers both.
	VerdictMint *job.Verdict
	// SpecMint is the content-hash-bound spec sign-off the spec gate minted (§11.5),
	// if any. The runtime persists it into jobs.spec_signoff and triggers
	// materialize_issues (project-OUT renders the GitHub issue).
	SpecMint  *job.SpecSignoff
	Directive *Directive
	Reject    *RejectReason
}

// redispatchTarget is the state a revoked/cancelled active-lease job returns to for
// re-dispatch (§10.7): a code_review revoke returns to review_pending (the build
// product still stands); spec stages return to spec_authoring; every build stage
// (leased/building) returns to ready. Mirrors the release/expiry edges.
func redispatchTarget(from job.State) job.State {
	switch from {
	case job.StateCodeReview:
		return job.StateReviewPending
	case job.StateSpecAuthoring, job.StateSpecReview:
		return job.StateSpecAuthoring
	default:
		return job.StateReady
	}
}

// fenced checks the caller's epoch against the live fence. It returns a non-nil
// Decision (with Reject set) when the call is stale.
func staleEpoch(claimed, live int) *RejectReason {
	if claimed != live {
		return &RejectReason{Reason: "stale lease epoch"}
	}
	return nil
}

// Decide is THE deterministic function. Pure: same (state, event) -> same
// Decision, always.
func Decide(s EngineState, e Event) Decision {
	switch ev := e.(type) {

	case Heartbeat:
		if r := staleEpoch(ev.Epoch, s.Epoch); r != nil {
			return Decision{Reject: r}
		}
		// Only live, active-lease jobs may heartbeat.
		if !job.HasActiveLease(s.Job.State) {
			return Decision{Reject: &RejectReason{Reason: "job not in an active lease state"}}
		}
		// §10.6 two free fast-paths, evaluated BEFORE the ladder (conclusive on their
		// face — not "kills"). agent_exited_zombie -> failed (the agent is already
		// dead, locally proven); awaiting_input -> cancel + clean re-dispatch.
		switch liveness.EvaluateFastPath(ev.Health, ev.AwaitingInput, ev.AgentExited) {
		case liveness.FastPathFailed:
			d := DirectiveCancel
			return Decision{
				Directive: &d,
				Transitions: []Transition{{
					From: s.Job.State, To: job.StateFailed, Kind: ledger.KindAgentExited,
					BumpEpoch: true, RevokeReason: "agent_exited",
				}},
			}
		case liveness.FastPathCancel:
			to := redispatchTarget(s.Job.State)
			d := DirectiveCancel
			return Decision{
				Directive: &d,
				Transitions: []Transition{{
					From: s.Job.State, To: to, Kind: ledger.KindFastCancelled,
					AttemptsDelta: 1, BumpEpoch: true, RevokeReason: "awaiting_input",
				}},
			}
		}
		// §6.7 / I-15: a job already over its $ ceiling (escalated, or mid-escalation)
		// must be told to STOP — a `cancel` directive on the next heartbeat is how the
		// live worker learns its lease was pulled for cost. The CostMeter event does the
		// actual escalation transition; this is the standing directive for an over-budget
		// job that is still mechanically in an active state.
		if s.Job.OverBudget || job.CostExceeded(s.Job.CostMicroUSD, s.Job.CostCeilingMicroUSD) {
			d := DirectiveCancel
			return Decision{Directive: &d}
		}
		d := DirectiveContinue
		return Decision{Directive: &d}

	case CostMeter:
		if r := staleEpoch(ev.Epoch, s.Epoch); r != nil {
			return Decision{Reject: r}
		}
		// Only an active-lease job accrues cost against a live lease. A report on a
		// non-active job is a protocol error (nothing to escalate / cancel).
		if !job.HasActiveLease(s.Job.State) {
			return Decision{Reject: &RejectReason{Reason: "job not in an active lease state"}}
		}
		// I-15: run the PURE ceiling predicate over the NEW accumulated meter (the
		// runtime already folded the delta in). Under the ceiling -> continue (the
		// meter still rolled up for the per-flow report). At/over the ceiling ->
		// escalate to needs_human + a `cancel` directive + mark over_budget so
		// project-OUT stamps flowbee:over-budget. The escalation is itself a lease
		// revocation: bump the epoch (fence the worker) and record the cost reason.
		if !job.CostExceeded(s.Job.CostMicroUSD, s.Job.CostCeilingMicroUSD) {
			d := DirectiveContinue
			return Decision{Directive: &d}
		}
		d := DirectiveCancel
		return Decision{
			Directive: &d,
			Transitions: []Transition{{
				From: s.Job.State, To: job.StateNeedsHuman, Kind: ledger.KindCostEscalated,
				BumpEpoch: true, RevokeReason: string(job.EscalationCost),
			}},
		}

	case LivenessVerdict:
		// only an active-lease job is subject to the ladder. A non-active job has
		// nothing to revoke.
		if !job.HasActiveLease(s.Job.State) {
			return Decision{Reject: &RejectReason{Reason: "job not in an active lease state"}}
		}
		kd := liveness.EvaluateKill(ev.Rungs)
		if !kd.Kill {
			// no two-rung agreement (or Rung-2 abstains, or the breaker tripped): the
			// job survives. §10.4 bias — a stalled job running a little long is the
			// cheaper error than killing healthy work.
			return Decision{}
		}
		// A kill IS a lease revocation (§10.3): bump the epoch (fence the zombie) and
		// fire compensation. The absolute cap is unilateral; a two-rung stall kill is
		// governed by Rung-4 anti-thrash.
		reason := "two_rung_stall"
		if kd.Unilateral {
			reason = "absolute_cap"
		}
		// Rung-4 governor (§10.7): a repeatedly killed-and-resumed job is held in
		// needs_human rather than re-armed. The absolute cap also escalates when
		// attempts are exhausted (§6.7). The runtime resolves the ceiling/attempts
		// booleans; the engine picks the arm.
		escalate := ev.GovernorCeilingReached
		if kd.Unilateral {
			escalate = ev.AttemptsExhausted || ev.GovernorCeilingReached
		}
		if escalate {
			return Decision{Transitions: []Transition{{
				From: s.Job.State, To: job.StateNeedsHuman, Kind: ledger.KindStallEscalated,
				StallRevocationsDelta: 1, BumpEpoch: true, RevokeReason: reason,
			}}}
		}
		return Decision{Transitions: []Transition{{
			From: s.Job.State, To: redispatchTarget(s.Job.State), Kind: ledger.KindLeaseRevoked,
			AttemptsDelta: 1, StallRevocationsDelta: 1, BumpEpoch: true, RevokeReason: reason,
		}}}

	case WorkResult:
		if r := staleEpoch(ev.Epoch, s.Epoch); r != nil {
			return Decision{Reject: r}
		}
		// A build result lands review_pending. The worker may post a result from
		// either `leased` (skipped explicit start) or `building`; normalize to a
		// building->review_pending transition, emitting work_started first if the
		// job is still `leased`.
		var ts []Transition
		from := s.Job.State
		if from == job.StateLeased {
			ts = append(ts, Transition{From: job.StateLeased, To: job.StateBuilding, Kind: ledger.KindWorkerStarted})
			from = job.StateBuilding
		}
		if from != job.StateBuilding {
			return Decision{Reject: &RejectReason{Reason: "job not in a state that accepts a result"}}
		}
		ts = append(ts, Transition{From: job.StateBuilding, To: job.StateReviewPending, Kind: ledger.KindResultAccepted})
		return Decision{Transitions: ts}

	case Release:
		if r := staleEpoch(ev.Epoch, s.Epoch); r != nil {
			return Decision{Reject: r}
		}
		if !job.HasActiveLease(s.Job.State) {
			return Decision{Reject: &RejectReason{Reason: "job not in an active lease state"}}
		}
		// a code_review lease releases back to review_pending (the build product
		// stands); other active states release back to ready.
		to := job.StateReady
		if s.Job.State == job.StateCodeReview {
			to = job.StateReviewPending
		}
		return Decision{Transitions: []Transition{
			{From: s.Job.State, To: to, Kind: ledger.KindLeaseReleased, AttemptsDelta: 1},
		}}

	case ReviewClaim:
		if r := staleEpoch(ev.Epoch, s.Epoch); r != nil {
			return Decision{Reject: r}
		}
		// the reviewer must be on the gate stage. A claim on any other state is a
		// protocol error, not a verdict.
		if s.Job.State != job.StateCodeReview {
			return Decision{Reject: &RejectReason{Reason: "job not in code_review"}}
		}
		// I-9: run the PURE gate over the RECONCILED facts (s.GitHub), NOT the
		// claim. The claim's value/disposition are inputs to the gate, never the
		// authority — a hostile `approved` over red facts bounces, never approves.
		out := job.EvaluateGate(job.GateInputs{
			Claim:     ev.Value,
			Disp:      ev.Disposition,
			Facts:     s.GitHub,
			Bounces:   s.Job.Bounces,
			MaxBounce: s.Job.MaxBounces,
			Policy:    s.Policy,
			Content:   s.Content, // §9.2 / I-11: forces handoff if the diff is not clear
		})
		switch out.Trigger {
		case job.TriggerApproved:
			return Decision{
				VerdictMint: out.Verdict,
				Transitions: []Transition{{
					From: job.StateCodeReview, To: job.StateMergeable,
					Kind: ledger.KindVerdictMinted, Verdict: out.Verdict,
				}},
			}
		case job.TriggerBounce:
			return Decision{Transitions: []Transition{{
				From: job.StateCodeReview, To: job.StateReady,
				Kind: ledger.KindReviewBounced, BouncesDelta: 1,
			}}}
		case job.TriggerBounceExhausted:
			return Decision{Transitions: []Transition{{
				From: job.StateCodeReview, To: job.StateNeedsHuman,
				Kind: ledger.KindBounceExhausted, BouncesDelta: 1,
			}}}
		default:
			return Decision{Reject: &RejectReason{Reason: "gate produced no transition"}}
		}

	case SpecReviewClaim:
		if r := staleEpoch(ev.Epoch, s.Epoch); r != nil {
			return Decision{Reject: r}
		}
		if s.Job.State != job.StateSpecReview {
			return Decision{Reject: &RejectReason{Reason: "job not in spec_review"}}
		}
		// I-9 / §11.5: run the PURE spec gate over the CURRENT content hash (the
		// bytes, the only ground truth pre-SHA) — NOT the claim. A hostile
		// `signed_off` over a stale binding or a failed conjunction never mints.
		out := job.EvaluateSpecGate(job.SpecGateInputs{
			Claim:             ev.Claim,
			ClaimBindsTo:      ev.ClaimBindsTo,
			MeetsStyle:        ev.MeetsStyle,
			MeetsRequirements: ev.MeetsRequirements,
			CurrentSpecHash:   s.Spec.CurrentHash,
			SpecVersion:       s.Spec.Version,
			ReviewerLens:      s.Spec.ReviewerLens,
			AuthorLens:        s.Spec.AuthorLens,
			Bounces:           s.Job.Bounces,
			MaxBounce:         s.Job.MaxBounces,
		})
		switch out.Trigger {
		case job.TriggerSpecSignedOff:
			return Decision{
				SpecMint: out.Signoff,
				Transitions: []Transition{{
					From: job.StateSpecReview, To: job.StateDone,
					Kind: ledger.KindSpecSignoffMinted, SpecSignoff: out.Signoff,
				}},
			}
		case job.TriggerSpecSuperseded:
			// the spec advanced mid-review: reject as superseded, re-arm the gate
			// (back to spec_authoring to re-review the new bytes).
			return Decision{Transitions: []Transition{{
				From: job.StateSpecReview, To: job.StateSpecAuthoring,
				Kind: ledger.KindSpecSuperseded,
			}}}
		case job.TriggerBounce:
			return Decision{Transitions: []Transition{{
				From: job.StateSpecReview, To: job.StateSpecAuthoring,
				Kind: ledger.KindSpecBounced, BouncesDelta: 1,
			}}}
		case job.TriggerBounceExhausted:
			return Decision{Transitions: []Transition{{
				From: job.StateSpecReview, To: job.StateNeedsHuman,
				Kind: ledger.KindBounceExhausted, BouncesDelta: 1,
			}}}
		default:
			return Decision{Reject: &RejectReason{Reason: "spec gate produced no transition"}}
		}

	case MergeDispatch:
		// the branch point (§5.4): a `mergeable` job moves onto its arm, decided
		// by the minted verdict's disposition under policy. self_merge is only
		// reachable when the policy enabled it AND the verdict carried it.
		if s.Job.State != job.StateMergeable {
			return Decision{Reject: &RejectReason{Reason: "job not mergeable"}}
		}
		// the branch arm is decided by the SINGLE canonical §5.4 predicate over the
		// MINTED verdict + reconciled facts + the content-integrity Result + policy.
		// self_merge is reachable only when the verdict carried the self_merge
		// disposition (the gate already enforced content/policy at mint time) AND the
		// predicate STILL holds (re-checks content + the SHA binding here — condition
		// 5: a SHA move since the mint supersedes and falls back to handoff, I-5).
		if s.Job.Verdict != nil && s.Job.Verdict.Disposition == job.DispositionSelfMerge &&
			job.SelfMergeEligible(*s.Job.Verdict, s.GitHub, s.Content, s.Policy) {
			return Decision{Transitions: []Transition{{
				From: job.StateMergeable, To: job.StateMerging, Kind: ledger.KindMergeStarted,
			}}}
		}
		return Decision{Transitions: []Transition{{
			From: job.StateMergeable, To: job.StateMergeHandoff, Kind: ledger.KindMergeHandoff,
		}}}
	}

	return Decision{Reject: &RejectReason{Reason: "unknown event"}}
}
