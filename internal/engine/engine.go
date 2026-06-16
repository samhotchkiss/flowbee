// Package engine is the deterministic CORE (DESIGN §1.2, §3.4): a pure function
// Decide(state, event) -> Decision. It performs zero I/O, reads no clock,
// generates no IDs — everything is injected via EngineState/Event values. An
// outer runtime (internal/store + internal/api) applies the returned Decision
// transactionally. This is what makes replay possible: same (state, event) ->
// same Decision, always. archcheck forbids clock/rand/ULID/GitHub imports here.
package engine

import (
	"time"

	"github.com/samhotchkiss/flowbee/internal/job"
	"github.com/samhotchkiss/flowbee/internal/ledger"
)

// EngineState is an immutable snapshot folded from persisted facts ONLY (M1
// subset). The core reads nothing else.
type EngineState struct {
	Job   job.Job
	Now   time.Time // injected clock reading — passed IN, never read by the core
	Epoch int       // the live lease epoch of the job (the fence in force)
}

// Event is the closed set of triggers the core understands (M1 subset).
type Event interface{ isEngineEvent() }

// Heartbeat is a fenced liveness ping. Epoch is the caller's claimed fence.
type Heartbeat struct{ Epoch int }

// WorkResult is a fenced work-product CLAIM (never a verdict, I-9). Epoch is the
// caller's claimed fence.
type WorkResult struct {
	Epoch int
}

// Release is a fenced voluntary release of the lease back to `ready`.
type Release struct{ Epoch int }

func (Heartbeat) isEngineEvent()  {}
func (WorkResult) isEngineEvent() {}
func (Release) isEngineEvent()    {}

// Directive is the continue|cancel reply to a heartbeat.
type Directive string

const (
	DirectiveContinue Directive = "continue"
	DirectiveCancel   Directive = "cancel"
)

// RejectReason describes a rejected fenced call (mapped to HTTP 409).
type RejectReason struct{ Reason string }

// Transition is a state move plus the ledger event kind to append for it.
type Transition struct {
	From job.State
	To   job.State
	Kind ledger.EventKind
}

// Decision is DATA describing intent. The runtime applies it; the core never acts.
type Decision struct {
	Transitions []Transition
	Directive   *Directive
	Reject      *RejectReason
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
		d := DirectiveContinue
		return Decision{Directive: &d}

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
		return Decision{Transitions: []Transition{
			{From: s.Job.State, To: job.StateReady, Kind: ledger.KindLeaseReleased},
		}}
	}

	return Decision{Reject: &RejectReason{Reason: "unknown event"}}
}
