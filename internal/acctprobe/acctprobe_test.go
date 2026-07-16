package acctprobe

import (
	"context"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/samhotchkiss/flowbee/internal/clock"
)

// ── shared test fakes & helpers ──

// fixedNow is the instant the tests' fake clock reports.
var fixedNow = time.Date(2026, 7, 16, 12, 0, 0, 0, time.UTC)

func fakeClock() clock.Clock { return clock.NewFake(fixedNow) }

func td(parts ...string) string {
	return filepath.Join(append([]string{"testdata"}, parts...)...)
}

type fakeExec struct {
	fn func(ctx context.Context, name string, args ...string) ([]byte, error)
}

func (f fakeExec) Output(ctx context.Context, name string, args ...string) ([]byte, error) {
	return f.fn(ctx, name, args...)
}

type fakeHTTP struct {
	fn func(*http.Request) (*http.Response, error)
}

func (f fakeHTTP) Do(r *http.Request) (*http.Response, error) { return f.fn(r) }

type fakeAppServer struct {
	result AppServerResult
	err    error
}

func (f fakeAppServer) Read(context.Context, string) (AppServerResult, error) {
	return f.result, f.err
}

func httpResp(status int, headers map[string]string, body string) *http.Response {
	h := http.Header{}
	for k, v := range headers {
		h.Set(k, v)
	}
	return &http.Response{StatusCode: status, Header: h, Body: io.NopCloser(strings.NewReader(body))}
}

// osProber builds a Prober over the real filesystem with a fake clock (the default
// for file-fixture tests that need no network/exec).
func osProber() *Prober { return NewWith(OSFS{}, nil, nil, nil, fakeClock()) }

// ── pure helpers ──

func TestWindowsFolds(t *testing.T) {
	w := Windows{
		{Kind: KindSession, Percent: 24, Severity: SeverityNormal},
		{Kind: KindWeeklyAll, Percent: 60, Severity: SeverityCritical},
		{Kind: KindWeeklyScoped, Percent: 2, Scope: "Fable"},
	}
	if p, ok := w.SessionPct(); !ok || p != 24 {
		t.Errorf("SessionPct=%v ok=%v want 24,true", p, ok)
	}
	if p, ok := w.WeeklyPct(); !ok || p != 60 {
		t.Errorf("WeeklyPct=%v ok=%v want 60,true", p, ok)
	}
	if p, ok := w.MaxPct(); !ok || p != 60 {
		t.Errorf("MaxPct=%v ok=%v want 60,true", p, ok)
	}
	if !w.Critical() {
		t.Error("Critical should be true (weekly is critical)")
	}
	// UNKNOWN must be distinguishable from zero: no session window -> ok=false.
	empty := Windows{{Kind: KindWeeklyAll, Percent: 0}}
	if _, ok := empty.SessionPct(); ok {
		t.Error("SessionPct on a set with no session window must report ok=false, not 0")
	}
}

func TestUsageReportBridge(t *testing.T) {
	r := Result{
		Identity: Identity{Provider: ProviderClaude, AccountKey: "acct-1"},
		Usage: Usage{Windows: Windows{
			{Kind: KindSession, Percent: 24.2},
			{Kind: KindWeeklyAll, Percent: 60.0},
		}},
	}
	rep := r.UsageReport("claude")
	if rep.AccountID != "acct-1" || rep.ModelFamily != "claude" {
		t.Errorf("bridge identity: %+v", rep)
	}
	if rep.UsagePct != 60 { // max window, rounded up
		t.Errorf("UsagePct=%d want 60", rep.UsagePct)
	}
	// a fractional max rounds UP so it never reads as idle to a ceiling gate.
	r2 := Result{Usage: Usage{Windows: Windows{{Kind: KindSession, Percent: 0.4}}}}
	if got := r2.UsageReport("x").UsagePct; got != 1 {
		t.Errorf("ceil rounding: UsagePct=%d want 1", got)
	}
	// rate-limited propagates to pin the account.
	r3 := Result{Usage: Usage{RateLimited: true}}
	if !r3.UsageReport("x").RateLimited {
		t.Error("RateLimited must propagate into the UsageReport")
	}
}

