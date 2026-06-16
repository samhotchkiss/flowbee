package web

import (
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/samhotchkiss/flowbee/internal/store"
)

// page is the shared template payload: the active nav tab + the view-specific
// body data. The base template renders the nav + the live SSE hook around it.
type page struct {
	Active  string // "board" | "fleet" | "dashboard" | "roster"
	Title   string
	Partial bool // true => render only the live body fragment (SSE refresh)
	Board   *boardView
	Fleet   *fleetView
	Dash    *dashView
	Roster  []store.RosterWorker
}

// ── BOARD ──

// boardLane is one column of the board (a stage, or the Backlog / ⚠ Needs-you
// special lanes). Cards within a lane keep board recency order.
type boardLane struct {
	Key      string
	Title    string
	Class    string // "" | "needsyou" | "backlog"
	Cards    []boardCard
}

// boardCard is a card's render view: the rich store card plus the resolved
// per-card timer class + a yellow flowbee marker flag (every actively-tracked
// card carries it; build-list §G "yellow flowbee marker").
type boardCard struct {
	store.BoardCard
	TimerClass string
	Flowbee    bool // the yellow flowbee umbrella marker
}

type boardView struct {
	Lanes []boardLane
}

// stageLanes is the ordered set of pipeline stage lanes (the flow left-to-right),
// keyed by the job STATE that occupies them. Backlog + Needs-you are appended as
// the two special lanes.
var stageLanes = []struct{ Key, Title string }{
	{"spec_authoring", "Spec"},
	{"spec_review", "Issue-review"},
	{"ready", "Ready"},
	{"building", "Build"},
	{"review_pending", "Review queue"},
	{"code_review", "Build-review"},
	{"resolving_conflict", "Conflict"},
	{"mergeable", "Mergeable"},
	{"merging", "Merging"},
	{"merge_handoff", "Merge handoff"},
	{"done", "Done"},
}

// leasedAlias folds the transient `leased` state into the build lane and any
// escalation/blocked states into the needs-you lane so every live card lands
// somewhere visible.
func laneKeyFor(state string) string {
	switch state {
	case "leased":
		return "building"
	case "needs_human", "needs_design", "failed", "blocked":
		return "needsyou"
	case "backlog":
		return "backlog"
	default:
		return state
	}
}

func (u *UI) board(w http.ResponseWriter, r *http.Request) {
	now := u.clock.Now()
	cards, err := u.data.BoardCards(r.Context(), now)
	if err != nil {
		http.Error(w, "board error", http.StatusInternalServerError)
		return
	}
	bl, err := u.data.Backlog(r.Context())
	if err != nil {
		http.Error(w, "board error", http.StatusInternalServerError)
		return
	}
	ni, err := u.data.NeedsInput(r.Context())
	if err != nil {
		http.Error(w, "board error", http.StatusInternalServerError)
		return
	}

	lanes := map[string]*boardLane{}
	order := []string{}
	add := func(key, title, class string) *boardLane {
		l := &boardLane{Key: key, Title: title, Class: class}
		lanes[key] = l
		order = append(order, key)
		return l
	}
	// the ⚠ Needs-you and Backlog lanes lead; then the pipeline stages.
	add("needsyou", "⚠ Needs-you", "needsyou")
	add("backlog", "Backlog", "backlog")
	for _, s := range stageLanes {
		add(s.Key, s.Title, "")
	}

	for _, c := range cards {
		key := laneKeyFor(c.State)
		l := lanes[key]
		if l == nil {
			l = lanes["ready"] // unknown live state: park it in Ready rather than drop it.
		}
		l.Cards = append(l.Cards, boardCard{
			BoardCard:  c,
			TimerClass: u.stageClass(c.StageAgeS),
			Flowbee:    true, // the yellow flowbee umbrella marker rides every tracked card.
		})
	}
	// the Needs-you lane is enriched with the needs-input reasons (design forks):
	// surface a job that is in needs_design but whose reason the board card lacks.
	enrichNeedsYou(lanes["needsyou"], ni)
	// the Backlog lane reflects the dedicated backlog read-model (the canonical
	// "needs full spec" flag) so it matches /v1/backlog exactly.
	syncBacklog(lanes["backlog"], bl, now)

	var view boardView
	for _, k := range order {
		view.Lanes = append(view.Lanes, *lanes[k])
	}
	u.renderPage(w, r, page{Active: "board", Title: "Board", Board: &view})
}

