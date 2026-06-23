package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/samhotchkiss/flowbee/internal/job"
	"github.com/samhotchkiss/flowbee/internal/store"
)

// TestPrintStatusModelBreakdown: the fleet line shows the live-worker per-backend tally
// (sorted, stable) so an operator sees the fleet is on codex; no models => no suffix.
func TestPrintStatusModelBreakdown(t *testing.T) {
	var buf bytes.Buffer
	printStatus(&buf, nil, store.FleetHealth{LiveWorkers: 16, ByModel: map[string]int{"codex": 14, "sonnet": 2}}, nil, false)
	if got := buf.String(); !strings.Contains(got, "16 live") || !strings.Contains(got, "(codex:14, sonnet:2)") {
		t.Errorf("expected live count + sorted model breakdown, got:\n%s", got)
	}
	var buf2 bytes.Buffer
	printStatus(&buf2, nil, store.FleetHealth{LiveWorkers: 3}, nil, false)
	if got := buf2.String(); strings.Contains(got, "(") {
		t.Errorf("no models => no breakdown suffix, got:\n%s", got)
	}
}

// TestPrintStatusAbandoned: dropped GitHub writes surface in the human view (sorted, pointing
// at the recovery command); none => no line.
func TestPrintStatusAbandoned(t *testing.T) {
	var buf bytes.Buffer
	printStatus(&buf, nil, store.FleetHealth{}, map[string]int{"issues.create": 4, "mergeQueue.enqueue": 6}, false)
	out := buf.String()
	if !strings.Contains(out, "abandoned GitHub writes: issues.create:4, mergeQueue.enqueue:6") || !strings.Contains(out, "flowbee retry-outbox") {
		t.Errorf("expected the abandoned line + recovery hint, got:\n%s", out)
	}
	var buf2 bytes.Buffer
	printStatus(&buf2, nil, store.FleetHealth{}, nil, false)
	if strings.Contains(buf2.String(), "abandoned") {
		t.Errorf("no abandoned actions => no line, got:\n%s", buf2.String())
	}
}

func TestPrintStatusMergeHandoff(t *testing.T) {
	jobs := []store.BoardJob{
		{ID: "1", Repo: "acme/api", State: "merge_handoff"},
		{ID: "2", Repo: "acme/api", State: "merge_handoff"},
		{ID: "3", Repo: "acme/api", State: "running"},
		{ID: "4", Repo: "octo/infra", State: "needs_human"},
	}
	health := store.FleetHealth{LiveWorkers: 2, StaleWorkers: 1}

	var buf bytes.Buffer
	printStatus(&buf, jobs, health, nil, false)
	out := buf.String()

	for _, want := range []string{
		"2 merge_handoff",
		"1 needs_human",
		"2 live",
		"1 stale",
		"acme/api",
		"octo/infra",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("expected %q in output:\n%s", want, out)
		}
	}
}

func TestPrintStatusEmpty(t *testing.T) {
	var buf bytes.Buffer
	printStatus(&buf, nil, store.FleetHealth{}, nil, false)
	out := buf.String()

	if !strings.Contains(out, "no jobs") {
		t.Errorf("expected 'no jobs' in empty output:\n%s", out)
	}
	if !strings.Contains(out, "0 merge_handoff") {
		t.Errorf("expected '0 merge_handoff' in empty output:\n%s", out)
	}
}

func TestPrintStatusRepoStateCounts(t *testing.T) {
	jobs := []store.BoardJob{
		{ID: "1", Repo: "corp/svc", State: "running"},
		{ID: "2", Repo: "corp/svc", State: "running"},
		{ID: "3", Repo: "corp/svc", State: "ready"},
	}
	var buf bytes.Buffer
	printStatus(&buf, jobs, store.FleetHealth{LiveWorkers: 1}, nil, false)
	out := buf.String()

	if !strings.Contains(out, "running:2") {
		t.Errorf("expected 'running:2' in output:\n%s", out)
	}
	if !strings.Contains(out, "ready:1") {
		t.Errorf("expected 'ready:1' in output:\n%s", out)
	}
}

