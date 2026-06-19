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
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() < 1 {
		return fmt.Errorf("usage: flowbee retry-outbox <job-id>")
	}
	jobID := fs.Arg(0)

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

	n, err := st.RetryAbandonedOutbox(ctx, jobID)
	if err != nil {
		if strings.Contains(err.Error(), "no such table") {
			return fmt.Errorf("no initialized flowbee database at %q — point FLOWBEE_CONFIG / database_url at the live DB (standard location: ~/.flowbee/flowbee.db)", cfg.DatabaseURL)
		}
		return err
	}
	if n == 0 {
		fmt.Printf("no abandoned outbox actions for job %q (nothing to retry)\n", jobID)
		return nil
	}
	fmt.Printf("re-armed %d abandoned outbox action(s) for job %q — the control plane will re-attempt them on its next drain\n", n, jobID)
	return nil
}
