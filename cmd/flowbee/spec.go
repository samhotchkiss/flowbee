package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/samhotchkiss/flowbee/client"
)

// runSpec submits a work item to the spec front door (POST /v1/specs) — the planner intake
// (the other intake path is labelling a GitHub issue `flowbee:build`). The control plane seeds
// a spec job that a spec_author drafts and an issue-reviewer signs off, which then materializes
// a GitHub issue and a build. A first-class CLI for the front door so an operator never has to
// hand-craft a curl. The task is the trailing argument (or all trailing words):
//
//	flowbee spec "add request timeouts to the HTTP client" --repo flowbee --title "HTTP timeouts"
func runSpec(args []string) error {
	fs := flag.NewFlagSet("spec", flag.ContinueOnError)
	repo := fs.String("repo", "", "repo id to build in (default: the primary registered repo)")
	title := fs.String("title", "", "short human label for the work item")
	acceptance := fs.String("acceptance", "", "optional done-when / acceptance criteria")
	if err := fs.Parse(args); err != nil {
		return err
	}
	task := strings.TrimSpace(strings.Join(fs.Args(), " "))
	if task == "" {
		return fmt.Errorf("usage: flowbee spec [--repo R] [--title T] [--acceptance A] <task description>")
	}

	url := envOr("FLOWBEE_URL", "http://127.0.0.1:7070")
	c := client.NewWithToken(url, os.Getenv("FLOWBEE_WORKER_TOKEN"))
	jobID, state, err := c.CreateSpec(context.Background(), client.SpecRequest{
		Task: task, Title: *title, Acceptance: *acceptance, Repo: *repo,
	})
	if err != nil {
		return err
	}
	fmt.Printf("spec submitted: job %s (%s)\n   a spec_author will draft it, issue-review signs off, then it materializes an issue + build.\n   watch it with `flowbee board` / `flowbee card %s`.\n", jobID, state, jobID)
	return nil
}
