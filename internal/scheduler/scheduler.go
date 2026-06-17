// Package scheduler is the §6.6 scheduler core: a topological walk over the job
// DAG (blocked_by), priority ordering, aging so nothing starves, and capability
// matching on the attested set. The hard ordering/match logic lives here as PURE
// functions over values (clock readings injected) so it is unit-testable without
// a DB; internal/store wires Pick into the long-poll claim loop, and a hand-rolled
// durable-timer drives the no_eligible_worker alarm (I-6).
//
// This is NOT a deterministic-core package (it is allowed to reason about the
// injected `now`), but it reads no clock itself — `now` is always passed in — so
// the same inputs always produce the same offer order.
package scheduler

import (
	"sort"
	"time"

	"github.com/samhotchkiss/flowbee/internal/job"
)

// AgingRate is how fast a waiting job's effective priority climbs: one priority
// point per AgingRate of wait. With the default, a job that has waited an hour
// out-ranks a fresh job priority+1 (3600s/600s = 6 points). Tuned by tests via
// the explicit parameter on PickWith.
const AgingRate = 600 * time.Second

// Candidate is a leasable (`ready`) job as the scheduler sees it: enough to rank
// and to test capability eligibility. It is folded from the jobs projection.
type Candidate struct {
	JobID                string
	Priority             int
	EnqueuedAt           time.Time
	RequiredCapabilities []string
	// CIReady is set ONLY for review candidates: true when the PR's reconciled CI is
	// green (the review can actually be done). A not-ready review can't progress, so a
	// ready one must be offered first — else a CI-red review starves CI-green ones (a
	// reviewer claims the oldest, finds CI not ready, backs off, re-claims the same).
	// Non-review candidates leave it false, so it has no effect on their ordering.
	CIReady bool
}

// EffectivePriority is the §6.6 aged priority: base priority plus one point per
// AgingRate of wait since EnqueuedAt. Higher wins. Pure in (now, agingRate).
func EffectivePriority(c Candidate, now time.Time, agingRate time.Duration) int {
	wait := now.Sub(c.EnqueuedAt)
	if wait < 0 {
		wait = 0
	}
	return c.Priority + int(wait/agingRate)
}

// Pick chooses the best candidate a worker with the given attested capabilities
// may win: among capability-eligible candidates, the one with the highest aged
// priority (ties broken by older EnqueuedAt, then JobID for determinism). Returns
// ok=false if no candidate is eligible. Uses the default AgingRate.
func Pick(cands []Candidate, attested []string, now time.Time) (Candidate, bool) {
	return PickWith(cands, attested, now, AgingRate)
}

// PickWith is Pick with an explicit aging rate (for tests).
func PickWith(cands []Candidate, attested []string, now time.Time, agingRate time.Duration) (Candidate, bool) {
	eligible := make([]Candidate, 0, len(cands))
	for _, c := range cands {
		if job.CapabilitiesSatisfy(attested, c.RequiredCapabilities) {
			eligible = append(eligible, c)
		}
	}
	if len(eligible) == 0 {
		return Candidate{}, false
	}
	sort.SliceStable(eligible, func(i, k int) bool {
		ei := EffectivePriority(eligible[i], now, agingRate)
		ek := EffectivePriority(eligible[k], now, agingRate)
		if ei != ek {
			return ei > ek // higher aged priority first
		}
		if !eligible[i].EnqueuedAt.Equal(eligible[k].EnqueuedAt) {
			return eligible[i].EnqueuedAt.Before(eligible[k].EnqueuedAt) // older first
		}
		return eligible[i].JobID < eligible[k].JobID
	})
	return eligible[0], true
}

// Order returns all candidates a worker may win, best-first (same ranking as
// Pick). Used by the long-poll loop to try candidates in offer order until one
// is claimed (the atomic claim is the correctness backstop).
func Order(cands []Candidate, attested []string, now time.Time) []Candidate {
	eligible := make([]Candidate, 0, len(cands))
	for _, c := range cands {
		if job.CapabilitiesSatisfy(attested, c.RequiredCapabilities) {
			eligible = append(eligible, c)
		}
	}
	sort.SliceStable(eligible, func(i, k int) bool {
		// CI-ready reviews first: a not-ready review can't be done, so it must never
		// be offered ahead of a ready one (anti-starvation). No effect when both equal
		// (all non-review candidates, both false).
		if eligible[i].CIReady != eligible[k].CIReady {
			return eligible[i].CIReady
		}
		ei := EffectivePriority(eligible[i], now, AgingRate)
		ek := EffectivePriority(eligible[k], now, AgingRate)
		if ei != ek {
			return ei > ek
		}
		if !eligible[i].EnqueuedAt.Equal(eligible[k].EnqueuedAt) {
			return eligible[i].EnqueuedAt.Before(eligible[k].EnqueuedAt)
		}
		return eligible[i].JobID < eligible[k].JobID
	})
	return eligible
}
