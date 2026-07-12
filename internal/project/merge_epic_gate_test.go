package project

import (
	"context"
	"testing"
	"time"

	"github.com/samhotchkiss/flowbee/internal/job"
	"github.com/samhotchkiss/flowbee/internal/store"
)

// epicMergingJob is mergingJob (merge_content_reverify_test.go) plus a repo scope, so
// the epic-PR detection (store.EpicForHeadSHA) has a repo to match against.
func epicMergingJob(t *testing.T, st *store.Store, id, repo, headSHA string) {
	t.Helper()
	ctx := context.Background()
	if _, err := st.SeedJob(ctx, store.SeedParams{
		ID: id, Kind: job.KindBuild, Flow: "build", Stage: "build", Role: job.RoleEngWorker,
		BaseSHA: "base-sha", Repo: repo, Now: time.Unix(1000, 0),
	}); err != nil {
		t.Fatal(err)
	}
	setMergingAuthorization(t, st, id, "base-sha", headSHA)
	if _, err := st.DB.ExecContext(ctx,
		`UPDATE jobs SET issue_number=42 WHERE id=?`, id); err != nil {
		t.Fatal(err)
	}
	if err := st.EnqueueOutbox(ctx, store.OutboxRow{
		JobID: id, Action: store.ActionEnqueueMerge, HeadSHA: headSHA, Payload: `{"pr_number":42}`,
	}); err != nil {
		t.Fatal(err)
	}
}

func epicFile(state, blockers, checklist string) string {
	return "---\ntitle: Foo\nscope:\n  - app/foo/**\n---\n\n" +
		"## Goal\n\nDo the thing.\n\n" +
		"## Steps\n\n" +
		"1. First step\nValidate: go test ./app/foo/...\n\n" +
		"2. Second step\nValidate: go test ./app/foo/bar/...\n\n" +
		"## Status\n\nUpdated: 2026-07-03T12:00:00Z\nCurrent: step 2/2\nState: " + state + "\n\n" +
		checklist + "\n\nBlockers: " + blockers + "\n"
}

const epicAllGreenChecklist = "- [x] Step 1 — first step (evidence: go test passed)\n" +
	"- [x] Step 2 — second step (evidence: go test bar passed)"

// TestEpicGateAllGreenMergesAutonomously: an epic PR whose ## Status claims State:
// done, every step checked with evidence, and no blockers, and whose diff stays
// inside scope: — merges autonomously exactly like an ordinary clean PR.
func TestEpicGateAllGreenMergesAutonomously(t *testing.T) {
	st, fake, sender, _ := newSender(t)
	ctx := context.Background()

	mustRegisterEpic(t, st, "russ", "2026-07-03-foo", "epic/2026-07-03-foo", "epics/2026-07-03-foo.md")
	fh := &fakeHistory{
		tip:     "t",
		diffOut: diffAdding("app/foo/a.go", "// x"),
		refTips: map[string]string{"refs/heads/epic/2026-07-03-foo": "epic-head-sha"},
		files:   map[string]map[string]string{"epic-head-sha": {"epics/2026-07-03-foo.md": epicFile("done", "none", epicAllGreenChecklist)}},
	}
	sender.WithHistory(fh, "main")
	epicMergingJob(t, st, "j", "russ", "epic-head-sha")
	setLiveGreenPR(fake, 42, "base-sha", "epic-head-sha")

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
		j, _ := st.GetJob(ctx, "j")
		t.Fatalf("an all-green epic PR must merge autonomously; calls=%v state=%s", fake.Calls(), j.State)
	}
	if j, _ := st.GetJob(ctx, "j"); j.State != job.StateMerging {
		t.Fatalf("state=%s, want merging (still in flight toward the merge queue)", j.State)
	}
}

// TestEpicGateUncheckedStepRoutesToHandoff: a claimed-done epic with one unchecked
// step is denied self-merge and routed to merge_handoff, naming the offending step.
func TestEpicGateUncheckedStepRoutesToHandoff(t *testing.T) {
	st, fake, sender, _ := newSender(t)
	ctx := context.Background()

	mustRegisterEpic(t, st, "russ", "2026-07-03-foo", "epic/2026-07-03-foo", "epics/2026-07-03-foo.md")
	checklist := "- [x] Step 1 — first step (evidence: go test passed)\n" +
		"- [ ] Step 2 — second step"
	fh := &fakeHistory{
		tip:     "t",
		diffOut: diffAdding("app/foo/a.go", "// x"),
		refTips: map[string]string{"refs/heads/epic/2026-07-03-foo": "epic-head-sha"},
		files:   map[string]map[string]string{"epic-head-sha": {"epics/2026-07-03-foo.md": epicFile("done", "none", checklist)}},
	}
	sender.WithHistory(fh, "main")
	epicMergingJob(t, st, "j", "russ", "epic-head-sha")
	setLiveGreenPR(fake, 42, "base-sha", "epic-head-sha")

	if _, err := sender.DrainOnce(ctx); err != nil {
		t.Fatalf("drain: %v", err)
	}
	for _, c := range fake.Calls() {
		if c == "EnqueueMergeQueue(42)" {
			t.Fatal("an epic PR with an unchecked step must NOT self-merge")
		}
	}
	j, _ := st.GetJob(ctx, "j")
	if j.State != job.StateMergeHandoff {
		t.Fatalf("state=%s, want merge_handoff", j.State)
	}
}

