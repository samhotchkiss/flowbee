package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/samhotchkiss/flowbee/internal/store"
	"github.com/samhotchkiss/flowbee/internal/testutil"
)

func TestGoalSessionCRUD(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 3, 12, 0, 0, 0, time.UTC)

	if err := st.AddGoalSession(ctx, store.GoalSession{
		ID: "russ-terra", Box: "buncher", TmuxName: "goal-terra", TZ: "America/Denver", Repo: "russ", Note: "epic lane phase 1",
	}, now); err != nil {
		t.Fatalf("add: %v", err)
	}

	// duplicate id is refused (not an upsert — see the WHY comment on AddGoalSession).
	if err := st.AddGoalSession(ctx, store.GoalSession{ID: "russ-terra", TmuxName: "x"}, now); !errors.Is(err, store.ErrGoalSessionExists) {
		t.Fatalf("expected ErrGoalSessionExists, got %v", err)
	}

	g, err := st.GetGoalSession(ctx, "russ-terra")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if g.Box != "buncher" || g.TmuxName != "goal-terra" || g.TZ != "America/Denver" || g.State != "unknown" || !g.Enabled {
		t.Fatalf("unexpected row: %+v", g)
	}

	if _, err := st.GetGoalSession(ctx, "nope"); !errors.Is(err, store.ErrGoalSessionNotFound) {
		t.Fatalf("expected ErrGoalSessionNotFound, got %v", err)
	}

	// a second, local session ('' box).
	if err := st.AddGoalSession(ctx, store.GoalSession{ID: "russ-sol", TmuxName: "goal-sol"}, now); err != nil {
		t.Fatalf("add second: %v", err)
	}

	all, err := st.ListGoalSessions(ctx)
	if err != nil || len(all) != 2 {
		t.Fatalf("list: %v %+v", err, all)
	}
	if all[0].ID != "russ-sol" || all[1].ID != "russ-terra" {
		t.Fatalf("list not ordered by id: %+v", all)
	}

	// pause russ-sol: it drops out of ListEnabled but stays in ListGoalSessions.
	if err := st.SetGoalSessionEnabled(ctx, "russ-sol", false, now); err != nil {
		t.Fatalf("pause: %v", err)
	}
	enabled, err := st.ListEnabledGoalSessions(ctx)
	if err != nil || len(enabled) != 1 || enabled[0].ID != "russ-terra" {
		t.Fatalf("list enabled: %v %+v", err, enabled)
	}
	all, _ = st.ListGoalSessions(ctx)
	if len(all) != 2 {
		t.Fatalf("paused session vanished from full list: %+v", all)
	}

	if err := st.RemoveGoalSession(ctx, "russ-sol"); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if err := st.RemoveGoalSession(ctx, "russ-sol"); !errors.Is(err, store.ErrGoalSessionNotFound) {
		t.Fatalf("expected ErrGoalSessionNotFound on double-remove, got %v", err)
	}
}

// TestGoalSessionUpsertObservation: last_change_at only advances when the pane
// hash changes — the invariant the watcher depends on to distinguish "genuinely
// active" from "sitting on an unchanged pane".
func TestGoalSessionUpsertObservation(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	t0 := time.Date(2026, 7, 3, 12, 0, 0, 0, time.UTC)

	if err := st.AddGoalSession(ctx, store.GoalSession{ID: "s1", TmuxName: "t1"}, t0); err != nil {
		t.Fatalf("add: %v", err)
	}

	if err := st.UpsertObservation(ctx, "s1", "hashA", "pursuing", "2d 4h", t0); err != nil {
		t.Fatalf("observe 1: %v", err)
	}
	g, _ := st.GetGoalSession(ctx, "s1")
	firstChange := g.LastChangeAt
	if g.State != "pursuing" || g.GoalElapsed != "2d 4h" || firstChange == "" {
		t.Fatalf("unexpected row after first observation: %+v", g)
	}

	// same hash, later tick: last_change_at must NOT advance.
	t1 := t0.Add(2 * time.Minute)
	if err := st.UpsertObservation(ctx, "s1", "hashA", "pursuing", "2d 6h", t1); err != nil {
		t.Fatalf("observe 2: %v", err)
	}
	g, _ = st.GetGoalSession(ctx, "s1")
	if g.LastChangeAt != firstChange {
		t.Fatalf("last_change_at advanced on an unchanged hash: was %q now %q", firstChange, g.LastChangeAt)
	}
	if g.GoalElapsed != "2d 6h" {
		t.Fatalf("goal_elapsed should still update every observation: %q", g.GoalElapsed)
	}
	if g.LastCheckedAt != t1.Format(time.RFC3339Nano) {
		t.Fatalf("last_checked_at should always advance: %q", g.LastCheckedAt)
	}

	// hash changes: last_change_at advances.
	t2 := t1.Add(2 * time.Minute)
	if err := st.UpsertObservation(ctx, "s1", "hashB", "pursuing", "2d 8h", t2); err != nil {
		t.Fatalf("observe 3: %v", err)
	}
	g, _ = st.GetGoalSession(ctx, "s1")
	if g.LastChangeAt != t2.Format(time.RFC3339Nano) {
		t.Fatalf("last_change_at should advance on hash change: %q want %q", g.LastChangeAt, t2.Format(time.RFC3339Nano))
	}
}

