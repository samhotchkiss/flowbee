package main

import (
	"context"
	"fmt"
	"os"
	"text/tabwriter"

	"github.com/samhotchkiss/flowbee/internal/config"
	"github.com/samhotchkiss/flowbee/internal/store"
)

// runReservations prints the F8 blast-radius reservation picture: which in-flight builds
// hold which paths, and for each ready job whether it's being WITHHELD from leasing and by
// whom. The answer to "8 ready / 14 idle / 0 building — why?" (russ #213). Read-only, local DB.
func runReservations(args []string) error {
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
	rep, err := st.ReservationReport(ctx)
	if err != nil {
		return err
	}

	w := os.Stdout
	fmt.Fprintf(w, "In-flight reservations (held only by ACTIVELY-building jobs): %d\n", len(rep.Active))
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	for _, r := range rep.Active {
		scope := fmt.Sprintf("%d path(s)", len(r.Paths))
		if r.Wide {
			scope = "WIDE (whole tree)"
		}
		fmt.Fprintf(tw, "  %s\t%s\n", shortID(r.JobID), scope)
	}
	tw.Flush()

	withheld := 0
	for _, c := range rep.Ready {
		if c.Blocked {
			withheld++
		}
	}
	fmt.Fprintf(w, "\nReady jobs: %d  (%d leasable, %d WITHHELD by an overlapping reservation)\n",
		len(rep.Ready), len(rep.Ready)-withheld, withheld)
	tw = tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	for _, c := range rep.Ready {
		status := "leasable"
		if c.Blocked {
			status = "WITHHELD by " + shortID(c.BlockedBy)
		}
		scope := fmt.Sprintf("%d path(s)", len(c.Paths))
		if c.Wide {
			scope = "WIDE"
		}
		fmt.Fprintf(tw, "  %s\t%s\t%s\n", shortID(c.JobID), scope, status)
	}
	tw.Flush()

	if withheld > 0 && withheld == len(rep.Ready) && len(rep.Ready) > 0 {
		fmt.Fprintln(w, "\n⚠ EVERY ready job is withheld — the fleet will starve. A reservation bites only while a job is ACTIVELY building, so this means overlapping in-flight builds; if a blast radius is declared too WIDE it serializes everything (see the paths above).")
	}
	return nil
}

func shortID(s string) string {
	if len(s) > 14 {
		return s[:14]
	}
	return s
}
