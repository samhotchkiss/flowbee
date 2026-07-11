package main

import (
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/samhotchkiss/flowbee/internal/llm"
)

// captureStdout runs f with os.Stdout redirected to a pipe and returns what it wrote.
func captureStdout(t *testing.T, f func()) string {
	t.Helper()
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	f()
	_ = w.Close()
	os.Stdout = old
	out, _ := io.ReadAll(r)
	return string(out)
}

// TestRoleAgentCmdInjectsPerRoleModel: with no override, a role's agent command injects
// `--model <family>`, so build (Sonnet) and review (Opus) run GENUINELY different models
// (§5.5 uncorrelated review). An override wins and forgoes per-role models.
func TestRoleAgentCmdInjectsPerRoleModel(t *testing.T) {
	build := roleAgentCmd("claude", "sonnet", true, "", "")
	if !strings.Contains(build, "--model sonnet") || !strings.Contains(build, "Create the file(s)") {
		t.Errorf("build cmd missing --model sonnet or the file-writing prompt: %q", build)
	}
	review := roleAgentCmd("claude", "opus", false, "", "")
	if !strings.Contains(review, "--model opus") {
		t.Errorf("review cmd missing --model opus: %q", review)
	}
	if strings.Contains(review, "Create the file(s)") {
		t.Errorf("review cmd must not use the file-writing build prompt: %q", review)
	}
	if build == review {
		t.Error("build and review commands are identical — no model diversity (the §5.5 caveat)")
	}
	// the builder model and the code_reviewer model must actually differ.
	var reviewerFamily string
	for _, r := range nonBuilderFleetRoles() {
		if r.role == "code_reviewer" {
			reviewerFamily = r.family
		}
	}
	if reviewerFamily == fleetBuilderFamily {
		t.Errorf("code_reviewer model %q == builder model %q — correlated review", reviewerFamily, fleetBuilderFamily)
	}
	// overrides win and disable per-role model injection.
	if got := roleAgentCmd("claude", "opus", false, "MY_REVIEW_CMD", ""); got != "MY_REVIEW_CMD" {
		t.Errorf("agent override not honored: %q", got)
	}
	if got := roleAgentCmd("claude", "sonnet", true, "", "MY_BUILD_CMD"); got != "MY_BUILD_CMD" {
		t.Errorf("build override not honored: %q", got)
	}
}

// TestRoleAgentCmdCodex: with --agent codex, every role runs `codex exec` (never claude),
// reads stdin from /dev/null (codex exec blocks on an open stdin), bypasses the sandbox so
// it can write the work-product / verdict file, and the BUILD prompt forbids git (Flowbee
// owns the commit). The per-role difference is the task CONTEXT (build prompt vs verdict),
// not the model — this is the operator's choice to spend Codex quota over the Claude limit.
func TestRoleAgentCmdCodex(t *testing.T) {
	build := roleAgentCmd("codex", "sonnet", true, "", "")
	review := roleAgentCmd("codex", "opus", false, "", "")
	for _, c := range []string{build, review} {
		if !strings.HasPrefix(c, "codex exec ") {
			t.Errorf("codex agent must run `codex exec`, got %q", c)
		}
		if strings.Contains(c, "claude") {
			t.Errorf("codex agent must not invoke claude: %q", c)
		}
		if !strings.Contains(c, "< /dev/null") {
			t.Errorf("codex exec must read stdin from /dev/null (else it blocks): %q", c)
		}
		if !strings.Contains(c, "--dangerously-bypass-approvals-and-sandbox") {
			t.Errorf("codex must bypass approvals+sandbox to write files non-interactively: %q", c)
		}
	}
	if !strings.Contains(build, "Create the file(s)") {
		t.Errorf("codex build cmd missing the file-writing prompt: %q", build)
	}
	if !strings.Contains(build, "Do NOT run git") {
		t.Errorf("codex build cmd must forbid git (Flowbee owns the commit): %q", build)
	}
	if strings.Contains(review, "Create the file(s)") {
		t.Errorf("codex review cmd must not use the file-writing build prompt: %q", review)
	}
	// explicit --agent-cmd / --build-agent-cmd overrides still win over the codex default.
	if got := roleAgentCmd("codex", "opus", false, "OVERRIDE", ""); got != "OVERRIDE" {
		t.Errorf("override must win even under --agent codex: %q", got)
	}
}

