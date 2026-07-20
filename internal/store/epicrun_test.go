package store_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/samhotchkiss/flowbee/internal/epicspec"
	"github.com/samhotchkiss/flowbee/internal/store"
	"github.com/samhotchkiss/flowbee/internal/testutil"
)

// mustAddEpicRun registers an epic at the DEFAULT host cap of 1 (the original
// one-box-one-epic behavior) — the cases exercising a higher cap call AddEpicRun
// directly with the cap they want.
func mustAddEpicRun(t *testing.T, st *store.Store, ctx context.Context, e store.EpicRun, now time.Time) {
	t.Helper()
	if err := st.AddEpicRun(ctx, e, 1, now); err != nil {
		t.Fatalf("add epic run %q: %v", e.ID, err)
	}
}

func bindEpicTestRepo(t *testing.T, st *store.Store, projectID, repoID string, now time.Time) {
	t.Helper()
	if err := st.RegisterRepo(context.Background(), store.Repo{ID: repoID, Owner: "fixture", Repo: repoID, Active: true}); err != nil {
		t.Fatal(err)
	}
	if err := st.AddProjectRepo(context.Background(), projectID, repoID, now); err != nil {
		t.Fatal(err)
	}
}

func TestAddEpicRunV2AdmissionIsIdempotentAndCreatesReviewObligation(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	if _, err := st.DB.Exec(`INSERT INTO projects(id,name) VALUES ('proj-a','Project A')`); err != nil {
		t.Fatal(err)
	}
	bindEpicTestRepo(t, st, "proj-a", "r", now)
	e := store.EpicRun{ID: "epic-ulid-1", ProjectID: "proj-a", AdmissionKey: "intent-1:v1", ContractHash: "sha256:contract", Repo: "r", Branch: "dev/russ", Scope: []string{"pkg/**"}}
	if err := st.AddEpicRun(ctx, e, 1, now); err != nil {
		t.Fatal(err)
	}
	// Lost-ack retry may choose a different local slug, but the stable key returns
	// success and must not create another delivery obligation.
	e.ID = "different-retry-slug"
	if err := st.AddEpicRun(ctx, e, 1, now); err != nil {
		t.Fatalf("idempotent retry: %v", err)
	}
	var epics, deliveries int
	if err := st.DB.QueryRow(`SELECT COUNT(*) FROM epics WHERE project_id=? AND admission_key=?`, e.ProjectID, e.AdmissionKey).Scan(&epics); err != nil {
		t.Fatal(err)
	}
	if err := st.DB.QueryRow(`SELECT COUNT(*) FROM epic_deliveries WHERE project_id=?`, e.ProjectID).Scan(&deliveries); err != nil {
		t.Fatal(err)
	}
	if epics != 1 || deliveries != 1 {
		t.Fatalf("duplicate admission: epics=%d deliveries=%d", epics, deliveries)
	}
	var required int
	if err := st.DB.QueryRow(`SELECT review_required FROM epic_deliveries WHERE epic_id=?`, "epic-ulid-1").Scan(&required); err != nil {
		t.Fatal(err)
	}
	if required != 1 {
		t.Fatalf("review obligation not durable: %d", required)
	}
}

func TestAddEpicRunV2AdmissionConflictsOnChangedContract(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	if _, err := st.DB.Exec(`INSERT INTO projects(id,name) VALUES ('proj-a','Project A')`); err != nil {
		t.Fatal(err)
	}
	bindEpicTestRepo(t, st, "proj-a", "r", now)
	e := store.EpicRun{ID: "epic-contract", ProjectID: "proj-a", AdmissionKey: "intent-2:v1", ContractHash: "hash-a", Repo: "r", Scope: []string{"a/**"}}
	if err := st.AddEpicRun(ctx, e, 1, now); err != nil {
		t.Fatal(err)
	}
	e.ID, e.ContractHash = "epic-contract-retry", "hash-b"
	if err := st.AddEpicRun(ctx, e, 1, now); !errors.Is(err, store.ErrEpicAdmissionConflict) {
		t.Fatalf("expected contract conflict, got %v", err)
	}
}

