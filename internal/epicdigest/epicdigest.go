// Package epicdigest is the deterministic-core assembly of the per-epic session
// digest and its fleet rollups (epic-lane Phase 6, plan §2.1 + §15.16). It is a PURE
// function of INJECTED state: the store folds an epic row + its linked goal-session +
// account_windows row + open attention items + the pinned checklist + the last ≤3
// ledgered interventions into an Input, and this package compiles the EpicDigest the
// `/v1/epics/digest` endpoint serves and the master consumes. No LLM, no clock read,
// no `tmux capture-pane` — every input (including the observation instant) is passed
// IN as a value, so the same inputs always yield the same digest (DESIGN §1.2; this
// package is registered in tools/archcheck's core set).
//
// The digest is a PUBLIC, versioned contract (plan §15.16): external consumers (the
// Stream Deck plugin, future clients) depend on these field names and JSON shapes, so
// they carry explicit `json:` tags, `[]` never serializes as null, and ordering is
// caller-stable. The one deliberate design commitment worth restating: `on_task` is a
// DETERMINISTIC rollup (§2.1) so a master can eyeball a 10-epic fleet in one screen and
// descend only where on_task=false — the throughput steal.
package epicdigest

import (
	"time"

	"github.com/samhotchkiss/flowbee/internal/attention"
)

// ChecklistItem is one "- [x] Step N — <criterion> (evidence: …)" line off a running
// epic's ## Status. Owned here (not re-exported from internal/epicspec) so the digest's
// serialization contract is self-contained.
type ChecklistItem struct {
	Step     int    `json:"step"`
	Checked  bool   `json:"checked"`
	Text     string `json:"text,omitempty"`
	Evidence string `json:"evidence,omitempty"`
}

// SeverityCritical is the server-critical severity token (mirrors acctprobe's value;
// duplicated as a local literal so this core package need not import acctprobe, which
// transitively pulls internal/clock and would break the archcheck boundary).
const SeverityCritical = "critical"

// Window is one usage window carried VERBATIM from acctprobe.LimitWindow (plan §2.1 +
// §15.16) — the per-window detail a scoped ring (e.g. a per-model weekly_scoped limit)
// needs and the session_pct/weekly_pct scalars cannot express. It is the public read
// model's window shape; a consumer's types line up field-for-field.
type Window struct {
	// The elgato §15.16 contract wants stable keys — severity/resets_at/scope are always
	// present (no omitempty) so a `[]`-consumer never has to key-check.
	Kind     string  `json:"kind"`     // session | weekly_all | weekly_scoped
	Percent  float64 `json:"percent"`  // real server-reported utilization, 0..100
	Severity string  `json:"severity"` // normal | critical
	ResetsAt string  `json:"resets_at"`
	Scope    string  `json:"scope"` // model display name for weekly_scoped; "" otherwise
}

// AccountSummary is the digest's per-epic account panel (plan §2.1), projected from the
// bound account's account_windows row. Percentages carry -1 for UNKNOWN. ProbeStale is
// the §12.14 flag the on-task gate and the dashboard read. Windows carries acctprobe's
// FULL per-window detail verbatim (never null); SessionPct/WeeklyPct stay as the
// convenience scalars existing consumers and the fallback path use.
type AccountSummary struct {
	AccountKey    string   `json:"account_key,omitempty"`
	Email         string   `json:"email,omitempty"`
	Model         string   `json:"model,omitempty"`
	SessionPct    float64  `json:"session_pct"`
	WeeklyPct     float64  `json:"weekly_pct"`
	Severity      string   `json:"severity,omitempty"`
	ResetsSession string   `json:"resets_session_at,omitempty"`
	ResetsWeekly  string   `json:"resets_weekly_at,omitempty"`
	Windows       []Window `json:"windows"`
	ProbeStale    bool     `json:"probe_stale"`
	Bound         bool     `json:"bound"` // false = no account bound to this epic yet
}

