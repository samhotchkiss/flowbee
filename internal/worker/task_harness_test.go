package worker_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/samhotchkiss/flowbee/client"
	"github.com/samhotchkiss/flowbee/internal/api"
	"github.com/samhotchkiss/flowbee/internal/clock"
	"github.com/samhotchkiss/flowbee/internal/gitops"
	"github.com/samhotchkiss/flowbee/internal/job"
	"github.com/samhotchkiss/flowbee/internal/store"
	"github.com/samhotchkiss/flowbee/internal/testutil"
	"github.com/samhotchkiss/flowbee/internal/ulid"
	"github.com/samhotchkiss/flowbee/internal/worker"
)

// newBareMirror builds a local bare repo with one commit on main (no network, no
// GitHub) and returns its path plus the base SHA — the §7.4 worktree fixture.
func newBareMirror(t *testing.T) (string, string) {
	t.Helper()
	root := t.TempDir()
	bare := filepath.Join(root, "mirror.git")
	m, err := gitops.InitBare(bare)
	if err != nil {
		t.Fatalf("init bare: %v", err)
	}
	work := filepath.Join(root, "seed")
	mustRun(t, "", "git", "clone", bare, work)
	if err := os.WriteFile(filepath.Join(work, "README.md"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustRun(t, work, "git", "-c", "user.email=t@t", "-c", "user.name=t", "add", "-A")
	mustRun(t, work, "git", "-c", "user.email=t@t", "-c", "user.name=t", "commit", "-m", "init")
	mustRun(t, work, "git", "branch", "-M", "main")
	mustRun(t, work, "git", "push", "origin", "main")
	sha, err := m.HeadSHA("refs/heads/main")
	if err != nil {
		t.Fatalf("head sha: %v", err)
	}
	return bare, sha
}

// TestHarnessWritesTaskAndLeaseCarriesContext is the F1 acceptance test (DONE-WHEN):
// a seeded job with a task -> a fake agent reads the task FILE and ENV in the
// worktree and ACTS on it (writes a file derived from the task), AND the lease JSON
// the worker received carries the resolved context block (task/spec/acceptance/
// identity/base_sha). Real SQLite, real git mirror, in-memory HTTP, no GitHub.
func TestHarnessWritesTaskAndLeaseCarriesContext(t *testing.T) {
	mirrorPath, baseSHA := newBareMirror(t)

	st := testutil.NewStore(t)
	srv := api.New(st, clock.Real{}, ulid.NewMinter(nil), api.Config{
		LeaseTTL: time.Minute, LongPollWait: time.Second, LeaseTTLS: 300, HeartbeatIntervalS: 30,
		MirrorPath: mirrorPath,
	}, "test")
	ts := httptest.NewServer(srv.PrivateHandler())
	defer ts.Close()
	ctx := context.Background()

	const (
		task   = "create greeting.txt containing the word flowbee"
		spec   = "the file must live at repo root"
		accept = "- greeting.txt exists\n- it contains 'flowbee'"
	)
	jobID := ulid.New()
	if _, err := st.SeedJob(ctx, store.SeedParams{
		ID: jobID, Kind: job.KindBuild, Flow: "build", Stage: "build",
		Role: job.RoleEngWorker, BaseSHA: baseSHA, Now: time.Unix(1, 0),
		TaskText: task, SpecText: spec, AcceptanceCriteria: accept,
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// The fake agent is the default agent-cmd convention: it reads $FLOWBEE_TASK_FILE
	// (and the inline $FLOWBEE_TASK / $FLOWBEE_ACCEPTANCE env) and acts on it. It is
	// a black box to the harness; here it proves it consumed BOTH the file and env by
	// echoing them into its work-product.
	agentCmd := `set -e
test -f "$FLOWBEE_TASK_FILE" || { echo "no task file" >&2; exit 1; }
grep -q flowbee "$FLOWBEE_TASK_FILE" || { echo "task file missing task" >&2; exit 1; }
echo "$FLOWBEE_TASK" > greeting.txt
echo "via-env-acceptance: $FLOWBEE_ACCEPTANCE" >> greeting.txt
cp "$FLOWBEE_TASK_FILE" agent-saw-task.md`

	out, err := worker.RunOnceHarness(ctx, worker.HarnessConfig{
		BaseURL: ts.URL, Identity: "builder-1", ModelFamily: "codex",
		Role: string(job.RoleEngWorker), AgentCmd: agentCmd,
	})
	if err != nil {
		t.Fatalf("RunOnceHarness: %v", err)
	}
	if !out.Got || out.JobID != jobID {
		t.Fatalf("harness did not lease the seeded job: %+v", out)
	}
	if out.JobState != string(job.StateReviewPending) {
		t.Fatalf("final state=%s want review_pending", out.JobState)
	}

	// The agent ACTED on the task: its work-product (the pushed epoch ref) contains
	// greeting.txt derived from the task text + acceptance env. We check out the
	// epoch ref into a fresh worktree and inspect it.
	m := gitops.Open(mirrorPath)
	ref := gitops.EpochRef(jobID, out.LeaseEpoch)
	sha, ok := m.RefSHA(ref)
	if !ok {
		t.Fatalf("epoch ref %s not pushed", ref)
	}
	checkout := filepath.Join(t.TempDir(), "verify")
	wt, err := m.AddWorktree(checkout, sha)
	if err != nil {
		t.Fatalf("add verify worktree: %v", err)
	}
	defer wt.Destroy()

	greeting, err := os.ReadFile(filepath.Join(checkout, "greeting.txt"))
	if err != nil {
		t.Fatalf("agent did not act on the task (no greeting.txt): %v", err)
	}
	if !strings.Contains(string(greeting), task) {
		t.Fatalf("greeting.txt does not reflect the task text: %q", greeting)
	}
	if !strings.Contains(string(greeting), "via-env-acceptance") {
		t.Fatalf("agent did not read the acceptance env: %q", greeting)
	}
	// the agent saw the .flowbee/task.md brief (it copied it into the work-product).
	saw, err := os.ReadFile(filepath.Join(checkout, "agent-saw-task.md"))
	if err != nil {
		t.Fatalf("agent did not see the task file: %v", err)
	}
	for _, want := range []string{task, spec, "greeting.txt exists", "builder-1"} {
		if !strings.Contains(string(saw), want) {
			t.Fatalf("task.md missing %q:\n%s", want, saw)
		}
	}
	// CRITICAL: the .flowbee/ scaffolding must NOT pollute the untrusted work-product.
	if _, err := os.Stat(filepath.Join(checkout, ".flowbee")); !os.IsNotExist(err) {
		t.Fatalf(".flowbee/ leaked into the work-product (err=%v)", err)
	}

	// The lease JSON itself carries the resolved context block (§B). We re-lease a
	// second seeded job over raw HTTP and assert the wire shape includes `context`.
	job2 := ulid.New()
	if _, err := st.SeedJob(ctx, store.SeedParams{
		ID: job2, Kind: job.KindBuild, Flow: "build", Stage: "build",
		Role: job.RoleEngWorker, BaseSHA: baseSHA, Now: time.Unix(2, 0),
		TaskText: task, SpecText: spec, AcceptanceCriteria: accept,
	}); err != nil {
		t.Fatalf("seed2: %v", err)
	}
	resp, err := http.Get(ts.URL + "/v1/lease?identity=builder-2&model_family=codex&role=eng_worker")
	if err != nil {
		t.Fatalf("lease http: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("lease status=%d", resp.StatusCode)
	}
	// decode into the strongly-typed client grant AND a raw map (to assert the JSON
	// key `context` is actually on the wire, not just a zero struct).
	var raw map[string]any
	body := mustReadAll(t, resp)
	if err := json.Unmarshal(body, &raw); err != nil {
		t.Fatalf("decode raw: %v", err)
	}
	if _, ok := raw["context"]; !ok {
		t.Fatalf("lease JSON missing `context` block:\n%s", body)
	}
	var grant client.LeaseGrant
	if err := json.Unmarshal(body, &grant); err != nil {
		t.Fatalf("decode grant: %v", err)
	}
	if grant.Context == nil {
		t.Fatalf("grant.Context nil")
	}
	if grant.Context.Task != task || grant.Context.Spec != spec || grant.Context.AcceptanceCriteria != accept {
		t.Fatalf("context block missing task fields: %+v", grant.Context)
	}
	if grant.Context.Identity != "builder-2" {
		t.Fatalf("context identity=%q want builder-2 (the resolved/fenced identity)", grant.Context.Identity)
	}
	if grant.Context.BaseSHA != baseSHA || grant.Context.Role != string(job.RoleEngWorker) {
		t.Fatalf("context base/role wrong: %+v", grant.Context)
	}
}

func mustRun(t *testing.T, dir, name string, args ...string) {
	t.Helper()
	cmd := exec.Command(name, args...)
	if dir != "" {
		cmd.Dir = dir
	}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%s %s: %v: %s", name, strings.Join(args, " "), err, out)
	}
}

func mustReadAll(t *testing.T, resp *http.Response) []byte {
	t.Helper()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return b
}
