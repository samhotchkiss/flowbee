// Package job holds the pure domain types and the pure §6.2 state machine. It is
// a deterministic-core package (DESIGN §1.2): it imports no clock, randomness, ID
// minter, GitHub, or LLM package (enforced by tools/archcheck). Times and IDs are
// passed IN as values; the type `time.Time` is used only as a value, never read.
package job

import (
	"sort"
	"strings"
	"time"
)

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
	// StateNeedsDesign is the F4 design-fork escalation sink: issue-review determined
	// the spec needs HUMAN design input (a decision Flowbee cannot make by amending
	// bytes). It holds NO active lease (it is not scheduled), is surfaced on
	// GET /v1/needs-input so the user's board-check loop can answer it, and resumes
	// (back to spec_review) once a human supplies the design decision. Distinct from
	// needs_human (the §12.6.1 attempts/bounces/cost/stall chokepoint): needs_design
	// is a deliberate "the machine should not decide this" signal, not a failure.
	StateNeedsDesign State = "needs_design"
	// StateBacklog is the tracked-but-NOT-scheduled state (flow-pass §D): a job that
	// is visible on the board but carries "needs full spec" and is never leased until
	// deliberately promoted. It holds no active lease.
	StateBacklog State = "backlog"
	// StateQuiescent is the ADOPT-mode mirrored-but-quiescent state (§12.7, I-16):
	// a job imported from a pre-existing GitHub issue/PR on first boot. It is
	// reconciled (full Domain-B facts) but NEVER scheduled (no lease) and NEVER
	// rendered OUT (project-OUT suppressed) until deliberate opt-in. It holds no
	// active lease, so it is absent from the one_active_lease_per_job index.
	StateQuiescent State = "quiescent"
	// StateResolvingConflict is the F8 merge-conflict resolution lease state (§E):
	// a build whose rebase onto the CURRENT main hit a REAL conflict (overlapping
	// edits a clean rebase could not resolve) is leased to a `conflict_resolver`
	// agent, which rebases + resolves in a worktree and returns the RESOLVED diff.
	// The resolved diff is untrusted code like any build product: it goes back
	// through build-review + re-CI before re-merge (resolution is just another job).
	// It holds an active lease (a worker is bound to it).
	StateResolvingConflict State = "resolving_conflict"
)

// Role is the slot a worker is bound to for a stage (DESIGN §5.2).
type Role string

const (
	RoleSpecAuthor   Role = "spec_author"
	RoleSpecReviewer Role = "spec_reviewer"
	RoleEngWorker    Role = "eng_worker"
	RoleCodeReviewer Role = "code_reviewer"
	RoleMerger       Role = "merger"
	// RoleConflictResolver is the F8 merge-conflict resolution slot (§E): an agent
	// that rebases a build onto current main and resolves a REAL conflict in a
	// worktree, returning the resolved diff. Its output is untrusted code re-reviewed
	// + re-CI'd like any build (resolution is just another job).
	RoleConflictResolver Role = "conflict_resolver"
	// RoleTester is the F10 `test` job slot: a worker that runs the build's test
	// suite and reports a pass/fail. It is capability-matched on the test job's
	// DIFF-DERIVED constraints (arch/os/tool); a green report produces a ci_green@sha
	// fact the merge gate honors (the pluggable-CI fact).
	RoleTester Role = "tester"
)

// Kind is a job kind (DESIGN §6.1). spec/build are the original two; F10 adds
// `test`: a capability-matched CI job whose green result is a PLUGGABLE source of
// the merge gate's ci_green@sha fact (an alternative to reconcile-from-Actions).
type Kind string

