package web_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/samhotchkiss/flowbee/internal/auth"
	"github.com/samhotchkiss/flowbee/internal/capacity"
	"github.com/samhotchkiss/flowbee/internal/job"
	"github.com/samhotchkiss/flowbee/internal/store"
	"github.com/samhotchkiss/flowbee/internal/testutil"
	"github.com/samhotchkiss/flowbee/internal/web"
	"github.com/samhotchkiss/flowbee/internal/worker"
)

// fixedClock feeds the UI a deterministic instant so the per-card stage timer +
// gauge ceilings render predictably (Flowbee is the sole clock; the core is fed
// time as a value, and the web layer reads this clock).
type fixedClock struct{ t time.Time }

func (c fixedClock) Now() time.Time { return c.t }

// mountUI builds the F12 web UI over a real store and returns an http.Handler.
func mountUI(t *testing.T, st *store.Store, clk fixedClock) http.Handler {
	t.Helper()
	return mountUIWithConfig(t, st, clk, web.Config{
		StaleHB:    90 * time.Second,
		StageAmber: 10 * time.Minute,
		StageRed:   30 * time.Minute,
	})
}

func mountUIWithConfig(t *testing.T, st *store.Store, clk fixedClock, cfg web.Config) http.Handler {
	t.Helper()
	if cfg.StaleHB == 0 {
		cfg.StaleHB = 90 * time.Second
	}
	if cfg.StageAmber == 0 {
		cfg.StageAmber = 10 * time.Minute
	}
	if cfg.StageRed == 0 {
		cfg.StageRed = 30 * time.Minute
	}
	ui := web.New(st, clk, cfg)
	mux := http.NewServeMux()
	ui.Mount(mux)
	return mux
}

func getBody(t *testing.T, h http.Handler, path string) (int, string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code, rec.Body.String()
}

