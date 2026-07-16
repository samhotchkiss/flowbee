// Command laddercheck enforces the migration-number ladder (epic-lane plan
// §12.6): every internal/store/migrations/*.sql must carry a number registered in
// LADDER.md, and no number may be duplicated on disk beyond a sanctioned
// grandfathered double. It FAILS a PR that introduces an unreserved or colliding
// migration number — the number-space analogue of archcheck's boundary gate,
// closing the self-inflicted 0023/0024 collision hole before parallel epic
// builders start.
//
// CI runs this alongside archcheck and providerlint; `make laddercheck` runs it
// locally. Usage: laddercheck [migrationsDir [ladderPath]] — defaults to the
// repo-relative locations when run from the repo root.
package main

import (
	"fmt"
	"os"

	"github.com/samhotchkiss/flowbee/internal/migladder"
)

func main() {
	migrationsDir := migladder.DefaultMigrationsDir()
	ladderPath := migladder.DefaultLadderPath()
	if len(os.Args) > 1 {
		migrationsDir = os.Args[1]
	}
	if len(os.Args) > 2 {
		ladderPath = os.Args[2]
	}

	violations, err := migladder.Check(migrationsDir, ladderPath)
	if err != nil {
		fmt.Printf("laddercheck: %v\n", err)
		os.Exit(1)
	}
	if len(violations) > 0 {
		for _, v := range violations {
			fmt.Printf("VIOLATION: %s\n", v)
		}
		fmt.Printf("laddercheck: %d migration-ladder violation(s)\n", len(violations))
		os.Exit(1)
	}
	fmt.Println("laddercheck: migration ladder clean")
}
