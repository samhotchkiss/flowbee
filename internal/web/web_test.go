package web_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/samhotchkiss/flowbee/internal/acctprobe"
	"github.com/samhotchkiss/flowbee/internal/capacity"
	"github.com/samhotchkiss/flowbee/internal/epicspec"
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
	ui := web.New(st, clk, web.Config{
		StaleHB:    90 * time.Second,
		StageAmber: 10 * time.Minute,
		StageRed:   30 * time.Minute,
	})
	mux := http.NewServeMux()
	ui.Mount(mux)
	return mux
}

// TestEpicFleetDashboardRendersLiveCapacityAndConcurrentSeats pins the target
// session-per-epic operator surface against real migrated store data. It also
// proves that the root route is exact, the SSE partial is chrome-free, and the
// dashboard exposes the concurrent-seat load that controls launch throughput.
func TestEpicFleetDashboardRendersLiveCapacityAndConcurrentSeats(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 18, 18, 36, 0, 0, time.UTC)

	cappedSeat := store.Seat{
		Box: "box-a", AgentFamily: "claude", ConfigDir: "/cfg/claude1",
		AccountKey: "acct-capped-6356db02", Health: store.SeatReady, MaxConcurrent: 2,
	}
	healthySeat := store.Seat{
		Box: "box-b", AgentFamily: "claude", ConfigDir: "/cfg/claude2",
		AccountKey: "acct-healthy-5ef2973c", Health: store.SeatReady, MaxConcurrent: 2,
	}
	codexSeat := store.Seat{
		Box: "box-a", AgentFamily: "codex", CodexHome: "/cfg/codex1",
		AccountKey: "acct-codex-42df4963", Health: store.SeatReady, MaxConcurrent: 2,
	}
	for _, seat := range []store.Seat{cappedSeat, healthySeat, codexSeat} {
		if err := st.AddSeat(ctx, seat, now); err != nil {
			t.Fatalf("add seat: %v", err)
		}
	}

	if err := st.UpsertAccountLimits(ctx, acctprobe.Result{
		Identity: acctprobe.Identity{Provider: "claude", AccountKey: cappedSeat.AccountKey, Email: "s@swh.me"},
		Usage: acctprobe.Usage{Windows: acctprobe.Windows{
			{Kind: acctprobe.KindSession, Percent: 100, Severity: acctprobe.SeverityCritical, ResetsAt: now.Add(14 * time.Minute)},
			{Kind: acctprobe.KindWeeklyAll, Percent: 53, Severity: acctprobe.SeverityNormal},
		}}, TrustState: acctprobe.TrustVerified, CapturedAt: now,
	}, now); err != nil {
		t.Fatalf("fold capped account: %v", err)
	}
	if err := st.UpsertAccountLimits(ctx, acctprobe.Result{
		Identity: acctprobe.Identity{Provider: "claude", AccountKey: healthySeat.AccountKey, Email: "pearl@swh.me"},
		Usage: acctprobe.Usage{Windows: acctprobe.Windows{
			{Kind: acctprobe.KindSession, Percent: 63, Severity: acctprobe.SeverityNormal},
			{Kind: acctprobe.KindWeeklyAll, Percent: 47, Severity: acctprobe.SeverityNormal},
		}}, TrustState: acctprobe.TrustVerified, CapturedAt: now,
	}, now); err != nil {
		t.Fatalf("fold healthy account: %v", err)
	}
	if err := st.UpsertAccountLimits(ctx, acctprobe.Result{
		Identity: acctprobe.Identity{Provider: "codex", AccountKey: codexSeat.AccountKey},
		Usage: acctprobe.Usage{Windows: acctprobe.Windows{
			{Kind: acctprobe.KindWeeklyAll, Percent: 59, Severity: acctprobe.SeverityNormal},
		}}, TrustState: acctprobe.TrustDisplayOnly, CapturedAt: now,
	}, now); err != nil {
		t.Fatalf("fold codex account: %v", err)
	}

	active := []store.EpicRun{
		{ID: "instant-cache", Repo: "russ", Title: "Instant cache", Scope: []string{"internal/cache/**"}, SeatID: cappedSeat.ComposeID(), Agent: "claude", Branch: "epic/instant-cache", TmuxName: "epic-instant-cache"},
		{ID: "register-lifecycle", Repo: "russ", Title: "Register lifecycle", Scope: []string{"internal/register/**"}, SeatID: cappedSeat.ComposeID(), Agent: "claude", Branch: "epic/register-lifecycle", TmuxName: "epic-register-lifecycle"},
	}
	for _, epic := range active {
		if err := st.AddEpicRun(ctx, epic, 99, now); err != nil {
			t.Fatalf("add concurrent epic %s: %v", epic.ID, err)
		}
		if err := st.MarkEpicLaunched(ctx, epic.ID, now); err != nil {
			t.Fatalf("launch epic %s: %v", epic.ID, err)
		}
	}
	if err := st.UpsertEpicStatus(ctx, "instant-cache", epicspec.StatusBlock{
		UpdatedRaw: now.Format(time.RFC3339), CurrentStep: 4, StepsTotal: 7, State: "building",
	}, now); err != nil {
		t.Fatalf("update active status: %v", err)
	}
	if err := st.UpsertEpicStatus(ctx, "register-lifecycle", epicspec.StatusBlock{
		UpdatedRaw: now.Format(time.RFC3339), CurrentStep: 7, StepsTotal: 7, State: "blocked", Blockers: "version-locked prompt tests need an update",
	}, now); err != nil {
		t.Fatalf("update blocked status: %v", err)
	}

	if err := st.AddEpicRun(ctx, store.EpicRun{
		ID: "reply-latency", Repo: "russ", Title: "Reply latency", Scope: []string{"internal/latency/**"},
		Host: "box-b", Agent: "claude", Branch: "epic/reply-latency", TmuxName: "epic-reply-latency",
	}, 2, now); err != nil {
		t.Fatalf("add completed epic: %v", err)
	}
	if err := st.MarkEpicLaunched(ctx, "reply-latency", now); err != nil {
		t.Fatalf("launch completed epic: %v", err)
	}
	if err := st.UpsertEpicStatus(ctx, "reply-latency", epicspec.StatusBlock{
		UpdatedRaw: now.Format(time.RFC3339), CurrentStep: 6, StepsTotal: 6, State: "done",
	}, now); err != nil {
		t.Fatalf("finish completed epic: %v", err)
	}

	if _, _, err := st.UpsertAttentionItem(ctx, store.AttentionItem{
		ID: "att-ci-red", Kind: "ci_red_on_epic_pr", EpicID: "register-lifecycle", Repo: "russ",
		Priority: 20, DedupKey: "register-lifecycle:ci-red:head", Blocking: true,
		Detail: "version-locked prompt tests need an update",
	}, now); err != nil {
		t.Fatalf("open attention: %v", err)
	}
	if _, err := st.RegisterSupervisor(ctx, store.Supervisor{
		Label: "flowbee-master", Kind: "claude", ModelFamily: "claude", Box: "box-master", TmuxName: "master", Repos: []string{"russ"},
	}, now); err != nil {
		t.Fatalf("register master: %v", err)
	}

	h := mountUI(t, st, fixedClock{t: now})
	for _, path := range []string{"/", "/epics"} {
		code, body := getBody(t, h, path)
		if code != http.StatusOK {
			t.Fatalf("%s status = %d", path, code)
		}
		for _, want := range []string{
			"Flowbee Fleet", "session-per-epic control plane", "Session cap hit", "s@swh.me", "pearl@swh.me",
			">codex1</strong>",
			"data-throughput", "data-usage", "data-account=\"acct-capped-6356db02\"", "data-seats",
			"data-epic=\"instant-cache\"", "data-epic=\"register-lifecycle\"", "data-epic=\"reply-latency\"",
			"2/2", "CI red", "version-locked prompt tests", "Completed today", "id=\"theme-toggle\"",
		} {
			if !strings.Contains(body, want) {
				t.Fatalf("%s missing %q\n---\n%s", path, want, body)
			}
		}
		if strings.Contains(body, "class=\"fb-nav\"") {
			t.Fatalf("epic target must not render the legacy gradient nav:\n%s", body)
		}
	}

	code, partial := getBody(t, h, "/epics?partial=1")
	if code != http.StatusOK || strings.Contains(partial, "<!doctype html>") || !strings.Contains(partial, "data-throughput") {
		t.Fatalf("epics partial contract failed (code=%d):\n%s", code, partial)
	}
	code, _ = getBody(t, h, "/v1/not-real")
	if code != http.StatusNotFound {
		t.Fatalf("unknown API path = %d, want 404", code)
	}
	_, js := getBody(t, h, "/assets/board.js")
	for _, want := range []string{"addEventListener(\"lifecycle\"", "addEventListener(\"epics\"", "flowbee-theme"} {
		if !strings.Contains(js, want) {
			t.Fatalf("board.js missing %q", want)
		}
	}
}

