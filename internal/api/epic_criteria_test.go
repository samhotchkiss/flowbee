package api

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/samhotchkiss/flowbee/internal/gitops"
	"github.com/samhotchkiss/flowbee/internal/store"
	"github.com/samhotchkiss/flowbee/internal/testutil"
)

// TestEpicMirrorPathFor mirrors cmd/flowbee/serve.go's controlMirrorFor derivation
// (epicMirrorPathFor's doc explains why it's duplicated here rather than imported).
func TestEpicMirrorPathFor(t *testing.T) {
	if got := epicMirrorPathFor("", "russ"); got != "" {
		t.Fatalf("no base mirror configured -> empty, got %q", got)
	}
	if got := epicMirrorPathFor("/data/mirror.git", ""); got != "/data/mirror.git" {
		t.Fatalf("empty repo id -> the base mirror itself, got %q", got)
	}
	if got := epicMirrorPathFor("/data/mirror.git", "default"); got != "/data/mirror.git" {
		t.Fatalf(`repo id "default" -> the base mirror itself, got %q`, got)
	}
	want := filepath.Join("/data", "russ.git")
	if got := epicMirrorPathFor("/data/mirror.git", "russ"); got != want {
		t.Fatalf("non-default repo id -> sibling mirror, got %q want %q", got, want)
	}
}

// epicMirrorFixture builds a TWO-LAYER real git fixture: an "upstream" bare repo
// (standing in for GitHub) carrying an epic file committed to an "epic/<slug>"
// branch, and a SEPARATE "mirror" bare repo cloned FROM upstream (standing in for
// the control-plane mirror controlMirrorFor/gitops.CloneBareMirror produces in
// production) — so injectEpicCriteria's mirror.FetchBranch call has a real "origin"
// remote to fetch from, exercising the real gitops.Mirror I/O end-to-end rather than
// a fake. A single bare repo with no remote (as production NEVER has) would make
// FetchBranch fail every time, silently skipping detection — this fixture avoids
// that trap. Returns the MIRROR's path (what s.mirrorPath is set to) and the epic
// branch's tip SHA.
func epicMirrorFixture(t *testing.T, slug, filePath, epicFileBody string) (mirrorPath, headSHA string) {
	t.Helper()
	root := t.TempDir()
	upstream := filepath.Join(root, "upstream.git")
	if _, err := gitops.InitBare(upstream); err != nil {
		t.Fatalf("init upstream bare: %v", err)
	}
	work := filepath.Join(root, "seed")
	runGit(t, "", "git", "clone", upstream, work)
	if err := os.MkdirAll(filepath.Join(work, filepath.Dir(filePath)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(work, filePath), []byte(epicFileBody), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, work, "git", "checkout", "-b", "epic/"+slug)
	runGit(t, work, "git", "-c", "user.email=t@t", "-c", "user.name=t", "add", "-A")
	runGit(t, work, "git", "-c", "user.email=t@t", "-c", "user.name=t", "commit", "-m", "epic init")
	runGit(t, work, "git", "push", "origin", "epic/"+slug)

	mirror := filepath.Join(root, "mirror.git")
	if err := gitops.CloneBareMirror(mirror, upstream); err != nil {
		t.Fatalf("clone bare mirror: %v", err)
	}

	m := gitops.Open(upstream)
	sha, err := m.HeadSHA("refs/heads/epic/" + slug)
	if err != nil {
		t.Fatalf("head sha: %v", err)
	}
	return mirror, sha
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%v: %v\n%s", args, err, out)
	}
}

const epicCriteriaFixtureBody = "---\ntitle: Foo\nscope:\n  - app/foo/**\n---\n\n" +
	"## Goal\n\nShip the epic-lane review gate.\n\n" +
	"## Steps\n\n1. First step\nValidate: go test ./app/foo/...\n\n" +
	"## Status\n\nUpdated: 2026-07-03T12:00:00Z\nCurrent: step 1/1\nState: done\n\n" +
	"- [x] Step 1 — first step (evidence: go test passed)\n\nBlockers: none\n"

