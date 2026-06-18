package project

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/samhotchkiss/flowbee/internal/job"
	"github.com/samhotchkiss/flowbee/internal/store"
)

// mergingJob seeds a build job sitting in `merging` with base/head SHAs set (the state a
// dispatched autonomous self-merge is in when project-out picks up its merge row).
func mergingJob(t *testing.T, st *store.Store, id string) {
	t.Helper()
	ctx := context.Background()
	if _, err := st.SeedJob(ctx, store.SeedParams{
		ID: id, Kind: job.KindBuild, Flow: "build", Stage: "build", Role: job.RoleEngWorker,
		BaseSHA: "base-sha", Now: time.Unix(1000, 0),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB.ExecContext(ctx,
		`UPDATE jobs SET state='merging', head_sha='head-sha', issue_number=42 WHERE id=?`, id); err != nil {
		t.Fatal(err)
	}
	if err := st.EnqueueOutbox(ctx, store.OutboxRow{
		JobID: id, Action: store.ActionEnqueueMerge, Payload: `{"pr_number":42}`,
	}); err != nil {
		t.Fatal(err)
	}
}

// diffAdding builds a minimal unified diff that adds one line to path.
func diffAdding(path, line string) string {
	return "diff --git a/" + path + " b/" + path + "\n" +
		"--- a/" + path + "\n+++ b/" + path + "\n@@ -0,0 +1 @@\n+" + line + "\n"
}

// TestAutonomousMergeDeniedWhenActualDiffHitsDenylist: project-out re-checks the ACTUAL
// base..head diff (from the mirror) before an autonomous merge; if it touches a denylisted
// path — even though the worker's reported patch was clean — the merge is NOT sent and the
// job is routed to the HUMAN merge gate (merge_handoff). This is the worker-say-so backstop.
func TestAutonomousMergeDeniedWhenActualDiffHitsDenylist(t *testing.T) {
	st, fake, sender, _ := newSender(t)
	ctx := context.Background()
	// the mirror's REAL diff touches a denylisted path (e.g. a sneaked source edit).
	sender.WithHistory(&fakeHistory{tip: "t", diffOut: diffAdding("internal/engine/engine.go", "// x")}, "main")
	mergingJob(t, st, "j")

	if _, err := sender.DrainOnce(ctx); err != nil {
		t.Fatalf("drain: %v", err)
	}

	for _, c := range fake.Calls() {
		if c == "EnqueueMergeQueue(42)" {
			t.Fatal("an autonomous merge with a denylisted ACTUAL diff was sent — must route to handoff")
		}
	}
	j, _ := st.GetJob(ctx, "j")
	if j.State != job.StateMergeHandoff {
		t.Fatalf("state=%s, want merge_handoff (denylisted actual diff downgrades to the human gate)", j.State)
	}
}

// TestAutonomousMergeDeniedWhenActualDiffLeaksSecret: the full content gate runs on the
// ACTUAL diff, so a secret introduced on the real branch (in a NON-denylisted file the
// worker under-reported) also blocks the autonomous merge — not just denylisted paths.
func TestAutonomousMergeDeniedWhenActualDiffLeaksSecret(t *testing.T) {
	st, fake, sender, _ := newSender(t)
	ctx := context.Background()
	secret := `aws_secret_access_key = "AKIAIOSFODNN7EXAMPLEKEYDATA0123456789abcd"`
	sender.WithHistory(&fakeHistory{tip: "t", diffOut: diffAdding("docs/notes.md", secret)}, "main")
	mergingJob(t, st, "j")

	if _, err := sender.DrainOnce(ctx); err != nil {
		t.Fatalf("drain: %v", err)
	}
	for _, c := range fake.Calls() {
		if c == "EnqueueMergeQueue(42)" {
			t.Fatal("an autonomous merge whose ACTUAL diff leaks a secret was sent — must route to handoff")
		}
	}
	if j, _ := st.GetJob(ctx, "j"); j.State != job.StateMergeHandoff {
		t.Fatalf("state=%s, want merge_handoff (secret in actual diff)", j.State)
	}
}

// TestAutonomousMergeProceedsWhenActualDiffClean: a clean actual diff (docs only) merges
// autonomously as before — the cross-check is additive, not a blanket block.
func TestAutonomousMergeProceedsWhenActualDiffClean(t *testing.T) {
	st, fake, sender, _ := newSender(t)
	ctx := context.Background()
	sender.WithHistory(&fakeHistory{tip: "t", diffOut: diffAdding("docs/operating.md", "a new clarifying sentence")}, "main")
	mergingJob(t, st, "j")

	if _, err := sender.DrainOnce(ctx); err != nil {
		t.Fatalf("drain: %v", err)
	}

	sent := false
	for _, c := range fake.Calls() {
		if c == "EnqueueMergeQueue(42)" {
			sent = true
		}
	}
	if !sent {
		t.Fatal("a clean actual diff must merge autonomously (EnqueueMergeQueue not called)")
	}
}

// TestAutonomousMergeRetriesWhenVerifyFails: if the CP can't compute the actual diff
// (transient fetch/diff error), the merge RETRIES — it must never silently merge unverified
// content, nor permanently strand on a transient git error.
func TestAutonomousMergeRetriesWhenVerifyFails(t *testing.T) {
	st, fake, sender, _ := newSender(t)
	ctx := context.Background()
	sender.WithHistory(&fakeHistory{tip: "t", diffErr: errors.New("mirror offline")}, "main")
	mergingJob(t, st, "j")

	_, _ = sender.DrainOnce(ctx)

	for _, c := range fake.Calls() {
		if c == "EnqueueMergeQueue(42)" {
			t.Fatal("merge was sent despite an unverifiable diff — must retry, not merge blind")
		}
	}
	j, _ := st.GetJob(ctx, "j")
	if j.State != job.StateMerging {
		t.Fatalf("state=%s, want merging (a verify error retries, not handoff/merge)", j.State)
	}
	row, ok, _ := st.NextPendingOutbox(ctx)
	if !ok || row.Action != store.ActionEnqueueMerge {
		t.Fatal("the merge row must remain pending for retry after a verify error")
	}
}
