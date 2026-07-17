// Phase 6b acceptance: the master supervision round-trip, proven end-to-end over the
// real HTTP surface + the consolidated supervision pass against a real SQLite store, with
// the pane-delivery seam faked (tmuxio's byte-level delivery is covered by its own
// integration tests).
//
// DONE-WHEN (proven below, non-skipped, under -race):
//   - a master REGISTERS (POST /v1/masters/register) and gets a fenced epoch;
//   - a BLOCKED epic session raises a typed attention item via the supervision producer;
//   - the master POLLS (POST /v1/masters/attention/lease) and leases it in one call;
//   - the master RESOLVES with a reply → the fenced delivering→awaiting_ack state machine
//     delivers into the (fake) pane and records a strong verdict → awaiting_ack (NOT resolved);
//   - the NEXT supervision pass observes the pane advanced and ACKS the item → resolved;
//   - the intervention is LEDGERED (recent_interventions carries it).
package acceptance

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/samhotchkiss/flowbee/internal/api"
	"github.com/samhotchkiss/flowbee/internal/clock"
	"github.com/samhotchkiss/flowbee/internal/epicspec"
	"github.com/samhotchkiss/flowbee/internal/epicsupervisor"
	"github.com/samhotchkiss/flowbee/internal/store"
	"github.com/samhotchkiss/flowbee/internal/testutil"
	"github.com/samhotchkiss/flowbee/internal/ulid"
)

// fakePane is a shared in-memory pane the supervision pass classifies and the master-resolve
// delivery types into — one struct backing BOTH the epicsupervisor.Pane and the api pane
// deliverer, so a delivery in the HTTP path visibly advances the pane the next pass observes.
type fakePane struct {
	mu        sync.Mutex
	state     map[string]string
	delivered map[string][]string
}

func newFakePane() *fakePane {
	return &fakePane{state: map[string]string{}, delivered: map[string][]string{}}
}

func (p *fakePane) Classify(_ context.Context, _, session string) (string, string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.state[session], "", nil
}

func (p *fakePane) Deliver(_ context.Context, _, session, message string) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.delivered[session] = append(p.delivered[session], message)
	p.state[session] = "working" // the steer landed → the agent starts working
	return "strong", nil
}

// apiPaneDeliverer adapts fakePane to the api.PaneDeliverer 3-return shape.
type apiPaneDeliverer struct{ p *fakePane }

func (d apiPaneDeliverer) Deliver(ctx context.Context, host, session, message string) (string, string, error) {
	v, err := d.p.Deliver(ctx, host, session, message)
	return v, "fake delivery", err
}

func TestP6bMasterRoundTrip(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	now := time.Now()

	pane := newFakePane()
	srv := api.New(st, clock.Real{}, ulid.NewMinter(nil), api.Config{HeartbeatIntervalS: 30}, "test")
	srv.SetPaneDeliverer(apiPaneDeliverer{pane})
	ts := httptest.NewServer(srv.PrivateHandler())
	defer ts.Close()

	// ── seed a BLOCKED epic session ──
	if err := st.AddEpicRun(ctx, store.EpicRun{
		ID: "x", Repo: "r", FilePath: "epics/x.md", Title: "X", Branch: "epic/x",
		TmuxName: "epic-x", Agent: "claude",
	}, 1, now); err != nil {
		t.Fatalf("add epic: %v", err)
	}
	if err := st.UpsertEpicStatus(ctx, "x", epicspec.StatusBlock{
		UpdatedRaw: now.Format(time.RFC3339), State: "blocked", Blockers: "needs a decision",
	}, now); err != nil {
		t.Fatalf("set epic blocked: %v", err)
	}
	pane.state["epic-x"] = "idle_at_prompt" // benign — the blocked item comes from lifecycle state

	supv := epicsupervisor.New(st, pane, nil, epicsupervisor.Config{}, slog.Default())

	// ── register the master ──
	var reg struct {
		MasterID string `json:"master_id"`
		Epoch    int    `json:"epoch"`
	}
	post(t, ts.URL+"/v1/masters/register", map[string]any{
		"label": "m", "kind": "claude", "model_family": "claude", "tmux_name": "master",
	}, &reg)
	if reg.MasterID == "" || reg.Epoch == 0 {
		t.Fatalf("register: got %+v", reg)
	}

	// ── pass 1: the producer raises a blocked_non_resumable item ──
	supv.Pass(ctx, now)
	open, err := st.ListOpenAttention(ctx, "open", nil, "")
	if err != nil {
		t.Fatalf("list open: %v", err)
	}
	if len(open) != 1 || open[0].Kind != "blocked_non_resumable" {
		t.Fatalf("expected one blocked_non_resumable item, got %+v", open)
	}

	// ── poll (heartbeat + digest + lease) — one call ──
	var poll struct {
		Revoked bool `json:"revoked"`
		Leased  []struct {
			ID        string `json:"id"`
			Kind      string `json:"kind"`
			ItemEpoch int    `json:"item_epoch"`
		} `json:"leased"`
	}
	post(t, ts.URL+"/v1/masters/attention/lease", map[string]any{
		"master_id": reg.MasterID, "epoch": reg.Epoch, "max": 5,
	}, &poll)
	if poll.Revoked || len(poll.Leased) != 1 {
		t.Fatalf("poll: expected one leased item, got %+v", poll)
	}
	item := poll.Leased[0]

	// ── resolve with a reply → verified send → awaiting_ack ──
	var res struct {
		Verdict string `json:"verdict"`
		State   string `json:"state"`
	}
	post(t, ts.URL+"/v1/masters/attention/"+item.ID+"/resolve", map[string]any{
		"master_id": reg.MasterID, "epoch": reg.Epoch, "item_epoch": item.ItemEpoch,
		"action": "reply", "payload": "unblock: point the fixture at internal/store/testdata and re-run",
		"idempotency_key": "k1",
	}, &res)
	if res.Verdict != "strong" || res.State != "awaiting_ack" {
		t.Fatalf("resolve: want strong/awaiting_ack, got %+v", res)
	}
	if got := pane.delivered["epic-x"]; len(got) != 1 {
		t.Fatalf("expected one delivery into epic-x, got %v", got)
	}
	if a, _ := st.GetAttentionItem(ctx, item.ID); a.State != "awaiting_ack" {
		t.Fatalf("item not awaiting_ack: %+v", a)
	}

	// ── pass 2: the pane advanced (working) → the ack loop resolves it as acked ──
	supv.Pass(ctx, now.Add(time.Minute))
	final, err := st.GetAttentionItem(ctx, item.ID)
	if err != nil {
		t.Fatalf("get item: %v", err)
	}
	if final.State != "resolved" || final.Resolution != "acked" {
		t.Fatalf("expected resolved/acked, got state=%q resolution=%q", final.State, final.Resolution)
	}

	// ── the intervention is ledgered (recent_interventions carries it) ──
	interventions, err := st.RecentInterventions(ctx, "x", 3)
	if err != nil {
		t.Fatalf("recent interventions: %v", err)
	}
	if len(interventions) == 0 {
		t.Fatalf("expected a ledgered intervention, got none")
	}
}

