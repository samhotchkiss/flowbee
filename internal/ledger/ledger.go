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
	// M7 spec-flow + project-OUT event kinds.
	KindSpecAuthored      EventKind = "spec_authored"       // Flowbee committed spec.md, opened spec_review (§11.6)
	KindSpecClaim         EventKind = "spec_claim"          // the spec reviewer's CLAIM (untrusted, I-9)
	KindSpecSignoffMinted EventKind = "spec_signoff_minted" // the gate MINTED a content-hash-bound spec sign-off (§11.5)
	KindSpecBounced       EventKind = "spec_bounced"        // spec_review -> spec_authoring (changes_requested)
	KindSpecSuperseded    EventKind = "spec_superseded"     // a spec edit voided the sign-off; gate re-armed (§11.5)
	KindIssueMaterialized EventKind = "issue_materialized"  // a signed-off spec materialized a GitHub issue (§11)
	// F4 issue-review amend-in-place + design fork + epic-level barrier.
	KindSpecAmended     EventKind = "spec_amended"      // issue-review committed an amended spec + minted a sign-off (no author bounce)
	KindSpecNeedsDesign EventKind = "spec_needs_design" // issue-review flagged a design fork -> needs_design
	KindDesignResolved  EventKind = "design_resolved"   // a human supplied the design decision; needs_design -> spec_review re-armed
	KindEpicReviewed    EventKind = "epic_reviewed"     // the epic-level issue-review barrier passed; the epic's issues fan out
	KindPROpened        EventKind = "pr_opened"         // Flowbee opened the PR and stamped # (§7.3, §8.2.1)
	KindAdopted         EventKind = "adopted"           // a pre-existing issue/PR imported quiescent (I-16)
	// M8 liveness event kinds (§10.7). A stall kill / absolute-cap revoke / fast-path
	// each emits its own audit event; the lease_revoked event carries the bumped
	// epoch (the zombie's fence) and the governor counter delta.
	KindLeaseRevoked   EventKind = "lease_revoked"   // two-rung kill OR absolute cap: epoch++, re-dispatch
	KindStallEscalated EventKind = "stall_escalated" // Rung-4 governor ceiling: active -> needs_human
	KindAgentExited    EventKind = "agent_exited"    // agent_exited_zombie fast-path -> failed (§10.6)
	KindFastCancelled  EventKind = "fast_cancelled"  // awaiting_input fast-path -> ready (§10.6)
	// M10 cost metering + ceiling event kinds (§6.7, I-15). A metered cost report is
	// recorded for audit + the per-flow rollup; an escalation revokes the lease
	// (epoch++, the worker's fence) and routes the job to needs_human + over-budget.
	KindCostMetered   EventKind = "cost_metered"   // a {tokens_in,tokens_out,$} report folded into the meter
	KindCostEscalated EventKind = "cost_escalated" // cost ceiling crossed: active -> needs_human (I-15)
	// M11 epoch-namespaced side-effects + compensation event kinds (§3.5/§6.5, I-12).
	KindEpochPromoted    EventKind = "epoch_promoted"    // Flowbee validated the live epoch + fast-forwarded its ref (§6.5.1)
	KindCompensated      EventKind = "compensated"       // compensate(job, dead_epoch): dropped ref, cancelled CI, draft-back (§6.5.4)
	KindUnattendedMerged EventKind = "unattended_merged" // self_merge: Flowbee enqueued+reconciled the merge with no human (§14 Branch B)
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
	// F1 task/context (set on job_created): the human intent folded onto the job.
	TaskText           string `json:",omitempty"`
	SpecText           string `json:",omitempty"`
	AcceptanceCriteria string `json:",omitempty"`
	// CreatedReady records whether the job entered the ledger already `ready`
	// (no unmet deps) vs `blocked`. Set on job_created; the fold reads ToState.

	// lease_claimed
	LeaseID          string
	BoundIdentity    string
	BoundModelFamily string

	// counter deltas (lease_released / state_changed / review_bounced)
	AttemptsDelta int
	BouncesDelta  int
	// stall_revocations governor counter delta (M8, §10.7); set on lease_revoked /
	// stall_escalated. Distinct from attempts/bounces.
	StallRevocationsDelta int `json:",omitempty"`
	// RevokeReason records WHY a lease was revoked (M8): "absolute_cap" |
	// "two_rung_stall" | "awaiting_input" | "agent_exited", for replay/audit.
	RevokeReason string `json:",omitempty"`

	// gate (M3): the reviewer's claim (untrusted) and the minted verdict (I-9).
	VerdictClaim job.VerdictValue `json:",omitempty"`
	Disposition  job.Disposition  `json:",omitempty"`
	Verdict      *job.Verdict     `json:",omitempty"`

	// spec flow (M7): the spec content hash + version + the minted sign-off (§11).
	SpecContentHash string           `json:",omitempty"`
	SpecVersion     int              `json:",omitempty"`
	SpecSignoff     *job.SpecSignoff `json:",omitempty"`
	IssueNumber     int              `json:",omitempty"`
	PRNumber        int              `json:",omitempty"`

	// cost meter (M10, §6.7, I-15). The DELTA reported on a metered event (folded
	// into the running meter at fold time) + the resulting accumulated totals.
	CostTokensInDelta  int64 `json:",omitempty"`
	CostTokensOutDelta int64 `json:",omitempty"`
	CostMicroUSDDelta  int64 `json:",omitempty"`
	// EscalationReason records WHY a cost_escalated event routed to needs_human
	// (always "cost" for I-15); recorded for the §12.6.1 chokepoint replay.
	EscalationReason string `json:",omitempty"`

	// M11 epoch-namespaced side-effects + compensation (§3.5/§6.5, I-12).
	// BuildEpoch is the epoch whose ref was promoted (epoch_promoted). DeadEpoch is
	// the orphaned epoch a compensation acted on (compensated). MergeProvenance is the
	// reconciled merge-commit SHA recorded on an unattended self_merge (Branch B).
	BuildEpoch      int    `json:",omitempty"`
	DeadEpoch       int    `json:",omitempty"`
	MergeProvenance string `json:",omitempty"`
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
			j.TaskText = e.Payload.TaskText
			j.SpecText = e.Payload.SpecText
			j.AcceptanceCriteria = e.Payload.AcceptanceCriteria
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

		case KindSpecAuthored:
			// Flowbee committed spec.md and opened the spec_review gate: record the
			// content hash + version it bound to. The lease is released (the author
			// stage handed off the draft).
			j.State = e.ToState
			j.SpecContentHash = e.Payload.SpecContentHash
			j.SpecVersion = e.Payload.SpecVersion
			j.LeaseID = ""
			j.BoundIdentity = ""
			j.BoundModelFamily = ""
		case KindSpecClaim:
			// the untrusted spec-review claim is recorded for audit; it changes no
			// projection field (a worker can never write a sign-off, I-9).
		case KindSpecSignoffMinted:
			// the gate minted a content-hash-bound spec sign-off. The spec job is
			// done; the sign-off is stamped + the build gate it authorizes opens.
			j.State = e.ToState
			j.SpecSignoff = e.Payload.SpecSignoff
			j.LeaseID = ""
			j.BoundIdentity = ""
			j.BoundModelFamily = ""
		case KindSpecBounced:
			// spec_review -> spec_authoring: changes_requested. Increment bounces,
			// release the gate lease, re-arm the author stage.
			j.State = e.ToState
			j.Bounces += e.Payload.BouncesDelta
			j.LeaseID = ""
			j.BoundIdentity = ""
			j.BoundModelFamily = ""
		case KindSpecSuperseded:
			// a spec edit landed mid-review: the prior sign-off (if any) is void; the
			// gate re-arms against the new bytes. The new hash rides the event.
			j.State = e.ToState
			j.SpecSignoff = nil
			if e.Payload.SpecContentHash != "" {
				j.SpecContentHash = e.Payload.SpecContentHash
				j.SpecVersion = e.Payload.SpecVersion
			}
			j.LeaseID = ""
			j.BoundIdentity = ""
			j.BoundModelFamily = ""
		case KindSpecAmended:
			// F4: issue-review AMENDED the spec in place (committed amended bytes) and
			// the gate minted a sign-off bound to the AMENDED hash. The spec advanced in
			// place (new hash/version), the sign-off is stamped, the job is done. NEVER an
			// author bounce.
			j.State = e.ToState
			if e.Payload.SpecContentHash != "" {
				j.SpecContentHash = e.Payload.SpecContentHash
				j.SpecVersion = e.Payload.SpecVersion
			}
			j.SpecSignoff = e.Payload.SpecSignoff
			j.LeaseID = ""
			j.BoundIdentity = ""
			j.BoundModelFamily = ""
		case KindSpecNeedsDesign:
			// F4: issue-review flagged a design fork. The job parks in needs_design
			// (surfaced on /v1/needs-input), the gate lease is released. No sign-off.
			j.State = e.ToState
			j.EscalationReason = string(job.EscalationDesign)
			j.LeaseID = ""
			j.BoundIdentity = ""
			j.BoundModelFamily = ""
		case KindDesignResolved:
			// F4: a human supplied the design decision. The job re-arms to spec_review
			// (a fresh review judges the now-resolved spec). The escalation reason clears.
			j.State = e.ToState
			j.EscalationReason = ""
			if e.Payload.SpecContentHash != "" {
				j.SpecContentHash = e.Payload.SpecContentHash
				j.SpecVersion = e.Payload.SpecVersion
			}
		case KindEpicReviewed:
			// F4: the epic-level issue-review barrier passed. Recorded on the epic job;
			// the runtime fans out the epic's issues (no per-job projection change here).
		case KindIssueMaterialized:
			// project-OUT created the GitHub issue; Flowbee stamped the number.
			j.IssueNum = e.Payload.IssueNumber
		case KindPROpened:
			// Flowbee opened the PR and stamped the number (§7.3). The worker never
			// supplies a PR field; Domain B owns existence.
			j.PRNumber = e.Payload.PRNumber
		case KindLeaseRevoked:
			// M8 (§10.7): a two-rung kill or absolute-cap revoke. The epoch was bumped
			// (the zombie's fence rides the event), the live lease cleared, the
			// governor counter incremented, the build re-armed (-> ready /
			// review_pending). attempts++ on a re-dispatch.
			j.State = e.ToState
			j.LeaseEpoch = e.LeaseEpoch
			j.Attempts += e.Payload.AttemptsDelta
			j.StallRevocations += e.Payload.StallRevocationsDelta
			j.LeaseID = ""
			j.BoundIdentity = ""
			j.BoundModelFamily = ""
			if e.ToState == job.StateReady && e.Payload.Role != "" {
				j.Role = e.Payload.Role
			}
		case KindStallEscalated:
			// M8 (§10.7): the Rung-4 governor ceiling routed the job to needs_human
			// (anti-thrash) rather than re-dispatching forever. Epoch bumped, counter
			// incremented, lease cleared.
			j.State = e.ToState
			j.LeaseEpoch = e.LeaseEpoch
			j.StallRevocations += e.Payload.StallRevocationsDelta
			j.LeaseID = ""
			j.BoundIdentity = ""
			j.BoundModelFamily = ""
		case KindAgentExited:
			// M8 (§10.6): the agent_exited_zombie fast-path -> failed. Epoch bumped
			// (the dead agent's zombie successor is fenced), lease cleared.
			j.State = e.ToState
			j.LeaseEpoch = e.LeaseEpoch
			j.LeaseID = ""
			j.BoundIdentity = ""
			j.BoundModelFamily = ""
		case KindFastCancelled:
			// M8 (§10.6): the awaiting_input fast-path -> ready (clean cancel). Epoch
			// bumped, lease cleared, build re-armed.
			j.State = e.ToState
			j.LeaseEpoch = e.LeaseEpoch
			j.Attempts += e.Payload.AttemptsDelta
			j.LeaseID = ""
			j.BoundIdentity = ""
			j.BoundModelFamily = ""
		case KindCostMetered:
			// M10 (§6.7, I-15): a {tokens_in, tokens_out, $} report folded into the
			// meter. Pure accumulation — no state change, no clock. The per-flow rollup
			// reads these totals; the ceiling predicate compares CostMicroUSD.
			j.CostTokensIn += e.Payload.CostTokensInDelta
			j.CostTokensOut += e.Payload.CostTokensOutDelta
			j.CostMicroUSD += e.Payload.CostMicroUSDDelta
		case KindCostEscalated:
			// M10 (§6.7, I-15): the $ ceiling was crossed. The lease is revoked (epoch
			// bumped, the worker's fence rides the event), the job routed to needs_human,
			// over_budget marked, and the escalation reason recorded so the §12.6.1
			// chokepoint shows the cost trigger. The final metered delta (if any) is
			// folded too so the meter reflects the report that tripped the ceiling.
			j.CostTokensIn += e.Payload.CostTokensInDelta
			j.CostTokensOut += e.Payload.CostTokensOutDelta
			j.CostMicroUSD += e.Payload.CostMicroUSDDelta
			j.State = e.ToState
			j.LeaseEpoch = e.LeaseEpoch
			j.OverBudget = true
			j.EscalationReason = e.Payload.EscalationReason
			j.LeaseID = ""
			j.BoundIdentity = ""
			j.BoundModelFamily = ""
		case KindEpochPromoted:
			// M11 (§6.5.1): Flowbee validated the live epoch and fast-forwarded its
			// epoch-namespaced ref onto the real branch. Records the promoted build
			// epoch; no state change (the promotion is a git side-effect, not a state
			// transition — review_pending was already reached by result_accepted).
			j.BuildEpoch = e.Payload.BuildEpoch
		case KindCompensated:
			// M11 (§6.5.4): compensate(job, dead_epoch) ran — the dead epoch's ref was
			// dropped, its CI cancelled, any draft PR drafted-back. No projection field
			// changes (the epoch bump rode the revoke/supersede event that triggered it);
			// recorded for replay/audit completeness.
		case KindUnattendedMerged:
			// M11 (§14 Branch B): a clean/in-budget/denylist-clear/unmoved-SHA diff
			// merged unattended via the queue. The reconciled merge commit is recorded
			// as provenance; the state move to done rides the reconciled merged fact
			// (job_completed), so this event only stamps provenance.
			j.MergeProvenance = e.Payload.MergeProvenance
		case KindAdopted:
			// a pre-existing issue/PR imported quiescent (I-16). State is the imported
			// quiescent marker; no scheduling.
			j.State = e.ToState
			if e.Payload.PRNumber != 0 {
				j.PRNumber = e.Payload.PRNumber
			}
			if e.Payload.IssueNumber != 0 {
				j.IssueNum = e.Payload.IssueNumber
			}
		}
		j.JobSeq = e.JobSeq
	}
	return j, nil
}
