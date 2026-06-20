package scheduler

import (
	"testing"
	"time"
)

func cand(id string, prio int, enqueued time.Time, req ...string) Candidate {
	return Candidate{JobID: id, Priority: prio, EnqueuedAt: enqueued, RequiredCapabilities: req}
}

// TestAgedNiceToHaveBeatsFreshUrgent is the core §6.6 aging claim under the 1..10
// lower-is-more-urgent scale: a nice-to-have job (priority 8) that has waited long enough
// out-ranks a fresh urgent newcomer (priority 2), so nothing starves.
func TestAgedNiceToHaveBeatsFreshUrgent(t *testing.T) {
	base := time.Unix(0, 0)
	rate := time.Minute // 1 urgency point per minute waited
	// nice-to-have (8) waited 100 min -> effective = -8 + 100 = 92.
	nice := cand("nice", 8, base)
	// fresh urgent (2) enqueued now -> effective = -2 + 0 = -2.
	now := base.Add(100 * time.Minute)
	urgent := cand("urgent", 2, now)

	picked, ok := PickWith([]Candidate{urgent, nice}, nil, now, rate)
	if !ok {
		t.Fatal("expected a pick")
	}
	if picked.JobID != "nice" {
		t.Fatalf("aged nice-to-have should win (anti-starvation), got %s", picked.JobID)
	}
}

// TestFreshUrgentBeatsFreshNice: with no aging gap, the LOWER priority number (more
// urgent) decides — priority 1 beats priority 9.
func TestFreshUrgentBeatsFreshNice(t *testing.T) {
	now := time.Unix(1000, 0)
	picked, ok := PickWith([]Candidate{cand("nice", 9, now), cand("urgent", 1, now)}, nil, now, time.Minute)
	if !ok || picked.JobID != "urgent" {
		t.Fatalf("fresh urgent (priority 1) should win, got %+v ok=%v", picked, ok)
	}
}

// TestCapabilityFilterExcludesIneligible: a worker lacking a required capability
// is never offered the job, even if it is the highest-priority candidate.
func TestCapabilityFilterExcludesIneligible(t *testing.T) {
	now := time.Unix(0, 0)
	needsCodex := cand("needs", 1, now, "role:eng_worker", "model_family:codex") // urgent
	open := cand("open", 5, now)                                                 // default

	// a worker without model_family:codex cannot win `needs`, only `open`.
	picked, ok := Pick([]Candidate{needsCodex, open}, []string{"role:eng_worker", "model_family:opus"}, now)
	if !ok || picked.JobID != "open" {
		t.Fatalf("ineligible worker should fall to open job, got %+v ok=%v", picked, ok)
	}

	// a codex worker wins the more-urgent (priority 1) `needs`.
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

// TestCIReadyReviewBeatsStarvingNotReady: a CI-ready review is offered before a
// not-ready one EVEN IF the not-ready one is older (higher aged priority). A not-ready
// review can't be done, so it must not starve a reviewable one (the live #56 stall:
// a reviewer kept re-claiming an older CI-red review and never reached a CI-green one).
func TestCIReadyReviewBeatsStarvingNotReady(t *testing.T) {
	now := time.Unix(1000, 0)
	notReady := Candidate{JobID: "old-red", EnqueuedAt: time.Unix(10, 0), CIReady: false} // older
	ready := Candidate{JobID: "new-green", EnqueuedAt: time.Unix(900, 0), CIReady: true}  // newer
	got := Order([]Candidate{notReady, ready}, nil, now)
	if len(got) != 2 || got[0].JobID != "new-green" {
		t.Fatalf("CI-ready review must be first (anti-starvation), got %v", ids(got))
	}
}

func ids(cs []Candidate) []string {
	out := make([]string, len(cs))
	for i, c := range cs {
		out[i] = c.JobID
	}
	return out
}
