package main

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/samhotchkiss/flowbee/internal/store"
)

func newSessionTestDB(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "flowbee.db")
	ctx := context.Background()
	st, err := store.Open(ctx, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.MigrateUp(ctx, st.DB); err != nil {
		t.Fatal(err)
	}
	st.Close()
	t.Setenv("FLOWBEE_DATABASE_URL", dbPath)
	t.Setenv("FLOWBEE_CONFIG", "") // don't pick up a stray flowbee.yaml
	return dbPath
}

// TestRunSessionAddListRmPause exercises the full CLI surface end to end against a
// real (temp-file) DB — `flowbee session add/list/pause/resume-watch/rm`.
func TestRunSessionAddListRmPause(t *testing.T) {
	dbPath := newSessionTestDB(t)
	ctx := context.Background()

	if err := runSession([]string{"add", "russ-terra", "--tmux", "goal-terra", "--box", "buncher", "--repo", "russ"}); err != nil {
		t.Fatalf("add: %v", err)
	}
	// duplicate id is rejected, not silently upserted.
	if err := runSession([]string{"add", "russ-terra", "--tmux", "x"}); err == nil {
		t.Fatalf("expected an error re-adding an existing id")
	}
	// --tmux is required.
	if err := runSession([]string{"add", "no-tmux"}); err == nil {
		t.Fatalf("expected an error with no --tmux")
	}

	st, err := store.Open(ctx, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	g, err := st.GetGoalSession(ctx, "russ-terra")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if g.Box != "buncher" || g.TmuxName != "goal-terra" || g.Repo != "russ" || !g.Enabled {
		t.Fatalf("unexpected row: %+v", g)
	}

	if err := runSession([]string{"pause", "russ-terra"}); err != nil {
		t.Fatalf("pause: %v", err)
	}
	g, _ = st.GetGoalSession(ctx, "russ-terra")
	if g.Enabled {
		t.Fatalf("expected paused (enabled=false)")
	}
	if err := runSession([]string{"resume-watch", "russ-terra"}); err != nil {
		t.Fatalf("resume-watch: %v", err)
	}
	g, _ = st.GetGoalSession(ctx, "russ-terra")
	if !g.Enabled {
		t.Fatalf("expected watching again (enabled=true)")
	}

	if err := runSession([]string{"rm", "russ-terra"}); err != nil {
		t.Fatalf("rm: %v", err)
	}
	if _, err := st.GetGoalSession(ctx, "russ-terra"); err == nil {
		t.Fatalf("expected the session to be gone after rm")
	}
	if err := runSession([]string{"rm", "russ-terra"}); err == nil {
		t.Fatalf("expected an error removing a nonexistent session")
	}
}

func TestPrintSessionList(t *testing.T) {
	var buf bytes.Buffer
	printSessionList(&buf, nil)
	if !strings.Contains(buf.String(), "no goal sessions registered") {
		t.Fatalf("expected the empty-registry message, got:\n%s", buf.String())
	}

	buf.Reset()
	printSessionList(&buf, []store.GoalSession{
		{ID: "russ-terra", Box: "buncher", TmuxName: "goal-terra", Repo: "russ", State: "pursuing", GoalElapsed: "2d 4h", Enabled: true},
		{ID: "russ-sol", TmuxName: "goal-sol", State: "blocked", StateDetail: "needs_operator: gh auth", Enabled: false},
	})
	out := buf.String()
	for _, want := range []string{"russ-terra", "buncher", "pursuing", "2d 4h", "russ-sol", "local", "blocked", "needs_operator", "PAUSED"} {
		if !strings.Contains(out, want) {
			t.Errorf("expected output to contain %q, got:\n%s", want, out)
		}
	}
}

func TestFormatSessionLine(t *testing.T) {
	line := formatSessionLine(store.GoalSession{
		ID: "russ-terra", Box: "buncher", State: "pursuing", GoalElapsed: "2d 4h 12m", Enabled: true,
	})
	if line != "russ-terra · buncher · pursuing (2d 4h 12m)" {
		t.Fatalf("got %q", line)
	}

	line = formatSessionLine(store.GoalSession{
		ID: "russ-sol", State: "blocked", StateDetail: "needs_operator: gh auth", Enabled: false,
	})
	if line != "russ-sol · local · blocked [needs_operator: gh auth] [paused]" {
		t.Fatalf("got %q", line)
	}
}