// enrichNeedsYou stamps the design-fork reason onto needs-you cards that have one.
func enrichNeedsYou(lane *boardLane, items []store.NeedsInputItem) {
	if lane == nil {
		return
	}
	reason := map[string]string{}
	for _, it := range items {
		reason[it.JobID] = it.Reason
	}
	for i := range lane.Cards {
		if r := reason[lane.Cards[i].JobID]; r != "" && lane.Cards[i].Title == lane.Cards[i].JobID {
			lane.Cards[i].Title = r
		}
	}
}

// syncBacklog ensures the Backlog lane reflects the canonical backlog read-model
// (it is the authoritative "needs full spec" source). The board-card pass already
// placed backlog cards; this overlays the needs-full-spec flag.
func syncBacklog(lane *boardLane, items []store.BacklogItem, now time.Time) {
	if lane == nil {
		return
	}
	needs := map[string]bool{}
	for _, it := range items {
		needs[it.JobID] = it.NeedsFullSpec
	}
	for i := range lane.Cards {
		lane.Cards[i].NeedsFullSpec = needs[lane.Cards[i].JobID]
	}
}

// ── DRAWER ──

// drawerView is the detail-drawer payload (build-list §G): the job's card facts,
// its per-stage ENTERED/LEFT absolute times, and the build-history timeline. The
// Related list lets the drawer click card->card (epic siblings, if any).
type drawerView struct {
	Detail store.JobDetail
}

func (u *UI) detail(w http.ResponseWriter, r *http.Request) {
	jobID := r.URL.Query().Get("job")
	if jobID == "" {
		http.Error(w, "missing job", http.StatusBadRequest)
		return
	}
	d, err := u.data.JobDetail(r.Context(), jobID, u.clock.Now())
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := u.tmpl.ExecuteTemplate(w, "drawer.html", drawerView{Detail: d}); err != nil {
		http.Error(w, "render error", http.StatusInternalServerError)
	}
}

// ── FLEET ──

// fleetBox is one box/machine in the fleet view (box-centric): its per-model slot
// pips, the accounts feeding those models with their usage gauges, and the live
// concurrent jobs it holds.
type fleetBox struct {
	WorkerID string
	Identity string
	Host     string
	Arch     string
	OS       string
	StaleHB  bool
	Models   []fleetModel
	LiveJobs []string
}

// fleetModel is one model family's slot pips + accounts on a box. Busy/Total are
// the live concurrency vs the advertised slots (the pips).
type fleetModel struct {
	Family   string
	Busy     int
	Total    int
	Accounts []store.AccountUsageRow
}

type fleetView struct {
	Boxes []fleetBox
}

func (u *UI) fleet(w http.ResponseWriter, r *http.Request) {
	now := u.clock.Now()
	roster, err := u.data.Roster(r.Context(), now, u.staleHB)
	if err != nil {
		http.Error(w, "fleet error", http.StatusInternalServerError)
		return
	}
	accounts, err := u.data.AllAccountUsage(r.Context())
	if err != nil {
		http.Error(w, "fleet error", http.StatusInternalServerError)
		return
	}
	view := buildFleet(roster, accounts)
	u.renderPage(w, r, page{Active: "fleet", Title: "Fleet", Fleet: &view})
}

