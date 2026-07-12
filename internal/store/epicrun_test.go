package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/samhotchkiss/flowbee/internal/epicspec"
	"github.com/samhotchkiss/flowbee/internal/store"
	"github.com/samhotchkiss/flowbee/internal/testutil"
)

func mustAddEpicRun(t *testing.T, st *store.Store, ctx context.Context, e store.EpicRun, now time.Time) {
	t.Helper()
	if err := st.AddEpicRun(ctx, e, now); err != nil {
		t.Fatalf("add epic run %q: %v", e.ID, err)
	}
}

func TestEpicRunCRUDAndLifecycle(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 3, 12, 0, 0, 0, time.UTC)

	mustAddEpicRun(t, st, ctx, store.EpicRun{
		ID: "2026-07-03-frobnicator", Repo: "russ", FilePath: "epics/2026-07-03-frobnicator.md",
		Title: "Frobnicate", Scope: []string{"internal/frob/**"}, Host: "buncher",
		Branch: "epic/2026-07-03-frobnicator", TmuxName: "epic-2026-07-03-frobnicator", Agent: "codex",
	}, now)

	if err := st.AddEpicRun(ctx, store.EpicRun{ID: "2026-07-03-frobnicator"}, now); !errors.Is(err, store.ErrEpicRunExists) {
		t.Fatalf("expected ErrEpicRunExists, got %v", err)
	}

	e, err := st.GetEpicRun(ctx, "2026-07-03-frobnicator")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if e.State != "launching" {
		t.Fatalf("expected state=launching right after AddEpicRun, got %q", e.State)
	}
	if len(e.Scope) != 1 || e.Scope[0] != "internal/frob/**" {
		t.Fatalf("scope not round-tripped: %+v", e.Scope)
	}

	// host occupancy: buncher is now held by this epic.
	held, ok, err := st.HostActiveEpic(ctx, "buncher")
	if err != nil || !ok || held.ID != e.ID {
		t.Fatalf("HostActiveEpic: ok=%v err=%v held=%+v", ok, err, held)
	}
	if _, ok, err := st.HostActiveEpic(ctx, "idle-box"); err != nil || ok {
		t.Fatalf("expected idle-box free, got ok=%v err=%v", ok, err)
	}

	if err := st.MarkEpicLaunched(ctx, e.ID, now.Add(time.Minute)); err != nil {
		t.Fatalf("mark launched: %v", err)
	}
	e, _ = st.GetEpicRun(ctx, e.ID)
	if e.State != "running" || e.LaunchedAt == "" {
		t.Fatalf("expected running+launched_at set: %+v", e)
	}

	active, err := st.ListActiveEpicRuns(ctx)
	if err != nil || len(active) != 1 {
		t.Fatalf("list active: %v %+v", err, active)
	}

	// status ingestion: building -> still active, current step set.
	sb := epicspec.StatusBlock{
		UpdatedRaw: "2026-07-03T12:05:00Z", CurrentStep: 1, StepsTotal: 2, State: "building",
		Checklist: []epicspec.ChecklistItem{{Step: 1, Checked: false, Text: "do it"}},
	}
	if err := st.UpsertEpicStatus(ctx, e.ID, sb, now.Add(2*time.Minute)); err != nil {
		t.Fatalf("upsert status: %v", err)
	}
	e, _ = st.GetEpicRun(ctx, e.ID)
	if e.State != "running" || e.StatusCurrentStep != 1 || e.StatusStepsTotal != 2 {
		t.Fatalf("unexpected state after building status: %+v", e)
	}

	// status ingestion: blocked.
	sb.State = "blocked"
	sb.Blockers = "needs gh auth"
	if err := st.UpsertEpicStatus(ctx, e.ID, sb, now.Add(3*time.Minute)); err != nil {
		t.Fatalf("upsert status blocked: %v", err)
	}
	e, _ = st.GetEpicRun(ctx, e.ID)
	if e.State != "blocked" || e.StatusBlockers != "needs gh auth" {
		t.Fatalf("expected blocked: %+v", e)
	}

	// status ingestion: done -> terminal, finished_at set, drops out of ListActiveEpicRuns.
	sb.State = "done"
	sb.Blockers = ""
	if err := st.UpsertEpicStatus(ctx, e.ID, sb, now.Add(4*time.Minute)); err != nil {
		t.Fatalf("upsert status done: %v", err)
	}
	e, _ = st.GetEpicRun(ctx, e.ID)
	if e.State != "done" || e.FinishedAt == "" {
		t.Fatalf("expected done+finished_at: %+v", e)
	}
	active, err = st.ListActiveEpicRuns(ctx)
	if err != nil || len(active) != 0 {
		t.Fatalf("expected no active epics after done: %v %+v", err, active)
	}
	// a done epic no longer holds its host.
	if _, ok, err := st.HostActiveEpic(ctx, "buncher"); err != nil || ok {
		t.Fatalf("expected buncher released after done, ok=%v err=%v", ok, err)
	}
}

