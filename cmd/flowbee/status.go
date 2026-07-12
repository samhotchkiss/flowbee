package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/samhotchkiss/flowbee/internal/config"
	"github.com/samhotchkiss/flowbee/internal/store"
)

// isPausedDB reports whether the fleet is paused (the pause marker file exists
// next to dbURL). Used by status to display the PAUSED banner.
func isPausedDB(dbURL string) bool {
	_, err := os.Stat(markerPath(dbURL))
	return err == nil
}

// runStatus prints a one-glance operator summary from the local DB: per-repo
// job counts by state, the human-action queue (merge_handoff + needs_human),
// and fleet worker liveness. Read-only, no network calls.
func runStatus(args []string) error {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	jsonOut := fs.Bool("json", false, "emit status as a single JSON object")
	if err := fs.Parse(args); err != nil {
		return err
	}

	cfg, err := config.Load()
	if err != nil {
		return err
	}
	ctx := context.Background()

	st, err := store.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer st.Close()

	jobs, err := st.BoardSnapshot(ctx)
	if err != nil {
		if strings.Contains(err.Error(), "no such table") {
			return fmt.Errorf("no initialized flowbee database at %q — start the control plane (`flowbee serve`) first, or point FLOWBEE_CONFIG / database_url at the live DB (standard location: ~/.flowbee/flowbee.db)",
				cfg.DatabaseURL)
		}
		return err
	}

	now := time.Now()
	staleHB := 3 * cfg.HeartbeatInterval()
	if staleHB <= 0 {
		staleHB = 90 * time.Second
	}
	health, err := st.FleetHealth(ctx, now, staleHB)
	if err != nil {
		return err
	}
	abandoned, _ := st.OutboxAbandonedByAction(ctx) // dropped GitHub writes (best-effort)

	// global pause = the CP-local marker OR the DB-backed, client-triggerable flag.
	globalPaused := isPausedDB(cfg.DatabaseURL)
	if dp, derr := st.DispatchPaused(ctx); derr == nil && dp {
		globalPaused = true
	}
	summary := summarizeStatus(jobs, health, abandoned, globalPaused)
	// parked repos (per-repo pause) + red-main repos (green-main stop-the-line): surfaced so
	// a parked repo is never silently idle and a red main never silently piles up PRs.
	if repos, rerr := st.ListRepos(ctx, false); rerr == nil {
		for _, rp := range repos {
			if !rp.Active {
				summary.ParkedRepos = append(summary.ParkedRepos, rp.ID)
			}
			if red, _ := st.RepoMainCIRed(ctx, rp.ID); red {
				summary.RedMainRepos = append(summary.RedMainRepos, rp.ID)
			}
		}
	}
	// goal-session watchdog surface (epic-lane Phase 1): every registered session's
	// last-observed state, plus early-warning account usage — read directly from the
	// same tables the watcher (internal/watchdog) writes/reads, so `flowbee status`
	// never needs the serve process up or a network round-trip (matches the rest of
	// this command's local-DB posture).
	if sessions, serr := st.ListGoalSessions(ctx); serr == nil {
		summary.GoalSessions = sessions
	}
	if accounts, aerr := st.AllAccountUsage(ctx); aerr == nil {
		for _, a := range accounts {
			if a.UsagePct < usageCeilingWarnPct {
				continue
			}
			// skip stale gauges (>24h since the last usage report) — same rule as the
			// watchdog's hourly WARN: an account whose box went quiet days ago pins a
			// frozen high-water usage_pct that isn't actionable capacity news.
			if a.ReportedAt != "" {
				if reported, perr := time.Parse(time.RFC3339Nano, a.ReportedAt); perr == nil && now.Sub(reported) > 24*time.Hour {
					continue
				}
			}
			summary.UsageWarnings = append(summary.UsageWarnings, a)
		}
	}

	if *jsonOut {
		return printStatusJSON(os.Stdout, summary)
	}
	printStatusSummary(os.Stdout, summary)
	return nil
}

// usageCeilingWarnPct mirrors watchdog.usageCeilingWarnPct (§ task brief point 5):
// an account at/above this usage fraction is surfaced here BEFORE the real (~90%)
// dispatch ceiling gates it, so the operator has runway to react. Duplicated as a
// small untyped constant rather than importing internal/watchdog just for one
// number — cmd/flowbee already avoids pulling watcher internals into the CLI.
const usageCeilingWarnPct = 75

// modelBreakdown renders the live-worker per-backend tally as " (codex:14, sonnet:2)"
// (sorted, stable), so an operator sees WHICH model the fleet runs — the live complement
// to the per-node model on a §F card. Empty (no worker advertised a model) renders "".
func modelBreakdown(byModel map[string]int) string {
	if len(byModel) == 0 {
		return ""
	}
	return " (" + sortedCounts(byModel) + ")"
}