func TestPrintStatusPausedBanner(t *testing.T) {
	var buf bytes.Buffer
	printStatus(&buf, nil, store.FleetHealth{}, nil, true)
	out := buf.String()

	if !strings.Contains(out, "PAUSED") {
		t.Errorf("expected PAUSED banner in paused output:\n%s", out)
	}
}

func TestStatusDefaultTextOutputByteExact(t *testing.T) {
	jobs := statusFixtureJobs()
	health := store.FleetHealth{
		LiveWorkers:  2,
		StaleWorkers: 3,
		ByModel:      map[string]int{"sonnet": 1, "codex": 1},
		StaleByModel: map[string]int{"codex": 1, "opus": 2},
	}
	abandoned := map[string]int{"issues.create": 2, "pulls.create": 1}

	var buf bytes.Buffer
	printStatus(&buf, jobs, health, abandoned, false)

	const want = "acme/api    merge_handoff:2  running:1\n" +
		"octo/infra  needs_human:1\n" +
		"\n" +
		"awaiting human: 2 merge_handoff, 1 needs_human\n" +
		"fleet: 2 live, 3 stale workers (codex:1, sonnet:1)\n" +
		"⚠ abandoned GitHub writes: issues.create:2, pulls.create:1 — fix the cause, then `flowbee retry-outbox <job-id>` / `--repo <id>` / `--all`\n"
	if got := buf.String(); got != want {
		t.Fatalf("status text changed\nwant:\n%q\ngot:\n%q", want, got)
	}
}

func TestStatusJSONOutput(t *testing.T) {
	summary := summarizeStatus(statusFixtureJobs(), store.FleetHealth{
		LiveWorkers:  2,
		StaleWorkers: 3,
		ByModel:      map[string]int{"sonnet": 1, "codex": 1},
		StaleByModel: map[string]int{"codex": 1, "opus": 2},
	}, map[string]int{"issues.create": 2, "pulls.create": 1}, false)

	var buf bytes.Buffer
	if err := printStatusJSON(&buf, summary); err != nil {
		t.Fatalf("printStatusJSON: %v", err)
	}
	if !strings.HasSuffix(buf.String(), "\n") {
		t.Fatalf("JSON output should end in one trailing newline, got %q", buf.String())
	}

	dec := json.NewDecoder(strings.NewReader(buf.String()))
	var out struct {
		Repos map[string]struct {
			States map[string]int `json:"states"`
		} `json:"repos"`
		AwaitingHuman struct {
			MergeHandoff int `json:"merge_handoff"`
			NeedsHuman   int `json:"needs_human"`
			Total        int `json:"total"`
		} `json:"awaiting_human"`
		Fleet struct {
			LiveWorkers  int `json:"live_workers"`
			StaleWorkers int `json:"stale_workers"`
			ByModel      map[string]struct {
				LiveWorkers  int `json:"live_workers"`
				StaleWorkers int `json:"stale_workers"`
				TotalWorkers int `json:"total_workers"`
			} `json:"by_model"`
		} `json:"fleet"`
		AbandonedGitHubWrites map[string]int `json:"abandoned_github_writes"`
	}
	if err := dec.Decode(&out); err != nil {
		t.Fatalf("JSON did not decode: %v\n%s", err, buf.String())
	}
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		t.Fatalf("JSON output has extra data after first object: %v", err)
	}

	if got := out.Repos["acme/api"].States; !reflect.DeepEqual(got, map[string]int{"merge_handoff": 2, "running": 1}) {
		t.Fatalf("acme/api states = %#v", got)
	}
	if got := out.Repos["octo/infra"].States; !reflect.DeepEqual(got, map[string]int{"needs_human": 1}) {
		t.Fatalf("octo/infra states = %#v", got)
	}
	if out.AwaitingHuman.MergeHandoff != 2 || out.AwaitingHuman.NeedsHuman != 1 || out.AwaitingHuman.Total != 3 {
		t.Fatalf("awaiting_human = %+v", out.AwaitingHuman)
	}
	if out.Fleet.LiveWorkers != 2 || out.Fleet.StaleWorkers != 3 {
		t.Fatalf("fleet counts = %+v", out.Fleet)
	}
	if got := out.Fleet.ByModel["codex"]; got.LiveWorkers != 1 || got.StaleWorkers != 1 || got.TotalWorkers != 2 {
		t.Fatalf("codex model counts = %+v", got)
	}
	if got := out.Fleet.ByModel["opus"]; got.LiveWorkers != 0 || got.StaleWorkers != 2 || got.TotalWorkers != 2 {
		t.Fatalf("opus model counts = %+v", got)
	}
	if got := out.Fleet.ByModel["sonnet"]; got.LiveWorkers != 1 || got.StaleWorkers != 0 || got.TotalWorkers != 1 {
		t.Fatalf("sonnet model counts = %+v", got)
	}
	if !reflect.DeepEqual(out.AbandonedGitHubWrites, map[string]int{"issues.create": 2, "pulls.create": 1}) {
		t.Fatalf("abandoned_github_writes = %#v", out.AbandonedGitHubWrites)
	}
}

