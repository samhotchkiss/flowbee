package scheduler

import (
	"testing"
	"time"
)

func cand(id string, prio int, enqueued time.Time, req ...string) Candidate {
	return Candidate{JobID: id, Priority: prio, EnqueuedAt: enqueued, RequiredCapabilities: req}
}

// TestAgedLowPriorityBeatsFreshHighPriority is the core §6.6 aging claim: a
// low-priority job that has waited long enough out-ranks a high-priority newcomer.
func TestAgedLowPriorityBeatsFreshHighPriority(t *testing.T) {
	base := time.Unix(0, 0)
	rate := time.Minute // 1 priority point per minute waited
	// low-prio job waited 100 minutes -> effective prio = 0 + 100 = 100.
	low := cand("low", 0, base)
	// high-prio newcomer enqueued at now -> effective prio = 50 + 0 = 50.
	now := base.Add(100 * time.Minute)
	high := cand("high", 50, now)

	picked, ok := PickWith([]Candidate{high, low}, nil, now, rate)
	if !ok {
		t.Fatal("expected a pick")
	}
	if picked.JobID != "low" {
		t.Fatalf("aged low-prio should win, got %s", picked.JobID)
	}
}

// TestFreshHighPriorityBeatsFreshLow: with no aging gap, priority decides.
func TestFreshHighPriorityBeatsFreshLow(t *testing.T) {
	now := time.Unix(1000, 0)
	picked, ok := PickWith([]Candidate{cand("lo", 1, now), cand("hi", 9, now)}, nil, now, time.Minute)
	if !ok || picked.JobID != "hi" {
		t.Fatalf("fresh high-prio should win, got %+v ok=%v", picked, ok)
	}
}

// TestCapabilityFilterExcludesIneligible: a worker lacking a required capability
// is never offered the job, even if it is the highest-priority candidate.
func TestCapabilityFilterExcludesIneligible(t *testing.T) {
	now := time.Unix(0, 0)
	needsCodex := cand("needs", 100, now, "role:eng_worker", "model_family:codex")
	open := cand("open", 1, now)

	// a worker without model_family:codex cannot win `needs`, only `open`.
	picked, ok := Pick([]Candidate{needsCodex, open}, []string{"role:eng_worker", "model_family:opus"}, now)
	if !ok || picked.JobID != "open" {
		t.Fatalf("ineligible worker should fall to open job, got %+v ok=%v", picked, ok)
	}

	// a codex worker wins the higher-priority `needs`.
	picked2, ok2 := Pick([]Candidate{needsCodex, open}, []string{"role:eng_worker", "model_family:codex"}, now)
	if !ok2 || picked2.JobID != "needs" {
		t.Fatalf("codex worker should win needs, got %+v ok=%v", picked2, ok2)
	}
}

// TestNoEligibleCandidate: a worker that satisfies nothing gets ok=false.
func TestNoEligibleCandidate(t *testing.T) {
	now := time.Unix(0, 0)
	_, ok := Pick([]Candidate{cand("a", 0, now, "role:code_reviewer")}, []string{"role:eng_worker"}, now)
	if ok {
		t.Fatal("worker without required cap must not be offered the job")
	}
	if got := Order([]Candidate{cand("a", 0, now, "role:code_reviewer")}, []string{"role:eng_worker"}, now); len(got) != 0 {
		t.Fatalf("Order should exclude ineligible, got %d", len(got))
	}
}

// TestModelFamilyWildcard: a "model_family:*" requirement is satisfied by any
// concrete model_family tag (§5.3).
func TestModelFamilyWildcard(t *testing.T) {
	now := time.Unix(0, 0)
	j := cand("j", 0, now, "role:eng_worker", "model_family:*")
	if _, ok := Pick([]Candidate{j}, []string{"role:eng_worker", "model_family:opus"}, now); !ok {
		t.Fatal("model_family:* must be satisfied by model_family:opus")
	}
	if _, ok := Pick([]Candidate{j}, []string{"role:eng_worker"}, now); ok {
		t.Fatal("model_family:* requires SOME model_family tag")
	}
}

// TestOrderIsDeterministic: equal aged priority breaks ties by older enqueue then
// id, so the offer order is stable across runs (replayability).
func TestOrderIsDeterministic(t *testing.T) {
	now := time.Unix(100, 0)
	a := cand("a", 5, time.Unix(10, 0))
	b := cand("b", 5, time.Unix(10, 0))
	c := cand("c", 5, time.Unix(5, 0)) // older -> first
	got := Order([]Candidate{a, b, c}, nil, now)
	if len(got) != 3 || got[0].JobID != "c" || got[1].JobID != "a" || got[2].JobID != "b" {
		t.Fatalf("unstable order: %v", ids(got))
	}
}

func ids(cs []Candidate) []string {
	out := make([]string, len(cs))
	for i, c := range cs {
		out[i] = c.JobID
	}
	return out
}