// sortedCounts renders a count map as "k:v, k:v" sorted by key (stable). Empty => "".
func sortedCounts(m map[string]int) string {
	if len(m) == 0 {
		return ""
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s:%d", k, m[k]))
	}
	return strings.Join(parts, ", ")
}

type statusRepoSummary struct {
	States map[string]int `json:"states"`
}

type statusAwaitingHumanSummary struct {
	MergeHandoff int `json:"merge_handoff"`
	NeedsHuman   int `json:"needs_human"`
	Total        int `json:"total"`
}

type statusModelSummary struct {
	LiveWorkers  int `json:"live_workers"`
	StaleWorkers int `json:"stale_workers"`
	TotalWorkers int `json:"total_workers"`
}

type statusFleetSummary struct {
	LiveWorkers  int                           `json:"live_workers"`
	StaleWorkers int                           `json:"stale_workers"`
	ByModel      map[string]statusModelSummary `json:"by_model"`
}

type statusSummary struct {
	Repos                 map[string]statusRepoSummary `json:"repos"`
	AwaitingHuman         statusAwaitingHumanSummary   `json:"awaiting_human"`
	Fleet                 statusFleetSummary           `json:"fleet"`
	AbandonedGitHubWrites map[string]int               `json:"abandoned_github_writes"`
	// ReadyJobs is work waiting to be claimed; ActiveJobs is work an agent is actively
	// running (claimed + in progress). ReadyJobs>0 with live workers but ActiveJobs==0 is a
	// starvation signal — the fleet is idle while work waits (e.g. a candidate-withholding
	// reservation/capacity bug). Surfaced so a wedge is never silent (the merge_handoff
	// reservation incident sat undetected for hours).
	ReadyJobs    int      `json:"ready_jobs"`
	ActiveJobs   int      `json:"active_jobs"`
	Starved      bool     `json:"starved"`
	Paused       bool     `json:"-"`
	ParkedRepos  []string `json:"parked_repos,omitempty"`
	RedMainRepos []string `json:"red_main_repos,omitempty"`
	// GoalSessions / UsageWarnings are the goal-session watchdog surface (epic-lane
	// Phase 1): every registered session's last-observed state, and any account
	// approaching its usage ceiling — populated by runStatus directly from the DB
	// (not by summarizeStatus, which stays a pure function over BoardJob/FleetHealth
	// for testability; these two are appended by the caller after the fact).
	GoalSessions           []store.GoalSession     `json:"goal_sessions,omitempty"`
	UsageWarnings          []store.AccountUsageRow `json:"usage_warnings,omitempty"`
	liveModelBreakdownOnly map[string]int
}

// agentActiveStates are the states in which a worker is actively running an agent (so the
// fleet is genuinely doing work). Deliberately EXCLUDES review_pending/mergeable/merge_handoff
// (awaiting a claim or a human) — those are not "the fleet is busy".
var agentActiveStates = map[string]bool{
	"leased": true, "building": true, "code_review": true,
	"resolving_conflict": true, "spec_authoring": true, "spec_review": true,
}

func summarizeStatus(jobs []store.BoardJob, health store.FleetHealth, abandoned map[string]int, paused bool) statusSummary {
	summary := statusSummary{
		Repos:                  map[string]statusRepoSummary{},
		AbandonedGitHubWrites:  map[string]int{},
		Paused:                 paused,
		liveModelBreakdownOnly: health.ByModel,
	}

	for _, j := range jobs {
		repo := j.Repo
		if repo == "" {
			repo = "-"
		}
		repoSummary := summary.Repos[repo]
		if repoSummary.States == nil {
			repoSummary.States = map[string]int{}
		}
		repoSummary.States[j.State]++
		summary.Repos[repo] = repoSummary
		switch j.State {
		case "merge_handoff":
			summary.AwaitingHuman.MergeHandoff++
		case "needs_human":
			summary.AwaitingHuman.NeedsHuman++
		case "ready":
			summary.ReadyJobs++
		}
		if agentActiveStates[j.State] {
			summary.ActiveJobs++
		}
	}
	summary.AwaitingHuman.Total = summary.AwaitingHuman.MergeHandoff + summary.AwaitingHuman.NeedsHuman
	// starvation: work is ready and workers are alive, but nothing is being actively worked.
	summary.Starved = summary.ReadyJobs > 0 && health.LiveWorkers > 0 && summary.ActiveJobs == 0

	summary.Fleet.LiveWorkers = health.LiveWorkers
	summary.Fleet.StaleWorkers = health.StaleWorkers
	summary.Fleet.ByModel = map[string]statusModelSummary{}
	for model, live := range health.ByModel {
		entry := summary.Fleet.ByModel[model]
		entry.LiveWorkers = live
		entry.TotalWorkers = entry.LiveWorkers + entry.StaleWorkers
		summary.Fleet.ByModel[model] = entry
	}
	for model, stale := range health.StaleByModel {
		entry := summary.Fleet.ByModel[model]
		entry.StaleWorkers = stale
		entry.TotalWorkers = entry.LiveWorkers + entry.StaleWorkers
		summary.Fleet.ByModel[model] = entry
	}
	for action, count := range abandoned {
		summary.AbandonedGitHubWrites[action] = count
	}
	return summary
}