// TestEpicGateEmptyEvidenceRoutesToHandoff: a step checked [x] but with no evidence
// string is denied — "claimed but unverifiable" is the primary failure mode.
func TestEpicGateEmptyEvidenceRoutesToHandoff(t *testing.T) {
	st, fake, sender, _ := newSender(t)
	ctx := context.Background()

	mustRegisterEpic(t, st, "russ", "2026-07-03-foo", "epic/2026-07-03-foo", "epics/2026-07-03-foo.md")
	checklist := "- [x] Step 1 — first step (evidence: go test passed)\n" +
		"- [x] Step 2 — second step"
	fh := &fakeHistory{
		tip:     "t",
		diffOut: diffAdding("app/foo/a.go", "// x"),
		refTips: map[string]string{"refs/heads/epic/2026-07-03-foo": "epic-head-sha"},
		files:   map[string]map[string]string{"epic-head-sha": {"epics/2026-07-03-foo.md": epicFile("done", "none", checklist)}},
	}
	sender.WithHistory(fh, "main")
	epicMergingJob(t, st, "j", "russ", "epic-head-sha")
	setLiveGreenPR(fake, 42, "base-sha", "epic-head-sha")

	if _, err := sender.DrainOnce(ctx); err != nil {
		t.Fatalf("drain: %v", err)
	}
	for _, c := range fake.Calls() {
		if c == "EnqueueMergeQueue(42)" {
			t.Fatal("an epic PR with a step lacking evidence must NOT self-merge")
		}
	}
	if j, _ := st.GetJob(ctx, "j"); j.State != job.StateMergeHandoff {
		t.Fatalf("state=%s, want merge_handoff", j.State)
	}
}

// TestEpicGateStateNotDoneRoutesToHandoff: State: != "done" denies self-merge even
// with a fully checked, evidenced checklist.
func TestEpicGateStateNotDoneRoutesToHandoff(t *testing.T) {
	st, fake, sender, _ := newSender(t)
	ctx := context.Background()

	mustRegisterEpic(t, st, "russ", "2026-07-03-foo", "epic/2026-07-03-foo", "epics/2026-07-03-foo.md")
	fh := &fakeHistory{
		tip:     "t",
		diffOut: diffAdding("app/foo/a.go", "// x"),
		refTips: map[string]string{"refs/heads/epic/2026-07-03-foo": "epic-head-sha"},
		files:   map[string]map[string]string{"epic-head-sha": {"epics/2026-07-03-foo.md": epicFile("building", "none", epicAllGreenChecklist)}},
	}
	sender.WithHistory(fh, "main")
	epicMergingJob(t, st, "j", "russ", "epic-head-sha")
	setLiveGreenPR(fake, 42, "base-sha", "epic-head-sha")

	if _, err := sender.DrainOnce(ctx); err != nil {
		t.Fatalf("drain: %v", err)
	}
	for _, c := range fake.Calls() {
		if c == "EnqueueMergeQueue(42)" {
			t.Fatal("an epic PR with State != done must NOT self-merge")
		}
	}
	if j, _ := st.GetJob(ctx, "j"); j.State != job.StateMergeHandoff {
		t.Fatalf("state=%s, want merge_handoff", j.State)
	}
}