// TestFleetRunsConflictResolverOffBuilderFamily: the fleet MUST run a conflict_resolver
// (else every real merge conflict escalates to needs_human instead of resolving
// autonomously), and any build-judging/resolving role MUST carry a non-builder
// model_family or the server-side anti-affinity makes it permanently unclaimable —
// silently wedging the pipeline.
func TestFleetRunsConflictResolverOffBuilderFamily(t *testing.T) {
	byRole := map[string]fleetRole{}
	for _, r := range nonBuilderFleetRoles() {
		byRole[r.role] = r
	}
	// both judge/resolve a build and are required for an autonomous pipeline.
	for _, must := range []string{"conflict_resolver", "code_reviewer"} {
		if _, ok := byRole[must]; !ok {
			t.Fatalf("fleet must run a %s worker (its work otherwise escalates to needs_human)", must)
		}
		if byRole[must].family == fleetBuilderFamily {
			t.Errorf("%s family %q == builder family %q — anti-affinity makes it unclaimable",
				byRole[must].family, must, fleetBuilderFamily)
		}
	}
	// the resolver authors files (resolves conflict markers) and pushes the resolution,
	// so it must run the file-writing build harness with the mirror.
	cr := byRole["conflict_resolver"]
	if !cr.writesFiles {
		t.Error("conflict_resolver must run the file-writing build harness (it resolves markers)")
	}
	if !cr.needsMirror {
		t.Error("conflict_resolver pushes the resolution, so it needs --mirror + --repo-url")
	}
}

// TestFleetSystemdTemplatesRequiredRepoURL: the --systemd env template MUST include
// FLOWBEE_REPO_URL — `flowbee fleet` hard-fails at startup without it, so omitting it
// (the prior bug) printed a unit that died on enable. With nothing in the env, a clear
// placeholder appears; the worker-auth secret line is always templated (as a
// placeholder, never a live value).
func TestFleetSystemdTemplatesRequiredRepoURL(t *testing.T) {
	t.Setenv("FLOWBEE_REPO_URL", "")
	t.Setenv("FLOWBEE_GITHUB_OWNER", "")
	t.Setenv("FLOWBEE_GITHUB_REPO", "")
	out := captureStdout(t, func() {
		printFleetSystemd("http://cp:7070", 3, "claude", "claude -p x", "claude -p y")
	})
	for _, want := range []string{
		"FLOWBEE_REPO_URL=git@github.com:OWNER/REPO.git", // required-to-start var, placeholder
		"FLOWBEE_WORKER_AUTH_SECRET=<shared-worker-secret>",
		"FLOWBEE_URL=http://cp:7070",
		"ExecStart=",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("fleet --systemd output missing %q\n--- output ---\n%s", want, out)
		}
	}
}

// TestFleetSystemdEchoesResolvedRepoURL: when FLOWBEE_REPO_URL is set, the template
// echoes the real value so the installed unit starts as-is.
func TestFleetSystemdEchoesResolvedRepoURL(t *testing.T) {
	t.Setenv("FLOWBEE_REPO_URL", "git@github.com:samhotchkiss/flowbee.git")
	out := captureStdout(t, func() {
		printFleetSystemd("http://cp:7070", 2, "claude", "a", "b")
	})
	if !strings.Contains(out, "FLOWBEE_REPO_URL=git@github.com:samhotchkiss/flowbee.git") {
		t.Errorf("must echo the resolved repo url; got:\n%s", out)
	}
}

// TestFleetSmokeRejectsEphemeralLLMRouter pins the production path called out in
// review: fleet smoke tests must resolve operator-editable model_slot_binding
// rows from the configured Flowbee database, never from a private in-memory seed.
func TestFleetSmokeRejectsEphemeralLLMRouter(t *testing.T) {
	llm.SetDefaultRouter(nil)
	t.Cleanup(func() { llm.SetDefaultRouter(nil) })
	t.Setenv("FLOWBEE_DATABASE_URL", "file:flowbee-llm-router?mode=memory&cache=shared")

	err := smokeAgent("echo ok > ok.txt")
	if err == nil {
		t.Fatal("smokeAgent must reject an ephemeral LLM router database")
	}
	if !strings.Contains(err.Error(), "not persistent") {
		t.Fatalf("smokeAgent error = %v, want persistent database guidance", err)
	}
}

// TestNextRespawnBackoff: the supervisor's respawn delay doubles and caps at 30s, so a
// crash-looping worker backs off instead of hot-spinning the box.
func TestNextRespawnBackoff(t *testing.T) {
	cases := []struct{ in, want time.Duration }{
		{1 * time.Second, 2 * time.Second},
		{2 * time.Second, 4 * time.Second},
		{16 * time.Second, 30 * time.Second}, // 32 -> capped
		{30 * time.Second, 30 * time.Second}, // stays capped
	}
	for _, c := range cases {
		if got := nextRespawnBackoff(c.in); got != c.want {
			t.Fatalf("nextRespawnBackoff(%s)=%s, want %s", c.in, got, c.want)
		}
	}
}
