package main

import (
	"context"
	"flag"
	"fmt"
	"strings"

	"github.com/samhotchkiss/flowbee/internal/config"
	"github.com/samhotchkiss/flowbee/internal/store"
)

// runRetryOutbox re-arms a job's dead-lettered (abandoned) outbox actions back to `pending`
// so the control plane re-attempts them — the operator recovery for the dropped GitHub writes
// surfaced by the `flowbee_outbox_abandoned` metric + the dead-letter WARN log. Use it after
// fixing the underlying cause (a transient outage, or a re-POSTed spec after a malformed one's
// issue-create failed). Every drained action is idempotent — an abandoned action took no effect
// — so a re-attempt can't double-apply. Local + a single UPDATE (mirrors `card`/`requeue`):
// run it against the control plane's DB (FLOWBEE_CONFIG / the standard ~/.flowbee/flowbee.db);
// the running control plane picks the re-armed rows up on its next drain tick.
func runRetryOutbox(args []string) error {
	fs := flag.NewFlagSet("retry-outbox", flag.ContinueOnError)
	repo := fs.String("repo", "", "re-arm abandoned outbox actions for every job in this repo")
	all := fs.Bool("all", false, "re-arm every abandoned outbox action across all repos")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if (*repo != "" && *all) || (fs.NArg() > 0 && (*repo != "" || *all)) || fs.NArg() > 1 {
		return fmt.Errorf("usage: flowbee retry-outbox <job-id> | --repo <repo-id> | --all")
	}
	if fs.NArg() == 0 && *repo == "" && !*all {
		return fmt.Errorf("usage: flowbee retry-outbox <job-id> | --repo <repo-id> | --all")
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

	scope := ""
	var n int
	switch {
	case *all:
		scope = "all repos"
		n, err = st.RetryAllAbandonedOutbox(ctx)
	case *repo != "":
		scope = "repo " + *repo
		n, err = st.RetryAbandonedOutboxForRepo(ctx, *repo)
	default:
		jobID := fs.Arg(0)
		scope = "job " + jobID
		n, err = st.RetryAbandonedOutbox(ctx, jobID)
	}
	if err != nil {
		if strings.Contains(err.Error(), "no such table") {
			return fmt.Errorf("no initialized flowbee database at %q — point FLOWBEE_CONFIG / database_url at the live DB (standard location: ~/.flowbee/flowbee.db)", cfg.DatabaseURL)
		}
		return err
	}
	if n == 0 {
		// name the DB: a cwd flowbee.yaml (e.g. the repo's sample config) silently
		// points the CLI at a different database than the serve daemon's — "nothing
		// to retry" while the live outbox holds abandoned rows (russ, 2026-07).
		fmt.Printf("no abandoned outbox actions for %s (nothing to retry; db: %s)\n", scope, cfg.DatabaseURL)
		return nil
	}
	fmt.Printf("re-armed %d abandoned outbox action(s) for %s (db: %s) — the control plane will re-attempt them on its next drain\n", n, scope, cfg.DatabaseURL)
	return nil
}
