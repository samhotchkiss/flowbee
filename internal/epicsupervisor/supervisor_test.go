package epicsupervisor

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/samhotchkiss/flowbee/internal/store"
	"github.com/samhotchkiss/flowbee/internal/testutil"
	"github.com/samhotchkiss/flowbee/internal/ulid"
)

// fakePane is a scripted pane for the supervision pass tests.
type fakePane struct {
	state   map[string]string
	stopErr map[string]error
	stops   map[string]int
}

func (p fakePane) Classify(_ context.Context, _, session string) (string, string, error) {
	return p.state[session], "", nil
}
func (p fakePane) Deliver(_ context.Context, _, _, _ string) (string, error) { return "strong", nil }
func (p fakePane) Stop(_ context.Context, host, session string) error {
	key := host + "|" + session
	if p.stops != nil {
		p.stops[key]++
	}
	return p.stopErr[key]
}

// panickyPane panics when classifying one specific session (a malformed/wedged pane), used
// to prove the M1 per-epic recover isolates one bad epic from the rest of the batch.
type panickyPane struct {
	panicOn string
	state   map[string]string
}

func (p panickyPane) Classify(_ context.Context, _, session string) (string, string, error) {
	if session == p.panicOn {
		panic("boom: malformed capture for " + session)
	}
	return p.state[session], "", nil
}
func (p panickyPane) Deliver(_ context.Context, _, _, _ string) (string, error) { return "strong", nil }
func (p panickyPane) Stop(_ context.Context, _, _ string) error                 { return nil }

// TestPassPerEpicPanicIsolation proves M1: a panic while observing ONE epic (a nil/wedged
// pane) is recovered per-epic — the pass does NOT crash and every OTHER epic still
// processes. Without the per-epic recover a single malformed epic file would crashloop the
// whole control plane under Restart=always.
func TestPassPerEpicPanicIsolation(t *testing.T) {
	ctx := context.Background()
	now := time.Now()
	// "epic-bad" (id "bad") panics on classify; "epic-good" (id "good") is a live prompt.
	pane := panickyPane{panicOn: "epic-bad", state: map[string]string{"epic-good": "awaiting_input"}}
	st, supv := newSupvPanicky(t, pane)
	for _, id := range []string{"bad", "good"} {
		if err := st.AddEpicRun(ctx, store.EpicRun{ID: id, Repo: "r", TmuxName: "epic-" + id, Agent: "claude"}, 1, now); err != nil {
			t.Fatalf("add epic %s: %v", id, err)
		}
		_ = st.MarkEpicLaunched(ctx, id, now)
	}

	// must NOT panic out of Pass (a recover'd panic returns normally).
	supv.Pass(ctx, now)

	// the GOOD epic still produced its item despite the bad epic panicking earlier in the loop.
	open, _ := st.ListOpenAttention(ctx, "open", nil, "")
	found := false
	for _, it := range open {
		if it.EpicID == "good" && it.Kind == "needs_input" {
			found = true
		}
	}
	if !found {
		t.Fatalf("the good epic was not processed after the bad epic panicked: %+v", open)
	}
}