// TestInjectEpicCriteriaPopulatesLeaseContext: a code_reviewer lease for a job bound
// to a registered epic (SHA-tip match against the epic's real branch on a real
// mirror) gets EpicCriteria/EpicChecklist rendered from the epic file's actual bytes.
func TestInjectEpicCriteriaPopulatesLeaseContext(t *testing.T) {
	st := testutil.NewStore(t)
	mirrorPath, headSHA := epicMirrorFixture(t, "2026-07-03-foo", "epics/2026-07-03-foo.md", epicCriteriaFixtureBody)

	if err := st.AddEpicRun(context.Background(), store.EpicRun{
		ID: "2026-07-03-foo", Repo: "", FilePath: "epics/2026-07-03-foo.md",
		Title: "Foo", Scope: []string{"app/foo/**"}, Branch: "epic/2026-07-03-foo",
		TmuxName: "epic-2026-07-03-foo",
	}, time.Unix(500, 0)); err != nil {
		t.Fatalf("register epic: %v", err)
	}

	// repo="" here is the legacy single-repo default (job.Job.Repo's own doc) — a
	// deliberate regression case: an earlier draft of EpicForHeadSHA rejected repo=="",
	// which would have made Epic-PR detection permanently dead code for every
	// non-F9 (single managed repo) deployment.
	srv := &Server{store: st, mirrorPath: mirrorPath}
	lc := &LeaseContext{}
	srv.injectEpicCriteria(context.Background(), "", headSHA, lc)

	if lc.EpicCriteria == "" {
		t.Fatal("EpicCriteria should be populated for a job bound to a registered epic PR")
	}
	if lc.EpicChecklist == "" {
		t.Fatal("EpicChecklist should be populated")
	}
	for _, want := range []string{"Ship the epic-lane review gate.", "First step", "go test ./app/foo/..."} {
		if !strings.Contains(lc.EpicCriteria, want) {
			t.Errorf("EpicCriteria missing %q:\n%s", want, lc.EpicCriteria)
		}
	}
	if !strings.Contains(lc.EpicChecklist, "State: done") || !strings.Contains(lc.EpicChecklist, "go test passed") {
		t.Errorf("EpicChecklist missing expected content:\n%s", lc.EpicChecklist)
	}
}

// TestInjectEpicCriteriaNonEpicPRNoOp: an ordinary PR's head SHA (matching no
// registered epic's branch tip) leaves both fields empty — zero behavior change.
func TestInjectEpicCriteriaNonEpicPRNoOp(t *testing.T) {
	st := testutil.NewStore(t)
	mirrorPath, _ := epicMirrorFixture(t, "2026-07-03-foo", "epics/2026-07-03-foo.md", epicCriteriaFixtureBody)

	if err := st.AddEpicRun(context.Background(), store.EpicRun{
		ID: "2026-07-03-foo", Repo: "", FilePath: "epics/2026-07-03-foo.md",
		Title: "Foo", Scope: []string{"app/foo/**"}, Branch: "epic/2026-07-03-foo",
		TmuxName: "epic-2026-07-03-foo",
	}, time.Unix(500, 0)); err != nil {
		t.Fatalf("register epic: %v", err)
	}

	srv := &Server{store: st, mirrorPath: mirrorPath}
	lc := &LeaseContext{}
	srv.injectEpicCriteria(context.Background(), "", "some-ordinary-pr-head-sha", lc)

	if lc.EpicCriteria != "" || lc.EpicChecklist != "" {
		t.Fatalf("an ordinary PR must leave both epic fields empty, got EpicCriteria=%q EpicChecklist=%q", lc.EpicCriteria, lc.EpicChecklist)
	}
}

// TestInjectEpicCriteriaNoMirrorConfiguredNoOp: with no mirror configured at all
// (the common non-epic-lane deployment), injectEpicCriteria is a complete no-op —
// no panic, no mirror I/O attempted.
func TestInjectEpicCriteriaNoMirrorConfiguredNoOp(t *testing.T) {
	st := testutil.NewStore(t)
	srv := &Server{store: st, mirrorPath: ""}
	lc := &LeaseContext{}
	srv.injectEpicCriteria(context.Background(), "", "any-head-sha", lc)
	if lc.EpicCriteria != "" || lc.EpicChecklist != "" {
		t.Fatal("no mirror configured must leave both epic fields empty")
	}
}