const (
	KindSpec  Kind = "spec"
	KindBuild Kind = "build"
	// KindTest is the F10 `test` job: Flowbee runs the build's tests itself (rather
	// than only reconciling GitHub-Actions CI). It is capability-matched on
	// DIFF-DERIVED constraints (arch/os/tool — the arch-lottery fix) so an arm64
	// build's tests route ONLY to an arm64-capable worker. A passing test job
	// records a ci_green@sha fact the merge gate honors exactly like reconciled
	// Actions CI (the §F10 pluggable-CI fact).
	KindTest Kind = "test"
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
	// StateResolvingConflict holds an active lease once a conflict_resolver claims it
	// (a resolver is bound + working). It joins the one_active_lease_per_job index so
	// the I-4 backstop still holds for the resolution stage.
	StateResolvingConflict: true,
}

// HasActiveLease reports whether s is a state that holds an active lease.
func HasActiveLease(s State) bool { return ActiveLeaseStates[s] }

// ActiveLeaseStatesSQL renders ActiveLeaseStates as a SQL IN-list literal —
// ('building','code_review',...) — sorted for determinism. The per-box slot gate and
// the roster MUST count exactly these states; deriving the clause here means it can
// never drift from the canonical set (a hand-copied clause that dropped
// resolving_conflict let a running resolver escape the slot gate → box overcommit).
func ActiveLeaseStatesSQL() string {
	lits := make([]string, 0, len(ActiveLeaseStates))
	for s := range ActiveLeaseStates {
		lits = append(lits, "'"+string(s)+"'")
	}
	sort.Strings(lits)
	return "(" + strings.Join(lits, ",") + ")"
}

// livenessEvaluableStates is ActiveLeaseStates MINUS the two worker-LESS states
// (merge_handoff, merging), derived so it can never drift from the source of truth.
// Both hold the one-active-lease uniqueness slot (so they stay in ActiveLeaseStates +
// the migration index) but neither has a bound worker to be "live."
var livenessEvaluableStates = func() map[State]bool {
	m := make(map[State]bool, len(ActiveLeaseStates))
	for s := range ActiveLeaseStates {
		if s != StateMergeHandoff && s != StateMerging {
			m[s] = true
		}
	}
	return m
}()

// LivenessEvaluable reports whether the liveness ladder (the two-rung stall kill +
// heartbeat-staleness reap) should evaluate a job in state s. It is HasActiveLease
// EXCEPT the two states with NO bound worker. Reconciliation may still supersede
// either state when GitHub's head no longer matches its SHA-bound verdict; the
// liveness ladder must not move them merely because no worker is heartbeating:
//
//   - merge_handoff: parked at the HUMAN merge gate, where a "stall" is the normal,
//     expected condition (a human may take hours/days). Evaluating it made the ladder
//     read the handoff's stale build-phase heartbeat as a dead worker and
//     revoke/escalate it — looping the build forever (the live #175/#177 regression).
//     Recovered by reconcile (PR merged -> done, PR closed -> pr_closed).
//   - merging: a merge in flight, dispatched to the project-OUT outbox (NOT a worker),
//     so it too has no heartbeat. Reaping it would yank the job back to build
//     MID-DISPATCH while the merge outbox row is still pending — a double-action /
//     inconsistency. Recovered by the outbox (retry / conflict -> resolver /
//     dead-letter) + reconcile, never the liveness ladder.
//
// Used by BOTH liveness entry points (the Rung-2 poller sweep AND the
// lease/phase-deadline timer).
func LivenessEvaluable(s State) bool { return livenessEvaluableStates[s] }

// CostExceeded is the pure §6.7 / I-15 ceiling predicate: a job is over budget
// iff it has a $ ceiling and its accumulated micro-USD meter reached it. With no
// ceiling (nil) the meter only accumulates (for the rollup) and never escalates.
// Pure: it reads only the passed-in resolved values, no clock, no I/O.
func CostExceeded(costMicroUSD int64, ceiling *int64) bool {
	return ceiling != nil && costMicroUSD >= *ceiling
}

// EscalationReason is the canonical reason a job sits in needs_human (§12.6.1):
// the four independent triggers that all deposit into the one chokepoint.
type EscalationReason string

