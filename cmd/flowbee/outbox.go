package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/samhotchkiss/flowbee/internal/config"
	"github.com/samhotchkiss/flowbee/internal/store"
)

// runOutbox lists every dead-lettered (abandoned) GitHub write with the context an operator
// needs to triage it: the owning job + repo, that job's CURRENT state, attempts, age, and —
// crucially — whether the abandon is ACTIONABLE (the job is still live / parked at needs_human
// and genuinely missing this side-effect) or BENIGN (the job reached done/cancelled anyway, so
// the dropped write no longer matters — a stale-SHA void or a superseded merge attempt).
//
// This is the view behind the `flowbee status` "abandoned GitHub writes" warning, which counts
// only the ACTIONABLE rows (russ #215): without it, that warning was a permanent false alarm
// because abandons for already-completed jobs never drain (they shouldn't). Read-only, local —
// run it against the control plane's DB (FLOWBEE_CONFIG / the standard ~/.flowbee/flowbee.db).
func runOutbox(args []string) error {
	fs := flag.NewFlagSet("outbox", flag.ContinueOnError)
	all := fs.Bool("all", false, "include benign abandons (owning job already done/cancelled)")
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

	rows, err := st.AbandonedOutbox(ctx)
	if err != nil {
		if strings.Contains(err.Error(), "no such table") {
			return fmt.Errorf("no initialized flowbee database at %q — point FLOWBEE_CONFIG / database_url at the live DB (standard location: ~/.flowbee/flowbee.db)", cfg.DatabaseURL)
		}
		return err
	}

	var actionable, benign int
	for _, r := range rows {
		if r.Actionable {
			actionable++
		} else {
			benign++
		}
	}

	if len(rows) == 0 {
		fmt.Println("no abandoned GitHub writes — the outbox is clean")
		return nil
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(w, "STATE\tACTION\tREPO\tJOB\tJOB_STATE\tATTEMPTS\tAGE")
	shown := 0
	for _, r := range rows {
		if !r.Actionable && !*all {
			continue
		}
		state := "ACTIONABLE"
		if !r.Actionable {
			state = "benign"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%d\t%dh\n",
			state, r.Action, orDash(r.Repo), r.JobID, r.JobState, r.Attempts, r.AgeHours)
		shown++
	}
	w.Flush()

	fmt.Printf("\n%d actionable, %d benign (done/cancelled job — dropped write no longer matters)\n", actionable, benign)
	if actionable > 0 {
		fmt.Println("actionable: fix the cause, then `flowbee retry-outbox <job-id>` or `flowbee retry-outbox --repo <repo-id>` / `--all` to re-arm writes")
	}
	if benign > 0 && !*all {
		fmt.Println("re-run with --all to also list the benign abandons")
	}
	return nil
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