func TestStatusJSONEmptyAbandonedWritesObject(t *testing.T) {
	var buf bytes.Buffer
	if err := printStatusJSON(&buf, summarizeStatus(nil, store.FleetHealth{}, nil, false)); err != nil {
		t.Fatalf("printStatusJSON: %v", err)
	}
	var out struct {
		AbandonedGitHubWrites map[string]int `json:"abandoned_github_writes"`
	}
	if err := json.Unmarshal(buf.Bytes(), &out); err != nil {
		t.Fatalf("JSON did not decode: %v\n%s", err, buf.String())
	}
	if out.AbandonedGitHubWrites == nil {
		t.Fatalf("abandoned_github_writes encoded as null, want empty object: %s", buf.String())
	}
	if len(out.AbandonedGitHubWrites) != 0 {
		t.Fatalf("abandoned_github_writes = %#v, want empty", out.AbandonedGitHubWrites)
	}
}

func TestRunStatusJSONFlag(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "flowbee.db")
	st, err := store.Open(ctx, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.MigrateUp(ctx, st.DB); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	seed := func(id, repo, state string) {
		t.Helper()
		if _, err := st.SeedJob(ctx, store.SeedParams{
			ID: id, Repo: repo, Kind: job.KindBuild, Flow: "build", Stage: "build", Role: job.RoleEngWorker, Now: now,
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := st.DB.ExecContext(ctx, `UPDATE jobs SET state=? WHERE id=?`, state, id); err != nil {
			t.Fatal(err)
		}
	}
	seed("j1", "acme/api", "merge_handoff")
	seed("j2", "acme/api", "needs_human")
	exp := now.Add(time.Hour).Format(time.RFC3339Nano)
	live := now.Format(time.RFC3339Nano)
	old := now.Add(-10 * time.Minute).Format(time.RFC3339Nano)
	if _, err := st.DB.ExecContext(ctx,
		`INSERT INTO workers (worker_id, identity, host, attested_capabilities, attestation_expires_at, last_seen_at)
		 VALUES ('w1','w1','box','["model:codex"]',?,?),
		        ('w2','w2','box','["model:opus"]',?,?)`, exp, live, exp, old); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB.ExecContext(ctx,
		`INSERT INTO outbox (job_id, action, head_sha, status) VALUES ('j1','issues.create','h1','abandoned')`); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	cfgPath := filepath.Join(dir, "flowbee.yaml")
	if err := os.WriteFile(cfgPath, []byte("database_url: "+dbPath+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FLOWBEE_CONFIG", cfgPath)
	t.Setenv("FLOWBEE_DATABASE_URL", "")

	var runErr error
	out := captureStdout(t, func() { runErr = runStatus([]string{"--json"}) })
	err = runErr
	if err != nil {
		t.Fatalf("runStatus --json: %v", err)
	}
	if strings.Contains(out, "awaiting human:") || strings.Contains(out, "fleet:") {
		t.Fatalf("JSON mode wrote human text:\n%s", out)
	}

	var decoded struct {
		Repos map[string]struct {
			States map[string]int `json:"states"`
		} `json:"repos"`
		AwaitingHuman struct {
			MergeHandoff int `json:"merge_handoff"`
			NeedsHuman   int `json:"needs_human"`
			Total        int `json:"total"`
		} `json:"awaiting_human"`
		Fleet struct {
			LiveWorkers  int `json:"live_workers"`
			StaleWorkers int `json:"stale_workers"`
			ByModel      map[string]struct {
				LiveWorkers  int `json:"live_workers"`
				StaleWorkers int `json:"stale_workers"`
				TotalWorkers int `json:"total_workers"`
			} `json:"by_model"`
		} `json:"fleet"`
		AbandonedGitHubWrites map[string]int `json:"abandoned_github_writes"`
	}
	if err := json.Unmarshal([]byte(out), &decoded); err != nil {
		t.Fatalf("status JSON did not decode: %v\n%s", err, out)
	}
	if decoded.Repos["acme/api"].States["merge_handoff"] != 1 || decoded.Repos["acme/api"].States["needs_human"] != 1 {
		t.Fatalf("repo states = %#v", decoded.Repos["acme/api"].States)
	}
	if decoded.AwaitingHuman.MergeHandoff != 1 || decoded.AwaitingHuman.NeedsHuman != 1 || decoded.AwaitingHuman.Total != 2 {
		t.Fatalf("awaiting_human = %+v", decoded.AwaitingHuman)
	}
	if decoded.Fleet.LiveWorkers != 1 || decoded.Fleet.StaleWorkers != 1 {
		t.Fatalf("fleet = %+v", decoded.Fleet)
	}
	if got := decoded.Fleet.ByModel["codex"]; got.LiveWorkers != 1 || got.StaleWorkers != 0 || got.TotalWorkers != 1 {
		t.Fatalf("codex = %+v", got)
	}
	if got := decoded.Fleet.ByModel["opus"]; got.LiveWorkers != 0 || got.StaleWorkers != 1 || got.TotalWorkers != 1 {
		t.Fatalf("opus = %+v", got)
	}
	if !reflect.DeepEqual(decoded.AbandonedGitHubWrites, map[string]int{"issues.create": 1}) {
		t.Fatalf("abandoned_github_writes = %#v", decoded.AbandonedGitHubWrites)
	}
}

func statusFixtureJobs() []store.BoardJob {
	return []store.BoardJob{
		{ID: "1", Repo: "acme/api", State: "merge_handoff"},
		{ID: "2", Repo: "acme/api", State: "merge_handoff"},
		{ID: "3", Repo: "acme/api", State: "running"},
		{ID: "4", Repo: "octo/infra", State: "needs_human"},
	}
}

// TestPrintStatusStarvationDetector: ready work + live workers + nothing actively building =
// the starvation symptom (the merge_handoff reservation incident). It must surface loudly so a
// future wedge is never silent. A fleet that IS building, or has no ready work, must NOT warn.
func TestPrintStatusStarvationDetector(t *testing.T) {
	starved := []store.BoardJob{
		{ID: "1", Repo: "r", State: "ready"},
		{ID: "2", Repo: "r", State: "ready"},
		{ID: "3", Repo: "r", State: "merge_handoff"},
	}
	var buf bytes.Buffer
	printStatus(&buf, starved, store.FleetHealth{LiveWorkers: 14}, nil, false)
	if !strings.Contains(buf.String(), "STARVATION") {
		t.Errorf("ready jobs + live workers + 0 active must warn STARVATION:\n%s", buf.String())
	}

	// a fleet actively building must NOT warn.
	working := []store.BoardJob{
		{ID: "1", Repo: "r", State: "ready"},
		{ID: "2", Repo: "r", State: "building"},
	}
	var buf2 bytes.Buffer
	printStatus(&buf2, working, store.FleetHealth{LiveWorkers: 14}, nil, false)
	if strings.Contains(buf2.String(), "STARVATION") {
		t.Errorf("a building fleet must NOT warn starvation:\n%s", buf2.String())
	}

	// no ready work => not starved (an idle fleet with nothing to do is fine).
	var buf3 bytes.Buffer
	printStatus(&buf3, []store.BoardJob{{ID: "1", Repo: "r", State: "done"}}, store.FleetHealth{LiveWorkers: 14}, nil, false)
	if strings.Contains(buf3.String(), "STARVATION") {
		t.Errorf("no ready work must NOT warn starvation:\n%s", buf3.String())
	}
}
