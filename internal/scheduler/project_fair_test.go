package scheduler

import (
	"fmt"
	"testing"
	"time"
)

func fairCandidate(projectID, id, pool string, priority int, enqueued time.Time, caps ...string) Candidate {
	return Candidate{ProjectID: projectID, JobID: id, Pool: pool, Priority: priority, EnqueuedAt: enqueued, RequiredCapabilities: caps}
}

func TestProjectFairWeightsConvergeUnderSustainedLoad(t *testing.T) {
	now := time.Unix(10_000, 0)
	cands := []Candidate{
		fairCandidate("heavy", "heavy-job", PoolBuild, 5, now),
		fairCandidate("light", "light-job", PoolBuild, 5, now),
	}
	policies := []ProjectPolicy{{ProjectID: "heavy", State: "active", Weight: 3}, {ProjectID: "light", State: "active", Weight: 1}}
	state := FairState{}
	counts := map[string]int{}
	for i := 0; i < 400; i++ {
		got := PickProjectFair(cands, policies, nil, state, FairConfig{Pool: PoolBuild, Now: now})
		if !got.OK {
			t.Fatal("expected a fair pick")
		}
		counts[got.WinningProject]++
		state = got.NextState
	}
	if counts["heavy"] != 300 || counts["light"] != 100 {
		t.Fatalf("3:1 weights did not converge exactly over complete rounds: %+v", counts)
	}
}

func TestProjectFairStarvationBoundOverridesNoisyHighWeightProject(t *testing.T) {
	start := time.Unix(20_000, 0)
	cands := []Candidate{
		fairCandidate("noisy", "noisy-job", PoolBuild, 1, start),
		fairCandidate("small", "small-job", PoolBuild, 9, start),
	}
	policies := []ProjectPolicy{{ProjectID: "noisy", State: "active", Weight: 100}, {ProjectID: "small", State: "active", Weight: 1}}
	state := FairState{}
	servedSmallAt := -1
	for minute := 0; minute <= 6; minute++ {
		now := start.Add(time.Duration(minute) * time.Minute)
		got := PickProjectFair(cands, policies, nil, state, FairConfig{
			Pool: PoolBuild, Now: now, StarvationBound: 5 * time.Minute,
		})
		if !got.OK {
			t.Fatal("expected a fair pick")
		}
		if got.WinningProject == "small" {
			servedSmallAt = minute
			if !got.ForcedByAge {
				t.Fatal("low-weight project should be served by the explicit age fence")
			}
			break
		}
		state = got.NextState
	}
	if servedSmallAt < 0 || servedSmallAt > 5 {
		t.Fatalf("continuously eligible low-weight project starved past bound; served minute=%d", servedSmallAt)
	}
}

func TestProjectFairConcurrencyCapDoesNotBlockOtherProject(t *testing.T) {
	now := time.Unix(30_000, 0)
	cands := []Candidate{
		fairCandidate("capped", "capped-job", PoolBuild, 1, now),
		fairCandidate("open", "open-job", PoolBuild, 9, now),
	}
	policies := []ProjectPolicy{
		{ProjectID: "capped", State: "active", Weight: 100, ConcurrencyCap: 2},
		{ProjectID: "open", State: "active", Weight: 1},
	}
	got := PickProjectFair(cands, policies, map[string]int{"capped": 2}, FairState{}, FairConfig{Pool: PoolBuild, Now: now})
	if !got.OK || got.Selected.JobID != "open-job" {
		t.Fatalf("cap in one project must not block another: %+v", got)
	}
	assertDecisionCode(t, got.Decisions, "capped-job", WhyConcurrencyCap)
}