func getBody(t *testing.T, h http.Handler, path string) (int, string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code, rec.Body.String()
}

// TestProductionDashboardRoutes pins the operator-facing URL contract: the URL
// printed by `flowbee up` is the epic fleet dashboard, while the pre-existing
// project-OUT audit remains available at its explicit /audit URL.
func TestProductionDashboardRoutes(t *testing.T) {
	st := testutil.NewStore(t)
	h := mountUI(t, st, fixedClock{t: time.Date(2026, 7, 18, 18, 36, 0, 0, time.UTC)})

	code, body := getBody(t, h, "/dashboard")
	if code != http.StatusOK {
		t.Fatalf("/dashboard status = %d", code)
	}
	if !strings.Contains(body, "Flowbee Fleet") || !strings.Contains(body, "session-per-epic control plane") {
		t.Fatalf("/dashboard must render the production epic fleet:\n%s", body)
	}
	if strings.Contains(body, "Audit (project-OUT)") {
		t.Fatalf("/dashboard must not render the legacy audit view:\n%s", body)
	}

	code, body = getBody(t, h, "/audit")
	if code != http.StatusOK {
		t.Fatalf("/audit status = %d", code)
	}
	if !strings.Contains(body, "Audit (project-OUT)") || !strings.Contains(body, "Needs human") {
		t.Fatalf("/audit must preserve the legacy project-OUT audit view:\n%s", body)
	}
	if strings.Contains(body, "session-per-epic control plane") {
		t.Fatalf("/audit must not render the epic fleet:\n%s", body)
	}
}