func TestEpicRunUnrecognizedStatusStateLeavesLifecycleUnchanged(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 3, 12, 0, 0, 0, time.UTC)
	mustAddEpicRun(t, st, ctx, store.EpicRun{ID: "e1", Repo: "r", TmuxName: "epic-e1"}, now)
	if err := st.MarkEpicLaunched(ctx, "e1", now); err != nil {
		t.Fatalf("mark launched: %v", err)
	}
	// garbage/unrecognized State: text must NOT change the lifecycle state.
	sb := epicspec.StatusBlock{State: "???totally unrecognized???"}
	if err := st.UpsertEpicStatus(ctx, "e1", sb, now); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	e, _ := st.GetEpicRun(ctx, "e1")
	if e.State != "running" {
		t.Fatalf("expected state to stay 'running' on unrecognized status text, got %q", e.State)
	}
}

func TestEpicRunAchievedViaLinkedGoalSession(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 3, 12, 0, 0, 0, time.UTC)

	mustAddEpicRun(t, st, ctx, store.EpicRun{
		ID: "e2", Repo: "r", TmuxName: "epic-e2",
	}, now)
	if err := st.MarkEpicLaunched(ctx, "e2", now); err != nil {
		t.Fatalf("mark launched: %v", err)
	}
	if err := st.AddGoalSession(ctx, store.GoalSession{ID: "epic-e2", TmuxName: "epic-e2"}, now); err != nil {
		t.Fatalf("add goal session: %v", err)
	}
	// simulate the watchdog observing StateAchieved on the linked session.
	if err := st.UpsertObservation(ctx, "epic-e2", "hash1", "achieved", "3d 1h", now); err != nil {
		t.Fatalf("upsert observation: %v", err)
	}
	// the agent's OWN ## Status still just says "building" (never wrote State: done)
	// — the linked session's achieved signal must surface the epic as achieved anyway.
	sb := epicspec.StatusBlock{State: "building", CurrentStep: 5, StepsTotal: 5}
	if err := st.UpsertEpicStatus(ctx, "e2", sb, now.Add(time.Minute)); err != nil {
		t.Fatalf("upsert status: %v", err)
	}
	e, _ := st.GetEpicRun(ctx, "e2")
	if e.State != "achieved" || e.FinishedAt == "" {
		t.Fatalf("expected achieved+finished_at via linked session: %+v", e)
	}
}

func TestEpicRunAbandonReleasesHostAndDisablesSession(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 3, 12, 0, 0, 0, time.UTC)

	mustAddEpicRun(t, st, ctx, store.EpicRun{ID: "e3", Repo: "r", Host: "buncher", TmuxName: "epic-e3"}, now)
	if err := st.AddGoalSession(ctx, store.GoalSession{ID: "epic-e3", TmuxName: "epic-e3"}, now); err != nil {
		t.Fatalf("add goal session: %v", err)
	}

	if err := st.AbandonEpicRun(ctx, "e3", now.Add(time.Hour)); err != nil {
		t.Fatalf("abandon: %v", err)
	}
	e, _ := st.GetEpicRun(ctx, "e3")
	if e.State != "abandoned" || e.FinishedAt == "" {
		t.Fatalf("expected abandoned+finished_at: %+v", e)
	}
	if _, ok, err := st.HostActiveEpic(ctx, "buncher"); err != nil || ok {
		t.Fatalf("expected host released after abandon, ok=%v err=%v", ok, err)
	}
	gs, err := st.GetGoalSession(ctx, "epic-e3")
	if err != nil {
		t.Fatalf("get goal session: %v", err)
	}
	if gs.Enabled {
		t.Fatalf("expected the linked goal session to be disabled (watch paused) after abandon")
	}
	// abandon does NOT delete the tmux session's registration, only pauses watching
	// it — the row must still exist (an operator decision to kill it, not ours).
	if _, err := st.GetGoalSession(ctx, "epic-e3"); err != nil {
		t.Fatalf("expected goal session row to still exist: %v", err)
	}

	if err := st.AbandonEpicRun(ctx, "nope", now); !errors.Is(err, store.ErrEpicRunNotFound) {
		t.Fatalf("expected ErrEpicRunNotFound, got %v", err)
	}
}

func TestEpicRunDeleteRollsBackFailedLaunch(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 3, 12, 0, 0, 0, time.UTC)
	mustAddEpicRun(t, st, ctx, store.EpicRun{ID: "e4", Repo: "r", Host: "buncher"}, now)
	if err := st.DeleteEpicRun(ctx, "e4"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := st.GetEpicRun(ctx, "e4"); !errors.Is(err, store.ErrEpicRunNotFound) {
		t.Fatalf("expected ErrEpicRunNotFound after delete, got %v", err)
	}
	// the host is free again (nothing left holding it).
	if _, ok, err := st.HostActiveEpic(ctx, "buncher"); err != nil || ok {
		t.Fatalf("expected buncher free after rollback, ok=%v err=%v", ok, err)
	}
}