// TestAddEpicRunSeatCapacityIsAtomic proves the throughput model's real invariant:
// capacity belongs to an authenticated seat, not to the host string. Two distinct
// cap-1 seats on the same (including local/empty) host can run together; one cap-2
// seat admits exactly two racing starts and refuses the third. The binding is part
// of the initial insert, so a launching row is visible to the next atomic count.
func TestAddEpicRunSeatCapacityIsAtomic(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	addSeat := func(seat store.Seat) store.Seat {
		t.Helper()
		if err := st.AddSeat(ctx, seat, now); err != nil {
			t.Fatalf("add seat %+v: %v", seat, err)
		}
		seat.ID = seat.ComposeID()
		return seat
	}
	localA := addSeat(store.Seat{
		Box: "", AgentFamily: "codex", CodexHome: "/flowbee/codex-a",
		AccountKey: "acct-a", Health: store.SeatReady, MaxConcurrent: 1,
	})
	localB := addSeat(store.Seat{
		Box: "", AgentFamily: "codex", CodexHome: "/flowbee/codex-b",
		AccountKey: "acct-b", Health: store.SeatReady, MaxConcurrent: 1,
	})
	fast := addSeat(store.Seat{
		Box: "fastbox", AgentFamily: "codex", CodexHome: "/flowbee/codex-fast",
		AccountKey: "acct-fast", Health: store.SeatReady, MaxConcurrent: 2,
	})
	sharedA := addSeat(store.Seat{
		Box: "shared-host", AgentFamily: "codex", CodexHome: "/flowbee/shared-a",
		AccountKey: "acct-shared-a", Health: store.SeatReady, MaxConcurrent: 1,
	})
	sharedB := addSeat(store.Seat{
		Box: "shared-host", AgentFamily: "codex", CodexHome: "/flowbee/shared-b",
		AccountKey: "acct-shared-b", Health: store.SeatReady, MaxConcurrent: 1,
	})

	if err := st.AddEpicRun(ctx, store.EpicRun{
		ID: "local-a", Repo: "r", Host: "wrong-host", SeatID: localA.ID,
		AccountKey: "wrong-account", BuilderModelFamily: "wrong-family", Scope: []string{"a/**"},
	}, 99, now); err != nil {
		t.Fatalf("first local seat: %v", err)
	}
	if err := st.AddEpicRun(ctx, store.EpicRun{
		ID: "local-b", Repo: "r", SeatID: localB.ID, Scope: []string{"b/**"},
	}, 1, now); err != nil {
		t.Fatalf("a distinct cap-1 seat on the same local host must run concurrently: %v", err)
	}
	if err := st.AddEpicRun(ctx, store.EpicRun{
		ID: "local-a-2", Repo: "r", SeatID: localA.ID, Scope: []string{"c/**"},
	}, 99, now); !errors.Is(err, store.ErrEpicHostBusy) {
		t.Fatalf("a second epic on the same cap-1 local seat must be refused, got %v", err)
	}
	got, err := st.GetEpicRun(ctx, "local-a")
	if err != nil || got.SeatID != localA.ID || got.Host != "" || got.AccountKey != "acct-a" || got.BuilderModelFamily != "codex" {
		t.Fatalf("initial insert must atomically persist the selected binding: err=%v epic=%+v", err, got)
	}
	for i, seat := range []store.Seat{sharedA, sharedB} {
		if err := st.AddEpicRun(ctx, store.EpicRun{
			ID: fmt.Sprintf("shared-%d", i), Repo: "r", SeatID: seat.ID,
			Scope: []string{fmt.Sprintf("shared/%d/**", i)},
		}, 1, now); err != nil {
			t.Fatalf("distinct cap-1 seat %d on one nonempty host must admit concurrently: %v", i, err)
		}
	}

	const racers = 3
	start := make(chan struct{})
	type result struct {
		id  string
		err error
	}
	results := make(chan result, racers)
	var wg sync.WaitGroup
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			id := fmt.Sprintf("race-%d", i)
			results <- result{id: id, err: st.AddEpicRun(ctx, store.EpicRun{
				ID: id, Repo: "r", SeatID: fast.ID,
				Scope: []string{fmt.Sprintf("race/%d/**", i)},
			}, 999, now)}
		}(i)
	}
	close(start)
	wg.Wait()
	close(results)
	succeeded, capped := 0, 0
	var admitted []string
	for result := range results {
		err := result.err
		switch {
		case err == nil:
			succeeded++
			admitted = append(admitted, result.id)
		case errors.Is(err, store.ErrEpicHostBusy):
			capped++
		default:
			t.Fatalf("unexpected racing registration error: %v", err)
		}
	}
	if succeeded != 2 || capped != 1 {
		t.Fatalf("cap-2 racing starts: succeeded=%d capped=%d, want 2/1", succeeded, capped)
	}
	if err := st.AbandonEpicRun(ctx, admitted[0], now.Add(time.Minute)); err != nil {
		t.Fatalf("release one slot: %v", err)
	}
	if err := st.AddEpicRun(ctx, store.EpicRun{
		ID: "after-release", Repo: "r", SeatID: fast.ID, Scope: []string{"after/**"},
	}, 1, now.Add(2*time.Minute)); err != nil {
		t.Fatalf("a terminal epic must release one exact-seat slot: %v", err)
	}
	if err := st.AddEpicRun(ctx, store.EpicRun{ID: "ghost", Repo: "r", SeatID: "missing", Scope: []string{"ghost/**"}}, 99, now); !errors.Is(err, store.ErrSeatNotFound) {
		t.Fatalf("unknown bound seat must fail closed, got %v", err)
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

	if err := st.AddEpicRun(ctx, store.EpicRun{ID: "2026-07-03-frobnicator"}, 1, now); !errors.Is(err, store.ErrEpicRunExists) {
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
	// The store transition does NOT delete the tmux session's registration, only pauses
	// watching it. The CLI has already stopped the process before invoking this method;
	// the registry row remains as durable history.
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

// TestEpicRunTerminalStateIsExactMatch is the M3 regression test: the raw
// "State:" word only flips the epic terminal on an EXACT "done" — words that
// merely CONTAIN "done" ("abandoned", "undone") must be a no-transition, because
// a terminal flip releases the scope+host reservation while the agent may still
// be mutating the tree.
func TestEpicRunTerminalStateIsExactMatch(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 12, 12, 0, 0, 0, time.UTC)

	for _, raw := range []string{"abandoned", "undone", "DONE-ish", "well done?"} {
		id := "exact-" + raw[:2] + raw[len(raw)-2:]
		mustAddEpicRun(t, st, ctx, store.EpicRun{ID: id, Repo: "r" + id, Host: "h" + id}, now)
		if err := st.MarkEpicLaunched(ctx, id, now); err != nil {
			t.Fatalf("mark launched: %v", err)
		}
		sb := epicspec.StatusBlock{State: raw, CurrentStep: 1, StepsTotal: 2}
		if err := st.UpsertEpicStatus(ctx, id, sb, now); err != nil {
			t.Fatalf("upsert (%q): %v", raw, err)
		}
		e, _ := st.GetEpicRun(ctx, id)
		if e.State != "running" || e.FinishedAt != "" {
			t.Errorf("State: %q must NOT be terminal — got state=%q finished_at=%q", raw, e.State, e.FinishedAt)
		}
		// the host reservation must still be held.
		if _, held, _ := st.HostActiveEpic(ctx, "h"+id); !held {
			t.Errorf("State: %q released the host reservation of a running epic", raw)
		}
	}

	// the genuine exact word (case-insensitive, trimmed) IS terminal.
	mustAddEpicRun(t, st, ctx, store.EpicRun{ID: "exact-done", Repo: "rd", Host: "hd"}, now)
	if err := st.MarkEpicLaunched(ctx, "exact-done", now); err != nil {
		t.Fatalf("mark launched: %v", err)
	}
	if err := st.UpsertEpicStatus(ctx, "exact-done", epicspec.StatusBlock{State: " Done "}, now); err != nil {
		t.Fatalf("upsert done: %v", err)
	}
	e, _ := st.GetEpicRun(ctx, "exact-done")
	if e.State != "done" || e.FinishedAt == "" {
		t.Fatalf("exact 'Done' should be terminal: %+v", e)
	}
}

// TestEpicRunEmptyStatusPreservesPriorFields is the m4 regression test: an empty
// parse (missing/garbage ## Status — e.g. the agent mid-edit committed a mangled
// section) must NOT clobber the last good ingested status with zero values.
func TestEpicRunEmptyStatusPreservesPriorFields(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 12, 12, 0, 0, 0, time.UTC)

	mustAddEpicRun(t, st, ctx, store.EpicRun{ID: "e-keep", Repo: "r", TmuxName: "epic-e-keep"}, now)
	if err := st.MarkEpicLaunched(ctx, "e-keep", now); err != nil {
		t.Fatalf("mark launched: %v", err)
	}
	good := epicspec.StatusBlock{
		UpdatedRaw: "2026-07-12T11:00:00Z", CurrentStep: 3, StepsTotal: 5, State: "blocked",
		Checklist: []epicspec.ChecklistItem{{Step: 1, Checked: true, Text: "a", Evidence: "ok"}},
		Blockers:  "needs gh auth",
	}
	if err := st.UpsertEpicStatus(ctx, "e-keep", good, now); err != nil {
		t.Fatalf("upsert good: %v", err)
	}
	// a later pass parses NOTHING (empty block) — the prior fields must survive.
	if err := st.UpsertEpicStatus(ctx, "e-keep", epicspec.StatusBlock{}, now.Add(2*time.Minute)); err != nil {
		t.Fatalf("upsert empty: %v", err)
	}
	e, _ := st.GetEpicRun(ctx, "e-keep")
	if e.StatusCurrentStep != 3 || e.StatusStepsTotal != 5 || e.StatusBlockers != "needs gh auth" ||
		e.StatusUpdatedAt != "2026-07-12T11:00:00Z" || len(e.StatusChecklist) != 1 {
		t.Fatalf("empty parse clobbered the last-good status: %+v", e)
	}
	if e.State != "blocked" {
		t.Fatalf("empty parse changed the lifecycle state: %q", e.State)
	}
}