func TestGoalSessionCaptureFailureUnreachableAfterThree(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 3, 12, 0, 0, 0, time.UTC)
	if err := st.AddGoalSession(ctx, store.GoalSession{ID: "s1", TmuxName: "t1"}, now); err != nil {
		t.Fatalf("add: %v", err)
	}

	for i := 1; i <= 2; i++ {
		n, err := st.RecordCaptureFailure(ctx, "s1", now)
		if err != nil || n != i {
			t.Fatalf("failure %d: n=%d err=%v", i, n, err)
		}
		g, _ := st.GetGoalSession(ctx, "s1")
		if g.State == "unreachable" {
			t.Fatalf("flipped to unreachable too early at failure %d", i)
		}
	}
	n, err := st.RecordCaptureFailure(ctx, "s1", now)
	if err != nil || n != 3 {
		t.Fatalf("third failure: n=%d err=%v", n, err)
	}
	g, _ := st.GetGoalSession(ctx, "s1")
	if g.State != "unreachable" {
		t.Fatalf("state = %q, want unreachable after 3 consecutive failures", g.State)
	}

	// a successful observation resets the streak.
	if err := st.UpsertObservation(ctx, "s1", "h1", "pursuing", "5m", now); err != nil {
		t.Fatalf("observe: %v", err)
	}
	g, _ = st.GetGoalSession(ctx, "s1")
	if g.ConsecutiveFailures != 0 {
		t.Fatalf("consecutive_failures = %d, want 0 after a successful observation", g.ConsecutiveFailures)
	}
}

func TestGoalSessionResumeAttemptRateLimit(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 3, 12, 0, 0, 0, time.UTC)
	if err := st.AddGoalSession(ctx, store.GoalSession{ID: "s1", TmuxName: "t1"}, now); err != nil {
		t.Fatalf("add: %v", err)
	}

	for i := 1; i <= 3; i++ {
		attempts, allowed, err := st.RecordResumeAttempt(ctx, "s1", now.Add(time.Duration(i)*time.Minute))
		if err != nil {
			t.Fatalf("attempt %d: %v", i, err)
		}
		if attempts != i || !allowed {
			t.Fatalf("attempt %d: attempts=%d allowed=%v, want %d/true", i, attempts, allowed, i)
		}
	}
	// 4th attempt within the same hour is refused.
	attempts, allowed, err := st.RecordResumeAttempt(ctx, "s1", now.Add(10*time.Minute))
	if err != nil {
		t.Fatalf("4th attempt: %v", err)
	}
	if allowed {
		t.Fatalf("4th attempt within the hour should be refused, got attempts=%d allowed=%v", attempts, allowed)
	}
	if attempts != 3 {
		t.Fatalf("refused attempt should not bump the persisted count: got %d want 3", attempts)
	}

	// past the hour window: resets to 1/allowed.
	later := now.Add(90 * time.Minute)
	attempts, allowed, err = st.RecordResumeAttempt(ctx, "s1", later)
	if err != nil {
		t.Fatalf("post-window attempt: %v", err)
	}
	if attempts != 1 || !allowed {
		t.Fatalf("post-window attempt: attempts=%d allowed=%v, want 1/true", attempts, allowed)
	}
}