// TestEpicGateBlockersPresentRoutesToHandoff: a non-empty Blockers: line denies
// self-merge regardless of the checklist/state.
func TestEpicGateBlockersPresentRoutesToHandoff(t *testing.T) {
	st, fake, sender, _ := newSender(t)
	ctx := context.Background()

	mustRegisterEpic(t, st, "russ", "2026-07-03-foo", "epic/2026-07-03-foo", "epics/2026-07-03-foo.md")
	fh := &fakeHistory{
		tip:     "t",
		diffOut: diffAdding("app/foo/a.go", "// x"),
		refTips: map[string]string{"refs/heads/epic/2026-07-03-foo": "epic-head-sha"},
		files:   map[string]map[string]string{"epic-head-sha": {"epics/2026-07-03-foo.md": epicFile("done", "waiting on a design call", epicAllGreenChecklist)}},
	}
	sender.WithHistory(fh, "main")
	epicMergingJob(t, st, "j", "russ", "epic-head-sha")
	setLiveGreenPR(fake, 42, "base-sha", "epic-head-sha")

	if _, err := sender.DrainOnce(ctx); err != nil {
		t.Fatalf("drain: %v", err)
	}
	for _, c := range fake.Calls() {
		if c == "EnqueueMergeQueue(42)" {
			t.Fatal("an epic PR with Blockers: present must NOT self-merge")
		}
	}
	if j, _ := st.GetJob(ctx, "j"); j.State != job.StateMergeHandoff {
		t.Fatalf("state=%s, want merge_handoff", j.State)
	}
}

// TestEpicGateOutOfScopeFileRoutesToHandoff: a diff touching a path outside every
// declared scope: glob is denied even though the ## Status is fully green.
func TestEpicGateOutOfScopeFileRoutesToHandoff(t *testing.T) {
	st, fake, sender, _ := newSender(t)
	ctx := context.Background()

	mustRegisterEpic(t, st, "russ", "2026-07-03-foo", "epic/2026-07-03-foo", "epics/2026-07-03-foo.md")
	fh := &fakeHistory{
		tip:     "t",
		diffOut: diffAdding("app/other/evil.go", "// x"), // outside app/foo/**
		refTips: map[string]string{"refs/heads/epic/2026-07-03-foo": "epic-head-sha"},
		files:   map[string]map[string]string{"epic-head-sha": {"epics/2026-07-03-foo.md": epicFile("done", "none", epicAllGreenChecklist)}},
	}
	sender.WithHistory(fh, "main")
	epicMergingJob(t, st, "j", "russ", "epic-head-sha")
	setLiveGreenPR(fake, 42, "base-sha", "epic-head-sha")

	if _, err := sender.DrainOnce(ctx); err != nil {
		t.Fatalf("drain: %v", err)
	}
	for _, c := range fake.Calls() {
		if c == "EnqueueMergeQueue(42)" {
			t.Fatal("an epic PR touching an out-of-scope path must NOT self-merge")
		}
	}
	j, _ := st.GetJob(ctx, "j")
	if j.State != job.StateMergeHandoff {
		t.Fatalf("state=%s, want merge_handoff", j.State)
	}
}

// TestEpicGateNonEpicPRUnaffected: a job whose head SHA does not match ANY
// registered epic's branch tip (the overwhelmingly common, non-epic case) merges
// exactly as it did before Phase 3 — proving zero behavior change. Registering an
// UNRELATED epic for the same repo (a different head SHA) exercises the "scan finds
// no match" path, not just "no epics at all".
func TestEpicGateNonEpicPRUnaffected(t *testing.T) {
	st, fake, sender, _ := newSender(t)
	ctx := context.Background()

	mustRegisterEpic(t, st, "russ", "2026-07-03-foo", "epic/2026-07-03-foo", "epics/2026-07-03-foo.md")
	fh := &fakeHistory{
		tip:     "t",
		diffOut: diffAdding("docs/operating.md", "a new clarifying sentence"),
		refTips: map[string]string{"refs/heads/epic/2026-07-03-foo": "epic-head-sha"}, // unrelated epic branch tip
	}
	sender.WithHistory(fh, "main")
	epicMergingJob(t, st, "j", "russ", "ordinary-pr-head-sha") // NOT the epic's tip
	setLiveGreenPR(fake, 42, "base-sha", "ordinary-pr-head-sha")

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
		t.Fatal("an ordinary (non-epic) PR must merge autonomously, unaffected by an unrelated registered epic")
	}
	if j, _ := st.GetJob(ctx, "j"); j.State != job.StateMerging {
		t.Fatalf("state=%s, want merging", j.State)
	}
}

func mustRegisterEpic(t *testing.T, st *store.Store, repo, id, branch, filePath string) {
	t.Helper()
	if err := st.AddEpicRun(context.Background(), store.EpicRun{
		ID: id, Repo: repo, FilePath: filePath, Title: "Foo",
		Scope: []string{"app/foo/**"}, Branch: branch, TmuxName: "epic-" + id,
	}, time.Unix(500, 0)); err != nil {
		t.Fatalf("register epic: %v", err)
	}
}
