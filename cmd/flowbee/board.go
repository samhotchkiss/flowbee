package main

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strconv"
	"text/tabwriter"
	"time"

	"github.com/samhotchkiss/flowbee/internal/config"
	"github.com/samhotchkiss/flowbee/internal/store"
)

// runBoard prints the live job board as a fixed-column table followed by a
// one-line fleet-health summary. It is local + read-only: it opens the same
// control-plane DB the server uses, lists every job and the fleet snapshot, and
// makes no writes, GitHub calls, or control-plane RPCs (mirrors `doctor`/`version`).
func runBoard(args []string) error {
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
		return err
	}

	// Fleet health is computed by the store; reuse the SAME staleness window the
	// serve watchdog uses so the operator view and the alarm agree (serve.go).
	now := time.Now()
	staleHB := 3 * cfg.HeartbeatInterval()
	if staleHB <= 0 {
		staleHB = 90 * time.Second
	}
	health, err := st.FleetHealth(ctx, now, staleHB)
	if err != nil {
		return err
	}

	sortBoardJobs(jobs)

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "REPO\tISSUE\tSTATE\tROLE\tBOUNCES\tAGE")
	for _, j := range jobs {
		fmt.Fprintln(w, joinCells(boardRow(j, now)))
	}
	if err := w.Flush(); err != nil {
		return err
	}

	fmt.Printf("\nfleet: %d live, %d stale workers · %d waiting jobs\n",
		health.LiveWorkers, health.StaleWorkers, health.WaitingJobs)
	return nil
}

// boardRow maps a job (plus a reference `now`) to its rendered cells in column
// order: {repo, issue, state, role, bounces, age}. Pure and testable: it owns
// the "-" placeholders for an issue-less / role-less job, the bounce→string,
// and the relative-age formatting (no time.Now() inside).
func boardRow(j store.BoardJob, now time.Time) []string {
	issue := "-"
	if j.IssueNumber > 0 {
		issue = strconv.Itoa(j.IssueNumber)
	}
	role := j.Role
	if role == "" {
		role = "-"
	}
	repo := j.Repo
	if repo == "" {
		repo = "-"
	}
	return []string{
		repo,
		issue,
		j.State,
		role,
		strconv.Itoa(j.Bounces),
		formatAge(now.Sub(j.UpdatedAt)),
	}
}

// joinCells renders a row's cells as a single tab-separated line for tabwriter.
func joinCells(cells []string) string {
	out := ""
	for i, c := range cells {
		if i > 0 {
			out += "\t"
		}
		out += c
	}
	return out
}

// sortBoardJobs orders jobs by repo (ascending, lexicographic) then by issue
// number (ascending, numeric). Issue-less rows (IssueNumber == 0) sort AFTER
// issued rows within the same repo — they sit at the bottom of their repo
// group. Ties fall back to job ID so the output is deterministic.
func sortBoardJobs(jobs []store.BoardJob) {
	sort.SliceStable(jobs, func(a, b int) bool {
		ja, jb := jobs[a], jobs[b]
		if ja.Repo != jb.Repo {
			return ja.Repo < jb.Repo
		}
		// Issue-less rows (0) sort after issued rows in the same repo.
		aHas, bHas := ja.IssueNumber > 0, jb.IssueNumber > 0
		if aHas != bHas {
			return aHas
		}
		if aHas && ja.IssueNumber != jb.IssueNumber {
			return ja.IssueNumber < jb.IssueNumber
		}
		return ja.ID < jb.ID
	})
}

// formatAge returns a compact human duration for d (elapsed time since a job's
// last update): "47s", "2m", "1h04m", "12d". Negative/zero render as "0s". Kept
// free of time.Now() so it is directly unit-testable.
func formatAge(d time.Duration) string {
	if d <= 0 {
		return "0s"
	}
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d/time.Second))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d/time.Minute))
	case d < 24*time.Hour:
		h := int(d / time.Hour)
		m := int((d % time.Hour) / time.Minute)
		return fmt.Sprintf("%dh%02dm", h, m)
	default:
		return fmt.Sprintf("%dd", int(d/(24*time.Hour)))
	}
}