// CriticalNonStale reports whether the bound account is critically capped AND the
// reading is trustworthy — the §12.14 predicate the on-task rollup gates on (a critical
// reading that is probe_stale is SUPPRESSED so a flaky-ssh reading cannot phantom-cap
// an epic). An unbound/absent account is never critical.
func (a AccountSummary) CriticalNonStale() bool {
	return a.Bound && a.Severity == SeverityCritical && !a.ProbeStale
}

// Intervention is one prior ledgered steer on this epic (plan §15.11a) — read from the
// ledger (no new table) so a post-`/clear` master neither repeats nor contradicts its
// own prior steer. The digest carries the last ≤3, injected.
type Intervention struct {
	Actor   string    `json:"actor"` // operator | master | <supervisor label>
	At      time.Time `json:"at"`
	Summary string    `json:"summary"` // bounded payload summary (never raw pane text)
}

// BaseDrift is the epic branch's position relative to main (plan §2.1). Injected — the
// ticker computes it from the mirror; this package only carries it.
type BaseDrift struct {
	CommitsBehindMain int `json:"commits_behind_main"`
	EpicStepCommits   int `json:"epic_step_commits"`
}

// Epic is the injected projection of one epic's persisted + disk-derived state.
type Epic struct {
	Slug   string
	Repo   string
	Branch string
	Host   string // '' = control-plane box (the §15.16a jump-target host)
	Agent  string
	Tmux   string // the explicit tmux session name jump target (§15.16a)

	LifecycleState string // epics.state
	StatusState    string // raw ## Status "State:" word

	CurrentStep int
	StepsTotal  int
	Checklist   []ChecklistItem
	Blockers    string

	PaneState  string  // last tmuxio.Classify token
	AuthState  string  // '' | ok | auth_dead
	ContextPct float64 // remaining %; -1 = unknown

	// observation instants (zero = unknown); the assembler turns these into ages.
	LastPaneChangeAt time.Time
	StatusUpdatedAt  time.Time
	LastCommitAt     time.Time

	BaseDrift    BaseDrift
	DriftSignals []string // fired deterministic drift signal names (plan §2.3)
}

// Input is everything Assemble needs — all injected, none read from I/O.
type Input struct {
	Epic                Epic
	Account             AccountSummary
	Attention           []attention.Item // OPEN items for THIS epic
	RecentInterventions []Intervention   // last ≤3 (injected; the assembler clamps)
	Now                 time.Time
	Config              Config
}

// Config carries the tunable thresholds the rollups read (all with sane defaults).
type Config struct {
	// ContextFloorPct is the remaining-context floor below which an epic is NOT on-task
	// (self-degradation risk, plan §2.3 default 15%).
	ContextFloorPct float64
	// RecentProgressWindow is how recently a commit / status update must have landed for
	// an IDLE_AT_PROMPT pane to still count as making progress (on-task).
	RecentProgressWindow time.Duration
	// MaxInterventions clamps RecentInterventions in the digest (plan §15.11a: last ~3).
	MaxInterventions int
}

// DefaultConfig is the shipped rollup tuning.
func DefaultConfig() Config {
	return Config{ContextFloorPct: 15, RecentProgressWindow: 20 * time.Minute, MaxInterventions: 3}
}

func (c Config) withDefaults() Config {
	if c.ContextFloorPct == 0 {
		c.ContextFloorPct = 15
	}
	if c.RecentProgressWindow == 0 {
		c.RecentProgressWindow = 20 * time.Minute
	}
	if c.MaxInterventions == 0 {
		c.MaxInterventions = 3
	}
	return c
}

// Steps is the digest's step panel (plan §2.1).
type Steps struct {
	Current   int             `json:"current"`
	Total     int             `json:"total"`
	Checked   int             `json:"checked"`
	Checklist []ChecklistItem `json:"checklist"`
}

// Ages is the digest's age panel, in whole seconds; -1 = unknown (the instant was zero).
type Ages struct {
	PaneChangeS   int64 `json:"pane_change_s"`
	StatusUpdateS int64 `json:"status_update_s"`
	LastCommitS   int64 `json:"last_commit_s"`
}

