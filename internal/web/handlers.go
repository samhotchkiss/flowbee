package web

import (
	"encoding/json"
	"fmt"
	"hash/fnv"
	"html/template"
	"net/http"
	"path"
	"sort"
	"strings"
	"time"

	attentioncore "github.com/samhotchkiss/flowbee/internal/attention"
	"github.com/samhotchkiss/flowbee/internal/store"
)

// page is the shared template payload: the active nav tab + the view-specific
// body data. The base template renders the nav + the live SSE hook around it.
type page struct {
	Active    string // "epics" | "workspace" | "board" | "fleet" | "audit" | "roster"
	Title     string
	Partial   bool // true => render only the live body fragment (SSE refresh)
	Epics     *epicsView
	Workspace *workspaceView
	Board     *boardView
	Fleet     *fleetView
	Dash      *dashView
	Roster    []store.RosterWorker
}

// workspaceView carries only the exact project scope into the HTML shell. The
// durable thread, message, delivery, and route state is always read from the
// authenticated conversation API by the browser; server-rendered placeholders
// never become a second conversation projection.
type workspaceView struct {
	ProjectID              string
	DriverControlRequired  bool
	DriverControlAvailable bool
	DriverControlGap       string
	DriverControlReason    string
}

func (u *UI) workspace(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/workspace" {
		http.NotFound(w, r)
		return
	}
	projectID := strings.TrimSpace(r.URL.Query().Get("project"))
	if projectID == "" {
		projectID = "default"
	}
	if !validWorkspaceProjectID(projectID) {
		http.Error(w, "invalid project", http.StatusBadRequest)
		return
	}
	driverControl := u.currentDriverControl()
	u.renderPage(w, r, page{Active: "workspace", Title: "Project workspace", Workspace: &workspaceView{
		ProjectID:              projectID,
		DriverControlRequired:  driverControl.Required,
		DriverControlAvailable: !driverControl.Required || driverControl.Available,
		DriverControlGap:       driverControl.Gap,
		DriverControlReason:    driverControl.Reason,
	}})
}

func (u *UI) currentDriverControl() DriverControlState {
	if u.driverControlCurrent != nil {
		return u.driverControlCurrent()
	}
	return DriverControlState{Required: u.driverControlRequired, Available: u.driverControlAvailable,
		Gap: u.driverControlGap, Reason: u.driverControlReason}
}

func validWorkspaceProjectID(value string) bool {
	if len(value) == 0 || len(value) > 128 {
		return false
	}
	for _, r := range value {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9') || r == '-' || r == '_' || r == '.' || r == ':' {
			continue
		}
		return false
	}
	return true
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
	Enabled             bool
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

type needsYouCount struct {
	Label string
	Count int
}

type needsYouOption struct {
	ID, Label, Description, Consequence, ValueJSON string
	Recommended                                    bool
}

type needsYouEvidence struct {
	Label, Kind, Ref, Hash, Version string
	URL                             template.URL
}

type needsYouAction struct {
	Kind, Label, Class string
}

type needsYouCard struct {
	ID, ProjectID, EpicID, DeliveryID string
	Kind, KindLabel, KindClass        string
	Title, Prompt, Summary            string
	State, StateLabel, StateClass     string
	Priority, RequestVersion          int
	PriorityLabel, UrgencyClass       string
	Blocking                          bool
	Impact, AgeLabel, DueLabel        string
	DueClass                          string
	RequestedBy, RouteTo              string
	SubjectArtifactRef                string
	SubjectArtifactURL                template.URL
	SubjectVersion                    int
	SubjectSHA256, SubjectShortHash   string
	ResponseSchemaJSON                string
	Options                           []needsYouOption
	Evidence                          []needsYouEvidence
	Actions                           []needsYouAction
	AllowAnswer                       bool
	AllowFreeAnswer                   bool
	AllowDefer                        bool
	RequiresAuthorizationScope        bool
	Actionable                        bool
	DeferredUntilLabel                string
	DeferCondition                    string
	SupersededBy                      string
	CancellationReason                string
	CurrentResponseID                 string
	ResponseKind, ResponseActor       string
	ResponseAckState                  string
	CreatedLabel, ViewedLabel         string
	ResponseCreatedLabel              string
	SortCreated, SortUpdated, SortDue time.Time
}

