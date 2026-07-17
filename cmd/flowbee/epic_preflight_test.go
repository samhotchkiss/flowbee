package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/samhotchkiss/flowbee/internal/store"
	"github.com/samhotchkiss/flowbee/internal/watchdog"
)

// fakePreRunner is a watchdog.Runner that answers the Preflight command set by matching
// the closed command templates, recording every call for assertions.
type fakePreRunner struct {
	home    string
	exists  string // "yes" (checkout present) / "no" (absent ⇒ clone)
	authErr error  // non-nil ⇒ `gh auth status` fails (not authenticated)
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

// TestEpicPreflight_ClonesAndResolvesCheckout: the preflight resolves the box home,
// computes CheckoutPath=<home>/dev/<repo> (REPO name, not slug), and fires CloneRepoCmd
// when the checkout is absent — the deliverable #9 contract.
func TestEpicPreflight_ClonesAndResolvesCheckout(t *testing.T) {
	r := &fakePreRunner{home: "/Users/grok1", exists: "no"}
	repo := store.Repo{ID: "russ", Owner: "acme", Repo: "russ"}

	checkout, err := epicPreflight(context.Background(), r, "grok1@localhost", repo)
	if err != nil {
		t.Fatalf("preflight: %v", err)
	}
	if checkout != "/Users/grok1/dev/russ" {
		t.Fatalf("checkout=%q want /Users/grok1/dev/russ (<home>/dev/<repo>)", checkout)
	}
	if !r.called(watchdog.HomeDirCmd("grok1@localhost")) {
		t.Fatalf("home was not resolved via HomeDirCmd: %v", r.calls)
	}
	// the clone fired for the absent checkout, at the resolved <home>/dev/<repo> path.
	wantClone := watchdog.CloneRepoCmd("grok1@localhost", "acme/russ", "/Users/grok1/dev/russ")
	if !r.called(wantClone) {
		t.Fatalf("expected clone %q in %v", wantClone, r.calls)
	}
	// disk was measured at HOME (the M1 existing-path probe), never the not-yet-cloned dir.
	if !r.called(watchdog.DiskFreeKBCmd("grok1@localhost", "/Users/grok1")) {
		t.Fatalf("disk not probed at home: %v", r.calls)
	}
}

// TestEpicPreflight_NoCloneWhenPresent: an existing checkout is NOT re-cloned.
func TestEpicPreflight_NoCloneWhenPresent(t *testing.T) {
	r := &fakePreRunner{home: "/Users/grok1", exists: "yes"}
	repo := store.Repo{ID: "russ", Owner: "acme", Repo: "russ"}
	if _, err := epicPreflight(context.Background(), r, "grok1@localhost", repo); err != nil {
		t.Fatalf("preflight: %v", err)
	}
	for _, c := range r.calls {
		if strings.Contains(c, "gh repo clone") {
			t.Fatalf("must not clone an existing checkout: %q", c)
		}
	}
}

// TestEpicPreflight_GhAuthFailedIsLaunchBlocking: an unauthenticated gh blocks the launch
// (the epic's Finish step opens a PR via gh).
func TestEpicPreflight_GhAuthFailedIsLaunchBlocking(t *testing.T) {
	r := &fakePreRunner{home: "/Users/grok1", exists: "yes", authErr: errors.New("not logged in")}
	repo := store.Repo{ID: "russ", Owner: "acme", Repo: "russ"}
	if _, err := epicPreflight(context.Background(), r, "grok1@localhost", repo); err == nil {
		t.Fatal("expected a launch-blocking error when gh is not authenticated")
	}
}

// TestEpicPreflight_MissingRepoCoords: a repo with no github owner/repo cannot resolve a
// checkout to clone.
func TestEpicPreflight_MissingRepoCoords(t *testing.T) {
	r := &fakePreRunner{home: "/Users/grok1", exists: "yes"}
	if _, err := epicPreflight(context.Background(), r, "", store.Repo{ID: "x"}); err == nil {
		t.Fatal("expected an error for a repo missing owner/repo")
	}
}
