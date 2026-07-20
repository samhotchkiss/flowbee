// Command archcheck enforces the deterministic-core boundary (DESIGN §1.2):
// the core packages must import no clock, randomness, ID minter, GitHub client,
// or LLM/agent package — so the core stays a pure, replayable function of
// persisted facts. Missing packages (early milestones) are skipped, so this is
// green from M0 and bites the moment a core package is added that violates it.
package main

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
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
	violations += checkP1HumanNotificationBoundary(".")
	if violations > 0 {
		fmt.Printf("archcheck: %d architecture boundary violation(s)\n", violations)
		os.Exit(1)
	}
	fmt.Println("archcheck: deterministic-core and Driver session boundaries clean")
}

// checkP1HumanNotificationBoundary makes the Phase-1 Interactor-only alert
// contract structural. A generic outbound alert package or the retired
// claim/2xx-ack store surface would create a second delivery truth alongside
// exact Driver processing evidence, so they are forbidden production symbols.
func checkP1HumanNotificationBoundary(root string) int {
	violations := 0
	forbiddenSymbols := []string{
		"ClaimNextControlAlert", "AcknowledgeControlAlert", "RetryControlAlert",
		"DeadLetterControlAlert", "ReclaimExpiredControlAlerts", "WebhookSink",
	}
	_ = filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			fmt.Printf("VIOLATION: inspect P1 notification boundary %s: %v\n", path, err)
			violations++
			return nil
		}
		if entry.IsDir() {
			if path == ".git" || path == ".claude" || path == "addons" ||
				strings.HasPrefix(path, ".git"+string(filepath.Separator)) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			fmt.Printf("VIOLATION: resolve P1 notification boundary %s: %v\n", path, relErr)
			violations++
			return nil
		}
		clean := filepath.ToSlash(rel)
		if clean == "tools/archcheck/main.go" {
			return nil
		}
		if strings.HasPrefix(clean, "internal/alerting/") {
			fmt.Printf("VIOLATION: generic outbound alert package restored in %s\n", clean)
			violations++
		}
		body, readErr := os.ReadFile(path)
		if readErr != nil {
			fmt.Printf("VIOLATION: read P1 notification boundary %s: %v\n", clean, readErr)
			violations++
			return nil
		}
		text := string(body)
		for _, symbol := range forbiddenSymbols {
			if strings.Contains(text, symbol) {
				fmt.Printf("VIOLATION: forbidden external alert acknowledgement symbol %s in %s\n", symbol, clean)
				violations++
			}
		}
		if clean != "cmd/flowbee/watchdog.go" && strings.Contains(text, "FLOWBEE_ALERT_WEBHOOK_URL") {
			fmt.Printf("VIOLATION: watchdog ingress URL became a production outbound sink in %s\n", clean)
			violations++
		}
		return nil
	})
	return violations
}

// checkRawTmuxSourceBoundary keeps the remaining legacy actuator surface small
// and auditable. The listed files are either the legacy implementation itself or
// a call site with an explicit durable-v2/runtime fence. Adding another import or
// direct shell mutation fails CI instead of silently creating a second v2 route.
func checkRawTmuxSourceBoundary() int {
	violations := scanRawTmuxSources(".")
	violations += checkLegacyTmuxFenceInvariants(".")
	return violations
}

var allowedRawImports = map[string]map[string]bool{
	"github.com/samhotchkiss/flowbee/internal/tmuxio": {
		"cmd/flowbee/epic.go":                   true, // durable v2 + writer-lock fence
		"internal/api/masters.go":               true, // DisableLegacyPaneActuation fence
		"internal/epicsupervisor/supervisor.go": true, // constructed only by guarded serve path
		"internal/watchdog/ladder.go":           true, // legacy implementation
	},
	"github.com/samhotchkiss/flowbee/internal/watchdog": {
		"cmd/flowbee/epic.go":  true, // durable v2 + writer-lock fence
		"cmd/flowbee/serve.go": true, // legacyPaneRuntimeEnabled fence
	},
	"github.com/samhotchkiss/flowbee/internal/epicsupervisor": {
		"cmd/flowbee/serve.go": true, // legacyPaneRuntimeEnabled fence
	},
}

var allowedRawMutationFiles = map[string]bool{
	"internal/tmuxio/send.go":      true,
	"internal/tmuxio/lifecycle.go": true,
	"internal/tmuxio/tmuxio.go":    true,
	"internal/watchdog/runner.go":  true,
	"tools/archcheck/main.go":      true,
}

