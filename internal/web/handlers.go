package web

import (
	"fmt"
	"hash/fnv"
	"html/template"
	"net/http"
	"sort"
	"strings"

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

// boardBucket is one of the five top-level board buckets (Needs-you, Spec, Build,
// Review, Merge). It holds ordered sub-status groups, each folding one or more job
// states under a single label, so the board reads as five columns instead of ~13.
type boardBucket struct {
	Key      string
	Title    string
	Class    string
	Count    int // total live cards across this bucket's statuses
	Statuses []boardStatus
}

// boardStatus is a sub-status group within a bucket (e.g. "building" under Build).
type boardStatus struct {
	Key   string
	Title string
	Cards []boardCard
}

// boardCard is a card's render view: the rich store card plus the resolved per-card
// timer class, the flowbee marker, and the per-project mark (emoji + left-stripe
// color) so an operator can tell at a glance which project a task belongs to.
type boardCard struct {
	store.BoardCard
	TimerClass   string
	Flowbee      bool         // the yellow flowbee umbrella marker
	ProjectEmoji string       // per-project glyph (replaces the generic bee on the card)
	ProjectColor template.CSS // per-project left-stripe color (derived from the repo)
}

type boardView struct {
	Buckets []boardBucket
	Done    []boardCard // completed jobs, listed below the buckets (newest first)
	// Repos is the set of distinct non-empty repo scopes present on the board (F9
	// multi-repo registry), rendered as the filter chips. SelectedRepo is the active
	// ?repo=<id> filter ("" => the "All" default, every repo shown).
	Repos        []string
	SelectedRepo string
}

// statusDef / bucketDef are the static board layout: five buckets, each an ordered
// list of sub-statuses, each folding one or more job states (first state is the
// canonical one). Any live state not listed here is parked under Build/ready.
type statusDef struct {
	Key, Title string
	States     []string
}
type bucketDef struct {
	Key, Title, Class string
	Statuses          []statusDef
}

var boardBuckets = []bucketDef{
	{"needsyou", "⚠ Needs-you", "needsyou", []statusDef{
		{"needs_human", "needs human", []string{"needs_human", "needs_design", "failed", "blocked"}},
		{"merge_handoff", "merge handoff", []string{"merge_handoff"}},
		{"needs_input", "needs input", []string{"needs_input"}},
	}},
	{"spec", "Spec", "spec", []statusDef{
		{"backlog", "backlog", []string{"backlog"}},
		{"spec_authoring", "authoring", []string{"spec_authoring"}},
		{"spec_review", "issue-review", []string{"spec_review"}},
	}},
	{"build", "Build", "build", []statusDef{
		{"ready", "ready", []string{"ready"}},
		{"building", "building", []string{"building", "leased"}},
	}},
	{"review", "Review", "review", []statusDef{
		{"review_pending", "review queue", []string{"review_pending"}},
		{"code_review", "in-review", []string{"code_review"}},
	}},
	{"merge", "Merge", "merge", []statusDef{
		{"resolving_conflict", "conflict", []string{"resolving_conflict"}},
		{"mergeable", "mergeable", []string{"mergeable"}},
		{"merging", "merging", []string{"merging"}},
	}},
}

// bsLoc locates a state's (bucket, status) slot; stateLoc is built once from
// boardBuckets so the per-card grouping is a single map lookup.
type bsLoc struct{ b, s int }

var stateLoc = func() map[string]bsLoc {
	m := map[string]bsLoc{}
	for bi, b := range boardBuckets {
		for si, st := range b.Statuses {
			for _, state := range st.States {
				m[state] = bsLoc{bi, si}
			}
		}
	}
	return m
}()

// ── per-project marks ──────────────────────────────────────────────────────
// Each project (repo scope) gets a stable emoji + left-stripe color so cards are
// visually groupable by project. A couple of favorites are pinned; everything else
// hashes deterministically into the palettes, so a new project gets a consistent
// mark with no config.
var pinnedProjectEmoji = map[string]string{"flowbee": "🐝", "russ": "🐻"}

var projectEmojiPalette = []string{
	"🦊", "🐙", "🦉", "🐳", "🦄", "🐢", "🦋", "🦅", "🐡", "🦎", "🦀", "🐝", "🐻",
}

func hashStr(s string) uint32 {
	h := fnv.New32a()
	_, _ = h.Write([]byte(s))
	return h.Sum32()
}

func projectEmoji(repo string) string {
	if repo == "" {
		return "🐝" // legacy single-repo default
	}
	if e, ok := pinnedProjectEmoji[repo]; ok {
		return e
	}
	return projectEmojiPalette[hashStr(repo)%uint32(len(projectEmojiPalette))]
}

// projectColor maps a repo to a stable hue for the card's left stripe. Returned as
// template.CSS (trusted: it is derived from the repo name, never user free-text in a
// CSS-injection sense) so html/template emits it verbatim into the style attribute.
func projectColor(repo string) template.CSS {
	if repo == "" {
		return template.CSS("hsl(48 90% 60%)") // bee yellow default
	}
	return template.CSS(fmt.Sprintf("hsl(%d 58%% 56%%)", int(hashStr(repo)%360)))
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

	// the distinct repo scopes are gathered across ALL cards (before filtering) so
	// every available repo gets a chip regardless of the current selection. The
	// ?repo=<id> filter then keeps only the matching cards; "" means the "All"
	// default and shows everything.
	repos := distinctRepos(cards)
	selRepo := r.URL.Query().Get("repo")
	if selRepo != "" {
		cards = filterByRepo(cards, selRepo)
	}

	// build the five buckets fresh from the static layout, then drop each card into
	// its (bucket, status) slot. `done` is collected separately for the list below.
	buckets := make([]boardBucket, len(boardBuckets))
	for i, b := range boardBuckets {
		bk := boardBucket{Key: b.Key, Title: b.Title, Class: b.Class}
		for _, st := range b.Statuses {
			bk.Statuses = append(bk.Statuses, boardStatus{Key: st.Key, Title: st.Title})
		}
		buckets[i] = bk
	}
	var done []boardCard
	for _, c := range cards {
		card := boardCard{
			BoardCard:    c,
			TimerClass:   u.stageClass(c.StageAgeS),
			Flowbee:      true,
			ProjectEmoji: projectEmoji(c.Repo),
			ProjectColor: projectColor(c.Repo),
		}
		if c.State == "done" {
			done = append(done, card)
			continue
		}
		loc, ok := stateLoc[c.State]
		if !ok {
			loc = stateLoc["ready"] // unknown live state: park in Build/ready, never drop.
		}
		buckets[loc.b].Statuses[loc.s].Cards = append(buckets[loc.b].Statuses[loc.s].Cards, card)
		buckets[loc.b].Count++
	}

	// enrich live cards: the needs-input reason (design forks) replaces a bare-id
	// title, and the canonical "needs full spec" flag is overlaid from the backlog
	// read-model so the board matches /v1/backlog exactly.
	reason := map[string]string{}
	for _, it := range ni {
		reason[it.JobID] = it.Reason
	}
	needs := map[string]bool{}
	for _, it := range bl {
		needs[it.JobID] = it.NeedsFullSpec
	}
	for bi := range buckets {
		for si := range buckets[bi].Statuses {
			for ci := range buckets[bi].Statuses[si].Cards {
				c := &buckets[bi].Statuses[si].Cards[ci]
				if r := reason[c.JobID]; r != "" && c.Title == c.JobID {
					c.Title = r
				}
				if v, ok := needs[c.JobID]; ok {
					c.NeedsFullSpec = v
				}
			}
		}
	}

	view := boardView{Buckets: buckets, Done: done, Repos: repos, SelectedRepo: selRepo}
	u.renderPage(w, r, page{Active: "board", Title: "Board", Board: &view})
}

// distinctRepos returns the sorted set of distinct non-empty repo scopes present
// on the board cards (the F9 repo filter chips). Legacy single-repo jobs carry an
// empty repo and are only reachable via the "All" default, so they contribute no
// chip; when every card is empty-repo the chip row is effectively a no-op.
func distinctRepos(cards []store.BoardCard) []string {
	seen := map[string]bool{}
	var out []string
	for _, c := range cards {
		if c.Repo != "" && !seen[c.Repo] {
			seen[c.Repo] = true
			out = append(out, c.Repo)
		}
	}
	sort.Strings(out)
	return out
}

// filterByRepo keeps only the cards whose repo scope matches repo (the ?repo=<id>
// server-side board filter). An empty repo never matches here (it is the "All"
// path), so selecting a repo hides the legacy single-repo cards.
func filterByRepo(cards []store.BoardCard, repo string) []store.BoardCard {
	out := cards[:0:0]
	for _, c := range cards {
		if c.Repo == repo {
			out = append(out, c)
		}
	}
	return out
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