// buildFleet folds the roster + per-account usage into the box-centric fleet view.
// Each box advertises identity/host; its live lease (if any) is its concurrent
// job. Accounts are grouped by model family and shared across boxes on the same
// login (build-list §C), so every box shows every model's accounts as its gauge.
func buildFleet(roster []store.RosterWorker, accounts []store.AccountUsageRow) fleetView {
	byFamily := map[string][]store.AccountUsageRow{}
	families := []string{}
	for _, a := range accounts {
		if _, ok := byFamily[a.ModelFamily]; !ok {
			families = append(families, a.ModelFamily)
		}
		byFamily[a.ModelFamily] = append(byFamily[a.ModelFamily], a)
	}
	sort.Strings(families)

	var v fleetView
	for _, w := range roster {
		box := fleetBox{
			WorkerID: w.WorkerID, Identity: w.Identity, Host: w.Host,
			Arch: w.Arch, OS: w.OS, StaleHB: w.StaleHB,
		}
		if w.ActiveJob != "" {
			box.LiveJobs = append(box.LiveJobs, w.ActiveJob)
		}
		// a box's model families come from its attested role/family caps if present,
		// else from every enrolled account family (shared logins). Show slot pips per
		// family; the busy count is its live concurrent jobs (1 if it holds a lease).
		fams := boxFamilies(w, families)
		for _, fam := range fams {
			busy := 0
			if w.ActiveJob != "" {
				busy = 1
			}
			total := slotTotal(w, fam)
			box.Models = append(box.Models, fleetModel{
				Family: fam, Busy: busy, Total: total, Accounts: byFamily[fam],
			})
		}
		v.Boxes = append(v.Boxes, box)
	}
	return v
}

// boxFamilies derives the model families a box presents. If the worker attested
// model_family:* caps, use those; otherwise fall back to every enrolled family so
// the box still shows the shared-login account gauges.
func boxFamilies(w store.RosterWorker, all []string) []string {
	var fams []string
	for _, cap := range w.Attested {
		if strings.HasPrefix(cap, "model_family:") {
			fams = append(fams, strings.TrimPrefix(cap, "model_family:"))
		}
	}
	if len(fams) == 0 {
		return all
	}
	sort.Strings(fams)
	return fams
}

// slotTotal is the advertised per-model slot count for the pips. The roster does
// not carry the slot table, so default to a 3-pip strip (the shipped default
// per-model concurrency) — the busy pip count is the live signal.
func slotTotal(_ store.RosterWorker, _ string) int { return 3 }

// ── DASHBOARD ──

type dashView struct {
	Budget     store.RateLimitGauge
	Cost       []store.FlowCostRow
	Audit      []store.AuditRow
	NeedsHuman []store.NeedsHumanRow
	Roster     []store.RosterWorker
}

func (u *UI) dashboard(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	now := u.clock.Now()
	budget, err := u.data.RateLimit(ctx)
	if err != nil {
		http.Error(w, "dashboard error", http.StatusInternalServerError)
		return
	}
	cost, err := u.data.AllJobCost(ctx)
	if err != nil {
		http.Error(w, "dashboard error", http.StatusInternalServerError)
		return
	}
	audit, err := u.data.AllAudit(ctx)
	if err != nil {
		http.Error(w, "dashboard error", http.StatusInternalServerError)
		return
	}
	nh, err := u.data.NeedsHumanView(ctx)
	if err != nil {
		http.Error(w, "dashboard error", http.StatusInternalServerError)
		return
	}
	roster, err := u.data.Roster(ctx, now, u.staleHB)
	if err != nil {
		http.Error(w, "dashboard error", http.StatusInternalServerError)
		return
	}
	u.renderPage(w, r, page{Active: "dashboard", Title: "Dashboard", Dash: &dashView{
		Budget: budget, Cost: cost, Audit: audit, NeedsHuman: nh, Roster: roster,
	}})
}

func (u *UI) roster(w http.ResponseWriter, r *http.Request) {
	roster, err := u.data.Roster(r.Context(), u.clock.Now(), u.staleHB)
	if err != nil {
		http.Error(w, "roster error", http.StatusInternalServerError)
		return
	}
	u.renderPage(w, r, page{Active: "roster", Title: "Roster", Roster: roster})
}

// renderPage renders either the full page (nav + body) or — when ?partial=1 — only
// the live body fragment the SSE hook swaps in place (so the open drawer survives a
// refresh; the board is never dimmed/reloaded).
func (u *UI) renderPage(w http.ResponseWriter, r *http.Request, p page) {
	p.Partial = r.URL.Query().Get("partial") == "1"
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	name := "page.html"
	if p.Partial {
		name = "body.html"
	}
	if err := u.tmpl.ExecuteTemplate(w, name, p); err != nil {
		http.Error(w, "render error", http.StatusInternalServerError)
	}
}
