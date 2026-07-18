// Package web is the F12 productionized operator UI (build-list §G): the FLEET
// view (box-centric per-model slot pips + per-account usage gauges with a ceiling
// line + rollover + live concurrent jobs), the BOARD view (epics/issues by stage
// incl. the Backlog + ⚠ Needs-you lanes, the yellow flowbee marker, a per-card
// gray->amber->red stage timer, full-width responsive collapsing to vertical, and
// a click->detail drawer that does NOT dim the board and supports click card->card
// with per-stage ENTERED/LEFT absolute times + the build-history timeline), plus
// the shared nav and the live SSE hook. Every asset is embedded via go:embed.
//
// The package is decoupled from the store via the Data interface so it renders off
// any read-model (the real store satisfies it) and is unit-testable with a fake.
// It is NOT a deterministic-core package: it reads a clock (for the stage timers)
// and the store read-models, and is wired by internal/api outside the core.
package web

import (
	"context"
	"embed"
	"html/template"
	"net/http"
	"time"

	"github.com/samhotchkiss/flowbee/internal/clock"
	"github.com/samhotchkiss/flowbee/internal/history"
	"github.com/samhotchkiss/flowbee/internal/store"
)

//go:embed assets/*.css assets/*.js
var assetsFS embed.FS

//go:embed templates/*.html
var tmplFS embed.FS

// Data is the read-model surface the UI renders off (build-list §G). The real
// *store.Store satisfies it; tests pass a fake. Every method is a pure read.
type Data interface {
	BoardCards(ctx context.Context, now time.Time) ([]store.BoardCard, error)
	JobDetail(ctx context.Context, jobID string, now time.Time) (store.JobDetail, error)
	Backlog(ctx context.Context) ([]store.BacklogItem, error)
	NeedsInput(ctx context.Context) ([]store.NeedsInputItem, error)
	NeedsHumanView(ctx context.Context) ([]store.NeedsHumanRow, error)
	Roster(ctx context.Context, now time.Time, staleAfter time.Duration) ([]store.RosterWorker, error)
	AllAccountUsage(ctx context.Context) ([]store.AccountUsageRow, error)
	RateLimit(ctx context.Context) (store.RateLimitGauge, error)
	AllJobCost(ctx context.Context) ([]store.FlowCostRow, error)
	AllAudit(ctx context.Context) ([]store.AuditRow, error)
	// Epic-lane read models. The legacy jobs/workers board is intentionally still
	// available at /board; these rows drive the session-per-epic operator surface.
	ListEpicRuns(ctx context.Context) ([]store.EpicRun, error)
	ListSeats(ctx context.Context) ([]store.Seat, error)
	ListSupervisors(ctx context.Context) ([]store.Supervisor, error)
	ListAccountWindows(ctx context.Context) ([]store.AccountWindow, error)
	ListOpenAttention(ctx context.Context, state string, kinds []string, repo string) ([]store.AttentionItem, error)
}

// UI serves the F12 dashboards. It holds the parsed templates + the data source +
// the clock the per-card stage timers read.
type UI struct {
	data    Data
	clock   clock.Clock
	staleHB time.Duration
	tmpl    *template.Template
	// amber/red are the per-card stage-timer thresholds (gray below amber, amber up
	// to red, red beyond). Operator-tunable; sensible defaults if zero.
	amber time.Duration
	red   time.Duration
}

// Config carries the UI knobs.
type Config struct {
	StaleHB    time.Duration // the roster stale-heartbeat threshold (mirrors api).
	StageAmber time.Duration // a card turns amber after this long in a stage.
	StageRed   time.Duration // a card turns red after this long in a stage.
}

// New builds the UI, parsing the embedded templates with the helper funcs.
func New(data Data, clk clock.Clock, cfg Config) *UI {
	amber, red := cfg.StageAmber, cfg.StageRed
	if amber <= 0 {
		amber = 10 * time.Minute
	}
	if red <= 0 {
		red = 30 * time.Minute
	}
	stale := cfg.StaleHB
	if stale <= 0 {
		stale = 90 * time.Second
	}
	u := &UI{data: data, clock: clk, staleHB: stale, amber: amber, red: red}
	u.tmpl = template.Must(template.New("web").Funcs(u.funcs()).ParseFS(tmplFS, "templates/*.html"))
	return u
}