// TestDashboardJSHasPeriodicRefreshFallback ensures a dashboard still converges
// when an SSE nudge is dropped or a producer mutates a live read-model without
// publishing. SSE remains the fast path; this interval is the correctness floor.
func TestDashboardJSHasPeriodicRefreshFallback(t *testing.T) {
	st := testutil.NewStore(t)
	h := mountUI(t, st, fixedClock{t: time.Now()})
	code, js := getBody(t, h, "/assets/board.js")
	if code != http.StatusOK {
		t.Fatalf("board.js status = %d", code)
	}
	if !strings.Contains(js, "setInterval") || !strings.Contains(js, "refresh") {
		t.Fatalf("board.js needs a periodic refresh fallback in addition to EventSource:\n%s", js)
	}
}

// TestEpicDashboardDerivesStaleMasterFromHeartbeat proves stored state='active'
// cannot paint a dead master green. The web layer receives the same stale-heartbeat
// threshold as the liveness reaper and must derive the operator-visible state by age.
func TestEpicDashboardDerivesStaleMasterFromHeartbeat(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 18, 18, 36, 0, 0, time.UTC)
	if _, err := st.RegisterSupervisor(ctx, store.Supervisor{
		Label: "flowbee-master", Kind: "claude", ModelFamily: "claude", Box: "box-master", TmuxName: "master",
	}, now.Add(-10*time.Minute)); err != nil {
		t.Fatalf("register stale master: %v", err)
	}

	_, body := getBody(t, mountUI(t, st, fixedClock{t: now}), "/dashboard")
	if !strings.Contains(body, "ops-master-kpi critical") || !strings.Contains(body, ">stale</") {
		t.Fatalf("master older than StaleHB must render stale/critical, not active:\n%s", body)
	}
}

