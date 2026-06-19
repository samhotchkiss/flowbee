package main

import (
	"context"
	"flag"
	"fmt"
	"strings"

	"github.com/samhotchkiss/flowbee/internal/config"
	"github.com/samhotchkiss/flowbee/internal/history"
	"github.com/samhotchkiss/flowbee/internal/store"
)

// runCard prints a single job's full §F history card — status, attempts, verdicts,
// lessons, and the institutional timeline — folded from the event ledger. The same
// curated view the archive writes to docs/history, but for ANY job (stuck, cancelled,
// in-flight, or done), so an operator can answer "why is this job here / how did it
// get built" without reading the DB or the GitHub archive. Local + read-only (mirrors
// `board`/`status`/`doctor`): opens the control-plane DB, no writes or RPCs.
func runCard(args []string) error {
	fs := flag.NewFlagSet("card", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() < 1 {
		return fmt.Errorf("usage: flowbee card <job-id>")
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

	card, err := st.HistoryCardForJob(ctx, jobID)
	if err != nil {
		if strings.Contains(err.Error(), "no such table") {
			return fmt.Errorf("no initialized flowbee database at %q — start the control plane first, or point FLOWBEE_CONFIG / database_url at the live DB (standard location: ~/.flowbee/flowbee.db)", cfg.DatabaseURL)
		}
		return err
	}
	// a job with no events folds to an empty card (no id): treat as not-found so a
	// mistyped/truncated id gets actionable guidance, not a blank render.
	if card.JobID == "" {
		return fmt.Errorf("no such job %q (check the FULL job id, not a truncated one — `flowbee board` lists them)", jobID)
	}
	fmt.Print(history.Render(card))
	return nil
}
