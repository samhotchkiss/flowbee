package onboarding

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/samhotchkiss/flowbee/internal/config"
	gh "github.com/samhotchkiss/flowbee/internal/github"
)

// TestDoctorMultiRepoPreflightsEachRepo: with a multi-repo registry (the production
// layout), doctor must preflight EACH registered repo — not warn "repo-coords unset" and
// skip the make-or-break checks (token write, CI-on-PR, branch protection), as it did
// when it only understood the single-repo github_owner/github_repo coords.
func TestDoctorMultiRepoPreflightsEachRepo(t *testing.T) {
	cfg := config.Config{Repos: []config.RepoConfig{
		{ID: "flowbee", Owner: "o", Repo: "flowbee", DefaultBranch: "main"},
		{ID: "russ", Owner: "o", Repo: "russ", DefaultBranch: "main"},
	}}
	rep := &DoctorReport{}
	probe := preflightProbe{gh.Preflight{CanWrite: true, HasCI: true, CITriggersOnPR: true}}
	checkGitHub(context.Background(), DoctorOptions{Probe: probe}, cfg, rep)

	for _, want := range []string{
		"github[flowbee]", "github[russ]",
		"github[flowbee] write", "github[russ] write",
		"github[flowbee] ci", "github[russ] ci",
	} {
		c := findCheck(*rep, want)
		if c.Name == "" {
			t.Fatalf("missing per-repo check %q — multi-repo deploy not preflighted", want)
		}
		if c.Status == StatusFail {
			t.Fatalf("per-repo check %q failed: %+v", want, c)
		}
	}
}

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
	for _, n := range []string{"github write", "github ci", "github protection"} {
		if c := findCheck(rep, n); c.Status != StatusPass {
			t.Fatalf("%q should pass, got %+v", n, c)
		}
	}

	// CI workflows exist but none trigger on pull_request -> WARN (PRs would stall).
	rep = run(gh.Preflight{CanWrite: true, HasCI: true, CITriggersOnPR: false})
	if !rep.Green() {
		t.Fatalf("a CI warning must not break green:\n%s", dump(rep))
	}
	if c := findCheck(rep, "github ci"); c.Status != StatusWarn {
		t.Fatalf("workflows-without-PR-trigger should warn, got %+v", c)
	}

	rep = run(gh.Preflight{CanWrite: false, HasCI: true})
	if rep.Green() {
		t.Fatal("a token without write access must break green")
	}
	if c := findCheck(rep, "github write"); c.Status != StatusFail {
		t.Fatalf("write access should fail, got %+v", c)
	}

	rep = run(gh.Preflight{CanWrite: true, HasCI: false, BranchProtected: true})
	if !rep.Green() {
		t.Fatalf("warnings must not break green:\n%s", dump(rep))
	}
	if c := findCheck(rep, "github ci"); c.Status != StatusWarn {
		t.Fatalf("missing CI should warn, got %+v", c)
	}
	if c := findCheck(rep, "github protection"); c.Status != StatusWarn {
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

func TestDoctorCostCeilingOff(t *testing.T) {
	root := initGitRepo(t)
	if _, err := Init(root); err != nil {
		t.Fatalf("Init: %v", err)
	}
	// Default init sets no cost_ceiling_usd — ceiling should be off.
	rep, err := Doctor(context.Background(), DoctorOptions{Root: root, SkipGitHub: true})
	if err != nil {
		t.Fatalf("Doctor: %v", err)
	}
	c := findCheck(rep, "cost-ceiling")
	if c.Name == "" {
		t.Fatal("cost-ceiling check missing from doctor output")
	}
	if c.Status != StatusPass {
		t.Fatalf("cost-ceiling off should be pass, got %+v", c)
	}
	if !strings.Contains(c.Detail, "off") {
		t.Fatalf("unset cost-ceiling should report off, got: %q", c.Detail)
	}
	if !rep.Green() {
		t.Fatalf("cost-ceiling off must not break green:\n%s", dump(rep))
	}
}

func TestDoctorCostCeilingArmed(t *testing.T) {
	root := initGitRepo(t)
	if _, err := Init(root); err != nil {
		t.Fatalf("Init: %v", err)
	}
	// Append cost_ceiling_usd to the scaffolded config.
	cfgPath := filepath.Join(root, "flowbee.yaml")
	f, err := os.OpenFile(cfgPath, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("open flowbee.yaml: %v", err)
	}
	_, _ = f.WriteString("\ncost_ceiling_usd: 2.50\n")
	f.Close()

	rep, err := Doctor(context.Background(), DoctorOptions{Root: root, SkipGitHub: true})
	if err != nil {
		t.Fatalf("Doctor: %v", err)
	}
	c := findCheck(rep, "cost-ceiling")
	if c.Name == "" {
		t.Fatal("cost-ceiling check missing from doctor output")
	}
	if c.Status != StatusPass {
		t.Fatalf("cost-ceiling armed should be pass, got %+v", c)
	}
	if !strings.Contains(c.Detail, "$2.50") {
		t.Fatalf("armed cost-ceiling should show dollar amount, got: %q", c.Detail)
	}
	if !rep.Green() {
		t.Fatalf("cost-ceiling armed must not break green:\n%s", dump(rep))
	}
}

func TestRecentSnapshot(t *testing.T) {
	dir := t.TempDir()
	// no snapshots -> not ok.
	if _, _, ok := recentSnapshot(dir); ok {
		t.Fatal("empty dir must yield ok=false")
	}
	// a fresh snapshot -> ok.
	fresh := filepath.Join(dir, "flowbee-20260101-000001.000.db")
	if err := os.WriteFile(fresh, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, ok := recentSnapshot(dir); !ok {
		t.Fatal("a just-written snapshot must be recent")
	}
	// an OLD snapshot (mtime > 25h ago) -> not ok.
	old := filepath.Join(dir, "flowbee-20250101-000001.000.db")
	if err := os.WriteFile(old, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	stale := time.Now().Add(-26 * time.Hour)
	_ = os.Chtimes(old, stale, stale)
	_ = os.Chtimes(fresh, stale, stale) // age BOTH out
	if _, _, ok := recentSnapshot(dir); ok {
		t.Fatal("all snapshots older than 25h must yield ok=false")
	}
	if _, _, ok := recentSnapshot(""); ok {
		t.Fatal("empty dir path must yield ok=false")
	}
}

func TestCheckWorkerAuth(t *testing.T) {
	cases := []struct {
		name string
		cfg  config.Config
		env  map[string]string
		want CheckStatus
	}{
		{"auth secret set", config.Config{PrivateAddr: ":7070", WorkerAuthSecret: "s"}, nil, StatusPass},
		{"loopback bind", config.Config{PrivateAddr: "127.0.0.1:7070"}, nil, StatusPass},
		{"open + insecure", config.Config{PrivateAddr: ":7070"}, map[string]string{"FLOWBEE_INSECURE": "1"}, StatusWarn},
		{"non-loopback, no auth, no insecure", config.Config{PrivateAddr: ":7070"}, nil, StatusWarn},
		{"env addr override -> loopback", config.Config{PrivateAddr: ":7070"}, map[string]string{"FLOWBEE_PRIVATE_ADDR": "127.0.0.1:7070"}, StatusPass},
		{"env auth secret override", config.Config{PrivateAddr: ":7070"}, map[string]string{"FLOWBEE_WORKER_AUTH_SECRET": "s"}, StatusPass},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv("FLOWBEE_INSECURE", "")
			t.Setenv("FLOWBEE_PRIVATE_ADDR", "")
			t.Setenv("FLOWBEE_WORKER_AUTH_SECRET", "")
			for k, v := range c.env {
				t.Setenv(k, v)
			}
			var rep DoctorReport
			checkWorkerAuth(c.cfg, &rep)
			got := findCheck(rep, "worker-auth")
			if got.Name == "" {
				t.Fatal("worker-auth check missing")
			}
			if got.Status != c.want {
				t.Fatalf("status=%v want %v (detail: %s)", got.Status, c.want, got.Detail)
			}
		})
	}
}

func TestDoctorActorProtocolBundleIsGreenAndIdentified(t *testing.T) {
	var rep DoctorReport
	checkActorProtocol(&rep)
	got := findCheck(rep, "actor-protocol")
	if got.Status != StatusPass {
		t.Fatalf("actor protocol check=%+v", got)
	}
	for _, want := range []string{"flowbee.actor-protocol/v2", "version=2.0", "bundle=sha256:", "roles=10"} {
		if !strings.Contains(got.Detail, want) {
			t.Fatalf("actor protocol detail %q missing %q", got.Detail, want)
		}
	}
}

// TestDoctorHonorsConfigPath: ConfigPath (what serve uses via FLOWBEE_CONFIG) must win
// over Root — doctor validates THAT file and resolves flows/ next to it, so it checks
// the same config serve runs instead of a stray cwd/flowbee.yaml.
func TestDoctorHonorsConfigPath(t *testing.T) {
	scaffolded := initGitRepo(t)
	if _, err := Init(scaffolded); err != nil {
		t.Fatalf("Init: %v", err)
	}
	emptyRoot := t.TempDir() // deliberately has NO flowbee.yaml

	rep, err := Doctor(context.Background(), DoctorOptions{
		Root:       emptyRoot, // would FAIL (config not found) if Root were used
		ConfigPath: filepath.Join(scaffolded, "flowbee.yaml"),
		SkipGitHub: true,
	})
	if err != nil {
		t.Fatalf("Doctor: %v", err)
	}
	if statusOf(rep, "config") != StatusPass {
		t.Fatalf("config must pass via ConfigPath (not the empty Root):\n%s", dump(rep))
	}
	if statusOf(rep, "flow") != StatusPass {
		t.Fatalf("flow must resolve next to ConfigPath, got %s", statusOf(rep, "flow"))
	}
}