// TestTerminalEpicPresentationOverridesStaleRuntime pins two pieces of terminal
// truth: a done lifecycle beats a stale pre-completion pane classification, and a
// done epic with no trustworthy step denominator is complete rather than 0%/unknown.
func TestTerminalEpicPresentationOverridesStaleRuntime(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 18, 18, 36, 0, 0, time.UTC)
	for _, tc := range []struct {
		id, pane       string
		current, total int
	}{
		{id: "done-stale-working", pane: "working", current: 2, total: 7},
		{id: "done-stale-idle", pane: "idle_at_prompt"},
	} {
		addRunningEpic(t, st, tc.id, now)
		if err := st.UpsertEpicStatus(ctx, tc.id, epicspec.StatusBlock{
			UpdatedRaw: now.Format(time.RFC3339), CurrentStep: tc.current, StepsTotal: tc.total, State: "done",
		}, now); err != nil {
			t.Fatalf("finish %s: %v", tc.id, err)
		}
		// Reproduce a persisted last observation from immediately before completion.
		if err := st.SetEpicRuntimeState(ctx, tc.id, store.EpicRuntimeState{
			ContextPct: store.ContextPctUnknown, PaneState: tc.pane,
		}, now); err != nil {
			t.Fatalf("set stale pane for %s: %v", tc.id, err)
		}
	}

	_, body := getBody(t, mountUI(t, st, fixedClock{t: now}), "/dashboard")
	for _, id := range []string{"done-stale-working", "done-stale-idle"} {
		card := articleFor(t, body, `data-epic="`+id+`"`)
		for _, want := range []string{
			"ops-pane complete", ">complete</span>", `aria-valuenow="100"`, `style="width:100%"`,
			`<strong class="ops-step">done</strong>`,
		} {
			if !strings.Contains(card, want) {
				t.Fatalf("terminal card %s missing %q:\n%s", id, want, card)
			}
		}
		if strings.Contains(card, "ops-pane working") || strings.Contains(card, "ops-pane idle") {
			t.Fatalf("terminal card %s exposed stale pane state:\n%s", id, card)
		}
	}
}

// TestAbandonedEpicDoesNotInflateCompletedKPI separates a released/abandoned
// reservation from successfully completed throughput.
func TestAbandonedEpicDoesNotInflateCompletedKPI(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 18, 18, 36, 0, 0, time.UTC)

	addRunningEpic(t, st, "completed-one", now)
	if err := st.UpsertEpicStatus(ctx, "completed-one", epicspec.StatusBlock{
		UpdatedRaw: now.Format(time.RFC3339), CurrentStep: 3, StepsTotal: 3, State: "done",
	}, now); err != nil {
		t.Fatalf("finish completed epic: %v", err)
	}
	addRunningEpic(t, st, "abandoned-one", now)
	if err := st.AbandonEpicRun(ctx, "abandoned-one", now); err != nil {
		t.Fatalf("abandon epic: %v", err)
	}

	_, body := getBody(t, mountUI(t, st, fixedClock{t: now}), "/dashboard")
	if got := kpiValue(t, body, "Epics completed"); got != "1" {
		t.Fatalf("completed KPI = %q, want 1 (abandoned is not throughput):\n%s", got, body)
	}
}

// TestDisabledSeatDoesNotRenderReady prevents a retired seat whose last probe was
// healthy from advertising launch capacity after enabled has been cleared.
func TestDisabledSeatDoesNotRenderReady(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 18, 18, 36, 0, 0, time.UTC)
	seat := store.Seat{
		Box: "retired-box", AgentFamily: "codex", CodexHome: "/cfg/retired-codex",
		AccountKey: "retired-account", Health: store.SeatReady, MaxConcurrent: 2,
	}
	if err := st.AddSeat(ctx, seat, now); err != nil {
		t.Fatalf("add seat: %v", err)
	}
	if _, err := st.DB.ExecContext(ctx, `UPDATE seats SET enabled = 0 WHERE id = ?`, seat.ComposeID()); err != nil {
		t.Fatalf("disable seat: %v", err)
	}

	_, body := getBody(t, mountUI(t, st, fixedClock{t: now}), "/dashboard")
	row := articleFor(t, body, `data-seat="`+seat.ComposeID()+`"`)
	if !strings.Contains(row, ">disabled</span>") {
		t.Fatalf("disabled seat must render disabled:\n%s", row)
	}
	if strings.Contains(row, ">ready</span>") {
		t.Fatalf("disabled seat must not advertise ready capacity:\n%s", row)
	}
	if got := kpiValue(t, body, "Seats · 0 families"); got != "0" {
		t.Fatalf("enabled-seat KPI = %q, want 0 for a disabled-only registry:\n%s", got, body)
	}
}