// TestP6bResolveFencesStaleEpoch proves the resolve state machine fences a superseded
// caller (a stale supervisor epoch) with 409, the double-completion-guard HTTP pattern.
func TestP6bResolveFencesStaleEpoch(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	now := time.Now()

	pane := newFakePane()
	srv := api.New(st, clock.Real{}, ulid.NewMinter(nil), api.Config{}, "test")
	srv.SetPaneDeliverer(apiPaneDeliverer{pane})
	ts := httptest.NewServer(srv.PrivateHandler())
	defer ts.Close()

	if err := st.AddEpicRun(ctx, store.EpicRun{ID: "y", Repo: "r", TmuxName: "epic-y", Agent: "claude"}, 1, now); err != nil {
		t.Fatalf("add epic: %v", err)
	}
	pane.state["epic-y"] = "awaiting_input"
	supv := epicsupervisor.New(st, pane, nil, epicsupervisor.Config{}, slog.Default())

	var reg struct {
		MasterID string `json:"master_id"`
		Epoch    int    `json:"epoch"`
	}
	post(t, ts.URL+"/v1/masters/register", map[string]any{"label": "m", "kind": "claude", "model_family": "claude"}, &reg)

	supv.Pass(ctx, now) // raises needs_input
	var poll struct {
		Leased []struct {
			ID        string `json:"id"`
			ItemEpoch int    `json:"item_epoch"`
		} `json:"leased"`
	}
	post(t, ts.URL+"/v1/masters/attention/lease", map[string]any{"master_id": reg.MasterID, "epoch": reg.Epoch, "max": 5}, &poll)
	if len(poll.Leased) != 1 {
		t.Fatalf("expected one leased item, got %+v", poll.Leased)
	}

	// resolve with a WRONG (stale) supervisor epoch → 409 fenced, no delivery.
	code := postStatus(t, ts.URL+"/v1/masters/attention/"+poll.Leased[0].ID+"/resolve", map[string]any{
		"master_id": reg.MasterID, "epoch": reg.Epoch + 99, "item_epoch": poll.Leased[0].ItemEpoch,
		"action": "reply", "payload": "stale caller", "idempotency_key": "k1",
	})
	if code != http.StatusConflict {
		t.Fatalf("stale-epoch resolve: want 409, got %d", code)
	}
	if len(pane.delivered["epic-y"]) != 0 {
		t.Fatalf("a fenced resolve must not deliver, got %v", pane.delivered["epic-y"])
	}
}

// ── HTTP helpers ──

func post(t *testing.T, url string, body, out any) {
	t.Helper()
	if postStatusInto(t, url, body, out) >= 300 {
		t.Fatalf("POST %s failed", url)
	}
}

func postStatus(t *testing.T, url string, body any) int {
	t.Helper()
	return postStatusInto(t, url, body, nil)
}

func postStatusInto(t *testing.T, url string, body, out any) int {
	t.Helper()
	b, _ := json.Marshal(body)
	resp, err := http.Post(url, "application/json", bytes.NewReader(b))
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	defer resp.Body.Close()
	if out != nil && resp.StatusCode < 300 {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			t.Fatalf("decode %s: %v", url, err)
		}
	}
	return resp.StatusCode
}