const (
	EscalationAttempts EscalationReason = "attempts"
	EscalationBounces  EscalationReason = "bounces"
	EscalationCost     EscalationReason = "cost"
	EscalationStall    EscalationReason = "stall"
	// EscalationProjectOut is the stuck-GitHub-write trigger: an outbox row (open PR /
	// merge / comment / label) failed PERMANENTLY (a 4xx — deleted branch/PR, 422, 404)
	// or exhausted its retry budget. The row is dead-lettered so the rest of the repo's
	// GitHub writes keep flowing (no head-of-line wedge), and the job is surfaced so a
	// human fixes the underlying GitHub state and requeues.
	EscalationProjectOut EscalationReason = "project_out"
	// EscalationCIStalled is the stuck-CI trigger: a review_pending job whose PR has
	// been open with CI NOT green for the entire (generous) stall window — CI is wedged
	// (the runner is down, no workflow was triggered, or the run is perpetually pending),
	// not merely slow. Distinct from a generic stall so the operator fixes CI (re-run /
	// requeue) rather than hunting the job. A silent indefinite review is a worse failure
	// than a clear page, so the watchdog surfaces it instead of waiting forever.
	EscalationCIStalled EscalationReason = "ci_stalled"
	// EscalationPostMergeCI is the post-merge red-main trigger: GitHub reports the PR
	// merged, but a required check at the merged head failed. Do not silently mark the
	// job done; surface an explicit repair/human state with the failed checks recorded.
	EscalationPostMergeCI EscalationReason = "post_merge_ci"
	// EscalationPRClosed: a human CLOSED the job's PR without merging (rejected the
	// change). The job is parked promptly with this legible reason instead of waiting
	// on a merge that will never come (and instead of a misleading `stall`).
	EscalationPRClosed EscalationReason = "pr_closed"
	// EscalationReviewerRejections is the per-review-node loop trigger (§12.6.1): a
	// SINGLE review node requested changes on the same task MaxReviewerRejections
	// times. That is a genuine standoff with one reviewer — park it for a human
	// rather than rebuild forever. Distinct from EscalationBounces, the cruder
	// total-across-ALL-reviewers backstop, which fires later (max_bounces).
	EscalationReviewerRejections EscalationReason = "reviewer_rejections"
	// EscalationDesign is the F4 design-fork escalation: issue-review determined the
	// spec needs human design input. It deposits into the needs_design surface (not
	// the needs_human chokepoint) — a deliberate "the machine should not decide this".
	EscalationDesign EscalationReason = "design"
)

// Default counters a job is created with. These are the single source of truth: the
// store INSERTs that create jobs use these values, and the ledger Fold reconstructs
// them, so projection == Fold(events). DefaultMaxBounces was lowered 9 -> 4 to cap
// rebuild cost on failing jobs (a doomed job burns ~1 build+review per bounce).
const (
	DefaultMaxAttempts = 5
	DefaultMaxBounces  = 4
)

// Priority is a 1..10 urgency where LOWER = MORE urgent: 1 = drop-everything, 5 = the
// default for any new issue, 10 = nice-to-have whenever there's time. The scheduler ranks
// a lower number first (scheduler.EffectivePriority), and aging makes a waiting job
// progressively MORE urgent so nothing starves. 0 is the "unset" sentinel (an omitted API
// field / a bare INSERT) and normalizes to the default.
const (
	MostUrgentPriority  = 1
	DefaultPriority     = 5
	LeastUrgentPriority = 10
)

