package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/samhotchkiss/flowbee/internal/store"
	"github.com/samhotchkiss/flowbee/internal/testutil"
	"github.com/samhotchkiss/flowbee/internal/watchdog"
)

type abandonRunner struct {
	err   error
	errs  []error
	calls []string
}

func (r *abandonRunner) Run(_ context.Context, cmd string) (string, error) {
	r.calls = append(r.calls, cmd)
	if len(r.errs) > 0 {
		err := r.errs[0]
		r.errs = r.errs[1:]
		return "", err
	}
	return "", r.err
}

func seedAbandonEpic(t *testing.T) (*store.Store, context.Context) {
	t.Helper()
	st := testutil.NewStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	if err := st.AddEpicRun(ctx, store.EpicRun{
		ID: "e1", Repo: "russ", FilePath: "epics/e1.md", Scope: []string{"internal/widget/**"},
		Host: "codex1@localhost", TmuxName: "epic-e1", AccountKey: "account-1",
	}, 1, now); err != nil {
		t.Fatalf("add epic: %v", err)
	}
	if err := st.MarkEpicLaunched(ctx, "e1", now); err != nil {
		t.Fatalf("mark epic launched: %v", err)
	}
	if err := st.AddGoalSession(ctx, store.GoalSession{
		ID: "epic-e1", Box: "codex1@localhost", TmuxName: "epic-e1", Repo: "russ",
	}, now); err != nil {
		t.Fatalf("add goal session: %v", err)
	}
	return st, ctx
}

func TestEpicAbandonStopsExactSessionBeforeReleasingReservations(t *testing.T) {
	st, ctx := seedAbandonEpic(t)
	runner := &abandonRunner{}
	var out bytes.Buffer

	if err := runEpicAbandonWithRunner(ctx, st, []string{"e1"}, runner, &out); err != nil {
		t.Fatalf("abandon: %v", err)
	}
	wantRemote := watchdog.KillTmuxSessionCmd("codex1@localhost", "epic-e1")
	wantLocal := watchdog.KillTmuxSessionCmd("", "epic-e1")
	if len(runner.calls) != 2 || runner.calls[0] != wantRemote || runner.calls[1] != wantLocal {
		t.Fatalf("stop calls = %q, want remote %q then local attach %q", runner.calls, wantRemote, wantLocal)
	}
	epic, err := st.GetEpicRun(ctx, "e1")
	if err != nil {
		t.Fatalf("get epic: %v", err)
	}
	if epic.State != "abandoned" {
		t.Fatalf("epic state = %q, want abandoned", epic.State)
	}
	active, err := st.ListActiveEpicRuns(ctx)
	if err != nil {
		t.Fatalf("list active epics: %v", err)
	}
	if len(active) != 0 {
		t.Fatalf("abandoned epic still holds reservations: %+v", active)
	}
	goal, err := st.GetGoalSession(ctx, "epic-e1")
	if err != nil {
		t.Fatalf("get goal session: %v", err)
	}
	if goal.Enabled {
		t.Fatal("goal-session watch remained enabled after confirmed stop")
	}
	if got := out.String(); !strings.Contains(got, `tmux session "epic-e1" stopped`) || !strings.Contains(got, "private worktree was preserved") {
		t.Fatalf("abandon output did not describe safe cleanup and recovery: %q", got)
	}
}

func TestEpicAbandonLocalAttachStopFailureRetainsReservations(t *testing.T) {
	st, ctx := seedAbandonEpic(t)
	runner := &abandonRunner{errs: []error{nil, errors.New("local tmux unavailable")}}
	var out bytes.Buffer

	err := runEpicAbandonWithRunner(ctx, st, []string{"e1"}, runner, &out)
	if err == nil || !strings.Contains(err.Error(), "could not confirm local tmux session") {
		t.Fatalf("abandon error=%v, want unconfirmed local attach failure", err)
	}
	wantRemote := watchdog.KillTmuxSessionCmd("codex1@localhost", "epic-e1")
	wantLocal := watchdog.KillTmuxSessionCmd("", "epic-e1")
	if len(runner.calls) != 2 || runner.calls[0] != wantRemote || runner.calls[1] != wantLocal {
		t.Fatalf("stop calls = %q, want remote %q then local attach %q", runner.calls, wantRemote, wantLocal)
	}
	epic, getErr := st.GetEpicRun(ctx, "e1")
	if getErr != nil || epic.State != "running" {
		t.Fatalf("failed local stop released epic: state=%q err=%v", epic.State, getErr)
	}
	active, listErr := st.ListActiveEpicRuns(ctx)
	if listErr != nil || len(active) != 1 {
		t.Fatalf("failed local stop released capacity: active=%+v err=%v", active, listErr)
	}
	if out.Len() != 0 {
		t.Fatalf("failed abandon printed success output: %q", out.String())
	}
}

func TestEpicAbandonStopFailureRetainsReservationsAndWatch(t *testing.T) {
	st, ctx := seedAbandonEpic(t)
	runner := &abandonRunner{err: errors.New("ssh: connect to host: connection timed out")}
	var out bytes.Buffer

	err := runEpicAbandonWithRunner(ctx, st, []string{"e1"}, runner, &out)
	if err == nil {
		t.Fatal("abandon succeeded without a confirmed tmux stop")
	}
	if !strings.Contains(err.Error(), "could not confirm remote tmux session") || !strings.Contains(err.Error(), "remains active") {
		t.Fatalf("error does not explain retained reservation: %v", err)
	}
	wantCmd := watchdog.KillTmuxSessionCmd("codex1@localhost", "epic-e1")
	if len(runner.calls) != 1 || runner.calls[0] != wantCmd {
		t.Fatalf("stop calls = %q, want exactly %q", runner.calls, wantCmd)
	}
	epic, getErr := st.GetEpicRun(ctx, "e1")
	if getErr != nil {
		t.Fatalf("get epic: %v", getErr)
	}
	if epic.State != "running" {
		t.Fatalf("epic state = %q after failed stop, want running", epic.State)
	}
	active, listErr := st.ListActiveEpicRuns(ctx)
	if listErr != nil {
		t.Fatalf("list active epics: %v", listErr)
	}
	if len(active) != 1 || active[0].ID != "e1" {
		t.Fatalf("failed stop released the epic reservations: %+v", active)
	}
	goal, goalErr := st.GetGoalSession(ctx, "epic-e1")
	if goalErr != nil {
		t.Fatalf("get goal session: %v", goalErr)
	}
	if !goal.Enabled {
		t.Fatal("failed stop disabled the goal-session watch")
	}
	if out.Len() != 0 {
		t.Fatalf("failed abandon printed success output: %q", out.String())
	}
}
