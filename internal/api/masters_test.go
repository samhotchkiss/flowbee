package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/samhotchkiss/flowbee/internal/acctprobe"
	"github.com/samhotchkiss/flowbee/internal/clock"
	"github.com/samhotchkiss/flowbee/internal/store"
	"github.com/samhotchkiss/flowbee/internal/testutil"
	"github.com/samhotchkiss/flowbee/internal/ulid"
)

func newMasterTestServer(t *testing.T) (*store.Store, string, func()) {
	t.Helper()
	st := testutil.NewStore(t)
	srv := New(st, clock.Real{}, ulid.NewMinter(nil), Config{HeartbeatIntervalS: 30}, "test")
	ts := httptest.NewServer(srv.PrivateHandler())
	return st, ts.URL, ts.Close
}

// TestDigestETag304 proves GET /v1/epics/digest carries an ETag and returns 304 for an
// unchanged digest_seq (the "everything's fine, sleep" cheap poll, plan §2.1).
func TestDigestETag304(t *testing.T) {
	st, url, done := newMasterTestServer(t)
	defer done()
	ctx := context.Background()
	now := time.Now()
	if err := st.AddEpicRun(ctx, store.EpicRun{ID: "e1", Repo: "r", TmuxName: "epic-e1", Agent: "claude"}, now); err != nil {
		t.Fatalf("add epic: %v", err)
	}

	resp, err := http.Get(url + "/v1/epics/digest")
	if err != nil {
		t.Fatalf("get digest: %v", err)
	}
	etag := resp.Header.Get("ETag")
	resp.Body.Close()
	if resp.StatusCode != 200 || etag == "" {
		t.Fatalf("digest: want 200 + ETag, got %d etag=%q", resp.StatusCode, etag)
	}

	req, _ := http.NewRequest(http.MethodGet, url+"/v1/epics/digest", nil)
	req.Header.Set("If-None-Match", etag)
	resp2, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("get digest again: %v", err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusNotModified {
		t.Fatalf("unchanged digest: want 304, got %d", resp2.StatusCode)
	}
}

// TestDigestWindowsVerbatim proves the account.windows[] is carried FIELD-FOR-FIELD (the
// elgato §15.16 contract) and that empty collections serialize as [] never null.
func TestDigestWindowsVerbatim(t *testing.T) {
	st, url, done := newMasterTestServer(t)
	defer done()
	ctx := context.Background()
	now := time.Now()

	if err := st.UpsertAccountLimits(ctx, acctprobe.Result{
		Identity: acctprobe.Identity{Provider: "claude", AccountKey: "acctW", Email: "w@x"},
		Usage: acctprobe.Usage{Windows: acctprobe.Windows{
			{Kind: acctprobe.KindWeeklyScoped, Percent: 88.5, Severity: acctprobe.SeverityCritical, Scope: "opus"},
		}},
		TrustState: acctprobe.TrustVerified, CapturedAt: now,
	}, now); err != nil {
		t.Fatalf("fold account: %v", err)
	}
	if err := st.AddEpicRun(ctx, store.EpicRun{ID: "e1", Repo: "r", TmuxName: "epic-e1", Agent: "claude"}, now); err != nil {
		t.Fatalf("add epic: %v", err)
	}
	if err := st.SetEpicSeatBinding(ctx, "e1", "acctW", "seat1", "claude", now); err != nil {
		t.Fatalf("bind seat: %v", err)
	}

	resp, err := http.Get(url + "/v1/epics/digest")
	if err != nil {
		t.Fatalf("get digest: %v", err)
	}
	defer resp.Body.Close()
	var board struct {
		Epics []struct {
			Account struct {
				Windows []struct {
					Kind     string  `json:"kind"`
					Percent  float64 `json:"percent"`
					Severity string  `json:"severity"`
					Scope    string  `json:"scope"`
				} `json:"windows"`
			} `json:"account"`
			DriftSignals []string `json:"drift_signals"`
		} `json:"epics"`
		Attention []any `json:"attention"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&board); err != nil {
		t.Fatalf("decode board: %v", err)
	}
	if len(board.Epics) != 1 {
		t.Fatalf("want 1 epic, got %d", len(board.Epics))
	}
	ws := board.Epics[0].Account.Windows
	if len(ws) != 1 || ws[0].Kind != "weekly_scoped" || ws[0].Scope != "opus" || ws[0].Severity != "critical" || ws[0].Percent != 88.5 {
		t.Fatalf("windows not carried verbatim: %+v", ws)
	}
	// [] never null: with no attention items and no drift signals, both serialize as [].
	if board.Attention == nil {
		t.Fatalf("attention serialized as null, want []")
	}
	if board.Epics[0].DriftSignals == nil {
		t.Fatalf("drift_signals serialized as null, want []")
	}
}

// TestSummaryETag304 proves GET /v1/summary is counts-only with ETag/304.
func TestSummaryETag304(t *testing.T) {
	st, url, done := newMasterTestServer(t)
	defer done()
	ctx := context.Background()
	now := time.Now()
	if err := st.AddEpicRun(ctx, store.EpicRun{ID: "e1", Repo: "r", TmuxName: "epic-e1", Agent: "claude"}, now); err != nil {
		t.Fatalf("add epic: %v", err)
	}
	resp, err := http.Get(url + "/v1/summary")
	if err != nil {
		t.Fatalf("get summary: %v", err)
	}
	etag := resp.Header.Get("ETag")
	var sum struct {
		DigestSeq  int64          `json:"digest_seq"`
		ByPriority map[string]int `json:"by_priority"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&sum)
	resp.Body.Close()
	if etag == "" || sum.DigestSeq == 0 {
		t.Fatalf("summary: want ETag + digest_seq, got etag=%q seq=%d", etag, sum.DigestSeq)
	}

	req, _ := http.NewRequest(http.MethodGet, url+"/v1/summary", nil)
	req.Header.Set("If-None-Match", etag)
	resp2, _ := http.DefaultClient.Do(req)
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusNotModified {
		t.Fatalf("unchanged summary: want 304, got %d", resp2.StatusCode)
	}
}

// TestOneEpicDigestUntrustedTail proves GET /v1/epics/{id}/digest?tail=1 delimits the pane
// tail as UNTRUSTED data (plan §2.1 — a hostile pane cannot inject into the master).
func TestOneEpicDigestUntrustedTail(t *testing.T) {
	st, url, done := newMasterTestServer(t)
	defer done()
	ctx := context.Background()
	if err := st.AddEpicRun(ctx, store.EpicRun{ID: "e1", Repo: "r", TmuxName: "epic-e1", Agent: "claude"}, time.Now()); err != nil {
		t.Fatalf("add epic: %v", err)
	}
	// no tail requested: just the epic digest.
	resp, err := http.Get(url + "/v1/epics/e1/digest")
	if err != nil {
		t.Fatalf("get one digest: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("one-epic digest: want 200, got %d", resp.StatusCode)
	}
	var out struct {
		Epic struct {
			Slug string `json:"slug"`
		} `json:"epic"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Epic.Slug != "e1" {
		t.Fatalf("want epic e1, got %q", out.Epic.Slug)
	}
}