// TestEpicRunEmptyStatusStillHonorsSessionAchieved: m4's preserve-on-empty must
// not suppress the ONE state signal that doesn't come from ## Status at all —
// the linked goal session's independently-observed 'achieved'.
func TestEpicRunEmptyStatusStillHonorsSessionAchieved(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 12, 12, 0, 0, 0, time.UTC)

	mustAddEpicRun(t, st, ctx, store.EpicRun{ID: "e-ach", Repo: "r", TmuxName: "epic-e-ach"}, now)
	if err := st.MarkEpicLaunched(ctx, "e-ach", now); err != nil {
		t.Fatalf("mark launched: %v", err)
	}
	if err := st.AddGoalSession(ctx, store.GoalSession{ID: "epic-e-ach", TmuxName: "epic-e-ach"}, now); err != nil {
		t.Fatalf("add session: %v", err)
	}
	if err := st.UpsertObservation(ctx, "epic-e-ach", "h", "achieved", "1d", now); err != nil {
		t.Fatalf("observe achieved: %v", err)
	}
	if err := st.UpsertEpicStatus(ctx, "e-ach", epicspec.StatusBlock{}, now.Add(time.Minute)); err != nil {
		t.Fatalf("upsert empty: %v", err)
	}
	e, _ := st.GetEpicRun(ctx, "e-ach")
	if e.State != "achieved" || e.FinishedAt == "" {
		t.Fatalf("session-achieved must fire even on an empty status parse: %+v", e)
	}
}

