package store_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/samhotchkiss/flowbee/internal/store"
	"github.com/samhotchkiss/flowbee/internal/testutil"
)

// fakeEpicMirror is a minimal in-memory store.EpicMirrorReader: branches map to a tip
// SHA, and (ref, path) pairs map to file content — enough to exercise EpicForHeadSHA
// and EpicContractAtRef without any real git process.
type fakeEpicMirror struct {
	branchTips map[string]string            // branch -> tip SHA
	files      map[string]map[string]string // ref -> path -> content
	fetchErr   map[string]error             // branch -> error (optional)
}

func (m *fakeEpicMirror) FetchBranch(branch string) error {
	if err, ok := m.fetchErr[branch]; ok {
		return err
	}
	if _, ok := m.branchTips[branch]; !ok {
		return fmt.Errorf("fetch %s: git fetch: fatal: couldn't find remote ref refs/heads/%s", branch, branch)
	}
	return nil
}

func (m *fakeEpicMirror) HeadSHA(ref string) (string, error) {
	branch := ref
	const prefix = "refs/heads/"
	if len(ref) > len(prefix) && ref[:len(prefix)] == prefix {
		branch = ref[len(prefix):]
	}
	tip, ok := m.branchTips[branch]
	if !ok {
		return "", fmt.Errorf("no such ref %q", ref)
	}
	return tip, nil
}

func (m *fakeEpicMirror) ReadFileAtRef(ref, path string) (string, bool, error) {
	byPath, ok := m.files[ref]
	if !ok {
		return "", false, nil
	}
	content, ok := byPath[path]
	return content, ok, nil
}

func TestEpicForHeadSHAMatchesByBranchTip(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 3, 12, 0, 0, 0, time.UTC)

	mustAddEpicRun(t, st, ctx, store.EpicRun{
		ID: "2026-07-03-foo", Repo: "russ", FilePath: "epics/2026-07-03-foo.md",
		Title: "Foo", Scope: []string{"internal/foo/**"}, Branch: "epic/2026-07-03-foo",
		TmuxName: "epic-2026-07-03-foo",
	}, now)

	mirror := &fakeEpicMirror{branchTips: map[string]string{"epic/2026-07-03-foo": "sha-abc"}}

	e, ok, err := st.EpicForHeadSHA(ctx, mirror, "russ", "sha-abc", now)
	if err != nil {
		t.Fatalf("EpicForHeadSHA: %v", err)
	}
	if !ok || e.ID != "2026-07-03-foo" {
		t.Fatalf("want match on 2026-07-03-foo, got ok=%v e=%+v", ok, e)
	}
}

func TestEpicForHeadSHANoMatchForOrdinaryPR(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 3, 12, 0, 0, 0, time.UTC)

	mustAddEpicRun(t, st, ctx, store.EpicRun{
		ID: "2026-07-03-foo", Repo: "russ", FilePath: "epics/2026-07-03-foo.md",
		Title: "Foo", Scope: []string{"internal/foo/**"}, Branch: "epic/2026-07-03-foo",
		TmuxName: "epic-2026-07-03-foo",
	}, now)

	mirror := &fakeEpicMirror{branchTips: map[string]string{"epic/2026-07-03-foo": "sha-abc"}}

	// an ordinary PR's head SHA never equals the epic branch's tip.
	_, ok, err := st.EpicForHeadSHA(ctx, mirror, "russ", "sha-ordinary-pr", now)
	if err != nil {
		t.Fatalf("EpicForHeadSHA: %v", err)
	}
	if ok {
		t.Fatal("want no match for an ordinary PR's head SHA")
	}
}

func TestEpicForHeadSHANoEpicsForRepoNoMirrorCalls(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()

	mirror := &fakeEpicMirror{branchTips: map[string]string{}}
	_, ok, err := st.EpicForHeadSHA(ctx, mirror, "some-repo-with-no-epics", "sha-x", time.Now())
	if err != nil || ok {
		t.Fatalf("want ok=false err=nil for a repo with no registered epics, got ok=%v err=%v", ok, err)
	}
}

func TestEpicForHeadSHANilMirror(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	_, ok, err := st.EpicForHeadSHA(ctx, nil, "russ", "sha-x", time.Now())
	if err != nil || ok {
		t.Fatalf("nil mirror must yield ok=false, err=nil (fail closed to 'not an epic'), got ok=%v err=%v", ok, err)
	}
}