func getBodyAs(t *testing.T, h http.Handler, path, role string) (int, string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if role != "" {
		req.Header.Set("X-Flowbee-Role", role)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code, rec.Body.String()
}

func getBodyWithToken(t *testing.T, h http.Handler, path, token string) (int, string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code, rec.Body.String()
}

// TestF12DashboardsRenderOffRealStore is the F12 acceptance test (build-list §G):
// /board, /fleet, /dashboard render off REAL store data (a temp-file SQLite DB),
// the board surfaces the Backlog + ⚠ Needs-you lanes + the yellow flowbee marker +
// the per-card stage timer, the fleet shows per-model slot pips + an account usage
// gauge with a ceiling line + rollover + live concurrent jobs, the detail drawer
// returns per-stage ENTERED/LEFT times + the build-history timeline WITHOUT dimming
// the board, the shared nav links every pane, and the live SSE hook is wired.
func TestF12DashboardsRenderOffRealStore(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	now := time.Date(2026, 6, 16, 12, 0, 0, 0, time.UTC)
	clk := fixedClock{t: now}

	// ── seed REAL board data through the store ──
	// a live build job, claimed long enough ago that its stage timer is RED.
	old := now.Add(-45 * time.Minute)
	if _, err := st.SeedJob(ctx, store.SeedParams{
		ID: "build-1", Kind: job.KindBuild, Flow: "build", Stage: "build",
		Role: job.RoleEngWorker, TaskText: "Add the widget endpoint\nmore detail",
		Now: old,
	}); err != nil {
		t.Fatalf("seed build job: %v", err)
	}
	// claim it AS a registered worker so it shows as a live concurrent job + a
	// roster lease, and the board card carries the bound identity.
	if _, err := st.ClaimReadyJob(ctx, store.ClaimParams{
		JobID: "build-1", LeaseID: "L1", Identity: "box-a", ModelFamily: "claude",
		Role: job.RoleEngWorker, Attested: []string{"role:eng_worker", "model_family:claude"},
		TTL: time.Hour, Now: old,
	}); err != nil {
		t.Fatalf("claim build job: %v", err)
	}

	// a backlog item (the Backlog lane) that needs a full spec.
	if _, err := st.SeedBacklog(ctx, store.SeedBacklogParams{
		ID: "bk-1", ChatRef: "c", IssueNumber: 77, Priority: 5,
		NeedsFullSpec: true, TaskText: "Design the export pipeline", Now: now,
	}); err != nil {
		t.Fatalf("seed backlog: %v", err)
	}
	// a PR-backed review card with CI still running, so the board exposes the
	// reconciled CI state inline instead of making the operator open GitHub.
	if _, err := st.SeedJob(ctx, store.SeedParams{
		ID: "review-1", Kind: job.KindBuild, Flow: "build", Stage: "build",
		Role: job.RoleEngWorker, TaskText: "Review the widget branch", Now: now,
	}); err != nil {
		t.Fatalf("seed review job: %v", err)
	}
	if _, err := st.DB.ExecContext(ctx, `
		UPDATE jobs SET state='review_pending', stage='review', pr_number=123 WHERE id='review-1'`); err != nil {
		t.Fatalf("move review job: %v", err)
	}
	if err := st.UpsertDomainBFacts(ctx, "review-1", job.DomainBFacts{
		PRExists: true, PRNumber: 123, HeadSHA: "head", BaseSHA: "base",
	}); err != nil {
		t.Fatalf("seed review facts: %v", err)
	}
	if err := st.MarkCIRunning(ctx, "review-1", true, now); err != nil {
		t.Fatalf("mark ci running: %v", err)
	}

	// register a worker box (the Fleet view) with per-model slots + named accounts,
	// then report usage that pushes one account over its ceiling (the rollover state).
	reg := worker.NewRegistry(st, 3600, 30, worker.OpenAllowlist())
	if _, err := reg.Register(ctx, worker.Registration{
		WorkerID: "w-box-a", Identity: "box-a", Host: "mac-studio", Arch: "arm64", OS: "darwin",
		Capabilities: []string{"role:eng_worker", "model_family:claude"},
		ModelSlots:   map[string]int{"claude": 3},
		Accounts: []worker.AccountSpecMsg{
			{AccountID: "claude-primary", ModelFamily: "claude", CeilingPct: 90, PreferenceRank: 0},
			{AccountID: "claude-backup", ModelFamily: "claude", CeilingPct: 90, PreferenceRank: 1},
		},
	}, now); err != nil {
		t.Fatalf("register worker: %v", err)
	}
	if _, err := st.RecordUsage(ctx, []capacity.UsageReport{
		{AccountID: "claude-primary", ModelFamily: "claude", UsagePct: 95}, // over ceiling -> rollover
		{AccountID: "claude-backup", ModelFamily: "claude", UsagePct: 20},
	}, now); err != nil {
		t.Fatalf("record usage: %v", err)
	}

	h := mountUI(t, st, clk)

	// ── /board renders off real data: lanes + flowbee marker + stage timer ──
	code, body := getBody(t, h, "/board")
	if code != http.StatusOK {
		t.Fatalf("/board status = %d", code)
	}
	for _, want := range []string{
		"Board",                            // pane title
		"⚠ Needs-you",                      // the needs-you bucket
		"Spec", "Build", "Review", "Merge", // the five top-level buckets (always rendered)
		"class=\"bucket",       // the bucket containers
		"data-job=\"build-1\"", // a real card from the store (under the Build bucket)
		"🐝",                    // the per-project marker (bee = flowbee/default)
		"class=\"timer red\"",  // the per-card stage timer (45m in stage => red)
		"box-a",                // the bound identity chip
		"CI running",           // PR-backed cards expose reconciled CI state
		"chip ci running",
		"/assets/board.js", // the live SSE hook script (EventSource) is wired
		"class=\"live\"",   // the live-connection indicator in the nav
		"id=\"fb-drawer\"", // the detail drawer shell (does not dim the board)
		"href=\"/fleet\"",  // shared nav
		"href=\"/dashboard\"",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("/board missing %q\n---\n%s", want, body)
		}
	}
	// the board must NOT dim: no full-screen overlay/backdrop element.
	if strings.Contains(body, "class=\"backdrop\"") || strings.Contains(body, "modal-overlay") {
		t.Fatalf("/board drawer must not dim the board (found a backdrop)")
	}

	// ── /fleet renders box-centric: slot pips + account gauge + ceiling + rollover ──
	code, body = getBody(t, h, "/fleet")
	if code != http.StatusOK {
		t.Fatalf("/fleet status = %d", code)
	}
	for _, want := range []string{
		"Fleet",
		"mac-studio",                      // the box host
		"claude",                          // the model family
		"class=\"pip busy\"",              // a busy slot pip (the live build)
		"data-account=\"claude-primary\"", // the account usage gauge
		"class=\"ceiling\"",               // the ceiling line on the gauge
		"rollover",                        // the over-ceiling rollover badge
		"build-1",                         // the live concurrent job on the box
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("/fleet missing %q\n---\n%s", want, body)
		}
	}

	// ── /dashboard renders off real data ──
	code, body = getBody(t, h, "/dashboard")
	if code != http.StatusOK {
		t.Fatalf("/dashboard status = %d", code)
	}
	for _, want := range []string{"Dashboard", "Needs human", "Roster", "Cost", "Audit", "box-a"} {
		if !strings.Contains(body, want) {
			t.Fatalf("/dashboard missing %q\n---\n%s", want, body)
		}
	}

	// ── /board/detail (the drawer fragment): per-stage ENTERED/LEFT + build history ──
	code, body = getBody(t, h, "/board/detail?job=build-1")
	if code != http.StatusOK {
		t.Fatalf("/board/detail status = %d", code)
	}
	for _, want := range []string{
		"Stages",        // the per-stage section
		"ENTERED",       // absolute enter time label
		"LEFT",          // absolute leave time label
		"Build history", // the build-history timeline
		"Lease claimed", // a real timeline note folded from the ledger
		"2026-06-16",    // an absolute timestamp from the seeded events
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("/board/detail missing %q\n---\n%s", want, body)
		}
	}

	// ── the embedded assets serve (go:embed) ──
	code, css := getBody(t, h, "/assets/app.css")
	if code != http.StatusOK || !strings.Contains(css, ".drawer") {
		t.Fatalf("/assets/app.css not served (code=%d)", code)
	}
	code, js := getBody(t, h, "/assets/board.js")
	if code != http.StatusOK || !strings.Contains(js, "EventSource") {
		t.Fatalf("/assets/board.js not served (code=%d)", code)
	}
}

