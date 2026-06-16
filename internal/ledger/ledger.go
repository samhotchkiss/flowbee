// Package ledger is the event-sourced spine (DESIGN §6, EVENT-SOURCED). It is a
// deterministic-core package (§1.2): no clock, no randomness, no ID minter, no
// GitHub/LLM imports. Append (the I/O write) lives in internal/store; this
// package defines the Event record and the PURE Fold: Fold(events) == jobs row.
//
// Events carry RESOLVED facts (a clock-derived deadline is recorded in the
// lease_claimed event, never recomputed at fold time), so Fold reads no clock.
package ledger

import (
	"time"

	"github.com/samhotchkiss/flowbee/internal/job"
)

// EventKind enumerates the ledger event kinds (M1 subset).
type EventKind string

const (
	KindJobCreated     EventKind = "job_created"
	KindLeaseClaimed   EventKind = "lease_claimed"
	KindWorkerStarted  EventKind = "worker_started"
	KindHeartbeat      EventKind = "heartbeat"
	KindResultAccepted EventKind = "result_accepted"
	KindLeaseReleased  EventKind = "lease_released"
	KindStateChanged   EventKind = "state_changed"
	// M2 scheduler/DAG event kinds.
	KindDepsCleared      EventKind = "deps_cleared"       // blocked -> ready
	KindJobCompleted     EventKind = "job_completed"      // review_pending -> done
	KindNoEligibleWorker EventKind = "no_eligible_worker" // scheduler alarm fired (I-6)
	// M3 flow/gate event kinds.
	KindReviewClaimed   EventKind = "review_claimed"   // review_pending -> code_review
	KindVerdictClaim    EventKind = "verdict_claim"    // the reviewer's CLAIM (untrusted, I-9)
	KindVerdictMinted   EventKind = "verdict_minted"   // the gate MINTED a tamper-evident verdict (I-9)
	KindReviewBounced   EventKind = "review_bounced"   // code_review -> building (changes_requested)
	KindBounceExhausted EventKind = "bounce_exhausted" // code_review -> needs_human (max_bounces)
	KindMergeHandoff    EventKind = "merge_handoff"    // mergeable -> merge_handoff
	KindMergeStarted    EventKind = "merge_started"    // mergeable -> merging (self_merge)
	// M6 reconcile-IN event kinds (Domain-B-driven transitions; actor='reconcile').
	KindFactsReconciled EventKind = "facts_reconciled" // a sweep/refetch wrote Domain-B facts (audit)
	KindSuperseded      EventKind = "superseded"       // SHA move re-armed the job to ready (I-5, §6.2.4)
)

// Event is one appended ledger row. Payload holds kind-specific RESOLVED facts as
// already-decoded values (not raw JSON) so Fold stays pure and total.
type Event struct {
	JobID      string
	JobSeq     int // per-job ordinal (1,2,3,…)
	Kind       EventKind
	FromState  job.State
	ToState    job.State
	LeaseEpoch int
	Actor      string
	CreatedAt  time.Time // resolved at append time; recorded, never recomputed
	Payload    Payload
}

// Payload carries the resolved, kind-specific facts a fold needs. Only the fields
// relevant to an event's kind are set; the rest are zero.
type Payload struct {
	// job_created
	Kind                 job.Kind
	Flow                 string
	Stage                string
	Role                 job.Role
	BaseSHA              string
	Priority             int
	BlockedBy            []string `json:",omitempty"`
	RequiredCapabilities []string `json:",omitempty"`
	// CreatedReady records whether the job entered the ledger already `ready`
	// (no unmet deps) vs `blocked`. Set on job_created; the fold reads ToState.

	// lease_claimed
	LeaseID          string
	BoundIdentity    string
	BoundModelFamily string

	// counter deltas (lease_released / state_changed / review_bounced)
	AttemptsDelta int
	BouncesDelta  int

	// gate (M3): the reviewer's claim (untrusted) and the minted verdict (I-9).
	VerdictClaim job.VerdictValue `json:",omitempty"`
	Disposition  job.Disposition  `json:",omitempty"`
	Verdict      *job.Verdict     `json:",omitempty"`
}

