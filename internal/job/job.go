// Package job holds the pure domain types and the pure §6.2 state machine. It is
// a deterministic-core package (DESIGN §1.2): it imports no clock, randomness, ID
// minter, GitHub, or LLM package (enforced by tools/archcheck). Times and IDs are
// passed IN as values; the type `time.Time` is used only as a value, never read.
package job

import "time"

// State is the §6.2.1 state-machine catalogue. M1 exercises the build subset.
type State string

const (
	StateSpecAuthoring State = "spec_authoring"
	StateSpecReview    State = "spec_review"
	StateReady         State = "ready"
	StateLeased        State = "leased"
	StateBuilding      State = "building"
	StateReviewPending State = "review_pending"
	StateCodeReview    State = "code_review"
	StateMergeable     State = "mergeable"
	StateMerging       State = "merging"
	StateMergeHandoff  State = "merge_handoff"
	StateDone          State = "done"
	StateBlocked       State = "blocked"
	StateNeedsHuman    State = "needs_human"
	StateSuperseded    State = "superseded"
	StateCancelled     State = "cancelled"
	// StateFailed is the terminal-for-the-attempt sink of the agent_exited_zombie
	// fast-path (§10.6): the worker-local supervisor waitpid'd and saw the agent PID
	// died (locally provable) -> straight to `failed`; compensation fires; the job
	// re-queues subject to max_attempts. Distinct from a "kill" (a lease revocation).
	StateFailed State = "failed"
	// StateQuiescent is the ADOPT-mode mirrored-but-quiescent state (§12.7, I-16):
	// a job imported from a pre-existing GitHub issue/PR on first boot. It is
	// reconciled (full Domain-B facts) but NEVER scheduled (no lease) and NEVER
	// rendered OUT (project-OUT suppressed) until deliberate opt-in. It holds no
	// active lease, so it is absent from the one_active_lease_per_job index.
	StateQuiescent State = "quiescent"
)

// Role is the slot a worker is bound to for a stage (DESIGN §5.2).
type Role string

const (
	RoleSpecAuthor   Role = "spec_author"
	RoleSpecReviewer Role = "spec_reviewer"
	RoleEngWorker    Role = "eng_worker"
	RoleCodeReviewer Role = "code_reviewer"
	RoleMerger       Role = "merger"
)

// Kind is one of exactly two job kinds (DESIGN §6.1).
type Kind string

const (
	KindSpec  Kind = "spec"
	KindBuild Kind = "build"
)

// ActiveLeaseStates is the set of states that hold an active lease (§6.2.1). It
// MUST exactly equal the partial-unique-index predicate in the jobs migration.
var ActiveLeaseStates = map[State]bool{
	StateLeased:        true,
	StateBuilding:      true,
	StateCodeReview:    true,
	StateMerging:       true,
	StateMergeHandoff:  true,
	StateSpecAuthoring: true,
	StateSpecReview:    true,
}

// HasActiveLease reports whether s is a state that holds an active lease.
func HasActiveLease(s State) bool { return ActiveLeaseStates[s] }

// CapabilitiesSatisfy reports whether the attested capability set satisfies every
// required capability tag. This is the pure §6.6 capability match (matching is on
// the attested set; the loop wires it into the atomic claim). Required tags are
// matched exactly, except a "model_family:*" requirement is satisfied by any
// "model_family:" capability (a role that accepts any family, §5.3).
func CapabilitiesSatisfy(attested, required []string) bool {
	have := make(map[string]bool, len(attested))
	for _, c := range attested {
		have[c] = true
	}
	for _, req := range required {
		if req == "model_family:*" {
			if !hasPrefix(attested, "model_family:") {
				return false
			}
			continue
		}
		if !have[req] {
			return false
		}
	}
	return true
}

func hasPrefix(caps []string, prefix string) bool {
	for _, c := range caps {
		if len(c) >= len(prefix) && c[:len(prefix)] == prefix {
			return true
		}
	}
	return false
}

// Job is the projection folded from job_events (Domain A). M1 carries the lease
// columns + counters the lease thread needs; later milestones extend it.
type Job struct {
	ID    string
	Kind  Kind
	Flow  string
	Stage string
	State State
	Role  Role

	// lineage (Domain A)
	ChatRef   string
	SpecRef   string
	ParentJob string
	IssueNum  int
	PRNumber  int

	// SHA binding (build)
	BaseSHA string
	HeadSHA string

	// spec binding (spec flow, §11)
	SpecContentHash string
	SpecVersion     int
	SpecSignoff     *SpecSignoff

	// scheduling
	BlockedBy []string // DAG predecessors; job is `ready` only when all are `done`
	Priority  int
	// EnqueuedAt is the instant the job entered `ready` (for aging, §6.6). It is a
	// resolved fact recorded in the ledger, never read from a clock at fold time.
	EnqueuedAt time.Time

	// RequiredCapabilities are the capability tags a worker MUST attest to win
	// this job's lease (§6.6 capability match on claimed-as-attested). Empty =>
	// any worker may win.
	RequiredCapabilities []string

	// LIVE lease columns
	LeaseID          string
	LeaseEpoch       int
	BoundIdentity    string
	BoundModelFamily string
	BoundLens        string

	// counters (§6.7)
	Attempts         int
	MaxAttempts      int
	Bounces          int
	MaxBounces       int
	StallRevocations int

	// verdict (gate stages only; written ONLY by gate logic, never a worker, I-9)
	Verdict *Verdict

	// fold cursor: latest job_events.job_seq applied
	JobSeq int
}