// TestAddEpicRunAtomicGates is the m6 regression test: the host-occupancy and
// same-repo scope-overlap gates run INSIDE AddEpicRun's transaction, so a second
// registration that raced past any caller-side pre-checks is still refused.
func TestAddEpicRunAtomicGates(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 12, 12, 0, 0, 0, time.UTC)

	mustAddEpicRun(t, st, ctx, store.EpicRun{
		ID: "first", Repo: "russ", Host: "buncher", Scope: []string{"internal/foo/**"},
	}, now)

	// same host at cap 1, disjoint scope, different repo: host occupancy refuses.
	err := st.AddEpicRun(ctx, store.EpicRun{
		ID: "second", Repo: "other", Host: "buncher", Scope: []string{"cmd/**"},
	}, 1, now)
	if !errors.Is(err, store.ErrEpicHostBusy) {
		t.Fatalf("expected ErrEpicHostBusy, got %v", err)
	}

	// different host, overlapping scope, SAME repo: scope reservation refuses.
	err = st.AddEpicRun(ctx, store.EpicRun{
		ID: "third", Repo: "russ", Host: "imac", Scope: []string{"internal/**"},
	}, 1, now)
	if !errors.Is(err, store.ErrEpicScopeOverlap) {
		t.Fatalf("expected ErrEpicScopeOverlap, got %v", err)
	}

	// different host, overlapping scope, DIFFERENT repo: allowed (scope is repo-local).
	if err := st.AddEpicRun(ctx, store.EpicRun{
		ID: "fourth", Repo: "other", Host: "imac", Scope: []string{"internal/**"},
	}, 1, now); err != nil {
		t.Fatalf("cross-repo overlapping scope should be allowed: %v", err)
	}

	// once the first epic is terminal, its host+scope free up.
	if err := st.AbandonEpicRun(ctx, "first", now); err != nil {
		t.Fatalf("abandon: %v", err)
	}
	if err := st.AddEpicRun(ctx, store.EpicRun{
		ID: "fifth", Repo: "russ", Host: "buncher", Scope: []string{"internal/foo/**"},
	}, 1, now); err != nil {
		t.Fatalf("expected the abandoned epic's reservations released: %v", err)
	}
}

