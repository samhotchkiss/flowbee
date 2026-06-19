package main

import (
	"context"
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

	printStatus(os.Stdout, jobs, health, abandoned, isPausedDB(cfg.DatabaseURL))
	return nil
}

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

// printStatus writes the operator summary to w. Kept separate from runStatus
// so it is unit-testable without a live database.
func printStatus(w io.Writer, jobs []store.BoardJob, health store.FleetHealth, abandoned map[string]int, paused bool) {
	// Single pass: tally per-repo state counts and human-action totals.
	repoStates := make(map[string]map[string]int)
	var mergeHandoff, needsHuman int
	for _, j := range jobs {
		repo := j.Repo
		if repo == "" {
			repo = "-"
		}
		if repoStates[repo] == nil {
			repoStates[repo] = make(map[string]int)
		}
		repoStates[repo][j.State]++
		switch j.State {
		case "merge_handoff":
			mergeHandoff++
		case "needs_human":
			needsHuman++
		}
	}

	repos := make([]string, 0, len(repoStates))
	for r := range repoStates {
		repos = append(repos, r)
	}
	sort.Strings(repos)

	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	if len(repos) == 0 {
		fmt.Fprintln(tw, "no jobs")
	} else {
		for _, repo := range repos {
			states := repoStates[repo]
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

	fmt.Fprintf(w, "\nawaiting human: %d merge_handoff, %d needs_human\n", mergeHandoff, needsHuman)
	fmt.Fprintf(w, "fleet: %d live, %d stale workers%s\n", health.LiveWorkers, health.StaleWorkers, modelBreakdown(health.ByModel))
	// dropped GitHub writes (dead-lettered) — work that never took effect. Surface it in the
	// human view too (not just the metric/log), pointing at the recovery command.
	if len(abandoned) > 0 {
		fmt.Fprintf(w, "⚠ abandoned GitHub writes: %s — fix the cause, then `flowbee retry-outbox <job-id>`\n", sortedCounts(abandoned))
	}
	if paused {
		fmt.Fprintln(w, "\n*** PAUSED — no new leases are being issued (`flowbee resume` to unpause) ***")
	}
}