// NormalizePriority maps any raw priority to the valid 1..10 band: an unset 0 becomes the
// default 5, and out-of-range values clamp (negative to most-urgent 1, >10 to least-urgent
// 10). Apply it wherever a job is created from user input so the stored priority is always
// a meaningful 1..10 (1 = most urgent).
func NormalizePriority(p int) int {
	switch {
	case p == 0:
		return DefaultPriority
	case p < MostUrgentPriority:
		return MostUrgentPriority
	case p > LeastUrgentPriority:
		return LeastUrgentPriority
	default:
		return p
	}
}

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

	// Repo is the F9 repo-scope handle (the repos.id this job belongs to). Empty is
	// the legacy single-repo default. It is a resolved Domain-A fact: the scheduler
	// is repo-AGNOSTIC (it ranks the union of all repos' ready jobs), but reconcile-IN
	// and project-OUT are repo-SCOPED (each repo has its own GitHub coords + loop), so
	// a swept PR number is bound back to a job only within its own repo.
	Repo string

	// lineage (Domain A)
	ChatRef   string
	SpecRef   string
	ParentJob string
	IssueNum  int
	PRNumber  int

	// F4 epic grouping. EpicID groups the issues of one epic decomposition; IsEpic
	// flags the epic-barrier job (the one the epic-level issue-review reviews as a
	// whole). EpicReviewed records that the epic-level barrier has passed.
	EpicID       string
	IsEpic       bool
	EpicReviewed bool

	// SHA binding (build)
	BaseSHA string
	HeadSHA string

	// task/context (F1). The human intent folded onto the job and shipped, fully
	// resolved, in the lease grant's context block (§B self-contained lease JSON).
	// TaskText is the imperative the agent must satisfy; SpecText the longer
	// spec/design context; AcceptanceCriteria the DONE-WHEN (newline-delimited).
	// All are RESOLVED facts (settable via `flowbee seed` or a GitHub issue body),
	// never read from a clock.
	TaskText           string
	SpecText           string
	AcceptanceCriteria string

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

	// LastReviewNotes carries the most recent code-review's changes-requested findings
	// (the reviewer's "fix X, Y, Z") forward to the rebuild's lease context, so the agent
	// addresses what was flagged instead of rebuilding blind (§F compounding memory, read
	// side). A projection field folded from the bounce event's payload.
	LastReviewNotes string

	// LastCIFailures carries the NAMES of the checks that failed CI on the prior attempt
	// (newline-separated, e.g. "Architecture and guardrail lints\ngolangci-lint") forward
	// to the rebuild's lease context, so the agent re-runs the named gate + fixes the real
	// violation instead of rebuilding blind and re-failing the same check (§F compounding
	// memory, read side). A projection field folded from the ci-fail bounce event.
	LastCIFailures string

	// DiffEmpty records an authoritative empty diff for an adopted PR review. It
	// distinguishes "this PR has no changes" from legacy/missing patch_diff=''.
	DiffEmpty bool

	// counters (§6.7)
	Attempts         int
	MaxAttempts      int
	Bounces          int
	MaxBounces       int
	StallRevocations int

	// cost meter (§6.7, I-15). The dollar meter is exact integer MICRO-USD
	// ($1.00 = 1_000_000) so the ceiling comparison is never a float. A nil
	// CostCeilingMicroUSD means "no $ ceiling" (still metered for the rollup).
	CostTokensIn        int64
	CostTokensOut       int64
	CostMicroUSD        int64
	CostCeilingMicroUSD *int64
	OverBudget          bool

	// FlowID groups the spec+build+review jobs of one feature for the per-flow
	// cost rollup (§12.6.5). Empty falls back to the job's own id.
	FlowID string

	// EscalationReason records WHY the job is in needs_human (the §12.6.1
	// chokepoint surfaces all four triggers: attempts | bounces | cost | stall).
	EscalationReason string

	// M11 epoch-namespaced side-effects (§3.5/§6.5, I-12). BuildEpoch is the epoch
	// whose ref Flowbee last PROMOTED onto the real branch (the live build epoch); a
	// result from a stale epoch is never promoted. MergeProvenance is the reconciled
	// merge-commit SHA recorded on an unattended self_merge (Branch B).
	BuildEpoch      int
	MergeProvenance string

	// verdict (gate stages only; written ONLY by gate logic, never a worker, I-9)
	Verdict *Verdict

	// fold cursor: latest job_events.job_seq applied
	JobSeq int
}
