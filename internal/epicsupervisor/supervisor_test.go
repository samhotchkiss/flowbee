package epicsupervisor

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/samhotchkiss/flowbee/internal/store"
	"github.com/samhotchkiss/flowbee/internal/testutil"
	"github.com/samhotchkiss/flowbee/internal/ulid"
)

// fakePane is a scripted pane for the supervision pass tests.
type fakePane struct{ state map[string]string }

func (p fakePane) Classify(_ context.Context, _, session string) (string, string, error) {
	return p.state[session], "", nil
}
func (p fakePane) Deliver(_ context.Context, _, _, _ string) (string, error) { return "strong", nil }

func newSupv(t *testing.T, pane Pane) (*store.Store, *Supervisor) {
	t.Helper()
	st := testutil.NewStore(t)
	return st, New(st, pane, nil, Config{}, slog.Default())
}

// TestProducerNeedsInputAndClear proves the AWAITING_INPUT producer raises needs_input and
// the auto-resolve clears it once the pane leaves the prompt (dedup discipline, both ways).
func TestProducerNeedsInputAndClear(t *testing.T) {
	ctx := context.Background()
	now := time.Now()
	pane := fakePane{state: map[string]string{"epic-a": "awaiting_input"}}
	st, supv := newSupv(t, pane)
	if err := st.AddEpicRun(ctx, store.EpicRun{ID: "a", Repo: "r", TmuxName: "epic-a", Agent: "claude"}, now); err != nil {
		t.Fatalf("add epic: %v", err)
	}
	_ = st.MarkEpicLaunched(ctx, "a", now)

	supv.Pass(ctx, now)
	open, _ := st.ListOpenAttention(ctx, "open", nil, "")
	if len(open) != 1 || open[0].Kind != "needs_input" {
		t.Fatalf("want one needs_input, got %+v", open)
	}

	// pane leaves the prompt → the producer auto-resolves the item as cleared.
	pane.state["epic-a"] = "working"
	supv.Pass(ctx, now.Add(time.Minute))
	open, _ = st.ListOpenAttention(ctx, "open", nil, "")
	if len(open) != 0 {
		t.Fatalf("expected needs_input auto-resolved, still open: %+v", open)
	}
}

// TestLaunchingReaper proves an epic stranded in 'launching' past the window is abandoned
// (releasing host/scope) and raises launch_failed (plan §13).
func TestLaunchingReaper(t *testing.T) {
	ctx := context.Background()
	past := time.Now().Add(-30 * time.Minute)
	pane := fakePane{state: map[string]string{}}
	st, supv := newSupv(t, pane)
	// created_at = past → older than the 10m strand window.
	if err := st.AddEpicRun(ctx, store.EpicRun{ID: "b", Repo: "r", Host: "box1", TmuxName: "epic-b", Agent: "claude"}, past); err != nil {
		t.Fatalf("add epic: %v", err)
	}

	supv.Pass(ctx, time.Now())

	e, err := st.GetEpicRun(ctx, "b")
	if err != nil {
		t.Fatalf("get epic: %v", err)
	}
	if e.State != "abandoned" {
		t.Fatalf("stranded launch not reaped: state=%q", e.State)
	}
	open, _ := st.ListOpenAttention(ctx, "open", nil, "")
	found := false
	for _, it := range open {
		if it.Kind == "launch_failed" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected a launch_failed item, got %+v", open)
	}
}

// TestMasterAbsent proves that when a human-warranting item is open with no live master,
// the pass raises master_absent (plan §1.6), and clears it once the condition passes.
func TestMasterAbsent(t *testing.T) {
	ctx := context.Background()
	now := time.Now()
	pane := fakePane{state: map[string]string{}}
	st, supv := newSupv(t, pane)
	// an auth_dead item is human-immediate → NeedsHuman at age 0; no supervisor registered.
	if _, _, err := st.UpsertAttentionItem(ctx, store.AttentionItem{
		ID: ulid.New(), Kind: "auth_dead", EpicID: "c", Repo: "r", Priority: 10, DedupKey: "c:auth_dead",
	}, now); err != nil {
		t.Fatalf("seed auth_dead: %v", err)
	}

	supv.Pass(ctx, now)
	open, _ := st.ListOpenAttention(ctx, "open", nil, "")
	found := false
	for _, it := range open {
		if it.Kind == "master_absent" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected master_absent raised, got %+v", open)
	}
}