// TestLegacySQLiteEpicTimesSortAndCount proves the dashboard remains compatible
// with rows written by SQLite datetime('now') before epic timestamps were uniformly
// RFC3339. The newest legacy row sorts first and contributes to today's throughput.
func TestLegacySQLiteEpicTimesSortAndCount(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 18, 18, 36, 0, 0, time.UTC)
	for _, id := range []string{"z-legacy-newest", "a-rfc-middle", "m-legacy-previous"} {
		addRunningEpic(t, st, id, now.Add(-time.Hour))
	}
	for _, row := range []struct {
		id, stamp string
	}{
		{id: "z-legacy-newest", stamp: "2026-07-18 18:00:00"},
		{id: "a-rfc-middle", stamp: "2026-07-18T17:00:00Z"},
		{id: "m-legacy-previous", stamp: "2026-07-17 23:00:00"},
	} {
		if _, err := st.DB.ExecContext(ctx, `
			UPDATE epics
			   SET state = 'done', status_state_detail = 'done', finished_at = ?, updated_at = ?
			 WHERE id = ?`, row.stamp, row.stamp, row.id); err != nil {
			t.Fatalf("set legacy time for %s: %v", row.id, err)
		}
	}

	_, body := getBody(t, mountUI(t, st, fixedClock{t: now}), "/dashboard")
	if got := kpiValue(t, body, "Completed today"); got != "2" {
		t.Fatalf("completed-today KPI = %q, want 2 across RFC3339 + legacy SQLite timestamps:\n%s", got, body)
	}
	newest := strings.Index(body, `data-epic="z-legacy-newest"`)
	middle := strings.Index(body, `data-epic="a-rfc-middle"`)
	previous := strings.Index(body, `data-epic="m-legacy-previous"`)
	if newest < 0 || middle < 0 || previous < 0 {
		t.Fatalf("all legacy timestamp cards must render:\n%s", body)
	}
	if !(newest < middle && middle < previous) {
		t.Fatalf("completed cards not newest-first: legacy-newest=%d rfc-middle=%d legacy-previous=%d\n%s",
			newest, middle, previous, body)
	}
}

func addRunningEpic(t *testing.T, st *store.Store, id string, now time.Time) {
	t.Helper()
	ctx := context.Background()
	if err := st.AddEpicRun(ctx, store.EpicRun{
		ID: id, Repo: "test", Title: id, Agent: "claude", Branch: "epic/" + id, TmuxName: "epic-" + id,
	}, 1, now); err != nil {
		t.Fatalf("add epic %s: %v", id, err)
	}
	if err := st.MarkEpicLaunched(ctx, id, now); err != nil {
		t.Fatalf("launch epic %s: %v", id, err)
	}
}

func articleFor(t *testing.T, body, marker string) string {
	t.Helper()
	i := strings.Index(body, marker)
	if i < 0 {
		t.Fatalf("response missing %q:\n%s", marker, body)
	}
	start := strings.LastIndex(body[:i], "<article")
	if start < 0 {
		t.Fatalf("response has %q outside an article", marker)
	}
	end := strings.Index(body[i:], "</article>")
	if end < 0 {
		t.Fatalf("article for %q is unterminated", marker)
	}
	return body[start : i+end+len("</article>")]
}

func kpiValue(t *testing.T, body, label string) string {
	t.Helper()
	labelAt := strings.Index(body, `<div class="ops-kpi-label">`+label)
	if labelAt < 0 {
		t.Fatalf("response missing KPI label %q:\n%s", label, body)
	}
	const open = `<div class="ops-kpi-value">`
	valueAt := strings.LastIndex(body[:labelAt], open)
	if valueAt < 0 {
		t.Fatalf("KPI %q has no value", label)
	}
	valueAt += len(open)
	end := strings.Index(body[valueAt:labelAt], "</div>")
	if end < 0 {
		t.Fatalf("KPI %q has an unterminated value", label)
	}
	return strings.TrimSpace(body[valueAt : valueAt+end])
}

// TestF12DashboardsRenderOffRealStore is the F12 acceptance test (build-list §G):
// /board, /fleet, /audit render off REAL store data (a temp-file SQLite DB),
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

	// ── /audit renders the legacy audit data ──
	code, body = getBody(t, h, "/audit")
	if code != http.StatusOK {
		t.Fatalf("/audit status = %d", code)
	}
	for _, want := range []string{"Dashboard", "Needs human", "Roster", "Cost", "Audit", "box-a"} {
		if !strings.Contains(body, want) {
			t.Fatalf("/audit missing %q\n---\n%s", want, body)
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