// Mount registers the UI routes + the embedded asset handler on a mux. The epic
// fleet is the home page; the legacy job board remains at /board. /board/detail
// serves its drawer fragment.
func (u *UI) Mount(mux *http.ServeMux) {
	mux.Handle("GET /assets/", http.FileServer(http.FS(assetsFS)))
	mux.HandleFunc("GET /", u.epics)
	mux.HandleFunc("GET /epics", u.epics)
	mux.HandleFunc("GET /board", u.board)
	mux.HandleFunc("GET /board/detail", u.detail)
	mux.HandleFunc("GET /fleet", u.fleet)
	mux.HandleFunc("GET /dashboard", u.dashboard)
	mux.HandleFunc("GET /roster", u.roster)
}

// stageClass maps a stage age to the per-card timer class (gray -> amber -> red).
func (u *UI) stageClass(ageS int) string {
	age := time.Duration(ageS) * time.Second
	switch {
	case age >= u.red:
		return "red"
	case age >= u.amber:
		return "amber"
	default:
		return ""
	}
}

func (u *UI) funcs() template.FuncMap {
	return template.FuncMap{
		"timerClass": u.stageClass,
		"hms":        humanDuration,
		"abs":        absTime,
		"pct":        clampPct,
		"barClass":   barClass,
		"timeline":   func(c history.Card) []history.TimelineEntry { return c.Timeline },
		"pips":       pips,
		"ratio":      ratioPct,
		"sub":        func(a, b int) int { return a - b },
	}
}

// ratioPct renders part/whole as a clamped [0,100] integer percent for a gauge
// fill width (the dashboard's GitHub-budget meter). A non-positive whole yields 0
// so a missing budget reads as an empty gauge rather than dividing by zero.
func ratioPct(part, whole int) int {
	if whole <= 0 {
		return 0
	}
	return clampPct(part * 100 / whole)
}

// pips returns one bool per slot: true=busy, false=free, for the fleet slot-pip
// strip. busy is clamped to [0,total]; a zero total yields a single free pip so a
// box without an advertised slot count still shows a strip.
func pips(busy, total int) []bool {
	if total < 1 {
		total = 1
	}
	if busy < 0 {
		busy = 0
	}
	if busy > total {
		busy = total
	}
	out := make([]bool, total)
	for i := 0; i < busy; i++ {
		out[i] = true
	}
	return out
}

// humanDuration renders an age in seconds as a compact h/m/s timer string.
func humanDuration(seconds int) string {
	if seconds < 0 {
		seconds = 0
	}
	d := time.Duration(seconds) * time.Second
	switch {
	case d >= time.Hour:
		h := int(d / time.Hour)
		m := int((d % time.Hour) / time.Minute)
		return itoa(h) + "h" + pad2(m) + "m"
	case d >= time.Minute:
		m := int(d / time.Minute)
		s := int((d % time.Minute) / time.Second)
		return itoa(m) + "m" + pad2(s) + "s"
	default:
		return itoa(int(d/time.Second)) + "s"
	}
}

// absTime renders an absolute instant for the drawer's ENTERED/LEFT columns. The
// zero time renders as an em-dash (the span is still open / never left).
func absTime(t time.Time) string {
	if t.IsZero() {
		return "—"
	}
	return t.UTC().Format("2006-01-02 15:04:05")
}

// clampPct clamps a usage percentage to [0,100] for the gauge fill width.
func clampPct(p int) int {
	if p < 0 {
		return 0
	}
	if p > 100 {
		return 100
	}
	return p
}

// barClass colors a usage gauge fill: green below the ceiling, amber within 10pts
// of it, red at/over it.
func barClass(usage, ceiling int) string {
	switch {
	case usage >= ceiling:
		return "over"
	case usage >= ceiling-10:
		return "warn"
	default:
		return ""
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}

func pad2(n int) string {
	if n < 10 {
		return "0" + itoa(n)
	}
	return itoa(n)
}