// TestAddEpicRunHostCapAdmitsUpToCapThenRefuses is the 2-concurrent-epics-per-seat gate:
// with cap=2 a host admits TWO active epics and refuses the THIRD with ErrEpicHostBusy. The
// count-then-insert is one tx on a MaxOpenConns(1) store, so a cap-2 host can never end with
// 3 active epics even under concurrent starts (the serialization is structural — this test
// pins the cap arithmetic the tx enforces). cap=1 reproduces one-box-one-epic exactly, and a
// cap read back as 0 (a row predating 0029, or an unresolved caller) normalizes to 1.
func TestAddEpicRunHostCapAdmitsUpToCapThenRefuses(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 16, 12, 0, 0, 0, time.UTC)

	// two disjoint-scope epics on the same cap-2 host both register.
	if err := st.AddEpicRun(ctx, store.EpicRun{
		ID: "c1", Repo: "russ", Host: "codexbox", Scope: []string{"internal/a/**"},
	}, 2, now); err != nil {
		t.Fatalf("first epic on a cap-2 host should register: %v", err)
	}
	if err := st.AddEpicRun(ctx, store.EpicRun{
		ID: "c2", Repo: "russ", Host: "codexbox", Scope: []string{"internal/b/**"},
	}, 2, now); err != nil {
		t.Fatalf("second epic on a cap-2 host should register (headroom): %v", err)
	}

	// the third is refused — the host is at its cap of 2, and the message names the cap.
	err := st.AddEpicRun(ctx, store.EpicRun{
		ID: "c3", Repo: "russ", Host: "codexbox", Scope: []string{"internal/c/**"},
	}, 2, now)
	if !errors.Is(err, store.ErrEpicHostBusy) {
		t.Fatalf("third epic past cap 2 must be refused with ErrEpicHostBusy, got %v", err)
	}
	if err == nil || !strings.Contains(err.Error(), "cap 2") {
		t.Fatalf("ErrEpicHostBusy message should mention the cap, got %v", err)
	}

	// cap=1 on a DIFFERENT host reproduces one-box-one-epic exactly (no regression).
	if err := st.AddEpicRun(ctx, store.EpicRun{
		ID: "s1", Repo: "russ", Host: "claudebox", Scope: []string{"internal/d/**"},
	}, 1, now); err != nil {
		t.Fatalf("first epic on a cap-1 host should register: %v", err)
	}
	if err := st.AddEpicRun(ctx, store.EpicRun{
		ID: "s2", Repo: "russ", Host: "claudebox", Scope: []string{"internal/e/**"},
	}, 1, now); !errors.Is(err, store.ErrEpicHostBusy) {
		t.Fatalf("second epic on a cap-1 host must be refused (one-box-one-epic), got %v", err)
	}

	// once one cap-2 epic finishes, a slot frees and the next registers.
	if err := st.AbandonEpicRun(ctx, "c1", now); err != nil {
		t.Fatalf("abandon c1: %v", err)
	}
	if err := st.AddEpicRun(ctx, store.EpicRun{
		ID: "c3b", Repo: "russ", Host: "codexbox", Scope: []string{"internal/c/**"},
	}, 2, now); err != nil {
		t.Fatalf("a freed cap-2 slot should admit the next epic: %v", err)
	}

	// a cap read back as 0 (a row predating the column, or a caller that forgot to resolve
	// it) is normalized to 1, never an unbounded host.
	if err := st.AddEpicRun(ctx, store.EpicRun{
		ID: "z1", Repo: "russ", Host: "zerobox", Scope: []string{"internal/z/**"},
	}, 0, now); err != nil {
		t.Fatalf("cap 0 should behave as cap 1 (admit the first): %v", err)
	}
	if err := st.AddEpicRun(ctx, store.EpicRun{
		ID: "z2", Repo: "russ", Host: "zerobox", Scope: []string{"internal/y/**"},
	}, 0, now); !errors.Is(err, store.ErrEpicHostBusy) {
		t.Fatalf("cap 0 normalized to 1 must refuse the second, got %v", err)
	}
}