func newSupvPanicky(t *testing.T, pane Pane) (*store.Store, *Supervisor) {
	t.Helper()
	st := testutil.NewStore(t)
	return st, New(st, pane, nil, Config{}, slog.Default())
}

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
	if err := st.AddEpicRun(ctx, store.EpicRun{ID: "a", Repo: "r", TmuxName: "epic-a", Agent: "claude"}, 1, now); err != nil {
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
	pane := fakePane{state: map[string]string{}, stops: map[string]int{}}
	st, supv := newSupv(t, pane)
	// created_at = past → older than the 10m strand window.
	if err := st.AddEpicRun(ctx, store.EpicRun{ID: "b", Repo: "r", Host: "box1", TmuxName: "epic-b", Agent: "claude"}, 1, past); err != nil {
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
	if pane.stops["box1|epic-b"] != 1 || pane.stops["|epic-b"] != 1 {
		t.Fatalf("stranded launch stop calls=%v, want remote agent then local attach", pane.stops)
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

func TestLaunchingReaperRetainsCapacityWhenStopIsUnconfirmed(t *testing.T) {
	ctx := context.Background()
	past := time.Now().Add(-30 * time.Minute)
	pane := fakePane{
		state:   map[string]string{},
		stops:   map[string]int{},
		stopErr: map[string]error{"box1|epic-b": errors.New("ssh unreachable")},
	}
	st, supv := newSupv(t, pane)
	if err := st.AddEpicRun(ctx, store.EpicRun{ID: "b", Repo: "r", Host: "box1", TmuxName: "epic-b", Agent: "claude"}, 1, past); err != nil {
		t.Fatalf("add epic: %v", err)
	}

	supv.Pass(ctx, time.Now())

	e, err := st.GetEpicRun(ctx, "b")
	if err != nil {
		t.Fatalf("get epic: %v", err)
	}
	if e.State != "launching" {
		t.Fatalf("unconfirmed cleanup released stranded epic: state=%q", e.State)
	}
	active, err := st.ListActiveEpicRuns(ctx)
	if err != nil || len(active) != 1 || active[0].ID != "b" {
		t.Fatalf("unconfirmed cleanup released capacity: active=%+v err=%v", active, err)
	}
	if pane.stops["box1|epic-b"] != 1 || pane.stops["|epic-b"] != 0 {
		t.Fatalf("stop calls=%v, want fail closed after remote cleanup failure", pane.stops)
	}
}

// TestAckLoopBareWorkingNotAcked proves the m3 fix: an awaiting_ack item whose epic pane was
// ALREADY working before the steer and merely STAYS working (no transition into working
// this pass) is NOT falsely acked — busyness is not proof the steer was processed. It stays
// awaiting_ack until it either transitions/advances (ack) or ack-expires (reopen).
func TestAckLoopBareWorkingNotAcked(t *testing.T) {
	ctx := context.Background()
	now := time.Now()
	pane := fakePane{state: map[string]string{"epic-m3": "working"}}
	st, supv := newSupv(t, pane)
	if err := st.AddEpicRun(ctx, store.EpicRun{ID: "m3", Repo: "r", TmuxName: "epic-m3", Agent: "claude"}, 1, now); err != nil {
		t.Fatalf("add epic: %v", err)
	}
	_ = st.MarkEpicLaunched(ctx, "m3", now)
	// stored prior pane state is ALREADY working, so this pass sees no transition.
	if err := st.SetEpicRuntimeState(ctx, "m3", store.EpicRuntimeState{PaneState: "working", ContextPct: -1}, now); err != nil {
		t.Fatalf("set runtime: %v", err)
	}

	// drive an item to awaiting_ack via a real lease + delivery.
	reg, err := st.RegisterSupervisor(ctx, store.Supervisor{Label: "m", Kind: "claude", ModelFamily: "claude"}, now)
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	if _, _, err := st.UpsertAttentionItem(ctx, store.AttentionItem{
		ID: ulid.New(), Kind: "drift_suspect", EpicID: "m3", Repo: "r", Priority: 15, DedupKey: "m3:drift",
	}, now); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	leased, err := st.LeaseAttention(ctx, reg.MasterID, reg.Epoch, 5, nil, time.Hour, now)
	if err != nil || len(leased) != 1 {
		t.Fatalf("lease: err=%v got %d", err, len(leased))
	}
	it := leased[0]
	if err := st.BeginDelivery(ctx, it.ID, reg.MasterID, reg.Epoch, it.ItemEpoch, "k", now); err != nil {
		t.Fatalf("begin delivery: %v", err)
	}
	if err := st.RecordDeliveryVerdict(ctx, it.ID, reg.MasterID, reg.Epoch, it.ItemEpoch, "strong", now); err != nil {
		t.Fatalf("record verdict: %v", err)
	}
	if a, _ := st.GetAttentionItem(ctx, it.ID); a.State != "awaiting_ack" {
		t.Fatalf("precondition: want awaiting_ack, got %q", a.State)
	}

	// a pass 1 minute later (< 6m T_ack) with the pane merely STILL working must NOT ack it.
	supv.Pass(ctx, now.Add(time.Minute))
	after, _ := st.GetAttentionItem(ctx, it.ID)
	if after.State != "awaiting_ack" {
		t.Fatalf("bare-working must not ack (m3); got state=%q resolution=%q", after.State, after.Resolution)
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
