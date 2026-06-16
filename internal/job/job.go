// Package job holds the pure domain types and the pure §6.2 state machine. It is
// a deterministic-core package (DESIGN §1.2): it imports no clock, randomness, ID
// minter, GitHub, or LLM package (enforced by tools/archcheck). Times and IDs are
// passed IN as values; the type `time.Time` is used only as a value, never read.
package job

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

	// scheduling
	Priority int

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

	// fold cursor: latest job_events.job_seq applied
	JobSeq int
}
