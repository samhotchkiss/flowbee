package main

import (
	"context"
	"flag"
	"fmt"

	"github.com/samhotchkiss/flowbee/internal/onboarding"
)

// runDoctor validates the scaffolded repo (F13): config parses + is valid, the
// flow files reference identities that exist with their lenses, and GitHub is
// reachable (or skipped with --offline). Exits non-zero (returns an error) iff a
// check failed; warnings (offline/no-token) are reported but stay green.
func runDoctor(args []string) error {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	dir := fs.String("dir", ".", "repo root to validate")
	offline := fs.Bool("offline", false, "skip the GitHub reachability check")
	if err := fs.Parse(args); err != nil {
		return err
	}

	rep, err := onboarding.Doctor(context.Background(), onboarding.DoctorOptions{
		Root:       *dir,
		SkipGitHub: *offline,
	})
	if err != nil {
		return err
	}

	for _, c := range rep.Checks {
		mark := "ok  "
		switch c.Status {
		case onboarding.StatusWarn:
			mark = "warn"
		case onboarding.StatusFail:
			mark = "FAIL"
		}
		fmt.Printf("  [%s] %-13s %s\n", mark, c.Name, c.Detail)
	}

	if rep.Green() {
		fmt.Println("\nflowbee doctor: green")
		return nil
	}
	return fmt.Errorf("doctor found failing checks (see above)")
}
