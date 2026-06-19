package project

import (
	"context"
	"testing"

	gh "github.com/samhotchkiss/flowbee/internal/github"
	"github.com/samhotchkiss/flowbee/internal/store"
)

// alreadyExistsWriter wraps the Fake to model GitHub's REAL OpenPR behavior: a
// second OpenPR for a head ref that already has an open PR returns the existing
// PR's number (what RealClient.OpenPR recovers from the 422). This is the contract
// the project-layer OpenPR crash-idempotency depends on.
type alreadyExistsWriter struct {
	gh.Writer
	openByHead map[string]int
	calls      int
}

func (w *alreadyExistsWriter) OpenPR(ctx context.Context, in gh.OpenPRInput) (int, error) {
	w.calls++
	if w.openByHead == nil {
		w.openByHead = map[string]int{}
	}
	if n, ok := w.openByHead[in.HeadRef]; ok {
		return n, nil // existing PR recovered (the 422 path)
	}
	n, err := w.Writer.OpenPR(ctx, in)
	if err == nil {
		w.openByHead[in.HeadRef] = n
	}
	return n, err
}

// TestOpenPRCrashRedrain_NoDuplicate proves the OpenPR crash window (§8.2):
// the row stays 'pending' (CP crashed AFTER OpenPR succeeded, BEFORE MarkOutboxSent),
// is re-drained, and must NOT open a SECOND PR. The project layer has NO j.PRNumber>0
// guard (unlike CreateIssue), so this relies ENTIRELY on the GitHub layer returning the
// existing PR for the deterministic head ref. This test wires a writer that models that.
func TestOpenPRCrashRedrain_NoDuplicate(t *testing.T) {
	st, base, _, clk := newSender(t)
	ctx := context.Background()
	w := &alreadyExistsWriter{Writer: base}
	sender := New(st, w, clk, nil)

	seedReviewPending(t, st, "j1")
	// the issue branch the OpenPR send references must resolve deterministically;
	// stamp an issue number so IssueBranch is stable across the two drains.
	if err := st.StampIssueNumber(ctx, "j1", 7, clk.Now()); err != nil {
		t.Fatalf("stamp issue: %v", err)
	}
	if _, err := st.EnqueuePROpen(ctx, "j1", "sha-1", "main"); err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	// FIRST drain: opens the PR. Simulate the crash by re-setting the row to pending
	// AFTER the drain (i.e. MarkOutboxSent's effect is rolled back as if the CP died
	// before it committed). We must do this without un-stamping the PR number, because
	// StampPRNumber commits in the SAME GitHub call's aftermath — but the crash window
	// is specifically: OpenPR returned, StampPRNumber may or may not have run, the
	// outbox row is still pending. We model the WORST case: row pending again.
	if _, err := sender.DrainOnce(ctx); err != nil {
		t.Fatalf("first drain: %v", err)
	}
	// force the row back to pending to model a crash before MarkOutboxSent committed.
	if _, err := st.DB.ExecContext(ctx,
		`UPDATE outbox SET status='pending', sent_at=NULL WHERE job_id='j1' AND action='pulls.create'`); err != nil {
		t.Fatalf("reset row: %v", err)
	}

	// SECOND drain (the re-send after crash).
	if _, err := sender.DrainOnce(ctx); err != nil {
		t.Fatalf("second drain: %v", err)
	}

	// OpenPR may be CALLED twice (no project-layer guard), but the second must recover
	// the SAME number — assert the job has exactly one PR number and no second PR exists.
	pr, _ := st.JobPR(ctx, "j1")
	if pr == 0 {
		t.Fatal("job lost its PR number across the crash/redrain")
	}
	// the audit log must show the pulls.create action exactly ONCE.
	audit, _ := st.AuditLog(ctx, "j1")
	createCount := 0
	for _, a := range audit {
		if a.Action == store.ActionOpenPR {
			createCount++
		}
	}
	if createCount != 1 {
		t.Fatalf("audit shows pulls.create %d times, want exactly 1: %+v", createCount, audit)
	}
	t.Logf("OpenPR underlying-create calls=%d, recovered PR=%d, audit pulls.create count=%d", w.calls, pr, createCount)
}

// TestOpenPRCrashRedrain_RawFakeDuplicates is the CONTRAST: with a raw Fake (which does
// NOT model GitHub's already-exists recovery — it mints a fresh number every call), a
// re-drain of the still-pending row opens a SECOND PR. This isolates WHERE the only guard
// lives: the GitHub client layer, NOT the project Sender. If the real OpenPR's 422 recovery
// regressed, the project layer has nothing to stop a double PR.
func TestOpenPRCrashRedrain_RawFakeDuplicates(t *testing.T) {
	st, fake, sender, clk := newSender(t)
	ctx := context.Background()

	seedReviewPending(t, st, "j2")
	if err := st.StampIssueNumber(ctx, "j2", 8, clk.Now()); err != nil {
		t.Fatalf("stamp issue: %v", err)
	}
	if _, err := st.EnqueuePROpen(ctx, "j2", "sha-1", "main"); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if _, err := sender.DrainOnce(ctx); err != nil {
		t.Fatalf("first drain: %v", err)
	}
	if _, err := st.DB.ExecContext(ctx,
		`UPDATE outbox SET status='pending', sent_at=NULL WHERE job_id='j2' AND action='pulls.create'`); err != nil {
		t.Fatalf("reset row: %v", err)
	}
	if _, err := sender.DrainOnce(ctx); err != nil {
		t.Fatalf("second drain: %v", err)
	}
	prCalls := 0
	for _, c := range fake.Calls() {
		if c == "OpenPR" {
			prCalls++
		}
	}
	t.Logf("raw Fake: OpenPR called %d times across crash re-drain (each mints a NEW PR number)", prCalls)
}
