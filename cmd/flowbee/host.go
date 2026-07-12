package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"text/tabwriter"
	"time"

	"github.com/samhotchkiss/flowbee/internal/config"
	"github.com/samhotchkiss/flowbee/internal/store"
)

// runHost is the `flowbee host <add|list|rm>` CLI: the epic-lane placement
// registry (epic-lane Phase 2, 0026_epics.sql). A box must be registered here
// before `flowbee epic start` will ever launch onto it — the one-box-one-epic
// placement rule needs SOMEWHERE to check occupancy against, and an unregistered
// box has no note/eligibility an operator has vouched for. Talks directly to the
// local control-plane DB (matches `flowbee session`'s posture — pure registry CRUD,
// no need to round-trip through the serve process).
func runHost(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: flowbee host <add|list|rm> ...")
	}
	sub, rest := args[0], args[1:]

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

	switch sub {
	case "add":
		return runHostAdd(ctx, st, rest)
	case "list", "ls":
		return runHostList(ctx, st, rest)
	case "rm", "remove":
		return runHostRm(ctx, st, rest)
	default:
		return fmt.Errorf("unknown `flowbee host` subcommand %q (want add|list|rm)", sub)
	}
}

func runHostAdd(ctx context.Context, st *store.Store, args []string) error {
	fs := flag.NewFlagSet("host add", flag.ContinueOnError)
	note := fs.String("note", "", "free-text note (operator-facing only)")
	// same "<name> can come before or after flags" re-parse trick as `flowbee
	// session add` / `flowbee repo add` (repo.go, session.go) — flag.Parse stops at
	// the first non-flag token, so a naive single Parse would mis-treat "--note" as
	// an extra positional when name comes first.
	var name string
	rest := args
	for len(rest) > 0 {
		if err := fs.Parse(rest); err != nil {
			return err
		}
		if fs.NArg() == 0 {
			break
		}
		if name != "" {
			return fmt.Errorf("unexpected extra argument %q (usage: flowbee host add <name> [--note <s>])", fs.Arg(0))
		}
		name = fs.Arg(0)
		rest = fs.Args()[1:]
	}
	if name == "" {
		return fmt.Errorf("usage: flowbee host add <name> [--note <s>]")
	}
	if err := st.AddEpicHost(ctx, store.EpicHost{Name: name, Note: *note}, time.Now()); err != nil {
		if errors.Is(err, store.ErrEpicHostExists) {
			return fmt.Errorf("host %q is already registered (use `flowbee host rm %s` first to re-register)", name, name)
		}
		return err
	}
	fmt.Printf("✓ registered epic host %q — eligible for `flowbee epic start --host %s`\n", name, name)
	return nil
}

func runHostRm(ctx context.Context, st *store.Store, args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: flowbee host rm <name>")
	}
	name := args[0]
	if err := st.RemoveEpicHost(ctx, name); err != nil {
		if errors.Is(err, store.ErrEpicHostNotFound) {
			return fmt.Errorf("no such host %q", name)
		}
		return err
	}
	fmt.Printf("removed epic host %q\n", name)
	return nil
}

func runHostList(ctx context.Context, st *store.Store, args []string) error {
	fs := flag.NewFlagSet("host list", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}
	hosts, err := st.ListEpicHosts(ctx)
	if err != nil {
		return err
	}
	// occupancy is a live join (0026 migration comment: "active" is a state
	// predicate, not a static flag epic_hosts carries) — computed here, once, for
	// the whole table rather than N+1 queries.
	active, err := st.ListActiveEpicRuns(ctx)
	if err != nil {
		return err
	}
	occupiedBy := map[string]string{}
	for _, e := range active {
		if e.Host != "" {
			occupiedBy[e.Host] = e.ID
		}
	}
	printHostList(os.Stdout, hosts, occupiedBy)
	return nil
}

func printHostList(w io.Writer, hosts []store.EpicHost, occupiedBy map[string]string) {
	if len(hosts) == 0 {
		fmt.Fprintln(w, "no epic hosts registered (flowbee host add <name> [--note <s>])")
		return
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tENABLED\tOCCUPIED-BY\tNOTE")
	for _, h := range hosts {
		enabled := "yes"
		if !h.Enabled {
			enabled = "no"
		}
		occ := dashIfEmpty(occupiedBy[h.Name])
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", h.Name, enabled, occ, dashIfEmpty(h.Note))
	}
	tw.Flush() //nolint:errcheck
}