func TestStaleness(t *testing.T) {
	r := Result{CapturedAt: fixedNow.Add(-10 * time.Minute)}
	d, ok := r.Staleness(fixedNow)
	if !ok || d != 10*time.Minute {
		t.Errorf("Staleness=%v ok=%v want 10m,true", d, ok)
	}
	if _, ok := (Result{}).Staleness(fixedNow); ok {
		t.Error("zero CapturedAt must report ok=false (unknown, not fresh)")
	}
}

func TestTrustRoutable(t *testing.T) {
	for _, tc := range []struct {
		state TrustState
		want  bool
	}{
		{TrustVerified, true},
		{TrustVerifiedLocal, true},
		{TrustStale, false},
		{TrustDisplayOnly, false},
		{TrustHeld, false},
	} {
		if got := tc.state.Routable(); got != tc.want {
			t.Errorf("%s.Routable()=%v want %v", tc.state, got, tc.want)
		}
	}
}

func TestFingerprintAndDigest(t *testing.T) {
	if fingerprint("") != "" {
		t.Error("fingerprint of empty id must be empty (never mint a fake-valid one)")
	}
	if got := fingerprint("org-abc"); len(got) != 16 {
		t.Errorf("fingerprint len=%d want 16", len(got))
	}
	if fingerprint("a") == fingerprint("b") {
		t.Error("distinct ids must fingerprint distinctly")
	}
	if _, ok := digest16(""); ok {
		t.Error("digest16 of empty secret must report ok=false")
	}
	d, ok := digest16("FAKE-token")
	if !ok || len(d) != 16 {
		t.Errorf("digest16=%q ok=%v want len 16", d, ok)
	}
}

func TestBucketWindowByDuration(t *testing.T) {
	cases := []struct {
		minutes  int
		pct      float64
		wantOK   bool
		wantKind WindowKind
	}{
		{300, 12, true, KindSession},
		{10080, 88, true, KindWeeklyAll},
		{60, 10, false, ""},   // unrecognized duration is rejected, not mis-bucketed
		{300, 150, false, ""}, // out-of-range percent rejected
		{300, -1, false, ""},
	}
	for _, c := range cases {
		lw, ok := bucketWindow(c.minutes, c.pct, time.Time{})
		if ok != c.wantOK {
			t.Errorf("bucketWindow(%d,%v) ok=%v want %v", c.minutes, c.pct, ok, c.wantOK)
			continue
		}
		if ok && lw.Kind != c.wantKind {
			t.Errorf("bucketWindow(%d) kind=%v want %v", c.minutes, lw.Kind, c.wantKind)
		}
	}
}

func TestParseResetsAt(t *testing.T) {
	if got := parseResetsAt([]byte("1784780146")); got.Unix() != 1784780146 {
		t.Errorf("epoch parse=%v", got)
	}
	if got := parseResetsAt([]byte(`"2026-07-16T23:50:00.238598+00:00"`)); got.IsZero() {
		t.Error("ISO parse produced zero time")
	}
	if got := parseResetsAt([]byte("null")); !got.IsZero() {
		t.Error("null must parse to zero time")
	}
}

func TestParseCodexModel(t *testing.T) {
	toml := "# comment\nmodel = \"gpt-5.6-sol\"\nother = 1\n[tui]\nmodel = \"ignored\"\n"
	if got := parseCodexModel([]byte(toml)); got != "gpt-5.6-sol" {
		t.Errorf("parseCodexModel=%q want gpt-5.6-sol", got)
	}
	if got := parseCodexModel([]byte("[x]\nmodel=\"y\"\n")); got != "" {
		t.Errorf("model under a section must be ignored, got %q", got)
	}
}

func TestHoldError(t *testing.T) {
	e := held(ReasonTokenExpired, io.EOF)
	if e.Reason != ReasonTokenExpired {
		t.Errorf("reason=%v", e.Reason)
	}
	if e.Unwrap() != io.EOF {
		t.Error("Unwrap must expose the wrapped cause")
	}
	if !strings.Contains(e.Error(), "token_expired") {
		t.Errorf("Error()=%q must mention the reason", e.Error())
	}
}
