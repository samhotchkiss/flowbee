package scheduler

import (
	"fmt"
	"sort"
	"time"

	"github.com/samhotchkiss/flowbee/internal/job"
)

// Project scheduling pools are deliberately capability/role based rather than
// provider based. A deployment may map build to Codex and review to Grok without
// baking either provider into the fair scheduler.
const (
	PoolBuild      = "build"
	PoolReview     = "review"
	PoolSpecAuthor = "spec_author"
	PoolSpecReview = "spec_review"
)

// ProjectPolicy is the scheduling projection for one project. Weight is its
// relative share while continuously eligible. ConcurrencyCap is global to the
// project across pools; zero means unlimited.
type ProjectPolicy struct {
	ProjectID      string
	State          string
	Weight         int
	ConcurrencyCap int
}

// FairState is the replayable state of the weighted scheduler. The caller owns
// persistence. State is partitioned by pool so service in one capability pool
// can never buy or consume credit in another.
type FairState struct {
	DeficitByPool    map[string]map[string]int64
	LastServedByPool map[string]map[string]time.Time
}

// FairConfig describes one deterministic scheduling turn. Now is injected;
// this package never reads the wall clock.
type FairConfig struct {
	Pool            string
	Attested        []string
	Now             time.Time
	StarvationBound time.Duration
}

// WhyNotCode is a stable, machine-readable explanation of a candidate's result.
type WhyNotCode string

const (
	WhySelected           WhyNotCode = "selected"
	WhyWrongPool          WhyNotCode = "wrong_pool"
	WhyUnknownProject     WhyNotCode = "unknown_project"
	WhyProjectInactive    WhyNotCode = "project_inactive"
	WhyInvalidPolicy      WhyNotCode = "invalid_project_policy"
	WhyConcurrencyCap     WhyNotCode = "project_concurrency_cap"
	WhyCapabilityMismatch WhyNotCode = "capability_mismatch"
	WhyFairTurn           WhyNotCode = "another_project_fair_turn"
	WhyWithinProjectOrder WhyNotCode = "lower_within_project_order"
)

// CandidateDecision makes every candidate legible, including candidates which
// were eligible but did not win this turn.
type CandidateDecision struct {
	Candidate Candidate
	Code      WhyNotCode
	Detail    string
}

// FairResult is one scheduling turn plus the exact state to durably commit with
// the eventual dispatch. Until that atomic accounting exists, callers may use
// this result in the Phase-2 fairness shadow only.
type FairResult struct {
	Selected       Candidate
	OK             bool
	WinningProject string
	ForcedByAge    bool
	NextState      FairState
	Decisions      []CandidateDecision
}