// Fold replays events into the jobs projection. PURE: no clock, no RNG, no I/O.
// Fold(events) == the jobs row the store maintains incrementally.
func Fold(events []Event) (job.Job, error) {
	var j job.Job
	for _, e := range events {
		switch e.Kind {
		case KindJobCreated:
			j.ID = e.JobID
			j.Kind = e.Payload.Kind
			j.Flow = e.Payload.Flow
			j.Stage = e.Payload.Stage
			j.Role = e.Payload.Role
			j.BaseSHA = e.Payload.BaseSHA
			j.Priority = e.Payload.Priority
			j.BlockedBy = e.Payload.BlockedBy
			j.RequiredCapabilities = e.Payload.RequiredCapabilities
			j.State = e.ToState
			// EnqueuedAt: a job created already-`ready` is enqueued now (aging
			// clock starts here); a `blocked` job starts aging when deps clear.
			if e.ToState == job.StateReady {
				j.EnqueuedAt = e.CreatedAt
			}
			// M1 default counters mirror the migration defaults.
			j.MaxAttempts = 5
			j.MaxBounces = 3
		case KindLeaseClaimed:
			j.State = e.ToState
			j.LeaseEpoch = e.LeaseEpoch
			j.LeaseID = e.Payload.LeaseID
			j.BoundIdentity = e.Payload.BoundIdentity
			j.BoundModelFamily = e.Payload.BoundModelFamily
		case KindWorkerStarted:
			j.State = e.ToState
		case KindHeartbeat:
			// liveness only; no projection field changes in M1.
		case KindResultAccepted:
			j.State = e.ToState
			// the lease is released on result: clear live lease columns, keep epoch.
			j.LeaseID = ""
			j.BoundIdentity = ""
			j.BoundModelFamily = ""
		case KindLeaseReleased:
			j.State = e.ToState
			j.Attempts += e.Payload.AttemptsDelta
			j.LeaseID = ""
			j.BoundIdentity = ""
			j.BoundModelFamily = ""
		case KindStateChanged:
			j.State = e.ToState
		case KindDepsCleared:
			// blocked -> ready: aging clock starts now (the job becomes leasable).
			j.State = e.ToState
			j.EnqueuedAt = e.CreatedAt
			j.BlockedBy = nil
		case KindJobCompleted:
			j.State = e.ToState
		case KindNoEligibleWorker:
			// the alarm is an observability event; no projection field changes
			// (the job stays `ready`). Recorded for replay/audit completeness.
		case KindReviewClaimed:
			// review_pending -> code_review: a reviewer leased the gate stage.
			j.State = e.ToState
			j.LeaseEpoch = e.LeaseEpoch
			j.LeaseID = e.Payload.LeaseID
			j.BoundIdentity = e.Payload.BoundIdentity
			j.BoundModelFamily = e.Payload.BoundModelFamily
		case KindVerdictClaim:
			// the untrusted claim is recorded for audit; it changes no projection
			// field (a worker can never write a verdict, I-9).
		case KindVerdictMinted:
			// the gate minted a tamper-evident verdict from reconciled facts. It
			// advances state and stamps jobs.verdict + binds the SHA pair.
			j.State = e.ToState
			j.Verdict = e.Payload.Verdict
			if e.Payload.Verdict != nil {
				j.HeadSHA = e.Payload.Verdict.HeadSHA
				if e.Payload.Verdict.BaseSHA != "" {
					j.BaseSHA = e.Payload.Verdict.BaseSHA
				}
			}
			// the gate stage's lease is released on the verdict.
			j.LeaseID = ""
			j.BoundIdentity = ""
			j.BoundModelFamily = ""
		case KindReviewBounced:
			// code_review -> ready: bounce, increment bounces, release the gate
			// lease, re-arm the build stage for a fresh eng_worker lease.
			j.State = e.ToState
			j.Bounces += e.Payload.BouncesDelta
			j.Role = job.RoleEngWorker
			j.EnqueuedAt = e.CreatedAt
			j.LeaseID = ""
			j.BoundIdentity = ""
			j.BoundModelFamily = ""
		case KindBounceExhausted:
			// code_review -> needs_human: max_bounces reached.
			j.State = e.ToState
			j.Bounces += e.Payload.BouncesDelta
			j.LeaseID = ""
			j.BoundIdentity = ""
			j.BoundModelFamily = ""
		case KindMergeHandoff:
			j.State = e.ToState
		case KindMergeStarted:
			j.State = e.ToState
		case KindFactsReconciled:
			// reconcile-IN wrote Domain-B facts; no Domain-A projection field changes
			// here (the facts live in domain_b_facts, not the jobs row). Recorded for
			// replay/audit completeness; a merged->done or supersede emits its own event.
		case KindSuperseded:
			// I-5 / §6.2.4: a head/base SHA move re-armed the job. The verdict is
			// invalidated, the lease revoked (epoch bumped on the event), the job
			// routed back to ready as an eng_worker against the new base.
			j.State = e.ToState
			j.Role = job.RoleEngWorker
			j.Stage = "build"
			j.RequiredCapabilities = []string{"role:eng_worker"}
			j.LeaseEpoch = e.LeaseEpoch
			j.Verdict = nil
			j.HeadSHA = ""
			if e.Payload.BaseSHA != "" {
				j.BaseSHA = e.Payload.BaseSHA
			}
			j.EnqueuedAt = e.CreatedAt
			j.LeaseID = ""
			j.BoundIdentity = ""
			j.BoundModelFamily = ""
		}
		j.JobSeq = e.JobSeq
	}
	return j, nil
}