// EpicDigest is the per-epic digest (plan §2.1) — the public, versioned read model.
type EpicDigest struct {
	Slug   string `json:"slug"`
	Repo   string `json:"repo"`
	Branch string `json:"branch"`
	Host   string `json:"host"` // '' = control-plane box
	Agent  string `json:"agent"`
	Tmux   string `json:"tmux"` // explicit jump target (§15.16a), never joined by slug

	LifecycleState string `json:"lifecycle_state"`
	StatusState    string `json:"status_state,omitempty"`

	Steps      Steps          `json:"steps"`
	Ages       Ages           `json:"ages"`
	Account    AccountSummary `json:"account"`
	ContextPct float64        `json:"context_pct"`
	PaneState  string         `json:"pane_state,omitempty"`
	AuthState  string         `json:"auth_state,omitempty"`
	Blockers   string         `json:"blockers,omitempty"`
	BaseDrift  BaseDrift      `json:"base_drift"`

	DriftSignals        []string       `json:"drift_signals"`
	RecentInterventions []Intervention `json:"recent_interventions"`

	OnTask bool `json:"on_task"`
}

// Assemble compiles one epic's digest from injected state. PURE.
func Assemble(in Input) EpicDigest {
	cfg := in.Config.withDefaults()
	e := in.Epic

	d := EpicDigest{
		Slug:           e.Slug,
		Repo:           e.Repo,
		Branch:         e.Branch,
		Host:           e.Host,
		Agent:          e.Agent,
		Tmux:           e.Tmux,
		LifecycleState: e.LifecycleState,
		StatusState:    e.StatusState,
		Steps: Steps{
			Current:   e.CurrentStep,
			Total:     e.StepsTotal,
			Checked:   countChecked(e.Checklist),
			Checklist: nonNilChecklist(e.Checklist),
		},
		Ages: Ages{
			PaneChangeS:   ageSeconds(e.LastPaneChangeAt, in.Now),
			StatusUpdateS: ageSeconds(e.StatusUpdatedAt, in.Now),
			LastCommitS:   ageSeconds(e.LastCommitAt, in.Now),
		},
		Account:             normalizeAccount(in.Account),
		ContextPct:          e.ContextPct,
		PaneState:           e.PaneState,
		AuthState:           e.AuthState,
		Blockers:            e.Blockers,
		BaseDrift:           e.BaseDrift,
		DriftSignals:        nonNilStrings(e.DriftSignals),
		RecentInterventions: clampInterventions(in.RecentInterventions, cfg.MaxInterventions),
		OnTask:              OnTask(in),
	}
	return d
}

// normalizeAccount guarantees the account's Windows slice serializes as [] never null
// (the §15.16d JSON contract), without mutating the caller's injected value.
func normalizeAccount(a AccountSummary) AccountSummary {
	if a.Windows == nil {
		a.Windows = []Window{}
	}
	return a
}

func countChecked(items []ChecklistItem) int {
	n := 0
	for _, it := range items {
		if it.Checked {
			n++
		}
	}
	return n
}

func nonNilChecklist(items []ChecklistItem) []ChecklistItem {
	if items == nil {
		return []ChecklistItem{}
	}
	return items
}

func nonNilStrings(ss []string) []string {
	if ss == nil {
		return []string{}
	}
	return ss
}

// clampInterventions keeps the LAST max interventions (the freshest), preserving order.
func clampInterventions(in []Intervention, max int) []Intervention {
	if len(in) <= max {
		if in == nil {
			return []Intervention{}
		}
		return in
	}
	return in[len(in)-max:]
}

// ageSeconds returns whole seconds between at and now, or -1 when at is zero (unknown)
// or in the future (a clock-skew guard — a negative age is meaningless, report unknown).
func ageSeconds(at, now time.Time) int64 {
	if at.IsZero() {
		return -1
	}
	d := now.Sub(at)
	if d < 0 {
		return -1
	}
	return int64(d / time.Second)
}