func TestGoalSessionBlockedUntilAndNeedsOperatorAndClearBlock(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 3, 12, 0, 0, 0, time.UTC)
	if err := st.AddGoalSession(ctx, store.GoalSession{ID: "s1", TmuxName: "t1"}, now); err != nil {
		t.Fatalf("add: %v", err)
	}

	reset := now.Add(3 * time.Hour)
	if err := st.SetBlockedUntil(ctx, "s1", reset, "usage_limit", now); err != nil {
		t.Fatalf("set blocked until: %v", err)
	}
	g, _ := st.GetGoalSession(ctx, "s1")
	if g.BlockedUntil != reset.Format(time.RFC3339Nano) || g.StateDetail != "usage_limit" {
		t.Fatalf("unexpected row: %+v", g)
	}

	if err := st.SetNeedsOperator(ctx, "s1", "gh auth", now); err != nil {
		t.Fatalf("set needs operator: %v", err)
	}
	g, _ = st.GetGoalSession(ctx, "s1")
	if g.StateDetail != "needs_operator: gh auth" {
		t.Fatalf("state_detail = %q", g.StateDetail)
	}

	if _, _, err := st.RecordResumeAttempt(ctx, "s1", now); err != nil {
		t.Fatalf("resume attempt: %v", err)
	}
	if err := st.ClearBlock(ctx, "s1", now); err != nil {
		t.Fatalf("clear block: %v", err)
	}
	g, _ = st.GetGoalSession(ctx, "s1")
	if g.StateDetail != "" || g.BlockedUntil != "" || g.ResumeAttempts != 0 || g.ResumeWindowStart != "" {
		t.Fatalf("clear block left stale fields: %+v", g)
	}

	if err := st.SetBlockedUntil(ctx, "nope", reset, "x", now); !errors.Is(err, store.ErrGoalSessionNotFound) {
		t.Fatalf("expected ErrGoalSessionNotFound, got %v", err)
	}
}

// TestAddGoalSessionRejectsArgvHostileValues: registration-time defense in depth
// behind remoteWrap's `--` separator — a leading-dash box would otherwise be read
// by ssh's own getopt as an OPTION (`-oProxyCommand=...` = local RCE), and
// whitespace/control chars have no legitimate use in a hostname or tmux target.
func TestAddGoalSessionRejectsArgvHostileValues(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 3, 12, 0, 0, 0, time.UTC)

	bad := []store.GoalSession{
		{ID: "b1", TmuxName: "t", Box: "-oProxyCommand=evil"},
		{ID: "b2", TmuxName: "t", Box: "host name"},
		{ID: "b3", TmuxName: "t", Box: "host\nname"},
		{ID: "b4", TmuxName: "-t-evil", Box: "ok"},
		{ID: "b5", TmuxName: "has space", Box: "ok"},
		{ID: "b6", TmuxName: "tab\there", Box: "ok"},
	}
	for _, g := range bad {
		if err := st.AddGoalSession(ctx, g, now); err == nil {
			t.Errorf("expected rejection for %+v", g)
		}
	}
	// none of them may have landed in the registry.
	all, _ := st.ListGoalSessions(ctx)
	if len(all) != 0 {
		t.Fatalf("hostile values were registered: %+v", all)
	}

	// a normal hostname/tmux target still registers fine.
	if err := st.AddGoalSession(ctx, store.GoalSession{ID: "ok", TmuxName: "goal-1", Box: "buncher.example.com"}, now); err != nil {
		t.Fatalf("legitimate values rejected: %v", err)
	}
}

// TestAddGoalSessionValidatesTZ: a typo'd IANA name must fail at ADD time — a
// silent serve-local fallback at resolve time would reintroduce the exact
// west-of-serve early-resume bug the tz column exists to fix.
func TestAddGoalSessionValidatesTZ(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 3, 12, 0, 0, 0, time.UTC)

	if err := st.AddGoalSession(ctx, store.GoalSession{ID: "s1", TmuxName: "t1", TZ: "America/Nowhere"}, now); err == nil {
		t.Fatalf("expected an invalid-tz rejection")
	}
	if err := st.AddGoalSession(ctx, store.GoalSession{ID: "s1", TmuxName: "t1", TZ: "America/Los_Angeles"}, now); err != nil {
		t.Fatalf("valid tz rejected: %v", err)
	}
	g, err := st.GetGoalSession(ctx, "s1")
	if err != nil || g.TZ != "America/Los_Angeles" {
		t.Fatalf("tz did not round-trip: %+v err=%v", g, err)
	}
}
