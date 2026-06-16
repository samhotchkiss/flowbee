package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/samhotchkiss/flowbee/internal/onboarding"
)

// runInit scaffolds Flowbee config into the repo (F13): flowbee.yaml + flows/, with
// owner/repo prefilled from the git remote, the db gitignored, and a 3-item
// checklist printed. Idempotent: existing files are reported, never clobbered.
func runInit(args []string) error {
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	dir := fs.String("dir", ".", "repo root to scaffold into")
	if err := fs.Parse(args); err != nil {
		return err
	}

	res, err := onboarding.Init(*dir)
	if err != nil {
		return err
	}

	if res.Owner != "" && res.Repo != "" {
		fmt.Printf("flowbee init: scaffolded config for %s/%s\n", res.Owner, res.Repo)
	} else {
		fmt.Println("flowbee init: scaffolded config (no git remote detected — set github_owner/github_repo in flowbee.yaml)")
	}
	for _, p := range res.Created {
		fmt.Printf("  created  %s\n", p)
	}
	for _, p := range res.Skipped {
		fmt.Printf("  kept     %s\n", p)
	}

	fmt.Println("\nNext steps:")
	for i, item := range res.Checklist() {
		fmt.Printf("  %d. %s\n", i+1, item)
	}
	fmt.Fprintln(os.Stderr) // trailing newline on stderr keeps stdout clean for piping
	return nil
}