// TestBoardRepoFilter proves the F9 multi-repo board filter: the board surfaces a
// chip per distinct repo, ?repo=<id> keeps only that repo's cards server-side (so
// it works with the live SSE refresh and without JS), and the bare /board shows
// every repo's cards.
func TestBoardRepoFilter(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	now := time.Date(2026, 6, 16, 12, 0, 0, 0, time.UTC)

	if _, err := st.SeedJob(ctx, store.SeedParams{
		ID: "alpha-1", Kind: job.KindBuild, Flow: "build", Stage: "build",
		Role: job.RoleEngWorker, TaskText: "alpha widget", Repo: "alpha", Now: now,
	}); err != nil {
		t.Fatalf("seed alpha: %v", err)
	}
	if _, err := st.SeedJob(ctx, store.SeedParams{
		ID: "beta-1", Kind: job.KindBuild, Flow: "build", Stage: "build",
		Role: job.RoleEngWorker, TaskText: "beta gadget", Repo: "beta", Now: now,
	}); err != nil {
		t.Fatalf("seed beta: %v", err)
	}
	h := mountUI(t, st, fixedClock{t: now})

	// bare /board: both repos' cards + a chip per repo + the "All" default.
	code, body := getBody(t, h, "/board")
	if code != http.StatusOK {
		t.Fatalf("/board status = %d", code)
	}
	for _, want := range []string{
		"data-job=\"alpha-1\"", "data-job=\"beta-1\"",
		"class=\"repo-chip active\"", // the "All" chip is active by default
		"href=\"/board?repo=alpha\"", // a chip link per repo
		"href=\"/board?repo=beta\"",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("/board missing %q\n---\n%s", want, body)
		}
	}

	// /board?repo=alpha: only alpha's cards, and the alpha chip is active.
	code, body = getBody(t, h, "/board?repo=alpha")
	if code != http.StatusOK {
		t.Fatalf("/board?repo=alpha status = %d", code)
	}
	if !strings.Contains(body, "data-job=\"alpha-1\"") {
		t.Fatalf("/board?repo=alpha must keep alpha cards:\n%s", body)
	}
	if strings.Contains(body, "data-job=\"beta-1\"") {
		t.Fatalf("/board?repo=alpha must hide beta cards:\n%s", body)
	}
	if !strings.Contains(body, "repo-chip active\" href=\"/board?repo=alpha\"") {
		t.Fatalf("/board?repo=alpha must mark the alpha chip active:\n%s", body)
	}
}

