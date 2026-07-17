package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/samhotchkiss/flowbee/internal/store"
	"github.com/samhotchkiss/flowbee/internal/watchdog"
)

// fakePreRunner is a watchdog.Runner that answers the Preflight + per-epic-worktree command
// set by matching the closed command templates, recording every call for assertions.
type fakePreRunner struct {
	home    string
	exists  string // "yes" (checkout present) / "no" (absent ⇒ clone)
	authErr error  // non-nil ⇒ `gh auth status` fails (not authenticated)
	wtErr   error  // non-nil ⇒ the `git worktree add` fails (launch-blocking)
	calls   []string
}

func (r *fakePreRunner) Run(_ context.Context, cmd string) (string, error) {
	r.calls = append(r.calls, cmd)
	switch {
	case strings.Contains(cmd, "echo $HOME"):
		return r.home + "\n", nil
	case strings.Contains(cmd, "gh auth status"):
		return "", r.authErr
	case strings.Contains(cmd, "df -Pk"):
		return "20971520\n", nil // 20G free
	case strings.Contains(cmd, "worktree add"):
		return "", r.wtErr
	case strings.Contains(cmd, "worktree remove"):
		return "", nil
	case strings.Contains(cmd, ".git") && strings.Contains(cmd, "test -d"):
		return r.exists + "\n", nil
	case strings.Contains(cmd, "gh repo clone"):
		return "", nil
	}
	return "", nil
}

func (r *fakePreRunner) called(want string) bool {
	for _, c := range r.calls {
		if c == want {
			return true
		}
	}
	return false
}

// TestEpicPreflight_ClonesAndResolvesCheckoutAndWorktree: the preflight resolves the box
// home, computes the SHARED base checkout <home>/dev/<repo> (REPO name, not slug), fires
// CloneRepoCmd when the base is absent, and cuts THIS epic's private worktree at
// <home>/dev/.flowbee-wt/<repo>/<slug> — handing the WORKTREE (not the base) back for the
// ladder's cwd — the deliverable #9 contract plus the 2-per-seat isolation.
func TestEpicPreflight_ClonesAndResolvesCheckoutAndWorktree(t *testing.T) {
	r := &fakePreRunner{home: "/Users/grok1", exists: "no"}
	repo := store.Repo{ID: "russ", Owner: "acme", Repo: "russ"}
	slug := "2026-07-16-foo"

	worktree, base, err := epicPreflight(context.Background(), r, "grok1@localhost", repo, slug)
	if err != nil {
		t.Fatalf("preflight: %v", err)
	}
	if base != "/Users/grok1/dev/russ" {
		t.Fatalf("base=%q want /Users/grok1/dev/russ (<home>/dev/<repo>)", base)
	}
	if worktree != "/Users/grok1/dev/.flowbee-wt/russ/2026-07-16-foo" {
		t.Fatalf("worktree=%q want /Users/grok1/dev/.flowbee-wt/russ/2026-07-16-foo", worktree)
	}
	// the worktree must live OUTSIDE the base (git worktree cannot nest inside its own base).
	if strings.HasPrefix(worktree, base+"/") {
		t.Fatalf("worktree %q must not be nested inside base %q", worktree, base)
	}
	if !r.called(watchdog.HomeDirCmd("grok1@localhost")) {
		t.Fatalf("home was not resolved via HomeDirCmd: %v", r.calls)
	}
	// the clone fired for the absent base, at the resolved <home>/dev/<repo> path.
	wantClone := watchdog.CloneRepoCmd("grok1@localhost", "acme/russ", "/Users/grok1/dev/russ")
	if !r.called(wantClone) {
		t.Fatalf("expected clone %q in %v", wantClone, r.calls)
	}
	// disk was measured at HOME (the M1 existing-path probe), never the not-yet-cloned dir.
	if !r.called(watchdog.DiskFreeKBCmd("grok1@localhost", "/Users/grok1")) {
		t.Fatalf("disk not probed at home: %v", r.calls)
	}
	// the per-epic worktree was cut DETACHED at origin/main (repo.DefaultBranch=="" ⇒ main).
	wantWt := watchdog.WorktreeAddCmd("grok1@localhost", base, worktree, "main")
	if !r.called(wantWt) {
		t.Fatalf("expected worktree-add %q in %v", wantWt, r.calls)
	}
}

// TestEpicPreflight_NoCloneWhenPresent: an existing base checkout is NOT re-cloned.
func TestEpicPreflight_NoCloneWhenPresent(t *testing.T) {
	r := &fakePreRunner{home: "/Users/grok1", exists: "yes"}
	repo := store.Repo{ID: "russ", Owner: "acme", Repo: "russ"}
	if _, _, err := epicPreflight(context.Background(), r, "grok1@localhost", repo, "slug"); err != nil {
		t.Fatalf("preflight: %v", err)
	}
	for _, c := range r.calls {
		if strings.Contains(c, "gh repo clone") {
			t.Fatalf("must not clone an existing checkout: %q", c)
		}
	}
}

// TestEpicPreflight_GhAuthFailedIsLaunchBlocking: an unauthenticated gh blocks the launch
// (the epic's Finish step opens a PR via gh) — and blocks BEFORE the worktree is cut.
func TestEpicPreflight_GhAuthFailedIsLaunchBlocking(t *testing.T) {
	r := &fakePreRunner{home: "/Users/grok1", exists: "yes", authErr: errors.New("not logged in")}
	repo := store.Repo{ID: "russ", Owner: "acme", Repo: "russ"}
	if _, _, err := epicPreflight(context.Background(), r, "grok1@localhost", repo, "slug"); err == nil {
		t.Fatal("expected a launch-blocking error when gh is not authenticated")
	}
	for _, c := range r.calls {
		if strings.Contains(c, "worktree add") {
			t.Fatalf("worktree must not be cut when gh auth failed: %q", c)
		}
	}
}

// TestEpicPreflight_WorktreeAddFailureIsLaunchBlocking: a failed `git worktree add` is a
// HARD launch failure — epicPreflight returns the error and NEVER falls back to the shared
// base tree (the cross-epic corruption the worktree exists to prevent).
func TestEpicPreflight_WorktreeAddFailureIsLaunchBlocking(t *testing.T) {
	r := &fakePreRunner{home: "/Users/grok1", exists: "yes", wtErr: errors.New("fatal: 'origin/main' is not a commit")}
	repo := store.Repo{ID: "russ", Owner: "acme", Repo: "russ"}
	worktree, base, err := epicPreflight(context.Background(), r, "grok1@localhost", repo, "slug")
	if err == nil {
		t.Fatal("a failed worktree add must be launch-blocking (returned as an error)")
	}
	if worktree != "" || base != "" {
		t.Fatalf("a blocked preflight must not hand back a usable checkout, got worktree=%q base=%q", worktree, base)
	}
}

// TestEpicPreflight_MissingRepoCoords: a repo with no github owner/repo cannot resolve a
// checkout to clone.
func TestEpicPreflight_MissingRepoCoords(t *testing.T) {
	r := &fakePreRunner{home: "/Users/grok1", exists: "yes"}
	if _, _, err := epicPreflight(context.Background(), r, "", store.Repo{ID: "x"}, "slug"); err == nil {
		t.Fatal("expected an error for a repo missing owner/repo")
	}
}