func printStatusJSON(w io.Writer, summary statusSummary) error {
	enc := json.NewEncoder(w)
	return enc.Encode(summary)
}

// printStatus writes the operator summary to w. Kept separate from runStatus
// so it is unit-testable without a live database.
func printStatus(w io.Writer, jobs []store.BoardJob, health store.FleetHealth, abandoned map[string]int, paused bool) {
	printStatusSummary(w, summarizeStatus(jobs, health, abandoned, paused))
}

func printStatusSummary(w io.Writer, summary statusSummary) {
	repos := make([]string, 0, len(summary.Repos))
	for r := range summary.Repos {
		repos = append(repos, r)
	}
	sort.Strings(repos)

	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	if len(repos) == 0 {
		fmt.Fprintln(tw, "no jobs")
	} else {
		for _, repo := range repos {
			states := summary.Repos[repo].States
			stateNames := make([]string, 0, len(states))
			for s := range states {
				stateNames = append(stateNames, s)
			}
			sort.Strings(stateNames)
			parts := make([]string, 0, len(stateNames))
			for _, s := range stateNames {
				parts = append(parts, fmt.Sprintf("%s:%d", s, states[s]))
			}
			fmt.Fprintf(tw, "%s\t%s\n", repo, strings.Join(parts, "  "))
		}
	}
	tw.Flush() //nolint:errcheck

	fmt.Fprintf(w, "\nawaiting human: %d merge_handoff, %d needs_human\n", summary.AwaitingHuman.MergeHandoff, summary.AwaitingHuman.NeedsHuman)
	fmt.Fprintf(w, "fleet: %d live, %d stale workers%s\n", summary.Fleet.LiveWorkers, summary.Fleet.StaleWorkers, modelBreakdown(summary.liveModelBreakdownOnly))
	// starvation detector — the fleet is idle while work waits. This is the symptom of ANY
	// candidate-withholding bug (a reservation/capacity gate offering nothing), regardless of
	// cause, so a future wedge surfaces immediately instead of sitting silent for hours.
	if summary.Starved {
		fmt.Fprintf(w, "⚠ possible fleet STARVATION: %d ready job(s) waiting + %d live worker(s) but 0 actively building/reviewing — the lease handler may be offering nothing (reservation/capacity withholding). Check `flowbee board` + the stuck jobs' declared_blast_radius vs in-flight reservations.\n",
			summary.ReadyJobs, summary.Fleet.LiveWorkers)
	}
	// dropped GitHub writes (dead-lettered) — work that never took effect. Surface it in the
	// human view too (not just the metric/log), pointing at the recovery command.
	if len(summary.AbandonedGitHubWrites) > 0 {
		fmt.Fprintf(w, "⚠ abandoned GitHub writes: %s — fix the cause, then `flowbee retry-outbox <job-id>` / `--repo <id>` / `--all`\n", sortedCounts(summary.AbandonedGitHubWrites))
	}
	if summary.Paused {
		fmt.Fprintln(w, "\n*** PAUSED — no new leases are being issued (`flowbee resume` to unpause) ***")
	}
	if len(summary.ParkedRepos) > 0 {
		fmt.Fprintf(w, "\n*** PARKED REPOS: %s — their jobs are withheld from leasing (`flowbee resume --repo <id>` to un-park) ***\n",
			strings.Join(summary.ParkedRepos, ", "))
	}
	if len(summary.RedMainRepos) > 0 {
		fmt.Fprintf(w, "\n*** RED MAIN: %s — the integration branch CI is failing. Feature PRs can't be fairly judged (they're held, not bounced). FIX MAIN FIRST — file the fix as `flowbee:p1` so it jumps the queue. ***\n",
			strings.Join(summary.RedMainRepos, ", "))
	}
	// goal sessions (epic-lane Phase 1 watchdog): one line per registered tmux "goal"
	// session in the same compact `id · box · state (elapsed) [detail]` shape the
	// watchdog itself logs, so what an operator sees here matches the serve log.
	if len(summary.GoalSessions) > 0 {
		fmt.Fprintln(w, "\ngoal sessions:")
		for _, g := range summary.GoalSessions {
			fmt.Fprintf(w, "  %s\n", formatSessionLine(g))
		}
	}
	// account usage early warning (§ task brief point 5): surfaced independently of
	// the watchdog's own hourly-throttled log line — this always shows the LIVE
	// number, so an operator checking status mid-hour still sees it.
	if len(summary.UsageWarnings) > 0 {
		fmt.Fprintln(w, "\n⚠ account usage approaching ceiling:")
		for _, a := range summary.UsageWarnings {
			fmt.Fprintf(w, "  %s (%s): %d%% of %d%% ceiling\n", a.AccountID, a.ModelFamily, a.UsagePct, a.CeilingPct)
		}
	}
}
