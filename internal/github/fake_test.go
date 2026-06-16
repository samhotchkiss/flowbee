package github

import (
	"context"
	"testing"
	"time"
)

// TestFakeRecordsAndScripts: the fake returns scripted PRs and records calls for
// dedupe/idempotency assertions, and a re-SetPR replaces (CI flip / SHA move).
func TestFakeRecordsAndScripts(t *testing.T) {
	ctx := context.Background()
	f := NewFake()
	f.SetPR(PullRequest{Number: 5, HeadRefOid: "h1", BaseRefOid: "b1", CIRollup: CIPending, UpdatedAt: time.Unix(1, 0)})
	f.SetRateLimit(RateLimit{Limit: 5000, Remaining: 4999})

	snap, err := f.BoardSweep(ctx)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if len(snap.PullRequests) != 1 || snap.PullRequests[0].Number != 5 {
		t.Fatalf("sweep PRs: %+v", snap.PullRequests)
	}
	if snap.RateLimit.Remaining != 4999 {
		t.Fatalf("rate limit not scripted: %+v", snap.RateLimit)
	}

	// a CI transition: re-script the same PR green.
	f.SetPR(PullRequest{Number: 5, HeadRefOid: "h1", BaseRefOid: "b1", CIRollup: CISuccess, UpdatedAt: time.Unix(2, 0)})
	pr, ok, err := f.PullRequest(ctx, 5)
	if err != nil || !ok {
		t.Fatalf("refetch ok=%v err=%v", ok, err)
	}
	if pr.CIRollup != CISuccess {
		t.Fatalf("CI not flipped: %s", pr.CIRollup)
	}

	// a refetch of an unknown PR is ok=false.
	if _, ok, _ := f.PullRequest(ctx, 999); ok {
		t.Fatalf("unknown PR returned ok=true")
	}

	calls := f.Calls()
	want := []string{"BoardSweep", "PullRequest(5)", "PullRequest(999)"}
	if len(calls) != len(want) {
		t.Fatalf("calls=%v want %v", calls, want)
	}
	for i := range want {
		if calls[i] != want[i] {
			t.Fatalf("call[%d]=%q want %q", i, calls[i], want[i])
		}
	}
}