func TestProjectFairBuildAndReviewPoolsAreIsolated(t *testing.T) {
	now := time.Unix(40_000, 0)
	cands := []Candidate{
		fairCandidate("a", "build-a", PoolBuild, 1, now, "role:eng_worker"),
		fairCandidate("b", "review-b", PoolReview, 1, now, "role:code_reviewer"),
	}
	policies := []ProjectPolicy{{ProjectID: "a", State: "active", Weight: 1}, {ProjectID: "b", State: "active", Weight: 1}}
	state := FairState{DeficitByPool: map[string]map[string]int64{PoolReview: {"b": 17}}}
	build := PickProjectFair(cands, policies, nil, state, FairConfig{Pool: PoolBuild, Attested: []string{"role:eng_worker"}, Now: now})
	if !build.OK || build.Selected.JobID != "build-a" {
		t.Fatalf("build pool selected lateral review work: %+v", build)
	}
	assertDecisionCode(t, build.Decisions, "review-b", WhyWrongPool)
	if build.NextState.DeficitByPool[PoolReview]["b"] != 17 {
		t.Fatal("a build turn mutated review-pool credit")
	}
	review := PickProjectFair(cands, policies, nil, build.NextState, FairConfig{Pool: PoolReview, Attested: []string{"role:code_reviewer"}, Now: now})
	if !review.OK || review.Selected.JobID != "review-b" {
		t.Fatalf("review pool selected build work: %+v", review)
	}
}

func TestProjectFairWhyNotReasonsAreCompleteAndLegible(t *testing.T) {
	now := time.Unix(50_000, 0)
	cands := []Candidate{
		fairCandidate("winner", "selected", PoolBuild, 1, now),
		fairCandidate("winner", "later", PoolBuild, 9, now),
		fairCandidate("other", "fair-turn", PoolBuild, 1, now),
		fairCandidate("paused", "paused", PoolBuild, 1, now),
		fairCandidate("winner", "review", PoolReview, 1, now),
		fairCandidate("missing", "unknown", PoolBuild, 1, now),
		fairCandidate("winner", "caps", PoolBuild, 1, now, "model_family:codex"),
	}
	policies := []ProjectPolicy{
		{ProjectID: "winner", State: "active", Weight: 2},
		{ProjectID: "other", State: "active", Weight: 1},
		{ProjectID: "paused", State: "paused", Weight: 1},
	}
	got := PickProjectFair(cands, policies, nil, FairState{}, FairConfig{Pool: PoolBuild, Attested: []string{"model_family:grok"}, Now: now})
	wants := map[string]WhyNotCode{
		"selected": WhySelected, "later": WhyWithinProjectOrder, "fair-turn": WhyFairTurn,
		"paused": WhyProjectInactive, "review": WhyWrongPool, "unknown": WhyUnknownProject,
		"caps": WhyCapabilityMismatch,
	}
	if len(got.Decisions) != len(cands) {
		t.Fatalf("got %d decisions for %d candidates", len(got.Decisions), len(cands))
	}
	for id, code := range wants {
		assertDecisionCode(t, got.Decisions, id, code)
	}
	for _, decision := range got.Decisions {
		if decision.Code == "" || decision.Detail == "" {
			t.Fatalf("candidate %s has illegible reason: %+v", decision.Candidate.JobID, decision)
		}
	}
}

func TestProjectFairIsDeterministicAndDoesNotMutateInputState(t *testing.T) {
	now := time.Unix(60_000, 0)
	cands := []Candidate{fairCandidate("b", "b", PoolBuild, 1, now), fairCandidate("a", "a", PoolBuild, 1, now)}
	policies := []ProjectPolicy{{ProjectID: "b", State: "active", Weight: 1}, {ProjectID: "a", State: "active", Weight: 1}}
	state := FairState{DeficitByPool: map[string]map[string]int64{PoolBuild: {"a": 2}}}
	one := PickProjectFair(cands, policies, nil, state, FairConfig{Pool: PoolBuild, Now: now})
	two := PickProjectFair(cands, policies, nil, state, FairConfig{Pool: PoolBuild, Now: now})
	if one.Selected.JobID != two.Selected.JobID || fmt.Sprint(one.NextState) != fmt.Sprint(two.NextState) {
		t.Fatalf("same facts produced different scheduling turns: one=%+v two=%+v", one, two)
	}
	if state.DeficitByPool[PoolBuild]["a"] != 2 || len(state.DeficitByPool[PoolBuild]) != 1 {
		t.Fatalf("input state mutated: %+v", state)
	}
}

func assertDecisionCode(t *testing.T, decisions []CandidateDecision, jobID string, want WhyNotCode) {
	t.Helper()
	for _, decision := range decisions {
		if decision.Candidate.JobID == jobID {
			if decision.Code != want {
				t.Fatalf("job %s reason=%s want=%s (%s)", jobID, decision.Code, want, decision.Detail)
			}
			return
		}
	}
	t.Fatalf("missing decision for %s", jobID)
}