// TestEpicForHeadSHATransientFetchErrorPropagates (review F2): a fetch error
// against a LIVE (non-terminal) epic's branch that is NOT git's "couldn't find
// remote ref" must PROPAGATE — the merge-gate caller retries rather than treating
// a possibly-epic PR as ordinary during a mirror outage.
func TestEpicForHeadSHATransientFetchErrorPropagates(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 3, 12, 0, 0, 0, time.UTC)

	mustAddEpicRun(t, st, ctx, store.EpicRun{
		ID: "2026-07-03-foo", Repo: "russ", FilePath: "epics/2026-07-03-foo.md",
		Title: "Foo", Scope: []string{"internal/foo/**"}, Branch: "epic/2026-07-03-foo",
		TmuxName: "epic-2026-07-03-foo",
	}, now)

	mirror := &fakeEpicMirror{
		branchTips: map[string]string{"epic/2026-07-03-foo": "sha-abc"},
		fetchErr:   map[string]error{"epic/2026-07-03-foo": fmt.Errorf("fetch epic/2026-07-03-foo: connection reset by peer")},
	}
	_, ok, err := st.EpicForHeadSHA(ctx, mirror, "russ", "any-head", now)
	if err == nil || ok {
		t.Fatalf("a transient fetch error on a live epic's branch must propagate (fail closed), got ok=%v err=%v", ok, err)
	}
}

// TestEpicForHeadSHAMissingRemoteRefIsCleanSkip (review F2): git's "couldn't find
// remote ref" means the branch genuinely does not exist at origin (a just-launched
// epic pre-first-push, or a branch deleted post-merge) — a PR head cannot belong to
// a branch that doesn't exist, so this is a clean non-match, NOT a retry, else one
// un-pushed epic would block every merge in the repo.
func TestEpicForHeadSHAMissingRemoteRefIsCleanSkip(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 3, 12, 0, 0, 0, time.UTC)

	mustAddEpicRun(t, st, ctx, store.EpicRun{
		ID: "2026-07-03-foo", Repo: "russ", FilePath: "epics/2026-07-03-foo.md",
		Title: "Foo", Scope: []string{"internal/foo/**"}, Branch: "epic/2026-07-03-foo",
		TmuxName: "epic-2026-07-03-foo",
	}, now)

	// the fake's default for an unknown branch IS the missing-remote-ref error shape.
	mirror := &fakeEpicMirror{branchTips: map[string]string{}}
	_, ok, err := st.EpicForHeadSHA(ctx, mirror, "russ", "any-head", now)
	if err != nil || ok {
		t.Fatalf("a genuinely absent branch must be a clean non-match, got ok=%v err=%v", ok, err)
	}
}

// TestListEpicRunsForRepoRetention (review F3): 'abandoned' epics are excluded from
// the detection window entirely; done/achieved epics stay only while finished_at is
// within the retention window; non-terminal epics always stay.
func TestListEpicRunsForRepoRetention(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	base := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)

	add := func(id, state, finishedAt string) {
		t.Helper()
		mustAddEpicRun(t, st, ctx, store.EpicRun{
			ID: id, Repo: "russ", FilePath: "epics/" + id + ".md", Title: id,
			Scope: []string{"internal/" + id + "/**"}, Branch: "epic/" + id, TmuxName: "epic-" + id,
		}, base)
		if _, err := st.DB.ExecContext(ctx,
			`UPDATE epics SET state = ?, finished_at = ? WHERE id = ?`, state, finishedAt, id); err != nil {
			t.Fatal(err)
		}
	}
	now := base.Add(24 * time.Hour)
	add("running-epic", "running", "")
	add("abandoned-epic", "abandoned", now.Add(-1*time.Hour).Format(time.RFC3339))
	add("recent-done-epic", "done", now.Add(-48*time.Hour).Format(time.RFC3339))
	add("ancient-done-epic", "done", now.Add(-30*24*time.Hour).Format(time.RFC3339))
	add("recent-achieved-epic", "achieved", now.Add(-1*time.Hour).Format(time.RFC3339))

	got, err := st.ListEpicRunsForRepo(ctx, "russ", now)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	ids := map[string]bool{}
	for _, e := range got {
		ids[e.ID] = true
	}
	for _, want := range []string{"running-epic", "recent-done-epic", "recent-achieved-epic"} {
		if !ids[want] {
			t.Errorf("%s should be in the detection window, got %v", want, ids)
		}
	}
	for _, absent := range []string{"abandoned-epic", "ancient-done-epic"} {
		if ids[absent] {
			t.Errorf("%s should be excluded from the detection window, got %v", absent, ids)
		}
	}
}