func TestBoardTraceMenuIsSuperadminOnly(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	now := time.Date(2026, 6, 16, 12, 0, 0, 0, time.UTC)
	if _, err := st.SeedJob(ctx, store.SeedParams{
		ID: "trace-1", Kind: job.KindBuild, Flow: "build", Stage: "build",
		Role: job.RoleEngWorker, TaskText: "Trace this card", Now: now,
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	authn := auth.NewBearer([]byte("server-secret"), []string{"admin", "viewer"}, false)
	h := mountUIWithConfig(t, st, fixedClock{t: now}, web.Config{
		Authenticator:        authn,
		SuperadminIdentities: []string{"admin"},
	})

	code, body := getBodyWithToken(t, h, "/board", authn.Mint("admin"))
	if code != http.StatusOK {
		t.Fatalf("superadmin /board status = %d", code)
	}
	for _, want := range []string{
		"class=\"card-menu\"",
		"aria-label=\"card actions\"",
		"data-trace-job=\"trace-1\"",
		"View trace",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("superadmin board missing %q\n---\n%s", want, body)
		}
	}

	code, body = getBodyWithToken(t, h, "/board", authn.Mint("viewer"))
	if code != http.StatusOK {
		t.Fatalf("non-superadmin /board status = %d", code)
	}
	for _, forbidden := range []string{"View trace", "data-trace-job", "card-menu", "card actions"} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("non-superadmin board must hide trace affordance %q\n---\n%s", forbidden, body)
		}
	}

	code, body = getBodyAs(t, h, "/board", "superadmin")
	if code != http.StatusOK {
		t.Fatalf("spoofed-header /board status = %d", code)
	}
	for _, forbidden := range []string{"View trace", "data-trace-job", "card-menu", "card actions"} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("spoofed header must not reveal trace affordance %q\n---\n%s", forbidden, body)
		}
	}
}

