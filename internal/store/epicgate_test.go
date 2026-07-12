package store_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/samhotchkiss/flowbee/internal/store"
	"github.com/samhotchkiss/flowbee/internal/testutil"
)

// fakeEpicMirror is a minimal in-memory store.EpicMirrorReader: branches map to a tip
// SHA, and (ref, path) pairs map to file content — enough to exercise EpicForHeadSHA
// and EpicContractAtHead without any real git process.
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
		return fmt.Errorf("no such branch %q", branch)
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

	e, ok, err := st.EpicForHeadSHA(ctx, mirror, "russ", "sha-abc")
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
	_, ok, err := st.EpicForHeadSHA(ctx, mirror, "russ", "sha-ordinary-pr")
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
	_, ok, err := st.EpicForHeadSHA(ctx, mirror, "some-repo-with-no-epics", "sha-x")
	if err != nil || ok {
		t.Fatalf("want ok=false err=nil for a repo with no registered epics, got ok=%v err=%v", ok, err)
	}
}

func TestEpicForHeadSHANilMirror(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	_, ok, err := st.EpicForHeadSHA(ctx, nil, "russ", "sha-x")
	if err != nil || ok {
		t.Fatalf("nil mirror must yield ok=false, err=nil (fail closed to 'not an epic'), got ok=%v err=%v", ok, err)
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

func TestEpicContractAtHeadReadsAndParses(t *testing.T) {
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

	spec, sb, err := st.EpicContractAtHead(mirror, e, "sha-head")
	if err != nil {
		t.Fatalf("EpicContractAtHead: %v", err)
	}
	if spec.Title != "Foo" || len(spec.Steps) != 1 {
		t.Fatalf("spec not parsed as expected: %+v", spec)
	}
	if sb.State != "done" || len(sb.Checklist) != 1 {
		t.Fatalf("status not parsed as expected: %+v", sb)
	}
}

func TestEpicContractAtHeadNotFound(t *testing.T) {
	e := store.EpicRun{ID: "x", FilePath: "epics/x.md"}
	mirror := &fakeEpicMirror{files: map[string]map[string]string{}}
	st := testutil.NewStore(t)

	if _, _, err := st.EpicContractAtHead(mirror, e, "sha-head"); err == nil {
		t.Fatal("want an error when the epic file is not found at the PR head")
	}
}
