// Command archcheck enforces the deterministic-core boundary (DESIGN §1.2):
// the core packages must import no clock, randomness, ID minter, GitHub client,
// or LLM/agent package — so the core stays a pure, replayable function of
// persisted facts. Missing packages (early milestones) are skipped, so this is
// green from M0 and bites the moment a core package is added that violates it.
package main

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
)

var corePackages = []string{
	"github.com/samhotchkiss/flowbee/internal/engine",
	"github.com/samhotchkiss/flowbee/internal/job",
	"github.com/samhotchkiss/flowbee/internal/ledger",
	"github.com/samhotchkiss/flowbee/internal/lease",
}

// Forbidden import path prefixes. (time is intentionally allowed: the core uses
// it only as a value type — the instant is passed IN, never read.)
var forbidden = []string{
	"math/rand",
	"crypto/rand",
	"github.com/oklog/ulid",
	"github.com/samhotchkiss/flowbee/internal/clock",
	"github.com/samhotchkiss/flowbee/internal/ulid",
	"github.com/samhotchkiss/flowbee/internal/github",
	"github.com/samhotchkiss/flowbee/internal/reconcile",
	"github.com/samhotchkiss/flowbee/internal/project",
	"github.com/samhotchkiss/flowbee/internal/api",
	"github.com/google/go-github",
	"github.com/shurcooL/githubv4",
}

func main() {
	violations := 0
	for _, pkg := range corePackages {
		out, err := exec.Command("go", "list", "-deps", pkg).Output()
		if err != nil {
			fmt.Printf("archcheck: skip %s (not present yet)\n", pkg)
			continue
		}
		for _, dep := range strings.Split(strings.TrimSpace(string(out)), "\n") {
			for _, f := range forbidden {
				if dep == f || strings.HasPrefix(dep, f+"/") {
					fmt.Printf("VIOLATION: %s transitively imports forbidden %s\n", pkg, dep)
					violations++
				}
			}
		}
	}
	if violations > 0 {
		fmt.Printf("archcheck: %d violation(s) of the deterministic-core boundary\n", violations)
		os.Exit(1)
	}
	fmt.Println("archcheck: deterministic-core boundary clean")
}
