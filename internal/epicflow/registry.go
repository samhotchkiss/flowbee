// Package epicflow is the executable v2 delivery contract. The registry guards
// against silent seams: every non-terminal state must name a deterministic next
// action or visible hold, a progress clock, and an overdue attention kind.
package epicflow

import (
	"fmt"

	"github.com/samhotchkiss/flowbee/internal/attention"
)

type Policy struct {
	State, NextAction, ProgressClock string
	AttentionKind                    attention.Kind
	VisibleHold                      bool
}

type EffectPolicy struct {
	Kind, RecoveryAction string
	AttentionKind        attention.Kind
}

var Registry = []Policy{
	{"admitted", "ensure_builder_launch", "state_due_at", attention.KindLaunchFailed, false},
	{"building", "observe_builder_progress", "fact_progress_at", attention.KindBuildStalled, false},
	{"awaiting_artifact", "observe_owned_pr", "state_due_at", attention.KindArtifactOverdue, false},
	{"awaiting_ci", "observe_required_ci", "state_due_at", attention.KindCIPendingOverdue, false},
	{"awaiting_review_dispatch", "ensure_native_review", "dispatch_due_at", attention.KindReviewDispatchStalled, false},
	{"review_queued", "claim_distinct_reviewer", "state_due_at", attention.KindReviewCapacityExhausted, false},
	{"in_review", "observe_sha_bound_verdict", "last_reviewer_fact_at", attention.KindReviewVerdictOverdue, false},
	{"changes_requested", "ensure_builder_rework", "state_due_at", attention.KindBuilderReworkStalled, false},
	{"rebuild_in_flight", "observe_new_head_and_ci", "fact_progress_at", attention.KindBuilderReworkStalled, false},
	{"merge_queued", "ensure_exact_head_merge", "state_due_at", attention.KindMergeDispatchStalled, false},
	{"merging", "observe_merge_commit", "fact_progress_at", attention.KindMergeDispatchStalled, false},
	{"conflict_resolution", "observe_resolved_head", "fact_progress_at", attention.KindConflictResolutionStalled, false},
	{"merged", "ensure_cleanup", "state_due_at", attention.KindCleanupOverdue, false},
	{"cleanup_pending", "verify_cleanup_absence", "fact_progress_at", attention.KindCleanupOverdue, false},
	{"paused", "await_typed_resume_or_abandon", "state_due_at", attention.KindHoldOverdue, true},
	{"needs_human", "await_typed_human_clear_or_abandon", "state_due_at", attention.KindHoldOverdue, true},
}

var NonTerminalStates = []string{
	"admitted", "building", "awaiting_artifact", "awaiting_ci",
	"awaiting_review_dispatch", "review_queued", "in_review", "changes_requested",
	"rebuild_in_flight", "merge_queued", "merging", "conflict_resolution", "merged",
	"cleanup_pending", "paused", "needs_human",
}

// EffectRegistry is checked alongside delivery states so adding an external
// effect cannot introduce a silent post-claim/pre-result seam.
var EffectRegistry = []EffectPolicy{
	{"builder_rework", "rearm_same_action_or_hold", attention.KindBuilderReworkStalled},
	{"conflict_resolution", "observe_new_head_or_hold", attention.KindConflictResolutionStalled},
	{"merge_dispatch", "verify_exact_pr_fact_or_rearm", attention.KindMergeDispatchStalled},
	{"cleanup", "verify_exact_target_absence_or_rearm", attention.KindCleanupOverdue},
}

var RequiredEffectSeams = []string{"builder_rework", "conflict_resolution", "merge_dispatch", "cleanup"}

func ValidateRegistry() error {
	known := make(map[string]Policy, len(Registry))
	for _, p := range Registry {
		if p.State == "" || p.NextAction == "" || p.ProgressClock == "" || p.AttentionKind == "" {
			return fmt.Errorf("incomplete epic state policy: %+v", p)
		}
		if _, duplicate := known[p.State]; duplicate {
			return fmt.Errorf("duplicate epic state policy %s", p.State)
		}
		if !attention.ValidKind(string(p.AttentionKind)) {
			return fmt.Errorf("epic state %s uses unknown attention kind %s", p.State, p.AttentionKind)
		}
		known[p.State] = p
	}
	for _, state := range NonTerminalStates {
		if _, ok := known[state]; !ok {
			return fmt.Errorf("non-terminal epic state %s has no next action or visible hold", state)
		}
	}
	if len(known) != len(NonTerminalStates) {
		return fmt.Errorf("epic registry contains an undeclared state")
	}
	effects := make(map[string]EffectPolicy, len(EffectRegistry))
	for _, effect := range EffectRegistry {
		if effect.Kind == "" || effect.RecoveryAction == "" || effect.AttentionKind == "" {
			return fmt.Errorf("incomplete epic effect policy: %+v", effect)
		}
		if !attention.ValidKind(string(effect.AttentionKind)) {
			return fmt.Errorf("epic effect %s uses unknown attention kind %s", effect.Kind, effect.AttentionKind)
		}
		if _, duplicate := effects[effect.Kind]; duplicate {
			return fmt.Errorf("duplicate epic effect policy %s", effect.Kind)
		}
		effects[effect.Kind] = effect
	}
	for _, kind := range RequiredEffectSeams {
		if _, ok := effects[kind]; !ok {
			return fmt.Errorf("epic effect seam %s has no recovery policy", kind)
		}
	}
	if len(effects) != len(RequiredEffectSeams) {
		return fmt.Errorf("epic effect registry contains an undeclared seam")
	}
	return nil
}

func PolicyFor(state string) (Policy, bool) {
	for _, policy := range Registry {
		if policy.State == state {
			return policy, true
		}
	}
	return Policy{}, false
}
