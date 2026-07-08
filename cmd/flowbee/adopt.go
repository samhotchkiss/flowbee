package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strconv"

	"github.com/samhotchkiss/flowbee/client"
)

// runAdopt imports pre-existing PRs — ones Flowbee did NOT originate (e.g. an
// external agent-pool branch labeled for review) — into a repo's Flowbee review
// pipeline. Each adopted PR becomes an opted-in code_reviewer job in review_pending;
// Flowbee's reviewer judges the diff and self-merges on approval + green CI, or routes
// it to needs_human on changes_requested (there is no eng_worker bound to a foreign
// branch to bounce back to).
//
//	flowbee adopt [--repo <id>] <pr> [<pr> ...]
//
// --repo is required when the control plane manages more than one repo (PR numbers are
// repo-scoped). It may be omitted when exactly one repo is registered. Idempotent: a
// PR Flowbee already tracks is reported as already-tracked and left alone.
func runAdopt(args []string) error {
	fs := flag.NewFlagSet("adopt", flag.ContinueOnError)
	repo := fs.String("repo", "", "repo id the PR(s) belong to (required with 2+ managed repos)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() == 0 {
		return fmt.Errorf("usage: flowbee adopt [--repo <id>] <pr> [<pr> ...]")
	}

	url := envOr("FLOWBEE_URL", "http://127.0.0.1:7070")
	c := client.NewWithToken(url, os.Getenv("FLOWBEE_WORKER_TOKEN"))

	var failed bool
	for _, arg := range fs.Args() {
		pr, err := strconv.Atoi(arg)
		if err != nil || pr <= 0 {
			fmt.Fprintf(os.Stderr, "skip %q: not a positive PR number\n", arg)
			failed = true
			continue
		}
		jobID, already, status, err := c.AdoptPR(context.Background(), *repo, pr)
		switch {
		case err != nil:
			fmt.Fprintf(os.Stderr, "adopt PR #%d: %v (status %d)\n", pr, err, status)
			failed = true
		case already:
			fmt.Printf("PR #%d already tracked by Flowbee — left alone\n", pr)
		default:
			fmt.Printf("adopted PR #%d -> review_pending (job %s)\n", pr, jobID)
		}
	}
	if failed {
		return fmt.Errorf("one or more PRs could not be adopted")
	}
	return nil
}