// PickProjectFair performs one deterministic weighted-deficit scheduling turn.
// It first chooses a project, then uses the existing priority/age ordering within
// that project. A project continuously eligible for StarvationBound is selected
// ahead of the weighted turn; last-service state makes this override rotate rather
// than repeatedly choosing the same old queue.
func PickProjectFair(cands []Candidate, policies []ProjectPolicy, activeByProject map[string]int, state FairState, cfg FairConfig) FairResult {
	result := FairResult{NextState: cloneFairState(state)}
	ensurePoolState(&result.NextState, cfg.Pool)

	policyByProject := make(map[string]ProjectPolicy, len(policies))
	invalidProjects := make(map[string]string)
	for _, policy := range policies {
		if policy.ProjectID == "" || policy.Weight < 1 || policy.ConcurrencyCap < 0 {
			invalidProjects[policy.ProjectID] = "weight must be positive and concurrency cap non-negative"
			continue
		}
		if prior, exists := policyByProject[policy.ProjectID]; exists {
			delete(policyByProject, policy.ProjectID)
			invalidProjects[policy.ProjectID] = fmt.Sprintf("duplicate policy (weights %d and %d)", prior.Weight, policy.Weight)
			continue
		}
		if _, alreadyInvalid := invalidProjects[policy.ProjectID]; alreadyInvalid {
			continue
		}
		policyByProject[policy.ProjectID] = policy
	}

	decisions := make([]CandidateDecision, len(cands))
	eligible := make(map[string][]Candidate)
	oldestEligible := make(map[string]time.Time)
	for i, cand := range cands {
		decisions[i].Candidate = cand
		switch {
		case cand.Pool == "" || cand.Pool != cfg.Pool:
			decisions[i].Code = WhyWrongPool
			decisions[i].Detail = fmt.Sprintf("candidate pool %q does not match scheduling pool %q", cand.Pool, cfg.Pool)
			continue
		case invalidProjects[cand.ProjectID] != "":
			decisions[i].Code = WhyInvalidPolicy
			decisions[i].Detail = invalidProjects[cand.ProjectID]
			continue
		}
		policy, exists := policyByProject[cand.ProjectID]
		if !exists {
			decisions[i].Code = WhyUnknownProject
			decisions[i].Detail = fmt.Sprintf("project %q has no scheduler policy", cand.ProjectID)
			continue
		}
		if policy.State != "active" {
			decisions[i].Code = WhyProjectInactive
			decisions[i].Detail = fmt.Sprintf("project %q is %s", cand.ProjectID, policy.State)
			continue
		}
		if policy.ConcurrencyCap > 0 && activeByProject[cand.ProjectID] >= policy.ConcurrencyCap {
			decisions[i].Code = WhyConcurrencyCap
			decisions[i].Detail = fmt.Sprintf("project %q has %d active work items at cap %d", cand.ProjectID, activeByProject[cand.ProjectID], policy.ConcurrencyCap)
			continue
		}
		if !job.CapabilitiesSatisfy(cfg.Attested, cand.RequiredCapabilities) {
			decisions[i].Code = WhyCapabilityMismatch
			decisions[i].Detail = "worker attestation does not satisfy required capabilities"
			continue
		}
		eligible[cand.ProjectID] = append(eligible[cand.ProjectID], cand)
		if oldestEligible[cand.ProjectID].IsZero() || cand.EnqueuedAt.Before(oldestEligible[cand.ProjectID]) {
			oldestEligible[cand.ProjectID] = cand.EnqueuedAt
		}
	}

	// Inactive projects do not bank unbounded credit while they have no eligible
	// work. Credit starts accruing again on the first eligible turn.
	poolDeficits := result.NextState.DeficitByPool[cfg.Pool]
	for projectID := range poolDeficits {
		if len(eligible[projectID]) == 0 {
			poolDeficits[projectID] = 0
		}
	}
	if len(eligible) == 0 {
		result.Decisions = decisions
		return result
	}

	projectIDs := make([]string, 0, len(eligible))
	var totalWeight int64
	for projectID := range eligible {
		projectIDs = append(projectIDs, projectID)
		weight := int64(policyByProject[projectID].Weight)
		poolDeficits[projectID] += weight
		totalWeight += weight
	}
	sort.Strings(projectIDs)

	winner := ""
	if cfg.StarvationBound > 0 {
		var oldestServiceBase time.Time
		for _, projectID := range projectIDs {
			base := oldestEligible[projectID]
			if served := result.NextState.LastServedByPool[cfg.Pool][projectID]; served.After(base) {
				base = served
			}
			if cfg.Now.Before(base) || cfg.Now.Sub(base) < cfg.StarvationBound {
				continue
			}
			if winner == "" || base.Before(oldestServiceBase) || (base.Equal(oldestServiceBase) && projectID < winner) {
				winner, oldestServiceBase = projectID, base
			}
		}
		result.ForcedByAge = winner != ""
	}
	if winner == "" {
		for _, projectID := range projectIDs {
			if winner == "" || poolDeficits[projectID] > poolDeficits[winner] ||
				(poolDeficits[projectID] == poolDeficits[winner] && projectID < winner) {
				winner = projectID
			}
		}
	}

	// Charge exactly one shared-pool service turn. This is what makes sustained
	// service converge to configured relative weights.
	poolDeficits[winner] -= totalWeight
	result.NextState.LastServedByPool[cfg.Pool][winner] = cfg.Now
	ordered := Order(eligible[winner], cfg.Attested, cfg.Now)
	result.Selected, result.OK, result.WinningProject = ordered[0], true, winner

	for i := range decisions {
		if decisions[i].Code != "" {
			continue
		}
		switch {
		case decisions[i].Candidate.JobID == result.Selected.JobID:
			decisions[i].Code = WhySelected
			if result.ForcedByAge {
				decisions[i].Detail = "selected because the project reached its starvation bound"
			} else {
				decisions[i].Detail = "selected by the project weighted-fair turn"
			}
		case decisions[i].Candidate.ProjectID == winner:
			decisions[i].Code = WhyWithinProjectOrder
			decisions[i].Detail = fmt.Sprintf("job %q ranked first within project %q", result.Selected.JobID, winner)
		default:
			decisions[i].Code = WhyFairTurn
			decisions[i].Detail = fmt.Sprintf("project %q won this %s pool turn", winner, cfg.Pool)
		}
	}
	result.Decisions = decisions
	return result
}

func cloneFairState(in FairState) FairState {
	out := FairState{
		DeficitByPool:    make(map[string]map[string]int64, len(in.DeficitByPool)),
		LastServedByPool: make(map[string]map[string]time.Time, len(in.LastServedByPool)),
	}
	for pool, values := range in.DeficitByPool {
		out.DeficitByPool[pool] = make(map[string]int64, len(values))
		for projectID, value := range values {
			out.DeficitByPool[pool][projectID] = value
		}
	}
	for pool, values := range in.LastServedByPool {
		out.LastServedByPool[pool] = make(map[string]time.Time, len(values))
		for projectID, value := range values {
			out.LastServedByPool[pool][projectID] = value
		}
	}
	return out
}

func ensurePoolState(state *FairState, pool string) {
	if state.DeficitByPool == nil {
		state.DeficitByPool = make(map[string]map[string]int64)
	}
	if state.LastServedByPool == nil {
		state.LastServedByPool = make(map[string]map[string]time.Time)
	}
	if state.DeficitByPool[pool] == nil {
		state.DeficitByPool[pool] = make(map[string]int64)
	}
	if state.LastServedByPool[pool] == nil {
		state.LastServedByPool[pool] = make(map[string]time.Time)
	}
}
