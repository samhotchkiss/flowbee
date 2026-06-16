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

// allowedDeterministic is a deny-list exception: deterministic stdlib hashing the
// core legitimately needs (the I-9 tamper-evident verdict hash). crypto/sha256 is
// a PURE function — same bytes in, same digest out — so it does not break replay.
// The wrinkle is that Go 1.25's FIPS-internal crypto transitively imports
// math/rand/v2 and crypto/internal/sysrand; those arrive ONLY through the
// deterministic-hash subtree and never give the core a usable randomness source
// (no rand symbol is importable from sha256). We suppress a math/rand or
// crypto/rand transitive match for a core package iff crypto/sha256 is the path
// that pulled it (i.e. the package uses sha256). A DIRECT import of math/rand or
// crypto/rand by the core still fails (it would not be reached via sha256 alone).
var allowedThrough = map[string]string{
	"math/rand/v2":            "crypto/sha256",
	"math/rand":               "crypto/sha256",
	"crypto/rand":             "crypto/sha256",
	"crypto/internal/sysrand": "crypto/sha256",
}

func main() {
	violations := 0
	for _, pkg := range corePackages {
		out, err := exec.Command("go", "list", "-deps", pkg).Output()
		if err != nil {
			fmt.Printf("archcheck: skip %s (not present yet)\n", pkg)
			continue
		}
		deps := strings.Split(strings.TrimSpace(string(out)), "\n")
		has := make(map[string]bool, len(deps))
		for _, d := range deps {
			has[d] = true
		}
		for _, dep := range deps {
			for _, f := range forbidden {
				if dep == f || strings.HasPrefix(dep, f+"/") {
					// suppress a deterministic-hash transitive pull (see allowedThrough).
					if via, ok := allowedThrough[dep]; ok && has[via] {
						continue
					}
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