// scanRawTmuxSources audits production Go syntax rather than comments. It
// catches direct raw-package imports, shell literals split across argv
// (`exec.Command("tmux", "send-keys", ...)`), standalone tmux-send wrappers,
// and product materializers attempting to author a session-origin route.
func scanRawTmuxSources(root string) int {
	violations := 0
	_ = filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
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
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			fmt.Printf("VIOLATION: resolve raw tmux boundary %s: %v\n", path, relErr)
			violations++
			return nil
		}
		clean := filepath.ToSlash(rel)
		parsed, parseErr := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if parseErr != nil {
			fmt.Printf("VIOLATION: parse raw tmux boundary %s: %v\n", clean, parseErr)
			violations++
			return nil
		}
		importsExec := false
		for _, imp := range parsed.Imports {
			name, unquoteErr := strconv.Unquote(imp.Path.Value)
			if unquoteErr != nil {
				continue
			}
			if owners, raw := allowedRawImports[name]; raw && !owners[clean] {
				fmt.Printf("VIOLATION: unaudited raw session-control import %s in %s\n", name, clean)
				violations++
			}
			if name == "os/exec" && strings.HasPrefix(clean, "internal/driver/") {
				fmt.Printf("VIOLATION: Driver boundary directly imports process execution in %s\n", clean)
				violations++
			}
			importsExec = importsExec || name == "os/exec"
		}
		hasTmuxBinaryLiteral := false
		ast.Inspect(parsed, func(node ast.Node) bool {
			lit, ok := node.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			value, unquoteErr := strconv.Unquote(lit.Value)
			if unquoteErr != nil {
				return true
			}
			lower := strings.ToLower(value)
			hasTmuxBinaryLiteral = hasTmuxBinaryLiteral || lower == "tmux" || strings.HasSuffix(lower, "/tmux")
			if !allowedRawMutationFiles[clean] &&
				(strings.Contains(lower, "send-keys") || strings.Contains(lower, "tmux-send")) {
				fmt.Printf("VIOLATION: unaudited direct session mutation literal in %s\n", clean)
				violations++
			}
			return true
		})
		if importsExec && hasTmuxBinaryLiteral && !allowedRawMutationFiles[clean] {
			fmt.Printf("VIOLATION: unaudited direct tmux process execution in %s\n", clean)
			violations++
		}
		// Session-origin transport remains in internal/driver solely as a wire
		// compatibility parser. Product/store code may materialize only the
		// flowbee-control principal origin; no session may author another's work.
		body, readErr := os.ReadFile(path)
		if readErr != nil {
			fmt.Printf("VIOLATION: read raw tmux boundary %s: %v\n", clean, readErr)
			violations++
			return nil
		}
		text := string(body)
		if !strings.HasPrefix(clean, "internal/driver/") && clean != "tools/archcheck/main.go" &&
			(strings.Contains(text, "sender_session_id") || strings.Contains(text, "SenderSessionID")) {
			fmt.Printf("VIOLATION: product session-origin materialization surface in %s\n", clean)
			violations++
		}
		return nil
	})
	return violations
}

// checkLegacyTmuxFenceInvariants makes each remaining production legacy call
// site prove its durable v2 fence. The legacy implementation stays available
// for explicit rollback, but v2 can never reach it by toggling an old watcher.
func checkLegacyTmuxFenceInvariants(root string) int {
	required := map[string][]string{
		"cmd/flowbee/epic.go": {
			"durableV2, err := st.DurableEpicReviewHandoffV2(ctx)",
			"if st.EnableEpicReviewHandoffV2 {\n\t\t\treturn legacyEpicTmuxCommandDisabled(\"start\")",
			"if st.EnableEpicReviewHandoffV2 {\n\t\t\treturn legacyEpicTmuxCommandDisabled(\"abandon\")",
		},
		"cmd/flowbee/serve.go": {
			"DisableLegacyPaneActuation: st.EnableEpicReviewHandoffV2",
			"legacyPaneRuntimeEnabled(st.EnableEpicReviewHandoffV2, cfg.SessionWatchDisabled)",
			"legacyPaneRuntimeEnabled(st.EnableEpicReviewHandoffV2, cfg.EpicSupervisionDisabled)",
		},
		"internal/api/masters.go": {
			"if s.disableLegacyPaneActuation {",
			"legacy pane delivery is disabled; v2 messages require a durable Driver grant and receipt",
		},
	}
	violations := 0
	for name, snippets := range required {
		body, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(name)))
		if err != nil {
			fmt.Printf("VIOLATION: read legacy tmux fence %s: %v\n", name, err)
			violations++
			continue
		}
		text := string(body)
		for _, snippet := range snippets {
			if !strings.Contains(text, snippet) {
				fmt.Printf("VIOLATION: legacy tmux call site %s lost required durable-v2 fence %q\n", name, snippet)
				violations++
			}
		}
	}
	return violations
}
