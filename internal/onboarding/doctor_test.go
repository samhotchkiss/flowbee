package onboarding

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	gh "github.com/samhotchkiss/flowbee/internal/github"
)

// initGitRepo makes a temp git repo with an origin remote pointing at
// github.com/acme/widgets, so DetectRemote / Init can prefill coords. Shared by
// the scaffold + doctor tests.
func initGitRepo(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	run := func(args ...string) {
		cmd := exec.Command("git", append([]string{"-C", root}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init")
	run("remote", "add", "origin", "git@github.com:acme/widgets.git")
	return root
}

// errProbe is a GitHubProbe that always fails — the "unreachable" case.
type errProbe struct{}

func (errProbe) BoardSweep(context.Context) (gh.BoardSnapshot, error) {
	return gh.BoardSnapshot{}, errors.New("dial tcp: simulated network failure")
}

// preflightProbe is reachable AND exposes a configurable deployment preflight.
type preflightProbe struct{ pf gh.Preflight }

func (preflightProbe) BoardSweep(context.Context) (gh.BoardSnapshot, error) {
	return gh.BoardSnapshot{}, nil
}
func (p preflightProbe) Preflight(context.Context, string) (gh.Preflight, error) { return p.pf, nil }

func findCheck(rep DoctorReport, name string) Check {
	for _, c := range rep.Checks {
		if c.Name == name {
			return c
		}
	}
	return Check{}
}

func TestDoctorPreflight(t *testing.T) {
	root := initGitRepo(t)
	if _, err := Init(root); err != nil {
		t.Fatalf("Init: %v", err)
	}
	run := func(pf gh.Preflight) DoctorReport {
		rep, err := Doctor(context.Background(), DoctorOptions{Root: root, Probe: preflightProbe{pf}})
		if err != nil {
			t.Fatalf("Doctor: %v", err)
		}
		return rep
	}

	rep := run(gh.Preflight{CanWrite: true, HasCI: true, CITriggersOnPR: true, BranchProtected: false})
	if !rep.Green() {
		t.Fatalf("expected green:\n%s", dump(rep))
	}
	for _, n := range []string{"github write access", "ci configured", "branch protection"} {
		if c := findCheck(rep, n); c.Status != StatusPass {
			t.Fatalf("%q should pass, got %+v", n, c)
		}
	}

	// CI workflows exist but none trigger on pull_request -> WARN (PRs would stall).
	rep = run(gh.Preflight{CanWrite: true, HasCI: true, CITriggersOnPR: false})
	if !rep.Green() {
		t.Fatalf("a CI warning must not break green:\n%s", dump(rep))
	}
	if c := findCheck(rep, "ci configured"); c.Status != StatusWarn {
		t.Fatalf("workflows-without-PR-trigger should warn, got %+v", c)
	}

	rep = run(gh.Preflight{CanWrite: false, HasCI: true})
	if rep.Green() {
		t.Fatal("a token without write access must break green")
	}
	if c := findCheck(rep, "github write access"); c.Status != StatusFail {
		t.Fatalf("write access should fail, got %+v", c)
	}

	rep = run(gh.Preflight{CanWrite: true, HasCI: false, BranchProtected: true})
	if !rep.Green() {
		t.Fatalf("warnings must not break green:\n%s", dump(rep))
	}
	if c := findCheck(rep, "ci configured"); c.Status != StatusWarn {
		t.Fatalf("missing CI should warn, got %+v", c)
	}
	if c := findCheck(rep, "branch protection"); c.Status != StatusWarn {
		t.Fatalf("protected branch should warn, got %+v", c)
	}
}

func TestDoctor_GreenOnFreshInit(t *testing.T) {
	root := initGitRepo(t)
	if _, err := Init(root); err != nil {
		t.Fatalf("Init: %v", err)
	}

	// reachable GitHub via the in-memory fake (no creds, no network).
	rep, err := Doctor(context.Background(), DoctorOptions{Root: root, Probe: gh.NewFake()})
	if err != nil {
		t.Fatalf("Doctor: %v", err)
	}
	if !rep.Green() {
		t.Fatalf("doctor not green on a fresh init:\n%s", dump(rep))
	}
	// config, repo-coords, flow, identities, github must all be present + pass.
	mustPass(t, rep, "config", "repo-coords", "flow", "identities", "github")
}

func TestDoctor_OfflineSkipsGitHubButStaysGreen(t *testing.T) {
	root := initGitRepo(t)
	if _, err := Init(root); err != nil {
		t.Fatalf("Init: %v", err)
	}
	rep, err := Doctor(context.Background(), DoctorOptions{Root: root, SkipGitHub: true})
	if err != nil {
		t.Fatalf("Doctor: %v", err)
	}
	if !rep.Green() {
		t.Fatalf("offline doctor should stay green:\n%s", dump(rep))
	}
	if statusOf(rep, "github") != StatusWarn {
		t.Errorf("offline github check should warn, got %s", statusOf(rep, "github"))
	}
}

func TestDoctor_UnreachableGitHubFails(t *testing.T) {
	root := initGitRepo(t)
	if _, err := Init(root); err != nil {
		t.Fatalf("Init: %v", err)
	}
	rep, err := Doctor(context.Background(), DoctorOptions{Root: root, Probe: errProbe{}})
	if err != nil {
		t.Fatalf("Doctor: %v", err)
	}
	if rep.Green() {
		t.Fatalf("doctor should fail when GitHub is unreachable:\n%s", dump(rep))
	}
	if statusOf(rep, "github") != StatusFail {
		t.Errorf("github check should fail, got %s", statusOf(rep, "github"))
	}
}

func TestDoctor_MissingConfigFails(t *testing.T) {
	root := t.TempDir() // nothing scaffolded
	rep, err := Doctor(context.Background(), DoctorOptions{Root: root, SkipGitHub: true})
	if err != nil {
		t.Fatalf("Doctor: %v", err)
	}
	if rep.Green() {
		t.Fatalf("doctor should fail with no config:\n%s", dump(rep))
	}
	if statusOf(rep, "config") != StatusFail {
		t.Errorf("config check should fail, got %s", statusOf(rep, "config"))
	}
}

func TestDoctor_MissingIdentityFails(t *testing.T) {
	root := initGitRepo(t)
	if _, err := Init(root); err != nil {
		t.Fatalf("Init: %v", err)
	}
	// remove an identity the flow references → doctor must catch it.
	if err := os.Remove(filepath.Join(root, "flows", "identities", "builder.yaml")); err != nil {
		t.Fatal(err)
	}
	rep, err := Doctor(context.Background(), DoctorOptions{Root: root, SkipGitHub: true})
	if err != nil {
		t.Fatalf("Doctor: %v", err)
	}
	if rep.Green() {
		t.Fatalf("doctor should fail when a referenced identity is missing:\n%s", dump(rep))
	}
	if statusOf(rep, "identities") != StatusFail {
		t.Errorf("identities check should fail, got %s", statusOf(rep, "identities"))
	}
}

func TestDoctor_MissingLensFails(t *testing.T) {
	root := initGitRepo(t)
	if _, err := Init(root); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if err := os.Remove(filepath.Join(root, "flows", "lenses", "builder.md")); err != nil {
		t.Fatal(err)
	}
	rep, err := Doctor(context.Background(), DoctorOptions{Root: root, SkipGitHub: true})
	if err != nil {
		t.Fatalf("Doctor: %v", err)
	}
	if statusOf(rep, "identities") != StatusFail {
		t.Errorf("identities check should fail when a lens is missing, got %s", statusOf(rep, "identities"))
	}
}

// --- test helpers ---

func statusOf(rep DoctorReport, name string) CheckStatus {
	for _, c := range rep.Checks {
		if c.Name == name {
			return c.Status
		}
	}
	return ""
}

func mustPass(t *testing.T, rep DoctorReport, names ...string) {
	t.Helper()
	for _, n := range names {
		if statusOf(rep, n) != StatusPass {
			t.Errorf("check %q = %s, want pass", n, statusOf(rep, n))
		}
	}
}

func dump(rep DoctorReport) string {
	s := ""
	for _, c := range rep.Checks {
		s += string(c.Status) + " " + c.Name + ": " + c.Detail + "\n"
	}
	return s
}
