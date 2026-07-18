package web

import (
	"fmt"
	"hash/fnv"
	"html/template"
	"net/http"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/samhotchkiss/flowbee/internal/store"
)

// page is the shared template payload: the active nav tab + the view-specific
// body data. The base template renders the nav + the live SSE hook around it.
type page struct {
	Active  string // "epics" | "board" | "fleet" | "dashboard" | "roster"
	Title   string
	Partial bool // true => render only the live body fragment (SSE refresh)
	Epics   *epicsView
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

// ── EPICS ──

// The epic-lane dashboard is deliberately projected into display-ready rows here.
// Templates receive closed CSS classes and already-formatted labels; raw provider,
// trust, pane, and attention tokens never become class names.
type usageBarView struct {
	Known bool
	Pct   int
	Label string
	Class string
}

type epicAccountRow struct {
	Key, ShortKey       string
	Email               string
	Family, FamilyClass string
	TrustLabel          string
	TrustClass          string
	Session, Weekly     usageBarView
	ResetLabel          string
	ActiveEpics         int
	SeatNames           string
	Critical            bool
}

type epicSeatRow struct {
	ID, Name            string
	Box                 string
	Family, FamilyClass string
	AccountKey          string
	HealthLabel         string
	HealthClass         string
	UsageLabel          string
	SeatLoad            int
	HostLoad            int
	MaxConcurrent       int
}

type epicCard struct {
	ID, Title, Subtitle string
	State               string
	StatusLabel         string
	StatusClass         string
	Family, FamilyClass string
	SeatName, Host      string
	Branch              string
	CurrentStep         int
	StepsTotal          int
	StepLabel           string
	Progress            int
	PaneLabel           string
	PaneClass           string
	ContextLabel        string
	AgeLabel            string
	AgeClass            string
	AttentionDetail     string
	SortAt              time.Time
}

type epicAttentionRow struct {
	ID, EpicID, EpicTitle string
	Kind, Detail          string
	State                 string
	Class                 string
	Priority              int
	Occurrences           int
}

type epicMasterView struct {
	Registered bool
	Label      string
	State      string
	StateClass string
	Epoch      int
	Kind       string
	Box        string
	Heartbeat  string
}

type epicCapacityAlert struct {
	Email, Family         string
	WindowLabel, PctLabel string
	WeeklyLabel           string
	Seats                 string
	ResetLabel            string
	HealthyEmail          string
	HealthyPct            string
}

type epicStatsView struct {
	Completed, CompletedToday int
	Active, Review, CIRed     int
	Seats, Families           int
	NeedsYou                  int
}

type epicsView struct {
	SnapshotAt, SnapshotAge string
	Stats                   epicStatsView
	Master                  epicMasterView
	Alert                   *epicCapacityAlert
	Attention               []epicAttentionRow
	Accounts                []epicAccountRow
	Seats                   []epicSeatRow
	Active                  []epicCard
	Review                  []epicCard
	Completed               []epicCard
}

func (u *UI) epics(w http.ResponseWriter, r *http.Request) {
	// "GET /" is a ServeMux subtree pattern. Refuse unknown paths instead of
	// accidentally turning /v1/typo into a 200 HTML dashboard.
	if r.URL.Path != "/" && r.URL.Path != "/epics" {
		http.NotFound(w, r)
		return
	}
	ctx := r.Context()
	epics, err := u.data.ListEpicRuns(ctx)
	if err != nil {
		http.Error(w, "epics error", http.StatusInternalServerError)
		return
	}
	seats, err := u.data.ListSeats(ctx)
	if err != nil {
		http.Error(w, "epics error", http.StatusInternalServerError)
		return
	}
	supervisors, err := u.data.ListSupervisors(ctx)
	if err != nil {
		http.Error(w, "epics error", http.StatusInternalServerError)
		return
	}
	windows, err := u.data.ListAccountWindows(ctx)
	if err != nil {
		http.Error(w, "epics error", http.StatusInternalServerError)
		return
	}
	attention, err := u.data.ListOpenAttention(ctx, "", nil, "")
	if err != nil {
		http.Error(w, "epics error", http.StatusInternalServerError)
		return
	}

	view := u.buildEpics(epics, seats, supervisors, windows, attention, u.clock.Now())
	u.renderPage(w, r, page{Active: "epics", Title: "Fleet", Epics: &view})
}

func (u *UI) buildEpics(epics []store.EpicRun, seats []store.Seat, supervisors []store.Supervisor, windows []store.AccountWindow, attention []store.AttentionItem, now time.Time) epicsView {
	v := epicsView{SnapshotAt: now.Format("Jan 02, 03:04 PM")}

	latest := time.Time{}
	considerLatest := func(raw string) {
		if t := store.ParseTimeOrZero(raw); t.After(latest) {
			latest = t
		}
	}

	activeByBox := map[string]int{}
	activeBySeat := map[string]int{}
	activeByAccount := map[string]int{}
	for _, e := range epics {
		considerLatest(e.UpdatedAt)
		if isActiveEpic(e.State) {
			activeByBox[e.Host]++
			if e.SeatID != "" {
				activeBySeat[e.SeatID]++
			}
			if e.AccountKey != "" {
				activeByAccount[e.AccountKey]++
			}
		}
	}

	// Friendly seat aliases are stable within each family and shared by account,
	// seat, and epic cards (claude1/codex1 rather than a composite database key).
	seatAliases := map[string]string{}
	familyOrdinal := map[string]int{}
	for _, s := range seats {
		fam := normalizeFamily(s.AgentFamily)
		familyOrdinal[fam]++
		seatAliases[s.ID] = friendlySeatName(s, fam, familyOrdinal[fam])
	}

	attentionByEpic := map[string][]store.AttentionItem{}
	for _, item := range attention {
		attentionByEpic[item.EpicID] = append(attentionByEpic[item.EpicID], item)
		v.Attention = append(v.Attention, buildAttentionRow(item, epics))
		considerLatest(item.UpdatedAt)
	}

	windowByKey := map[string]store.AccountWindow{}
	for _, aw := range windows {
		windowByKey[aw.AccountKey] = aw
		considerLatest(aw.ReportedAt)
	}

	seatNamesByAccount := map[string][]string{}
	families := map[string]bool{}
	for _, s := range seats {
		fam := normalizeFamily(s.AgentFamily)
		families[fam] = true
		name := seatAliases[s.ID]
		if s.AccountKey != "" {
			seatNamesByAccount[s.AccountKey] = append(seatNamesByAccount[s.AccountKey], name)
		}
		cap := s.MaxConcurrent
		if cap < 1 {
			cap = 1
		}
		row := epicSeatRow{
			ID: s.ID, Name: name, Box: boxLabel(s.Box),
			Family: familyLabel(fam), FamilyClass: familyClass(fam),
			AccountKey: s.AccountKey, SeatLoad: activeBySeat[s.ID], HostLoad: activeByBox[s.Box], MaxConcurrent: cap,
		}
		row.HealthLabel, row.HealthClass = seatHealth(s, windowByKey[s.AccountKey])
		if aw, ok := windowByKey[s.AccountKey]; ok {
			row.UsageLabel = preferredUsageLabel(aw)
		} else {
			row.UsageLabel = "—"
		}
		v.Seats = append(v.Seats, row)
		considerLatest(s.UpdatedAt)
	}

	for _, aw := range windows {
		fam := normalizeFamily(firstNonEmpty(aw.ModelFamily, aw.Provider))
		critical := accountCritical(aw)
		row := epicAccountRow{
			Key: aw.AccountKey, ShortKey: shortID(aw.AccountKey), Email: firstNonEmpty(aw.Email, aw.AccountKey),
			Family: familyLabel(fam), FamilyClass: familyClass(fam),
			Session: buildUsageBar(aw.SessionPct, aw, false), Weekly: buildUsageBar(aw.WeeklyPct, aw, true),
			ActiveEpics: activeByAccount[aw.AccountKey], SeatNames: strings.Join(seatNamesByAccount[aw.AccountKey], ", "),
			Critical: critical,
		}
		row.TrustLabel, row.TrustClass = accountTrust(aw)
		reset := aw.ResetsSessionAt
		if aw.WeeklyPct > aw.SessionPct {
			reset = aw.ResetsWeeklyAt
		}
		row.ResetLabel = resetLabel(reset, now)
		v.Accounts = append(v.Accounts, row)
	}
	sort.SliceStable(v.Accounts, func(i, j int) bool {
		if v.Accounts[i].Critical != v.Accounts[j].Critical {
			return v.Accounts[i].Critical
		}
		if v.Accounts[i].Family != v.Accounts[j].Family {
			return v.Accounts[i].Family < v.Accounts[j].Family
		}
		return v.Accounts[i].Email < v.Accounts[j].Email
	})
	sort.SliceStable(v.Seats, func(i, j int) bool {
		if v.Seats[i].Family != v.Seats[j].Family {
			return v.Seats[i].Family < v.Seats[j].Family
		}
		return v.Seats[i].Name < v.Seats[j].Name
	})

	terminalCount, doneToday, ciRed := 0, 0, 0
	dayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	for _, e := range epics {
		items := attentionByEpic[e.ID]
		card := u.buildEpicCard(e, items, seatAliases, now)
		terminal := isTerminalEpic(e.State)
		if terminal {
			terminalCount++
			if finished := store.ParseTimeOrZero(e.FinishedAt); !finished.IsZero() && !finished.Before(dayStart) {
				doneToday++
			}
		}
		for _, item := range items {
			if strings.Contains(strings.ToLower(item.Kind), "ci_red") {
				ciRed++
				break
			}
		}
		switch {
		case len(items) > 0 || e.State == "blocked" || strings.Contains(strings.ToLower(e.StatusStateDetail), "review"):
			v.Review = append(v.Review, card)
		case terminal:
			v.Completed = append(v.Completed, card)
		default:
			v.Active = append(v.Active, card)
		}
	}
	sort.SliceStable(v.Active, func(i, j int) bool { return v.Active[i].SortAt.After(v.Active[j].SortAt) })
	sort.SliceStable(v.Review, func(i, j int) bool {
		if v.Review[i].StatusClass != v.Review[j].StatusClass {
			return v.Review[i].StatusClass == "critical"
		}
		return v.Review[i].SortAt.After(v.Review[j].SortAt)
	})
	sort.SliceStable(v.Completed, func(i, j int) bool { return v.Completed[i].SortAt.After(v.Completed[j].SortAt) })

	v.Stats = epicStatsView{
		Completed: terminalCount, CompletedToday: doneToday,
		Active: len(v.Active), Review: len(v.Review), CIRed: ciRed,
		Seats: len(seats), Families: len(families), NeedsYou: len(attention),
	}
	v.Master = buildMaster(supervisors, now)
	for _, sup := range supervisors {
		considerLatest(sup.UpdatedAt)
	}
	v.Alert = buildCapacityAlert(windows, seats, seatAliases, now)
	if latest.IsZero() {
		v.SnapshotAge = "awaiting first reading"
	} else {
		v.SnapshotAge = humanAgo(latest, now)
	}
	return v
}

func (u *UI) buildEpicCard(e store.EpicRun, items []store.AttentionItem, seatAliases map[string]string, now time.Time) epicCard {
	fam := normalizeFamily(firstNonEmpty(e.BuilderModelFamily, e.Agent))
	sortAt := store.ParseTimeOrZero(firstNonEmpty(e.FinishedAt, e.UpdatedAt, e.CreatedAt))
	progress := 0
	stepLabel := "—"
	if e.StatusStepsTotal > 0 {
		progress = clampPct(e.StatusCurrentStep * 100 / e.StatusStepsTotal)
		stepLabel = fmt.Sprintf("%d/%d", e.StatusCurrentStep, e.StatusStepsTotal)
	}
	status, statusClass := epicStatus(e, items)
	pane, paneClass := paneStatus(e.PaneState, e.State)
	updated := store.ParseTimeOrZero(firstNonEmpty(e.StatusUpdatedAt, e.UpdatedAt))
	ageS := 0
	if !updated.IsZero() && now.After(updated) {
		ageS = int(now.Sub(updated) / time.Second)
	}
	contextLabel := ""
	if e.ContextPct >= 0 {
		contextLabel = fmt.Sprintf("%.0f%% ctx", e.ContextPct)
	}
	detail := ""
	if len(items) > 0 {
		detail = firstNonEmpty(items[0].Detail, humanizeToken(items[0].Kind))
	} else if e.StatusBlockers != "" {
		detail = e.StatusBlockers
	}
	return epicCard{
		ID: e.ID, Title: firstNonEmpty(e.Title, e.ID), Subtitle: epicSubtitle(e),
		State: e.State, StatusLabel: status, StatusClass: statusClass,
		Family: familyLabel(fam), FamilyClass: familyClass(fam),
		SeatName: firstNonEmpty(seatAliases[e.SeatID], shortID(e.SeatID)), Host: boxLabel(e.Host),
		Branch: e.Branch, CurrentStep: e.StatusCurrentStep, StepsTotal: e.StatusStepsTotal,
		StepLabel: stepLabel, Progress: progress, PaneLabel: pane, PaneClass: paneClass,
		ContextLabel: contextLabel, AgeLabel: humanAgo(updated, now), AgeClass: u.stageClass(ageS),
		AttentionDetail: detail, SortAt: sortAt,
	}
}

func buildAttentionRow(item store.AttentionItem, epics []store.EpicRun) epicAttentionRow {
	title := item.EpicID
	for _, e := range epics {
		if e.ID == item.EpicID {
			title = firstNonEmpty(e.Title, e.ID)
			break
		}
	}
	class := "warning"
	if item.Blocking || item.Priority <= 20 {
		class = "critical"
	}
	return epicAttentionRow{
		ID: item.ID, EpicID: item.EpicID, EpicTitle: title,
		Kind: humanizeToken(item.Kind), Detail: firstNonEmpty(item.Detail, "Master review requested"),
		State: item.State, Class: class, Priority: item.Priority, Occurrences: item.Occurrences,
	}
}

func buildMaster(supervisors []store.Supervisor, now time.Time) epicMasterView {
	var picked *store.Supervisor
	for i := range supervisors {
		s := &supervisors[i]
		if picked == nil || (s.State == "active" && picked.State != "active") ||
			(s.State == picked.State && store.ParseTimeOrZero(s.LastHeartbeatAt).After(store.ParseTimeOrZero(picked.LastHeartbeatAt))) {
			picked = s
		}
	}
	if picked == nil {
		return epicMasterView{State: "offline", StateClass: "muted", Heartbeat: "no master registered", Box: "—"}
	}
	stateClass := "muted"
	if picked.State == "active" {
		stateClass = "active"
	} else if picked.State == "stale" || picked.State == "revoked" {
		stateClass = "critical"
	}
	return epicMasterView{
		Registered: true, Label: picked.Label, State: picked.State, StateClass: stateClass,
		Epoch: picked.Epoch, Kind: picked.Kind, Box: boxLabel(picked.Box),
		Heartbeat: humanAgo(store.ParseTimeOrZero(picked.LastHeartbeatAt), now),
	}
}

func buildCapacityAlert(windows []store.AccountWindow, seats []store.Seat, aliases map[string]string, now time.Time) *epicCapacityAlert {
	var capped *store.AccountWindow
	best := -1.0
	for i := range windows {
		aw := &windows[i]
		score := maxFloat(aw.SessionPct, aw.WeeklyPct)
		if !accountCritical(*aw) || score < best {
			continue
		}
		capped, best = aw, score
	}
	if capped == nil {
		return nil
	}
	windowLabel, pct, reset := "weekly", capped.WeeklyPct, capped.ResetsWeeklyAt
	if capped.SessionPct >= capped.WeeklyPct {
		windowLabel, pct, reset = "5-hour session", capped.SessionPct, capped.ResetsSessionAt
	}
	var names []string
	for _, s := range seats {
		if s.AccountKey == capped.AccountKey {
			names = append(names, aliases[s.ID])
		}
	}
	alert := &epicCapacityAlert{
		Email: firstNonEmpty(capped.Email, capped.AccountKey), Family: familyLabel(normalizeFamily(capped.ModelFamily)),
		WindowLabel: windowLabel, PctLabel: percentLabel(pct), WeeklyLabel: percentLabel(capped.WeeklyPct),
		Seats: strings.Join(names, ", "), ResetLabel: resetLabel(reset, now),
	}
	for i := range windows {
		aw := windows[i]
		if aw.AccountKey == capped.AccountKey || normalizeFamily(aw.ModelFamily) != normalizeFamily(capped.ModelFamily) ||
			accountCritical(aw) || !aw.Routable() {
			continue
		}
		alert.HealthyEmail = firstNonEmpty(aw.Email, aw.AccountKey)
		alert.HealthyPct = preferredUsageLabel(aw)
		break
	}
	return alert
}

func buildUsageBar(p float64, aw store.AccountWindow, weekly bool) usageBarView {
	if p < 0 {
		return usageBarView{Label: "—", Class: "unknown"}
	}
	class := "healthy"
	if aw.ProbeStale {
		class = "stale"
	} else if (weekly && aw.Severity == "critical") || p >= 95 {
		class = "critical"
	} else if p >= 80 {
		class = "warning"
	}
	return usageBarView{Known: true, Pct: pctToInt(p), Label: percentLabel(p), Class: class}
}

func accountTrust(aw store.AccountWindow) (string, string) {
	if aw.ProbeStale || aw.TrustState == "stale" {
		return "stale", "stale"
	}
	switch aw.TrustState {
	case "verified", "verified_local":
		return "live", "live"
	case "display_only":
		return "display only", "muted"
	case "held":
		return "held", "critical"
	case "":
		return "unknown", "muted"
	default:
		return humanizeToken(aw.TrustState), "muted"
	}
}

func accountCritical(aw store.AccountWindow) bool {
	return !aw.ProbeStale && (aw.Severity == "critical" || aw.SessionPct >= 100 || aw.WeeklyPct >= 100)
}

func seatHealth(s store.Seat, aw store.AccountWindow) (string, string) {
	if aw.AccountKey != "" && accountCritical(aw) {
		return "acct capped", "critical"
	}
	switch s.Health {
	case store.SeatReady:
		return "ready", "ready"
	case store.SeatLimitCritical:
		return "limit critical", "critical"
	case store.SeatAuthDead:
		return "auth dead", "critical"
	case store.SeatUnreachable:
		return "unreachable", "warning"
	case "":
		return "unknown", "muted"
	default:
		return humanizeToken(s.Health), "warning"
	}
}

func epicStatus(e store.EpicRun, items []store.AttentionItem) (string, string) {
	for _, item := range items {
		if strings.Contains(strings.ToLower(item.Kind), "ci_red") {
			return "CI red", "critical"
		}
	}
	if len(items) > 0 {
		return humanizeToken(items[0].Kind), "review"
	}
	switch e.State {
	case "done":
		return "complete", "success"
	case "achieved":
		return "achieved", "success"
	case "abandoned":
		return "abandoned", "muted"
	case "blocked":
		return "blocked", "critical"
	case "launching", "pending":
		return e.State, "warning"
	default:
		return firstNonEmpty(e.StatusStateDetail, e.State), "active"
	}
}

func paneStatus(pane, state string) (string, string) {
	t := strings.ToLower(pane)
	switch {
	case strings.Contains(t, "work"):
		return "working", "working"
	case strings.Contains(t, "await"):
		return "awaiting", "awaiting"
	case strings.Contains(t, "idle"):
		return "idle", "idle"
	case strings.Contains(t, "stall") || strings.Contains(t, "wedg"):
		return "stalled", "critical"
	case isTerminalEpic(state):
		return "complete", "complete"
	case pane == "":
		return "unknown", "unknown"
	default:
		return humanizeToken(pane), "unknown"
	}
}

func isActiveEpic(state string) bool {
	switch state {
	case "launching", "running", "blocked":
		return true
	default:
		return false
	}
}

func isTerminalEpic(state string) bool {
	switch state {
	case "done", "achieved", "abandoned":
		return true
	default:
		return false
	}
}

func normalizeFamily(raw string) string {
	raw = strings.ToLower(raw)
	for _, fam := range []string{"claude", "codex", "grok"} {
		if strings.Contains(raw, fam) {
			return fam
		}
	}
	if raw == "" {
		return "unknown"
	}
	return "other"
}

func familyClass(fam string) string {
	switch fam {
	case "claude", "codex", "grok":
		return fam
	default:
		return "other"
	}
}

func familyLabel(fam string) string {
	if fam == "unknown" || fam == "other" || fam == "" {
		return "AGENT"
	}
	return strings.ToUpper(fam)
}

func friendlySeatName(s store.Seat, fam string, ordinal int) string {
	base := strings.TrimPrefix(path.Base(s.Ident()), ".")
	if base == "" || base == "." || base == fam || base == fam+"-home" {
		return fmt.Sprintf("%s%d", fam, ordinal)
	}
	if len(base) > 20 {
		return fmt.Sprintf("%s%d", fam, ordinal)
	}
	return base
}

func epicSubtitle(e store.EpicRun) string {
	if e.StatusBlockers != "" {
		return e.StatusBlockers
	}
	if len(e.Scope) > 0 {
		parts := make([]string, 0, 2)
		for _, scope := range e.Scope {
			scope = strings.TrimSuffix(strings.TrimSuffix(scope, "/**"), "/*")
			parts = append(parts, scope)
			if len(parts) == 2 {
				break
			}
		}
		return strings.Join(parts, " · ")
	}
	return firstNonEmpty(e.Repo, e.Branch, "Epic session")
}

func preferredUsageLabel(aw store.AccountWindow) string {
	if aw.SessionPct >= 0 {
		return percentLabel(aw.SessionPct)
	}
	return percentLabel(aw.WeeklyPct)
}

func percentLabel(p float64) string {
	if p < 0 {
		return "—"
	}
	return fmt.Sprintf("%.0f%%", p)
}

func pctToInt(p float64) int {
	if p <= 0 {
		return 0
	}
	if p >= 100 {
		return 100
	}
	return int(p + .5)
}

func resetLabel(raw string, now time.Time) string {
	t := store.ParseTimeOrZero(raw)
	if t.IsZero() {
		return "reset unknown"
	}
	d := t.Sub(now)
	prefix := "resetting…"
	if d > time.Minute {
		prefix = "resets in " + compactDuration(d)
	}
	return prefix + " · " + t.In(now.Location()).Format("03:04 PM")
}

func humanAgo(at, now time.Time) string {
	if at.IsZero() {
		return "unknown"
	}
	d := now.Sub(at)
	if d < 0 {
		d = 0
	}
	switch {
	case d < 10*time.Second:
		return "now"
	case d < time.Minute:
		return fmt.Sprintf("%ds ago", int(d/time.Second))
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d/time.Minute))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d/time.Hour))
	default:
		return fmt.Sprintf("%dd ago", int(d/(24*time.Hour)))
	}
}

func compactDuration(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	if d >= 24*time.Hour {
		return fmt.Sprintf("%dd", int(d/(24*time.Hour)))
	}
	if d >= time.Hour {
		return fmt.Sprintf("%dh %dm", int(d/time.Hour), int((d%time.Hour)/time.Minute))
	}
	return fmt.Sprintf("%dm", int(d/time.Minute))
}

func boxLabel(box string) string {
	if box == "" {
		return "control-plane"
	}
	return box
}

func shortID(id string) string {
	if len(id) <= 10 {
		return id
	}
	return id[:8]
}

func humanizeToken(token string) string {
	token = strings.TrimSpace(strings.ReplaceAll(strings.ReplaceAll(token, "_", " "), "-", " "))
	if token == "" {
		return "unknown"
	}
	return token
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func maxFloat(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
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
