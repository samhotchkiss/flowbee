package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/samhotchkiss/flowbee/internal/config"
	"github.com/samhotchkiss/flowbee/internal/store"
)

// runList prints the primary Flowbee resource, jobs, as a focused read-only list.
// It uses the same local store projection as the board but omits fleet/operator
// health so "flowbee list" is a straightforward item listing entry point.
func runList(args []string) error {
	if len(args) > 0 {
		return fmt.Errorf("usage: flowbee list")
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

	jobs, err := listJobs(ctx, st)
	if err != nil {
		return listLoadError(err)
	}
	return printList(os.Stdout, jobs, time.Now())
}

func listJobs(ctx context.Context, st *store.Store) ([]store.BoardJob, error) {
	return st.BoardSnapshot(ctx)
}

func listLoadError(err error) error {
	if strings.Contains(err.Error(), "no such table") {
		return fmt.Errorf("no initialized flowbee database; start the control plane (`flowbee serve`) first, or point FLOWBEE_CONFIG / database_url at the live DB")
	}
	return fmt.Errorf("could not load jobs; check that FLOWBEE_CONFIG / database_url points at a readable Flowbee database")
}

func printList(w io.Writer, jobs []store.BoardJob, now time.Time) error {
	if len(jobs) == 0 {
		_, err := fmt.Fprintln(w, "No jobs found.")
		return err
	}

	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tREPO\tISSUE\tSTATE\tROLE\tUPDATED")
	for _, j := range jobs {
		fmt.Fprintln(tw, joinCells(listRow(j, now)))
	}
	return tw.Flush()
}

func listRow(j store.BoardJob, now time.Time) []string {
	repo := j.Repo
	if repo == "" {
		repo = "-"
	}
	issue := "-"
	if j.IssueNumber > 0 {
		issue = strconv.Itoa(j.IssueNumber)
	}
	role := j.Role
	if role == "" {
		role = "-"
	}
	return []string{
		j.ID,
		repo,
		issue,
		j.State,
		role,
		formatAge(now.Sub(j.UpdatedAt)),
	}
}