type needsYouView struct {
	Actionable, Deferred, Recent     []needsYouCard
	OpenCount, UrgentCount           int
	OverdueCount                     int
	OldestLabel                      string
	Urgencies, Projects, Types, Ages []needsYouCount
}

type epicsView struct {
	SnapshotAt, SnapshotAge string
	Stats                   epicStatsView
	Master                  epicMasterView
	Alert                   *epicCapacityAlert
	NeedsYou                needsYouView
	Projects                []projectPortfolioCard
	Attention               []epicAttentionRow
	Accounts                []epicAccountRow
	Seats                   []epicSeatRow
	Active                  []epicCard
	Review                  []epicCard
	Completed               []epicCard
	Archived                []epicCard
	CompletedHidden         int
}

type projectPortfolioCard struct {
	ID, Name, State, StateClass          string
	PauseReason, Blocker, BlockerKind    string
	InteractorStatus, OrchestratorStatus string
	Priority, Weight, Cap                int
	Active, Parked, NeedsYou, Allocated  int
	BlockerAge                           string
	Scheduler                            []projectSchedulerCard
}

type projectSchedulerCard struct {
	Pool, PoolLabel, ServiceShare, WeightShare, EligibleWait string
	Allocated, ServiceTurns, PoolServiceTurns, Eligible      int64
	Starved                                                  bool
}

const dashboardCompletedLimit = 24