func TestEpicForRepoBranchNearMissAndRepoMismatch(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 3, 12, 0, 0, 0, time.UTC)

	mustAddEpicRun(t, st, ctx, store.EpicRun{
		ID: "2026-07-03-foo", Repo: "russ", FilePath: "epics/2026-07-03-foo.md",
		Title: "Foo", Scope: []string{"internal/foo/**"}, Branch: "epic/2026-07-03-foo",
		TmuxName: "epic-2026-07-03-foo",
	}, now)

	// exact match.
	e, ok, err := st.EpicForRepoBranch(ctx, "russ", "epic/2026-07-03-foo")
	if err != nil || !ok || e.ID != "2026-07-03-foo" {
		t.Fatalf("want match, got ok=%v err=%v e=%+v", ok, err, e)
	}

	// near-miss branch names never match.
	for _, branch := range []string{"epic/2026-07-03-foo-two", "epic2026-07-03-foo", "feature/epic/2026-07-03-foo"} {
		if _, ok, err := st.EpicForRepoBranch(ctx, "russ", branch); err != nil || ok {
			t.Fatalf("branch %q: want no match, got ok=%v err=%v", branch, ok, err)
		}
	}

	// same slug, different repo: must not match (an epic id is not globally unique
	// in spirit even though the id column happens to be a PK across all repos here —
	// the repo check is the real safety property under test).
	if _, ok, err := st.EpicForRepoBranch(ctx, "some-other-repo", "epic/2026-07-03-foo"); err != nil || ok {
		t.Fatalf("cross-repo slug: want no match, got ok=%v err=%v", ok, err)
	}
}

func TestEpicContractAtRefReadsAndParses(t *testing.T) {
	st := testutil.NewStore(t)

	e := store.EpicRun{
		ID: "2026-07-03-foo", Repo: "russ", FilePath: "epics/2026-07-03-foo.md",
		Title: "Foo", Scope: []string{"internal/foo/**"}, Branch: "epic/2026-07-03-foo",
	}

	fileBody := "---\ntitle: Foo\nscope:\n  - internal/foo/**\n---\n\n" +
		"## Goal\n\nDo the thing.\n\n" +
		"## Steps\n\n1. First step\nValidate: go test ./internal/foo/...\n\n" +
		"## Status\n\nUpdated: 2026-07-03T12:00:00Z\nCurrent: step 1/1\nState: done\n\n" +
		"- [x] Step 1 — first step (evidence: go test passed)\n\nBlockers: none\n"

	mirror := &fakeEpicMirror{
		files: map[string]map[string]string{
			"sha-head": {"epics/2026-07-03-foo.md": fileBody},
		},
	}

	spec, sb, err := st.EpicContractAtRef(mirror, e, "sha-head")
	if err != nil {
		t.Fatalf("EpicContractAtRef: %v", err)
	}
	if spec.Title != "Foo" || len(spec.Steps) != 1 {
		t.Fatalf("spec not parsed as expected: %+v", spec)
	}
	if sb.State != "done" || len(sb.Checklist) != 1 {
		t.Fatalf("status not parsed as expected: %+v", sb)
	}
}

func TestEpicContractAtRefNotFound(t *testing.T) {
	e := store.EpicRun{ID: "x", FilePath: "epics/x.md"}
	mirror := &fakeEpicMirror{files: map[string]map[string]string{}}
	st := testutil.NewStore(t)

	_, _, err := st.EpicContractAtRef(mirror, e, "sha-head")
	if err == nil {
		t.Fatal("want an error when the epic file is not found at the ref")
	}
	// the error must be the typed ErrEpicFileAbsent so a caller (epicDenyReason) can
	// tell an absent pinned contract (handoff) from a transient I/O error (retry).
	if !errors.Is(err, store.ErrEpicFileAbsent) {
		t.Fatalf("want ErrEpicFileAbsent, got %v", err)
	}
}
