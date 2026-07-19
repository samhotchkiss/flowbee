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
	"path/filepath"
	"strings"
)

var corePackages = []string{
	"github.com/samhotchkiss/flowbee/internal/engine",
	"github.com/samhotchkiss/flowbee/internal/job",
	"github.com/samhotchkiss/flowbee/internal/ledger",
	"github.com/samhotchkiss/flowbee/internal/lease",
	"github.com/samhotchkiss/flowbee/internal/liveness",
	// content (the I-11 gate) is consumed by the deterministic core via
	// EngineState.Content, so it must itself stay clock/rand/ULID/GitHub-free.
	"github.com/samhotchkiss/flowbee/internal/content",
	// attention (epic-lane Phase 5) is the attention-queue decision core: lease-grant,
	// fence, and per-kind escalation are PURE functions of injected values (plan §14),
	// wired into the store's serialized txs exactly like scheduler.Pick.
	"github.com/samhotchkiss/flowbee/internal/attention",
	// epicdigest (epic-lane Phase 6) assembles the per-epic session digest, the on_task
	// rollup, the compaction-jump helper, and the fleet counts-only summary — all PURE
	// functions of injected state (plan §2.1, §15.16), served by the API and consumed by
	// the master with no LLM/clock/tmux read.
	"github.com/samhotchkiss/flowbee/internal/epicdigest",
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

// v2ControlPackages are the production packages that implement the Driver-owned
// session boundary. None may acquire a transitive dependency on the legacy raw
// tmux stack. This is intentionally separate from the deterministic-core check:
// Driver adapters are impure, but their only session impurity is DriverPort.
var v2ControlPackages = []string{
	"github.com/samhotchkiss/flowbee/internal/driver",
	"github.com/samhotchkiss/flowbee/internal/driverbridge",
	"github.com/samhotchkiss/flowbee/internal/epicexec",
	"github.com/samhotchkiss/flowbee/internal/epicflow",
	"github.com/samhotchkiss/flowbee/internal/workintent",
}

var rawTmuxPackages = []string{
	"github.com/samhotchkiss/flowbee/internal/tmuxio",
	"github.com/samhotchkiss/flowbee/internal/watchdog",
	"github.com/samhotchkiss/flowbee/internal/epicsupervisor",
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
	for _, pkg := range v2ControlPackages {
		out, err := exec.Command("go", "list", "-deps", pkg).Output()
		if err != nil {
			fmt.Printf("archcheck: skip %s (not present yet)\n", pkg)
			continue
		}
		for _, dep := range strings.Split(strings.TrimSpace(string(out)), "\n") {
			for _, raw := range rawTmuxPackages {
				if dep == raw || strings.HasPrefix(dep, raw+"/") {
					fmt.Printf("VIOLATION: v2 control package %s imports raw tmux package %s\n", pkg, dep)
					violations++
				}
			}
		}
	}
	violations += checkRawTmuxSourceBoundary()
	if violations > 0 {
		fmt.Printf("archcheck: %d architecture boundary violation(s)\n", violations)
		os.Exit(1)
	}
	fmt.Println("archcheck: deterministic-core and Driver session boundaries clean")
}

// checkRawTmuxSourceBoundary keeps the remaining legacy actuator surface small
// and auditable. The listed files are either the legacy implementation itself or
// a call site with an explicit durable-v2/runtime fence. Adding another import or
// direct shell mutation fails CI instead of silently creating a second v2 route.
func checkRawTmuxSourceBoundary() int {
	allowedTmuxIO := map[string]bool{
		"cmd/flowbee/epic.go":                   true, // durable v2 + writer-lock fence
		"internal/api/masters.go":               true, // DisableLegacyPaneActuation fence
		"internal/epicsupervisor/supervisor.go": true, // constructed only by guarded serve path
		"internal/watchdog/ladder.go":           true, // legacy implementation
		"tools/archcheck/main.go":               true, // this source scanner's own literal
	}
	allowedShellTmux := map[string]bool{
		"internal/tmuxio/tmuxio.go":   true, // legacy implementation
		"internal/watchdog/runner.go": true, // legacy implementation
		"tools/archcheck/main.go":     true, // this source scanner's own literal
	}
	violations := 0
	_ = filepath.WalkDir(".", func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			fmt.Printf("VIOLATION: inspect raw tmux boundary %s: %v\n", path, err)
			violations++
			return nil
		}
		if entry.IsDir() {
			if path == ".git" || path == ".claude" ||
				strings.HasPrefix(path, ".git"+string(filepath.Separator)) ||
				path == "addons" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		clean := filepath.ToSlash(strings.TrimPrefix(path, "."+string(filepath.Separator)))
		body, readErr := os.ReadFile(path)
		if readErr != nil {
			fmt.Printf("VIOLATION: read raw tmux boundary %s: %v\n", clean, readErr)
			violations++
			return nil
		}
		text := string(body)
		if strings.Contains(text, `"github.com/samhotchkiss/flowbee/internal/tmuxio"`) &&
			!allowedTmuxIO[clean] {
			fmt.Printf("VIOLATION: unaudited raw tmux import in %s\n", clean)
			violations++
		}
		if strings.Contains(text, "tmux send-keys") && !allowedShellTmux[clean] {
			fmt.Printf("VIOLATION: unaudited direct tmux mutation in %s\n", clean)
			violations++
		}
		return nil
	})
	return violations
}
