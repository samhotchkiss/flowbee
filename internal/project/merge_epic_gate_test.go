package project

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/samhotchkiss/flowbee/internal/job"
	"github.com/samhotchkiss/flowbee/internal/store"
)

// epicMergingJob is mergingJob (merge_content_reverify_test.go) plus a repo scope, so
// the epic-PR detection (store.EpicForHeadSHA) has a repo to match against.
func epicMergingJob(t *testing.T, st *store.Store, id, repo, headSHA string) {
	t.Helper()
	epicMergingJobWithBase(t, st, id, repo, "base-sha", headSHA)
}

// epicMergingJobWithBase is epicMergingJob with an explicit merge-authorization base SHA,
// so a test can drive the empty-base fail-closed path (base == "") — the verdict is minted
// bound to that base, so an empty base skips the (base-guarded) content re-verify and
// leaves the epic gate as the sole remaining check.
func epicMergingJobWithBase(t *testing.T, st *store.Store, id, repo, base, headSHA string) {
	t.Helper()
	ctx := context.Background()
	if _, err := st.SeedJob(ctx, store.SeedParams{
		ID: id, Kind: job.KindBuild, Flow: "build", Stage: "build", Role: job.RoleEngWorker,
		BaseSHA: "base-sha", Repo: repo, Now: time.Unix(1000, 0),
	}); err != nil {
		t.Fatal(err)
	}
	setMergingAuthorization(t, st, id, base, headSHA)
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

const epicSpecPath = "epics/2026-07-03-foo.md"

// epicFilesBaseHead serves the epic spec at BOTH the launch-pinned PR base ("base-sha",
// what epicMergingJob authorizes) and the PR head ("epic-head-sha"). Review M1: the gate
// reads the Goal/Steps contract from the PINNED base and only the claimed ## Status from
// head, so a test must provide both. The base carries the SAME ## Steps as head (an
// unedited, spec-immutable contract → no drift); its own ## Status is irrelevant (only
// its Steps/Goal are read at the base).
func epicFilesBaseHead(head string) map[string]map[string]string {
	return map[string]map[string]string{
		"base-sha":      {epicSpecPath: epicFile("building", "none", "")},
		"epic-head-sha": {epicSpecPath: head},
	}
}

// TestEpicGateAllGreenMergesAutonomously: an epic PR whose ## Status claims State:
// done, every step checked with evidence, and no blockers, and whose diff stays
// inside scope: — merges autonomously exactly like an ordinary clean PR. The diff
// deliberately INCLUDES the epic's own epics/<slug>.md (review F1): every REAL epic
// PR touches it, because epics/INSTRUCTIONS.md mandates ## Status updates on the
// epic's own branch — so the realistic all-green case is scope-globs + the epic
// file, and the file must be implicitly in scope or the lane could never self-merge
// anything (the catch-22 the review caught).
func TestEpicGateAllGreenMergesAutonomously(t *testing.T) {
	st, fake, sender, _ := newSender(t)
	ctx := context.Background()

	mustRegisterEpic(t, st, "russ", "2026-07-03-foo", "epic/2026-07-03-foo", "epics/2026-07-03-foo.md")
	fh := &fakeHistory{
		tip: "t",
		diffOut: diffAdding("app/foo/a.go", "// x") +
			diffAdding("epics/2026-07-03-foo.md", "- [x] Step 2 — second step (evidence: go test bar passed)"),
		refTips: map[string]string{"refs/heads/epic/2026-07-03-foo": "epic-head-sha"},
		files:   epicFilesBaseHead(epicFile("done", "none", epicAllGreenChecklist)),
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
		t.Fatalf("an all-green epic PR (own status-file commits included) must merge autonomously; calls=%v state=%s", fake.Calls(), j.State)
	}
	if j, _ := st.GetJob(ctx, "j"); j.State != job.StateMerging {
		t.Fatalf("state=%s, want merging (still in flight toward the merge queue)", j.State)
	}
}

// TestEpicGateExplainerInScopeMergesAutonomously (plan §15.14 / review F1 parity): an
// all-green epic PR whose diff touches ONLY in-scope source, the epic's own spec .md,
// AND its epics/<slug>-explainer.html merges autonomously — the explainer is implicitly
// in scope exactly like the spec file, so maintaining the mandated human-facing explainer
// never trips the scope gate.
func TestEpicGateExplainerInScopeMergesAutonomously(t *testing.T) {
	st, fake, sender, _ := newSender(t)
	ctx := context.Background()

	mustRegisterEpic(t, st, "russ", "2026-07-03-foo", "epic/2026-07-03-foo", "epics/2026-07-03-foo.md")
	fh := &fakeHistory{
		tip: "t",
		diffOut: diffAdding("app/foo/a.go", "// x") +
			diffAdding("epics/2026-07-03-foo.md", "- [x] Step 2 — second step (evidence: go test bar passed)") +
			diffAdding("epics/2026-07-03-foo-explainer.html", "<p>as-built</p>"),
		refTips: map[string]string{"refs/heads/epic/2026-07-03-foo": "epic-head-sha"},
		files:   epicFilesBaseHead(epicFile("done", "none", epicAllGreenChecklist)),
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
		t.Fatalf("an epic PR touching only in-scope + its spec + its explainer must merge; calls=%v state=%s", fake.Calls(), j.State)
	}
}

// TestEpicGateHeadScopeWideningIgnored (review M1a): the gate matches touched paths
// against the LAUNCH-PINNED scope (e.Scope), never the head frontmatter. A lying head
// that widens scope: to '**' cannot admit an out-of-pinned-scope file — it still routes
// to handoff.
func TestEpicGateHeadScopeWideningIgnored(t *testing.T) {
	st, fake, sender, _ := newSender(t)
	ctx := context.Background()

	mustRegisterEpic(t, st, "russ", "2026-07-03-foo", "epic/2026-07-03-foo", "epics/2026-07-03-foo.md")
	// head widens scope to '**' (the lie); base keeps the pinned app/foo/** + 2 steps.
	headWideScope := "---\ntitle: Foo\nscope:\n  - '**'\n---\n\n" +
		"## Goal\n\nDo the thing.\n\n" +
		"## Steps\n\n1. First step\nValidate: go test ./app/foo/...\n\n2. Second step\nValidate: go test ./app/foo/bar/...\n\n" +
		"## Status\n\nUpdated: 2026-07-03T12:00:00Z\nCurrent: step 2/2\nState: done\n\n" +
		epicAllGreenChecklist + "\n\nBlockers: none\n"
	fh := &fakeHistory{
		tip:     "t",
		diffOut: diffAdding("internal/secret/x.go", "// out of pinned scope"), // only head's '**' would admit it
		refTips: map[string]string{"refs/heads/epic/2026-07-03-foo": "epic-head-sha"},
		files: map[string]map[string]string{
			"base-sha":      {epicSpecPath: epicFile("building", "none", "")},
			"epic-head-sha": {epicSpecPath: headWideScope},
		},
	}
	sender.WithHistory(fh, "main")
	epicMergingJob(t, st, "j", "russ", "epic-head-sha")
	setLiveGreenPR(fake, 42, "base-sha", "epic-head-sha")

	if _, err := sender.DrainOnce(ctx); err != nil {
		t.Fatalf("drain: %v", err)
	}
	for _, c := range fake.Calls() {
		if c == "EnqueueMergeQueue(42)" {
			t.Fatal("widening scope: to '**' at head must NOT let an out-of-pinned-scope file self-merge")
		}
	}
	if j, _ := st.GetJob(ctx, "j"); j.State != job.StateMergeHandoff {
		t.Fatalf("state=%s, want merge_handoff", j.State)
	}
}

// TestEpicGateStepsDriftAtHeadRoutesToHandoff (review M1c): a head whose ## Steps differ
// from the launch-pinned ones is a spec-immutability breach and is denied — an agent
// cannot shrink/renumber its own contract at head to self-certify a smaller checklist.
func TestEpicGateStepsDriftAtHeadRoutesToHandoff(t *testing.T) {
	st, fake, sender, _ := newSender(t)
	ctx := context.Background()

	mustRegisterEpic(t, st, "russ", "2026-07-03-foo", "epic/2026-07-03-foo", "epics/2026-07-03-foo.md")
	// head DROPS Step 2 and marks its single remaining step done (in scope, evidenced) —
	// only the pinned-Steps comparison catches it.
	headOneStep := "---\ntitle: Foo\nscope:\n  - app/foo/**\n---\n\n" +
		"## Goal\n\nDo the thing.\n\n" +
		"## Steps\n\n1. First step\nValidate: go test ./app/foo/...\n\n" +
		"## Status\n\nUpdated: 2026-07-03T12:00:00Z\nCurrent: step 1/1\nState: done\n\n" +
		"- [x] Step 1 — first step (evidence: go test passed)\n\nBlockers: none\n"
	fh := &fakeHistory{
		tip:     "t",
		diffOut: diffAdding("app/foo/a.go", "// x") + diffAdding("epics/2026-07-03-foo.md", "status"),
		refTips: map[string]string{"refs/heads/epic/2026-07-03-foo": "epic-head-sha"},
		files: map[string]map[string]string{
			"base-sha":      {epicSpecPath: epicFile("building", "none", "")}, // pinned: 2 steps
			"epic-head-sha": {epicSpecPath: headOneStep},                      // head: 1 step
		},
	}
	sender.WithHistory(fh, "main")
	epicMergingJob(t, st, "j", "russ", "epic-head-sha")
	setLiveGreenPR(fake, 42, "base-sha", "epic-head-sha")

	if _, err := sender.DrainOnce(ctx); err != nil {
		t.Fatalf("drain: %v", err)
	}
	for _, c := range fake.Calls() {
		if c == "EnqueueMergeQueue(42)" {
			t.Fatal("a head that dropped a pinned step must NOT self-merge")
		}
	}
	if j, _ := st.GetJob(ctx, "j"); j.State != job.StateMergeHandoff {
		t.Fatalf("state=%s, want merge_handoff (spec-immutability breach)", j.State)
	}
}

// TestEpicGateOtherEpicsFileIsStillOutOfScope (review F1): only the epic's OWN
// spec file is implicitly in scope; a diff touching a DIFFERENT epic's file must
// still fail the scope-honesty check and route to handoff.
func TestEpicGateOtherEpicsFileIsStillOutOfScope(t *testing.T) {
	st, fake, sender, _ := newSender(t)
	ctx := context.Background()

	mustRegisterEpic(t, st, "russ", "2026-07-03-foo", "epic/2026-07-03-foo", "epics/2026-07-03-foo.md")
	fh := &fakeHistory{
		tip: "t",
		diffOut: diffAdding("epics/2026-07-03-foo.md", "- [x] Step 2 (evidence: ...)") +
			diffAdding("epics/2026-07-01-other-epic.md", "tampered status"),
		refTips: map[string]string{"refs/heads/epic/2026-07-03-foo": "epic-head-sha"},
		files:   epicFilesBaseHead(epicFile("done", "none", epicAllGreenChecklist)),
	}
	sender.WithHistory(fh, "main")
	epicMergingJob(t, st, "j", "russ", "epic-head-sha")
	setLiveGreenPR(fake, 42, "base-sha", "epic-head-sha")

	if _, err := sender.DrainOnce(ctx); err != nil {
		t.Fatalf("drain: %v", err)
	}
	for _, c := range fake.Calls() {
		if c == "EnqueueMergeQueue(42)" {
			t.Fatal("a diff touching ANOTHER epic's file must NOT self-merge")
		}
	}
	if j, _ := st.GetJob(ctx, "j"); j.State != job.StateMergeHandoff {
		t.Fatalf("state=%s, want merge_handoff (another epic's file is out of scope)", j.State)
	}
}

// TestEpicGateTransientFetchErrorRetries (review F2): a transient mirror error
// fetching a LIVE epic's branch during detection must RETRY the merge (row stays
// pending, job stays merging) — never merge blind (the PR might be that epic's,
// unevidenced) and never handoff (nothing is proven wrong with it either).
func TestEpicGateTransientFetchErrorRetries(t *testing.T) {
	st, fake, sender, _ := newSender(t)
	ctx := context.Background()

	mustRegisterEpic(t, st, "russ", "2026-07-03-foo", "epic/2026-07-03-foo", "epics/2026-07-03-foo.md")
	fh := &fakeHistory{
		tip:       "t",
		diffOut:   diffAdding("docs/notes.md", "clean ordinary change"),
		fetchErrs: map[string]error{"epic/2026-07-03-foo": errors.New("connection reset by peer")},
	}
	sender.WithHistory(fh, "main")
	epicMergingJob(t, st, "j", "russ", "some-head-sha")
	setLiveGreenPR(fake, 42, "base-sha", "some-head-sha")

	_, _ = sender.DrainOnce(ctx)

	for _, c := range fake.Calls() {
		if c == "EnqueueMergeQueue(42)" {
			t.Fatal("merge was sent despite epic-PR detection failing — must retry, not merge blind")
		}
	}
	j, _ := st.GetJob(ctx, "j")
	if j.State != job.StateMerging {
		t.Fatalf("state=%s, want merging (a detection error retries, not handoff/merge)", j.State)
	}
	row, ok, _ := st.NextPendingOutbox(ctx)
	if !ok || row.Action != store.ActionEnqueueMerge {
		t.Fatal("the merge row must remain pending for retry after a detection error")
	}
}

// TestEpicGateMissingEpicBranchIsOrdinaryPR (review F2's clean-non-match half): a
// registered epic whose branch does not exist at origin yet (git's "couldn't find
// remote ref" — a just-launched, not-yet-pushed epic) must NOT block an ordinary
// PR's merge; the PR merges via the ordinary path.
func TestEpicGateMissingEpicBranchIsOrdinaryPR(t *testing.T) {
	st, fake, sender, _ := newSender(t)
	ctx := context.Background()

	mustRegisterEpic(t, st, "russ", "2026-07-03-foo", "epic/2026-07-03-foo", "epics/2026-07-03-foo.md")
	fh := &fakeHistory{
		tip:     "t",
		diffOut: diffAdding("docs/notes.md", "clean ordinary change"),
		fetchErrs: map[string]error{
			"epic/2026-07-03-foo": errors.New("fetch epic/2026-07-03-foo: fatal: couldn't find remote ref refs/heads/epic/2026-07-03-foo"),
		},
	}
	sender.WithHistory(fh, "main")
	epicMergingJob(t, st, "j", "russ", "some-head-sha")
	setLiveGreenPR(fake, 42, "base-sha", "some-head-sha")

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
		t.Fatalf("an ordinary PR must merge even while a registered epic's branch is un-pushed; calls=%v state=%s", fake.Calls(), j.State)
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
		files:   epicFilesBaseHead(epicFile("done", "none", checklist)),
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
		files:   epicFilesBaseHead(epicFile("done", "none", checklist)),
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
		files:   epicFilesBaseHead(epicFile("building", "none", epicAllGreenChecklist)),
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
		files:   epicFilesBaseHead(epicFile("done", "waiting on a design call", epicAllGreenChecklist)),
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
		files:   epicFilesBaseHead(epicFile("done", "none", epicAllGreenChecklist)),
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

// TestEpicGateEmptyBaseFailsClosedToHandoff (review m4, phase-4 residue): an epic-DETECTED
// PR whose BaseSHA is empty cannot be contract-verified — there is no launch-pinned ref to
// read the frozen ## Steps / scope from, and no base..head diff to scope-check. epicDenyReason
// runs OUTSIDE the base-guarded content re-verify block (project.go), so the epic gate STILL
// fires on an empty base and routes the PR to merge_handoff with the explicit "no base SHA"
// reason rather than silently skipping its own gate and merging blind.
func TestEpicGateEmptyBaseFailsClosedToHandoff(t *testing.T) {
	st, fake, sender, _ := newSender(t)
	ctx := context.Background()

	mustRegisterEpic(t, st, "russ", "2026-07-03-foo", "epic/2026-07-03-foo", "epics/2026-07-03-foo.md")
	// the head SHA matches the epic's branch tip, so detection FINDS the epic; but with no
	// base SHA the gate has no launch-pinned contract to verify against and must fail closed.
	fh := &fakeHistory{
		tip:     "t",
		refTips: map[string]string{"refs/heads/epic/2026-07-03-foo": "epic-head-sha"},
	}
	sender.WithHistory(fh, "main")
	epicMergingJobWithBase(t, st, "j", "russ", "", "epic-head-sha")
	setLiveGreenPR(fake, 42, "", "epic-head-sha")

	if _, err := sender.DrainOnce(ctx); err != nil {
		t.Fatalf("drain: %v", err)
	}
	for _, c := range fake.Calls() {
		if c == "EnqueueMergeQueue(42)" {
			t.Fatal("an epic PR with NO base SHA must NOT self-merge — it cannot be contract-verified")
		}
	}
	j, _ := st.GetJob(ctx, "j")
	if j.State != job.StateMergeHandoff {
		t.Fatalf("state=%s, want merge_handoff (empty base fails closed)", j.State)
	}
	// the handoff reason must name the no-base cause (epicDenyReason's empty-base branch),
	// proving it routed via the EPIC gate specifically and not some incidental path.
	events, err := st.LoadEvents(ctx, "j")
	if err != nil {
		t.Fatalf("load events: %v", err)
	}
	var reason string
	for _, ev := range events {
		if ev.ToState == job.StateMergeHandoff {
			reason = ev.Payload.RevokeReason
		}
	}
	if !strings.Contains(reason, "no base SHA") {
		t.Fatalf("handoff reason must name the missing base SHA, got %q", reason)
	}
}

// TestEpicGateNonEpicEmptyBaseUnaffected (phase-4 residue control): a NON-epic PR with an
// empty base SHA is NOT routed to handoff by the EPIC gate — epicDenyReason returns a clean
// non-match, so the PR proceeds to the merge queue exactly as it would with the epic gate
// absent. This isolates the empty-base fail-closed behavior above to epic-DETECTED PRs only
// (the empty base itself is not what routes to handoff).
func TestEpicGateNonEpicEmptyBaseUnaffected(t *testing.T) {
	st, fake, sender, _ := newSender(t)
	ctx := context.Background()

	mustRegisterEpic(t, st, "russ", "2026-07-03-foo", "epic/2026-07-03-foo", "epics/2026-07-03-foo.md")
	fh := &fakeHistory{
		tip:     "t",
		refTips: map[string]string{"refs/heads/epic/2026-07-03-foo": "epic-head-sha"}, // unrelated epic tip
	}
	sender.WithHistory(fh, "main")
	epicMergingJobWithBase(t, st, "j", "russ", "", "ordinary-pr-head-sha") // NOT the epic's tip
	setLiveGreenPR(fake, 42, "", "ordinary-pr-head-sha")

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
		t.Fatalf("a non-epic empty-base PR must not be blocked by the epic gate; calls=%v state=%s", fake.Calls(), j.State)
	}
	if j, _ := st.GetJob(ctx, "j"); j.State != job.StateMerging {
		t.Fatalf("state=%s, want merging (epic gate did not touch a non-epic empty-base PR)", j.State)
	}
}

func mustRegisterEpic(t *testing.T, st *store.Store, repo, id, branch, filePath string) {
	t.Helper()
	if err := st.AddEpicRun(context.Background(), store.EpicRun{
		ID: id, Repo: repo, FilePath: filePath, Title: "Foo",
		Scope: []string{"app/foo/**"}, Branch: branch, TmuxName: "epic-" + id,
	}, 1, time.Unix(500, 0)); err != nil {
		t.Fatalf("register epic: %v", err)
	}
}