func TestBoardTraceEndpointRequiresSuperadminAndReusesDrawer(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	now := time.Date(2026, 6, 16, 12, 0, 0, 0, time.UTC)
	if _, err := st.SeedJob(ctx, store.SeedParams{
		ID: "trace-1", Kind: job.KindBuild, Flow: "build", Stage: "build",
		Role: job.RoleEngWorker, TaskText: "Trace this card", Now: now,
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := st.ClaimReadyJob(ctx, store.ClaimParams{
		JobID: "trace-1", LeaseID: "trace-lease", Identity: "box-a", ModelFamily: "claude",
		Role: job.RoleEngWorker, Attested: []string{"role:eng_worker", "model_family:claude"},
		TTL: time.Hour, Now: now,
	}); err != nil {
		t.Fatalf("claim: %v", err)
	}
	authn := auth.NewBearer([]byte("server-secret"), []string{"admin", "viewer"}, false)
	h := mountUIWithConfig(t, st, fixedClock{t: now}, web.Config{
		Authenticator:        authn,
		SuperadminIdentities: []string{"admin"},
	})

	code, body := getBody(t, h, "/board/trace?job=trace-1")
	if code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated /board/trace status = %d, body:\n%s", code, body)
	}
	if strings.Contains(body, "Stages") || strings.Contains(body, "Build history") || strings.Contains(body, "Lease claimed") {
		t.Fatalf("forbidden trace response must not disclose drawer contents:\n%s", body)
	}

	code, body = getBodyAs(t, h, "/board/trace?job=trace-1", "superadmin")
	if code != http.StatusUnauthorized {
		t.Fatalf("spoofed-header /board/trace status = %d, want 401; body:\n%s", code, body)
	}
	if strings.Contains(body, "Stages") || strings.Contains(body, "Build history") || strings.Contains(body, "Lease claimed") {
		t.Fatalf("spoofed trace response must not disclose drawer contents:\n%s", body)
	}

	code, body = getBodyWithToken(t, h, "/board/trace?job=trace-1", authn.Mint("viewer"))
	if code != http.StatusForbidden {
		t.Fatalf("non-superadmin /board/trace status = %d, body:\n%s", code, body)
	}
	if strings.Contains(body, "Stages") || strings.Contains(body, "Build history") || strings.Contains(body, "Lease claimed") {
		t.Fatalf("forbidden trace response must not disclose drawer contents:\n%s", body)
	}

	code, body = getBodyWithToken(t, h, "/board/trace?job=trace-1", authn.Mint("admin"))
	if code != http.StatusOK {
		t.Fatalf("superadmin /board/trace status = %d, body:\n%s", code, body)
	}
	for _, want := range []string{
		"Trace this card",
		"trace-1",
		"Stages",
		"ENTERED",
		"LEFT",
		"Build history",
		"Lease claimed",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("superadmin trace drawer missing %q\n---\n%s", want, body)
		}
	}

	code, js := getBody(t, h, "/assets/board.js")
	if code != http.StatusOK {
		t.Fatalf("/assets/board.js status = %d", code)
	}
	for _, want := range []string{"data-trace-job", "/board/trace", "openTraceDrawer"} {
		if !strings.Contains(js, want) {
			t.Fatalf("board.js missing trace menu wiring %q\n---\n%s", want, js)
		}
	}
}

// TestF12PartialRefreshFragment proves the SSE refresh path: ?partial=1 returns
// only the live body fragment (so the SSE hook swaps it in place without reloading
// the page or dimming the open drawer). It must NOT carry the <html>/nav chrome.
func TestF12PartialRefreshFragment(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	now := time.Date(2026, 6, 16, 12, 0, 0, 0, time.UTC)
	if _, err := st.SeedJob(ctx, store.SeedParams{
		ID: "b1", Kind: job.KindBuild, Flow: "build", Stage: "build",
		Role: job.RoleEngWorker, TaskText: "task one", Now: now,
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	h := mountUI(t, st, fixedClock{t: now})

	code, full := getBody(t, h, "/board")
	if code != http.StatusOK || !strings.Contains(full, "<!doctype html>") {
		t.Fatalf("full board should carry chrome (code=%d)", code)
	}
	code, frag := getBody(t, h, "/board?partial=1")
	if code != http.StatusOK {
		t.Fatalf("partial status = %d", code)
	}
	if strings.Contains(frag, "<!doctype html>") || strings.Contains(frag, "fb-nav") {
		t.Fatalf("partial fragment must omit page chrome:\n%s", frag)
	}
	if !strings.Contains(frag, "data-job=\"b1\"") {
		t.Fatalf("partial fragment must still render the live board:\n%s", frag)
	}
}
