package main

import (
	"fmt"
	"os"

	"github.com/samhotchkiss/flowbee/internal/migladder"
)

// runMigration is the `flowbee migration <reserve>` CLI (epic-lane plan §12.6).
// It mutates the repo's migration-number ladder (a source file), so it operates
// on the working tree directly — like `flowbee migration reserve`, run from the
// repo root — rather than talking to the control-plane DB. This is deliberately
// distinct from `flowbee migrate up`, which APPLIES migrations to a database.
func runMigration(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: flowbee migration reserve <slug> [--ladder <path>]")
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "reserve":
		return runMigrationReserve(rest)
	default:
		return fmt.Errorf("unknown `flowbee migration` subcommand %q (want reserve)", sub)
	}
}

// runMigrationReserve reserves the next free migration number for a slug and
// prints the reserved filename (NNNN_slug.sql). The reservation is atomic under
// a file lock, so two builders running this concurrently get distinct numbers.
func runMigrationReserve(args []string) error {
	ladderPath := migladder.DefaultLadderPath()
	var slug string
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--ladder":
			if i+1 >= len(args) {
				return fmt.Errorf("--ladder needs a path")
			}
			ladderPath = args[i+1]
			i++
		default:
			if slug != "" {
				return fmt.Errorf("unexpected extra argument %q (usage: flowbee migration reserve <slug>)", args[i])
			}
			slug = args[i]
		}
	}
	if slug == "" {
		return fmt.Errorf("usage: flowbee migration reserve <slug> [--ladder <path>]")
	}
	if _, err := os.Stat(ladderPath); err != nil {
		return fmt.Errorf("ladder %s not found (run from the repo root, or pass --ladder): %w", ladderPath, err)
	}
	stem, err := migladder.Reserve(ladderPath, slug)
	if err != nil {
		return err
	}
	fmt.Printf("%s.sql\n", stem)
	return nil
}
