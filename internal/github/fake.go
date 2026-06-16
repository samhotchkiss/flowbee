package github

import (
	"context"
	"sync"
)

// Fake is an in-memory, scriptable Client (BUILD.md §6.4). It records every call
// (for dedupe/idempotency assertions) and lets a test script the board the sweep
// returns — driving supersession, CI transitions, the merged terminal fact. No
// real GitHub: ALL reconcile-IN tests from M6 on run against this.
//
// It is safe for concurrent use (the control plane sweeps from one goroutine but
// tests may drive it from several).
type Fake struct {
	mu    sync.Mutex
	prs   map[int]PullRequest
	rate  RateLimit
	calls []string // "BoardSweep" / "PullRequest(N)" in order, for assertions
}

// NewFake builds an empty Fake with a healthy starting rate-limit budget.
func NewFake() *Fake {
	return &Fake{
		prs:  map[int]PullRequest{},
		rate: RateLimit{Limit: 5000, Remaining: 5000},
	}
}

// SetPR scripts (or replaces) one PR in the fake's board. A later call with the
// same number — e.g. a new HeadRefOid, a flipped CIRollup, or Merged=true — is how
// a test drives a CI transition / SHA move / terminal merge through the sweep.
func (f *Fake) SetPR(pr PullRequest) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.prs[pr.Number] = pr
}

// SetRateLimit scripts the budget gauge the sweep self-meters (I-14).
func (f *Fake) SetRateLimit(r RateLimit) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rate = r
}

// Calls returns the recorded call log (a copy) for dedupe/idempotency assertions.
func (f *Fake) Calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

// BoardSweep returns the scripted snapshot and records the call.
func (f *Fake) BoardSweep(ctx context.Context) (BoardSnapshot, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "BoardSweep")
	var snap BoardSnapshot
	for _, pr := range f.prs {
		snap.PullRequests = append(snap.PullRequests, pr)
	}
	snap.RateLimit = f.rate
	return snap, nil
}

// PullRequest returns one scripted PR (the targeted refetch) and records the call.
// The returned fact is the REAL scripted state — so a forged webhook claiming a
// PR is approved/merged, when the script says otherwise, refetches the true state
// and can never fast-track (§8.1.3, I-3).
func (f *Fake) PullRequest(ctx context.Context, number int) (PullRequest, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, sprintfPR(number))
	pr, ok := f.prs[number]
	return pr, ok, nil
}

func sprintfPR(n int) string {
	// tiny local helper to avoid importing fmt just for the call log.
	const digits = "0123456789"
	if n == 0 {
		return "PullRequest(0)"
	}
	var b []byte
	neg := n < 0
	if neg {
		n = -n
	}
	for n > 0 {
		b = append([]byte{digits[n%10]}, b...)
		n /= 10
	}
	if neg {
		b = append([]byte{'-'}, b...)
	}
	return "PullRequest(" + string(b) + ")"
}
