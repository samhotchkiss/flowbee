// F13 acceptance: Onboarding — `flowbee init` + `flowbee doctor` + docs.
//
// Proven end-to-end over the REAL flowbee binary (built from cmd/flowbee), run in
// a fresh temp git repo. No real GitHub, no network: doctor's reachability check is
// exercised both ways — once skipped offline (the CLI's --offline path, which must
// stay GREEN), and once reachable via the in-memory fakeGitHub through the
// onboarding package's injectable probe.
//
// DONE-WHEN (each proven below by a real, non-skipped test):
//   - `flowbee init` scaffolds config INTO the repo (flowbee.yaml +
//     flows/{default.yaml, identities/*, lenses/*}), prefills github_owner/repo
//     from the git remote, gitignores the db, and prints a 3-item checklist;
//   - `flowbee doctor` reports GREEN on that scaffolded repo (config valid + flow
//     identities exist + GitHub reachable-or-skipped);
//   - the runbook docs exist (SETUP.md, docs/config.md, docs/identities.md,
//     AGENTS.md) so the agent-guided install is followable.
package acceptance

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	gh "github.com/samhotchkiss/flowbee/internal/github"
	"github.com/samhotchkiss/flowbee/internal/onboarding"
)

func TestF13_InitScaffoldsAndDoctorGreen(t *testing.T) {
	bin := buildFlowbee(t)

	// a fresh temp git repo with an origin remote (so init prefills coords).
	repo := t.TempDir()
	gitInit(t, repo)
	gitRun(t, repo, "remote", "add", "origin", "git@github.com:acme/widgets.git")

	// ── flowbee init ───────────────────────────────────────────────────────
	initOut := runFlowbeeIn(t, bin, repo, nil, "init")

	// scaffolded files exist in the repo.
	for _, rel := range []string{
		"flowbee.yaml",
		"flows/default.yaml",
		"flows/identities/builder.yaml",
		"flows/identities/issue-reviewer.yaml",
		"flows/identities/reviewer-correctness.yaml",
		"flows/lenses/builder.md",
		"flows/lenses/issue-reviewer.md",
	} {
		if _, err := os.Stat(filepath.Join(repo, rel)); err != nil {
			t.Errorf("init did not scaffold %s: %v", rel, err)
		}
	}

	// coords prefilled from the remote.
	yaml, err := os.ReadFile(filepath.Join(repo, "flowbee.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(yaml), "github_owner: acme") ||
		!strings.Contains(string(yaml), "github_repo: widgets") {
		t.Errorf("flowbee.yaml did not prefill acme/widgets:\n%s", yaml)
	}

	// db gitignored.
	gi, err := os.ReadFile(filepath.Join(repo, ".gitignore"))
	if err != nil {
		t.Fatalf("read .gitignore: %v", err)
	}
	if !strings.Contains(string(gi), "flowbee.db") {
		t.Errorf(".gitignore does not ignore flowbee.db:\n%s", gi)
	}

	// the printed 3-item checklist (numbered 1./2./3.).
	for _, n := range []string{"1.", "2.", "3."} {
		if !strings.Contains(initOut, n) {
			t.Errorf("init output missing checklist item %q:\n%s", n, initOut)
		}
	}

	// ── flowbee doctor (offline path → GREEN) ──────────────────────────────
	docOut := runFlowbeeIn(t, bin, repo, nil, "doctor", "--offline")
	if !strings.Contains(docOut, "flowbee doctor: green") {
		t.Fatalf("doctor not green offline:\n%s", docOut)
	}

	// ── flowbee doctor (reachable GitHub via the in-memory fake) ────────────
	// Drive the same scaffolded repo through the onboarding package directly so
	// we can inject the fakeGitHub probe (the binary cannot, by design — workers
	// and the CLI hold no creds in tests). This proves the reachability check
	// passes against a reachable GitHub, completing the GREEN contract.
	rep, err := onboarding.Doctor(context.Background(), onboarding.DoctorOptions{
		Root:  repo,
		Probe: gh.NewFake(),
	})
	if err != nil {
		t.Fatalf("onboarding.Doctor: %v", err)
	}
	if !rep.Green() {
		var sb strings.Builder
		for _, c := range rep.Checks {
			sb.WriteString(string(c.Status) + " " + c.Name + ": " + c.Detail + "\n")
		}
		t.Fatalf("doctor not green with reachable GitHub:\n%s", sb.String())
	}
}

func TestF13_RunbookDocsExist(t *testing.T) {
	root := repoRoot(t)
	for _, rel := range []string{
		"SETUP.md",
		"docs/config.md",
		"docs/identities.md",
		"AGENTS.md",
	} {
		fi, err := os.Stat(filepath.Join(root, rel))
		if err != nil {
			t.Errorf("missing runbook doc %s: %v", rel, err)
			continue
		}
		if fi.Size() == 0 {
			t.Errorf("runbook doc %s is empty", rel)
		}
	}
}

// --- helpers ---

func gitInit(t *testing.T, dir string) {
	t.Helper()
	gitRun(t, dir, "init")
}

func gitRun(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// runFlowbeeIn runs the flowbee binary with its working directory set to dir
// (init/doctor default to "."), returning combined stdout+stderr.
func runFlowbeeIn(t *testing.T, bin, dir string, env map[string]string, args ...string) string {
	t.Helper()
	cmd := exec.Command(bin, args...)
	cmd.Dir = dir
	cmd.Env = os.Environ()
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Run(); err != nil {
		t.Fatalf("flowbee %s: %v\noutput: %s", strings.Join(args, " "), err, out.String())
	}
	return out.String()
}

// repoRoot walks up from the test's working directory to the module root (the
// dir holding go.mod), so the docs assertions find the real repo files.
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("could not find module root (go.mod)")
		}
		dir = parent
	}
}
