package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/samhotchkiss/flowbee/client"
)

// markerPath returns the pause marker file path that sits beside the live DB. It is the
// CP-local operator override (serve + status check it); the `pause`/`resume` commands now
// drive the DB-backed, client-triggerable control endpoint instead so a REMOTE client (the
// russ worker) can pause the dispatcher too — not just a process on the control-plane box.
func markerPath(dbURL string) string {
	return filepath.Join(filepath.Dir(dbURL), "paused")
}

// runPause tells the dispatcher to stop issuing new leases. `flowbee pause` pauses
// EVERYTHING; `flowbee pause --repo russ` parks just that repo (other repos keep flowing).
// In-flight leases, heartbeats, and result submissions are unaffected. Hits the control
// plane over FLOWBEE_URL (default loopback) so it works from any box, not just the CP.
func runPause(args []string) error { return runControl(args, true) }

// runResume is the inverse — resume global dispatch or a single repo.
func runResume(args []string) error { return runControl(args, false) }

func runControl(args []string, pause bool) error {
	name := "resume"
	if pause {
		name = "pause"
	}
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	repo := fs.String("repo", "", "scope to one repo id (default: everything)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	url := envOr("FLOWBEE_URL", "http://127.0.0.1:7070")
	c := client.NewWithToken(url, os.Getenv("FLOWBEE_WORKER_TOKEN"))
	ctx := context.Background()

	var err error
	if pause {
		err = c.Pause(ctx, *repo)
	} else {
		err = c.Resume(ctx, *repo)
	}
	if err != nil {
		return fmt.Errorf("%s via %s: %w", name, url, err)
	}

	scope := "everything"
	if *repo != "" {
		scope = "repo " + *repo
	}
	if pause {
		fmt.Printf("dispatch PAUSED (%s) — no new leases; in-flight jobs continue, heartbeats/results still flow\n", scope)
		fmt.Printf("  run `flowbee resume%s` when ready\n", repoArg(*repo))
	} else {
		fmt.Printf("dispatch RESUMED (%s)\n", scope)
	}
	return nil
}

func repoArg(repo string) string {
	if repo == "" {
		return ""
	}
	return " --repo " + repo
}
