package github

import (
	"context"
	"testing"
	"time"
)

// TestFakeBoardSweepReturnsIssues: the fake returns scripted OPEN issues (F7
// direct-to-GitHub issues) alongside PRs, so the adopt sweep can import them.
func TestFakeBoardSweepReturnsIssues(t *testing.T) {
	ctx := context.Background()
	f := NewFake()
	f.SetIssue(Issue{Number: 42, UpdatedAt: time.Unix(1, 0), Labels: []string{"flowbee:adopt"}, Title: "t", Body: "b"})
	snap, err := f.BoardSweep(ctx)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if len(snap.Issues) != 1 || snap.Issues[0].Number != 42 {
		t.Fatalf("sweep issues: %+v", snap.Issues)
	}
	if snap.Issues[0].Body != "b" || len(snap.Issues[0].Labels) != 1 {
		t.Fatalf("issue facts not scripted: %+v", snap.Issues[0])
	}
}

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

// TestFakeCompensationWrites covers the M11 compensation Writer methods (§6.5.4):
// ConvertToDraft drafts a PR back; CancelCI records the cancelled SHA. Both are
// recorded for the once-per-action audit + compensation assertions.
func TestFakeCompensationWrites(t *testing.T) {
	ctx := context.Background()
	f := NewFake()
	f.SetPR(PullRequest{Number: 7, HeadRefOid: "h7", BaseRefOid: "main", IsDraft: false})

	if err := f.ConvertToDraft(ctx, 7); err != nil {
		t.Fatalf("convert to draft: %v", err)
	}
	if d := f.Drafted(); len(d) != 1 || d[0] != 7 {
		t.Fatalf("PR 7 must be drafted back, got %v", d)
	}
	if pr, _ := f.PRState(7); !pr.IsDraft {
		t.Fatalf("the drafted-back PR must read IsDraft=true")
	}

	if err := f.CancelCI(ctx, "dead-sha"); err != nil {
		t.Fatalf("cancel ci: %v", err)
	}
	if c := f.Cancelled(); len(c) != 1 || c[0] != "dead-sha" {
		t.Fatalf("the dead SHA's CI must be cancelled, got %v", c)
	}
}