func (u *UI) epics(w http.ResponseWriter, r *http.Request) {
	// "GET /" is a ServeMux subtree pattern. Refuse unknown paths instead of
	// accidentally turning /v1/typo into a 200 HTML dashboard.
	if r.URL.Path != "/" && r.URL.Path != "/epics" && r.URL.Path != "/dashboard" {
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
	decisions, err := u.data.ListDecisionInboxAllProjects(ctx, 24)
	if err != nil {
		http.Error(w, "decision inbox error", http.StatusInternalServerError)
		return
	}
	projects, err := u.data.ProjectDashboard(ctx)
	if err != nil {
		http.Error(w, "project portfolio error", http.StatusInternalServerError)
		return
	}
	store.EvaluateProjectDashboardStarvation(projects, u.clock.Now(), store.ProjectStarvationBound)

	view := u.buildEpics(epics, seats, supervisors, windows, attention, u.clock.Now())
	view.NeedsYou = buildNeedsYou(decisions, u.clock.Now())
	view.Projects = buildProjectPortfolio(projects, u.clock.Now())
	view.Stats.NeedsYou += view.NeedsYou.OpenCount
	u.renderPage(w, r, page{Active: "epics", Title: "Fleet", Epics: &view})
}

func buildProjectPortfolio(rows []store.ProjectDashboardRow, now time.Time) []projectPortfolioCard {
	out := make([]projectPortfolioCard, 0, len(rows))
	for _, row := range rows {
		card := projectPortfolioCard{
			ID: row.Project.ID, Name: row.Project.Name, State: row.Project.State,
			StateClass: strings.ToLower(row.Project.State), PauseReason: row.Project.PauseReason,
			Priority: row.Project.Priority, Weight: row.Project.SchedulerWeight,
			Cap: row.Project.ConcurrencyCap, Active: row.ActiveEpics, Parked: row.ParkedEpics,
			NeedsYou: row.NeedsYou, Allocated: row.Capacity.Allocated,
			Blocker: row.OldestBlocker, BlockerKind: humanizeToken(row.BlockerKind),
			InteractorStatus: row.Interactor.Status, OrchestratorStatus: row.Orchestrator.Status,
		}
		for _, metric := range row.Scheduler {
			schedulerCard := projectSchedulerCard{
				Pool: metric.Pool, PoolLabel: humanizeToken(metric.Pool),
				Allocated: int64(metric.Allocated), ServiceTurns: metric.ServiceTurns,
				PoolServiceTurns: metric.PoolServiceTurns, Eligible: int64(metric.Eligible),
				ServiceShare: formatBasisPoints(metric.ServiceShareBasisPoints),
				WeightShare:  formatBasisPoints(metric.ConfiguredWeightShareBasisPoints),
				Starved:      metric.Starved,
			}
			if metric.Eligible > 0 {
				schedulerCard.EligibleWait = durationAge(time.Duration(metric.EligibleWaitSeconds) * time.Second)
			}
			card.Scheduler = append(card.Scheduler, schedulerCard)
		}
		if !row.BlockedSince.IsZero() {
			card.BlockerAge = humanAgo(row.BlockedSince, now)
		}
		out = append(out, card)
	}
	return out
}

func formatBasisPoints(value int) string {
	if value < 0 {
		value = 0
	}
	return fmt.Sprintf("%d.%02d%%", value/100, value%100)
}

func buildNeedsYou(rows []store.DecisionInboxRow, now time.Time) needsYouView {
	view := needsYouView{}
	projectCounts := map[string]int{}
	typeCounts := map[string]int{}
	urgencyCounts := map[string]int{}
	ageCounts := map[string]int{"under 1h": 0, "1-24h": 0, "1-3d": 0, "3d+": 0}
	oldest := time.Duration(0)
	for _, row := range rows {
		card := buildNeedsYouCard(row, now)
		switch card.State {
		case "open", "viewed":
			view.Actionable = append(view.Actionable, card)
			view.OpenCount++
			projectCounts[card.ProjectID]++
			typeCounts[card.KindLabel]++
			urgencyCounts[card.PriorityLabel]++
			age := now.Sub(card.SortCreated)
			if age < 0 {
				age = 0
			}
			if age > oldest {
				oldest = age
			}
			switch {
			case age < time.Hour:
				ageCounts["under 1h"]++
			case age < 24*time.Hour:
				ageCounts["1-24h"]++
			case age < 72*time.Hour:
				ageCounts["1-3d"]++
			default:
				ageCounts["3d+"]++
			}
			if card.UrgencyClass == "critical" {
				view.UrgentCount++
			}
			if card.DueClass == "overdue" {
				view.OverdueCount++
			}
		case "deferred":
			view.Deferred = append(view.Deferred, card)
		default:
			view.Recent = append(view.Recent, card)
		}
	}
	sort.SliceStable(view.Actionable, func(i, j int) bool {
		a, b := view.Actionable[i], view.Actionable[j]
		if a.Priority != b.Priority {
			return a.Priority < b.Priority
		}
		if a.Blocking != b.Blocking {
			return a.Blocking
		}
		if a.SortDue.IsZero() != b.SortDue.IsZero() {
			return !a.SortDue.IsZero()
		}
		if !a.SortDue.Equal(b.SortDue) {
			return a.SortDue.Before(b.SortDue)
		}
		if !a.SortCreated.Equal(b.SortCreated) {
			return a.SortCreated.Before(b.SortCreated)
		}
		return a.ID < b.ID
	})
	sort.SliceStable(view.Deferred, func(i, j int) bool {
		a, b := view.Deferred[i], view.Deferred[j]
		if !a.SortDue.Equal(b.SortDue) {
			if a.SortDue.IsZero() != b.SortDue.IsZero() {
				return !a.SortDue.IsZero()
			}
			return a.SortDue.Before(b.SortDue)
		}
		return a.ID < b.ID
	})
	sort.SliceStable(view.Recent, func(i, j int) bool {
		if !view.Recent[i].SortUpdated.Equal(view.Recent[j].SortUpdated) {
			return view.Recent[i].SortUpdated.After(view.Recent[j].SortUpdated)
		}
		return view.Recent[i].ID < view.Recent[j].ID
	})
	view.OldestLabel = durationAge(oldest)
	view.Urgencies = orderedDecisionCounts(urgencyCounts, []string{"urgent", "high", "normal", "low"})
	view.Projects = orderedDecisionCounts(projectCounts, nil)
	view.Types = orderedDecisionCounts(typeCounts, []string{"Question", "Plan review", "Design review", "Authorization", "Exception"})
	view.Ages = orderedDecisionCounts(ageCounts, []string{"under 1h", "1-24h", "1-3d", "3d+"})
	return view
}

func buildNeedsYouCard(row store.DecisionInboxRow, now time.Time) needsYouCard {
	r := row.Request
	kindLabel, kindClass := decisionKindPresentation(string(r.Kind))
	priorityLabel, urgencyClass := decisionPriorityPresentation(r.Priority)
	dueLabel, dueClass := "no deadline", ""
	if !r.DueAt.IsZero() {
		dueLabel = "due " + humanRelative(r.DueAt, now)
		if !r.DueAt.After(now) {
			dueClass, urgencyClass = "overdue", "critical"
		} else if r.DueAt.Sub(now) <= time.Hour {
			dueClass = "soon"
		}
	}
	stateLabel, stateClass := decisionStatePresentation(string(r.State))
	actionable := r.State == "open" || r.State == "viewed"
	card := needsYouCard{
		ID: r.ID, ProjectID: r.ProjectID, EpicID: r.EpicID, DeliveryID: r.DeliveryID,
		Kind: string(r.Kind), KindLabel: kindLabel, KindClass: kindClass,
		Title: r.Title, Prompt: r.Prompt, Summary: r.Summary,
		State: string(r.State), StateLabel: stateLabel, StateClass: stateClass,
		Priority: r.Priority, PriorityLabel: priorityLabel, UrgencyClass: urgencyClass,
		Blocking: row.Blocking, AgeLabel: humanAgo(r.CreatedAt, now), DueLabel: dueLabel, DueClass: dueClass,
		RequestedBy: r.RequestedBy, RouteTo: r.RouteTo,
		SubjectArtifactRef: r.SubjectArtifactRef, SubjectArtifactURL: safeEvidenceURL(r.SubjectArtifactRef),
		SubjectVersion: r.SubjectVersion, SubjectSHA256: r.SubjectSHA256,
		SubjectShortHash: shortDecisionHash(r.SubjectSHA256), RequestVersion: r.RequestVersion,
		ResponseSchemaJSON: r.ResponseSchemaJSON, Options: parseDecisionOptions(r.OptionsJSON),
		Evidence: parseDecisionEvidence(r.EvidenceRefsJSON), Actionable: actionable,
		DeferCondition: r.DeferCondition, SupersededBy: r.SupersededBy,
		CancellationReason: r.CancellationReason, CurrentResponseID: r.CurrentResponseID,
		ResponseKind: humanizeToken(string(row.ResponseKind)), ResponseActor: row.ResponseActorID,
		ResponseAckState: humanizeToken(row.DownstreamAckState),
		CreatedLabel:     r.CreatedAt.Format("Jan 02, 03:04 PM"),
		SortCreated:      r.CreatedAt, SortUpdated: r.UpdatedAt, SortDue: r.DueAt,
	}
	if !row.ViewedAt.IsZero() {
		card.ViewedLabel = row.ViewedAt.Format("Jan 02, 03:04 PM")
	}
	if !row.ResponseCreatedAt.IsZero() {
		card.ResponseCreatedLabel = row.ResponseCreatedAt.Format("Jan 02, 03:04 PM")
	}
	card.RequiresAuthorizationScope = (r.Kind == "authorization" || r.Kind == "exception") && actionable
	if !r.DeferredUntil.IsZero() {
		card.DeferredUntilLabel = humanRelative(r.DeferredUntil, now)
		card.SortDue = r.DeferredUntil
	}
	if dueClass == "overdue" {
		card.Impact = "The deadline passed. Waiting work remains held until this is resolved."
	} else if row.Blocking {
		card.Impact = "Waiting work cannot continue until this decision is recorded."
	} else {
		card.Impact = "Flowbee will keep this request visible until it is resolved."
	}
	if actionable {
		for _, kind := range r.ExpectedResponseKinds {
			switch string(kind) {
			case "answer":
				card.AllowAnswer = true
				card.AllowFreeAnswer = r.Kind == "question" && len(card.Options) == 0
			case "approve":
				card.Actions = append(card.Actions, needsYouAction{Kind: "approve", Label: "Approve", Class: "primary"})
			case "request_changes":
				card.Actions = append(card.Actions, needsYouAction{Kind: "request-changes", Label: "Request changes", Class: "secondary"})
			case "defer":
				card.AllowDefer = true
			case "deny":
				card.Actions = append(card.Actions, needsYouAction{Kind: "deny", Label: "Deny", Class: "danger"})
			}
		}
	}
	return card
}

func decisionKindPresentation(kind string) (string, string) {
	switch kind {
	case "question":
		return "Question", "question"
	case "plan_review":
		return "Plan review", "plan"
	case "design_review":
		return "Design review", "design"
	case "authorization":
		return "Authorization", "authorization"
	case "exception":
		return "Exception", "exception"
	default:
		return "Decision", "question"
	}
}

func decisionPriorityPresentation(priority int) (string, string) {
	switch priority {
	case 1:
		return "urgent", "critical"
	case 2:
		return "high", "high"
	case 3:
		return "normal", "normal"
	default:
		return "low", "low"
	}
}

func decisionStatePresentation(state string) (string, string) {
	switch state {
	case "open":
		return "new", "open"
	case "viewed":
		return "viewed", "viewed"
	case "deferred":
		return "deferred", "deferred"
	case "superseded":
		return "superseded", "stale"
	case "cancelled":
		return "cancelled", "stale"
	case "changes_requested":
		return "changes requested", "resolved"
	default:
		return humanizeToken(state), "resolved"
	}
}

func orderedDecisionCounts(counts map[string]int, order []string) []needsYouCount {
	seen := map[string]bool{}
	out := []needsYouCount{}
	for _, label := range order {
		seen[label] = true
		if counts[label] > 0 {
			out = append(out, needsYouCount{Label: label, Count: counts[label]})
		}
	}
	labels := make([]string, 0, len(counts))
	for label, count := range counts {
		if count > 0 && !seen[label] {
			labels = append(labels, label)
		}
	}
	sort.Strings(labels)
	for _, label := range labels {
		out = append(out, needsYouCount{Label: label, Count: counts[label]})
	}
	return out
}

func parseDecisionOptions(raw string) []needsYouOption {
	var values []any
	if json.Unmarshal([]byte(raw), &values) != nil {
		return nil
	}
	out := make([]needsYouOption, 0, len(values))
	for i, value := range values {
		option := needsYouOption{ID: fmt.Sprintf("option-%d", i+1)}
		choice := value
		switch typed := value.(type) {
		case string:
			option.ID, option.Label = typed, typed
		case map[string]any:
			option.ID = firstJSONText(typed, "id", "value", "key")
			option.Label = firstJSONText(typed, "label", "title", "name", "id", "value")
			option.Description = firstJSONText(typed, "description", "detail", "summary")
			option.Consequence = firstJSONText(typed, "consequence", "impact")
			option.Recommended, _ = typed["recommended"].(bool)
			if rawValue, ok := typed["value"]; ok {
				choice = rawValue
			} else if id, ok := typed["id"]; ok {
				choice = id
			}
		}
		if option.ID == "" {
			option.ID = fmt.Sprintf("option-%d", i+1)
		}
		if option.Label == "" {
			option.Label = option.ID
		}
		blob, err := json.Marshal(choice)
		if err != nil {
			continue
		}
		option.ValueJSON = string(blob)
		out = append(out, option)
	}
	return out
}

func parseDecisionEvidence(raw string) []needsYouEvidence {
	var values []any
	if json.Unmarshal([]byte(raw), &values) != nil {
		return nil
	}
	out := make([]needsYouEvidence, 0, len(values))
	for _, value := range values {
		evidence := needsYouEvidence{}
		switch typed := value.(type) {
		case string:
			evidence.Label, evidence.Ref = typed, typed
		case map[string]any:
			evidence.Label = firstJSONText(typed, "label", "title", "name", "kind", "ref", "url")
			evidence.Kind = firstJSONText(typed, "kind", "type")
			evidence.Ref = firstJSONText(typed, "ref", "artifact_ref", "url")
			evidence.Hash = firstJSONText(typed, "sha256", "hash")
			evidence.Version = firstJSONText(typed, "version", "artifact_version")
		}
		if evidence.Ref == "" {
			continue
		}
		if evidence.Label == "" {
			evidence.Label = evidence.Ref
		}
		evidence.URL = safeEvidenceURL(evidence.Ref)
		out = append(out, evidence)
	}
	return out
}

func firstJSONText(value map[string]any, keys ...string) string {
	for _, key := range keys {
		if text, ok := value[key].(string); ok && strings.TrimSpace(text) != "" {
			return strings.TrimSpace(text)
		}
	}
	return ""
}

func safeEvidenceURL(value string) template.URL {
	value = strings.TrimSpace(value)
	if strings.HasPrefix(value, "/") && !strings.HasPrefix(value, "//") {
		return template.URL(value)
	}
	if strings.HasPrefix(value, "https://") || strings.HasPrefix(value, "http://") {
		return template.URL(value)
	}
	return ""
}

func shortDecisionHash(value string) string {
	value = strings.TrimPrefix(value, "sha256:")
	if len(value) > 12 {
		return value[:12]
	}
	return value
}

func durationAge(age time.Duration) string {
	if age <= 0 {
		return "0m"
	}
	if age >= 24*time.Hour {
		return fmt.Sprintf("%dd %dh", int(age/(24*time.Hour)), int(age%(24*time.Hour)/time.Hour))
	}
	if age >= time.Hour {
		return fmt.Sprintf("%dh %dm", int(age/time.Hour), int(age%time.Hour/time.Minute))
	}
	return fmt.Sprintf("%dm", maxInt(1, int(age/time.Minute)))
}

func humanRelative(target, now time.Time) string {
	if target.IsZero() {
		return ""
	}
	if target.Before(now) {
		return durationAge(now.Sub(target)) + " ago"
	}
	return "in " + durationAge(target.Sub(now))
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func (u *UI) buildEpics(epics []store.EpicRun, seats []store.Seat, supervisors []store.Supervisor, windows []store.AccountWindow, attention []store.AttentionItem, now time.Time) epicsView {
	v := epicsView{SnapshotAt: now.Format("Jan 02, 03:04 PM")}

	latest := time.Time{}
	considerLatest := func(raw string) {
		if t := parseDashboardTime(raw, now.Location()); t.After(latest) {
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
		if isEpicDashboardAttention(item) {
			attentionByEpic[item.EpicID] = append(attentionByEpic[item.EpicID], item)
		}
		if isHumanAttention(item) {
			v.Attention = append(v.Attention, buildAttentionRow(item, epics))
		}
		considerLatest(item.UpdatedAt)
	}

	windowByKey := map[string]store.AccountWindow{}
	for _, aw := range windows {
		windowByKey[aw.AccountKey] = aw
		considerLatest(aw.ReportedAt)
	}

	seatNamesByAccount := map[string][]string{}
	families := map[string]bool{}
	enabledSeats := 0
	for _, s := range seats {
		fam := normalizeFamily(s.AgentFamily)
		if s.Enabled {
			families[fam] = true
			enabledSeats++
		}
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
			AccountKey: s.AccountKey, Enabled: s.Enabled, SeatLoad: activeBySeat[s.ID], HostLoad: activeByBox[s.Box], MaxConcurrent: cap,
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
		displayName := strings.TrimSpace(aw.Email)
		if displayName == "" {
			displayName = firstNonEmpty(strings.Join(seatNamesByAccount[aw.AccountKey], ", "), shortID(aw.AccountKey))
		}
		row := epicAccountRow{
			Key: aw.AccountKey, ShortKey: shortID(aw.AccountKey), Email: displayName,
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

	completedCount, doneToday, ciRed := 0, 0, 0
	dayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	for _, e := range epics {
		items := attentionByEpic[e.ID]
		card := u.buildEpicCard(e, items, seatAliases, now)
		successful := isSuccessfulEpic(e.State)
		if successful {
			completedCount++
			if finished := parseDashboardTime(e.FinishedAt, now.Location()); !finished.IsZero() && !finished.Before(dayStart) {
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
		case successful:
			v.Completed = append(v.Completed, card)
		case e.State == "abandoned":
			v.Archived = append(v.Archived, card)
		case len(items) > 0 || e.State == "blocked" || deliveryIsReviewLane(e.DeliveryState) || strings.Contains(strings.ToLower(e.StatusStateDetail), "review"):
			v.Review = append(v.Review, card)
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
	sort.SliceStable(v.Archived, func(i, j int) bool { return v.Archived[i].SortAt.After(v.Archived[j].SortAt) })
	if len(v.Completed) > dashboardCompletedLimit {
		v.CompletedHidden = len(v.Completed) - dashboardCompletedLimit
		v.Completed = v.Completed[:dashboardCompletedLimit]
	}

	v.Stats = epicStatsView{
		Completed: completedCount, CompletedToday: doneToday,
		Active: len(v.Active), Review: len(v.Review), CIRed: ciRed,
		Seats: enabledSeats, Families: len(families), NeedsYou: len(v.Attention),
	}
	v.Master = buildMaster(supervisors, now, u.staleHB)
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
	sortAt := firstDashboardTime(now.Location(), e.FinishedAt, e.UpdatedAt, e.CreatedAt)
	progress, stepLabel := 0, "—"
	if isSuccessfulEpic(e.State) {
		progress = 100
		stepLabel = "done"
		if e.StatusStepsTotal > 0 && e.StatusCurrentStep >= e.StatusStepsTotal {
			stepLabel = fmt.Sprintf("%d/%d", e.StatusCurrentStep, e.StatusStepsTotal)
		}
	} else if e.StatusStepsTotal > 0 {
		progress = clampPct(e.StatusCurrentStep * 100 / e.StatusStepsTotal)
		stepLabel = fmt.Sprintf("%d/%d", e.StatusCurrentStep, e.StatusStepsTotal)
	}
	status, statusClass := epicStatus(e, items)
	pane, paneClass := paneStatus(e.PaneState, e.State)
	updated := firstDashboardTime(now.Location(), e.StatusUpdatedAt, e.UpdatedAt)
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
	} else if !isTerminalEpic(e.State) && e.StatusBlockers != "" {
		detail = e.StatusBlockers
	}
	title, subtitle := epicDisplayText(e)
	return epicCard{
		ID: e.ID, Title: title, Subtitle: subtitle,
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

// isHumanAttention keeps the top-level "Needs you" signal distinct from the
// master's own durable queue. Leased/delivering items are already being handled;
// master-first conditions such as CI red stay visible on their epic card without
// falsely paging the operator.
func isHumanAttention(item store.AttentionItem) bool {
	if item.State != attentioncore.StateOpen {
		return false
	}
	kind := attentioncore.Kind(item.Kind)
	return attentioncore.TierFor(kind) == attentioncore.TierHumanImmediate ||
		kind == attentioncore.KindBlockedNonResumable
}

// isEpicDashboardAttention drops informational queue entries that should not move
// a terminal epic back into the review lane. They remain available through the
// versioned attention API and audit view.
func isEpicDashboardAttention(item store.AttentionItem) bool {
	switch attentioncore.Kind(item.Kind) {
	case attentioncore.KindEpicFinished, attentioncore.KindMergeMainSuggested,
		attentioncore.KindCIInfraIncident, attentioncore.KindMasterAbsent:
		return false
	default:
		return item.EpicID != ""
	}
}

func buildMaster(supervisors []store.Supervisor, now time.Time, staleAfter time.Duration) epicMasterView {
	var picked *store.Supervisor
	for i := range supervisors {
		s := &supervisors[i]
		if picked == nil || supervisorRank(*s, now, staleAfter) > supervisorRank(*picked, now, staleAfter) ||
			(supervisorRank(*s, now, staleAfter) == supervisorRank(*picked, now, staleAfter) &&
				parseDashboardTime(s.LastHeartbeatAt, now.Location()).After(parseDashboardTime(picked.LastHeartbeatAt, now.Location()))) {
			picked = s
		}
	}
	if picked == nil {
		return epicMasterView{State: "offline", StateClass: "muted", Heartbeat: "no master registered", Box: "—"}
	}
	state := effectiveSupervisorState(*picked, now, staleAfter)
	stateClass := "muted"
	if state == "active" {
		stateClass = "active"
	} else if state == "stale" || state == "revoked" {
		stateClass = "critical"
	}
	return epicMasterView{
		Registered: true, Label: picked.Label, State: state, StateClass: stateClass,
		Epoch: picked.Epoch, Kind: picked.Kind, Box: boxLabel(picked.Box),
		Heartbeat: humanAgo(parseDashboardTime(picked.LastHeartbeatAt, now.Location()), now),
	}
}

func effectiveSupervisorState(s store.Supervisor, now time.Time, staleAfter time.Duration) string {
	if s.State != "active" {
		return s.State
	}
	if staleAfter <= 0 {
		staleAfter = 90 * time.Second
	}
	hb := parseDashboardTime(s.LastHeartbeatAt, now.Location())
	if hb.IsZero() || now.Sub(hb) > staleAfter {
		return "stale"
	}
	return "active"
}

func supervisorRank(s store.Supervisor, now time.Time, staleAfter time.Duration) int {
	switch effectiveSupervisorState(s, now, staleAfter) {
	case "active":
		return 2
	case "stale":
		return 1
	default:
		return 0
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
	case "verified", "verified_local", "live": // "live" is the pre-0031 persisted spelling.
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
	if !s.Enabled {
		return "disabled", "muted"
	}
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
	// Durable terminal state wins over stale status markdown or an attention row that
	// was still in flight when the epic closed.
	switch e.State {
	case "done":
		return "complete", "success"
	case "achieved":
		return "achieved", "success"
	case "abandoned":
		return "abandoned", "muted"
	}
	switch e.DeliveryState {
	case "awaiting_review_dispatch":
		return "built · awaiting review dispatch", "critical"
	case "review_queued":
		return "review queued", "review"
	case "in_review":
		return "in review", "review"
	case "changes_requested", "rebuild_in_flight":
		return humanizeToken(e.DeliveryState), "warning"
	case "merge_queued", "merging", "conflict_resolution", "cleanup_pending":
		return humanizeToken(e.DeliveryState), "review"
	}
	for _, item := range items {
		if strings.Contains(strings.ToLower(item.Kind), "ci_red") {
			return "CI red", "critical"
		}
	}
	if len(items) > 0 {
		return humanizeToken(items[0].Kind), "review"
	}
	switch e.State {
	case "blocked":
		return "blocked", "critical"
	case "launching", "pending":
		return e.State, "warning"
	default:
		return firstNonEmpty(e.StatusStateDetail, e.State), "active"
	}
}

func deliveryIsReviewLane(state string) bool {
	switch state {
	case "awaiting_review_dispatch", "review_queued", "in_review", "changes_requested",
		"rebuild_in_flight", "merge_queued", "merging", "conflict_resolution", "cleanup_pending":
		return true
	default:
		return false
	}
}

func paneStatus(pane, state string) (string, string) {
	if isTerminalEpic(state) {
		return "complete", "complete"
	}
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

func isSuccessfulEpic(state string) bool {
	return state == "done" || state == "achieved"
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

// epicDisplayText understands the title shape emitted by the epic authoring flow:
// "epic/<slug> — description". Keeping the slug and description separate matches
// the operator mockup and avoids truncating the useful part of real, long titles.
func epicDisplayText(e store.EpicRun) (string, string) {
	raw := strings.TrimSpace(e.Title)
	if heading, description, ok := strings.Cut(raw, " — "); ok {
		heading = strings.TrimPrefix(strings.TrimSpace(heading), "epic/")
		heading = strings.TrimPrefix(heading, "mail-")
		return firstNonEmpty(heading, shortEpicSlug(firstNonEmpty(e.Slug, e.ID))), strings.TrimSpace(description)
	}
	return firstNonEmpty(raw, shortEpicSlug(firstNonEmpty(e.Slug, e.ID)), e.ID), epicSubtitle(e)
}

func shortEpicSlug(id string) string {
	id = strings.TrimSpace(id)
	if len(id) > len("2006-01-02-") && id[4] == '-' && id[7] == '-' && id[10] == '-' {
		id = id[11:]
	}
	return strings.TrimPrefix(id, "mail-")
}

func epicSubtitle(e store.EpicRun) string {
	if !isTerminalEpic(e.State) && e.StatusBlockers != "" {
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

// parseDashboardTime accepts the current RFC3339 store contract plus the legacy
// SQLite timestamp shape present in pre-0031 epic rows. The latter has no zone and
// is interpreted in the operator clock's location; it is display compatibility only,
// never used by deterministic store decisions.
func parseDashboardTime(raw string, loc *time.Location) time.Time {
	if t := store.ParseTimeOrZero(raw); !t.IsZero() {
		return t
	}
	if loc == nil {
		loc = time.Local
	}
	raw = strings.TrimSpace(raw)
	for _, layout := range []string{"2006-01-02 15:04:05.999999999", "2006-01-02 15:04:05"} {
		if t, err := time.ParseInLocation(layout, raw, loc); err == nil {
			return t
		}
	}
	return time.Time{}
}

func firstDashboardTime(loc *time.Location, values ...string) time.Time {
	for _, value := range values {
		if t := parseDashboardTime(value, loc); !t.IsZero() {
			return t
		}
	}
	return time.Time{}
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

func (u *UI) audit(w http.ResponseWriter, r *http.Request) {
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
	u.renderPage(w, r, page{Active: "audit", Title: "Audit", Dash: &dashView{
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
