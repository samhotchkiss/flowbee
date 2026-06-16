// F14 acceptance: documentation reconciliation (the docs must match built reality).
//
// This is the docs milestone — a real, non-skipped doc-lint that fails CI if DESIGN.md
// or BUILD.md drift back to the pre-build narrative. It asserts the decisions the build
// actually made:
//
// DONE-WHEN (each proven below by a real, non-skipped assertion):
//   - the STORE OF RECORD is SQLite, not Postgres/River: DESIGN §12.3 (the store
//     section) and BUILD §3.2 (the library table) name SQLite as the store and do NOT
//     assert Postgres/River as the substrate (the only permitted mentions are explicit
//     "dropped / not used / reconciled-away" negations);
//   - DETERMINISM is recorded as an explicit invariant (I-0) in DESIGN;
//   - the §8 / I-8 reframe is recorded: Flowbee owns actor identity, branch protection
//     is a STRUCTURAL gate, not the reviewer-not-equal-author arbiter;
//   - Mode A (claude -p / codex, subscription-covered, harness-driven) is the default;
//   - GitHub auth: a fine-grained repo-scoped PAT for the single operator, a GitHub App
//     for multi-repo / org scale;
//   - §14 is RESOLVED = Branch B (autonomous merge, no human gate), configurable via
//     Policy.AllowSelfMerge — recorded in BOTH DESIGN §14 and BUILD.
//
// The test reads the REAL committed DESIGN.md / BUILD.md from the repo root (found by
// walking up to go.mod via repoRoot, shared with the F13 onboarding test). No store, no
// network, no GitHub — a pure file-content check, as a docs milestone should be.
package acceptance

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

func readDoc(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(repoRoot(t), name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return string(b)
}

// section returns the body of the Markdown section whose heading line matches headingRe,
// from just after that heading up to (but not including) the next heading at the same or
// shallower depth. depth is the number of leading '#' on the target heading.
func section(t *testing.T, doc, headingRe string, depth int) string {
	t.Helper()
	lines := strings.Split(doc, "\n")
	head := regexp.MustCompile(headingRe)
	start := -1
	for i, ln := range lines {
		if head.MatchString(ln) {
			start = i
			break
		}
	}
	if start < 0 {
		t.Fatalf("section heading not found: %q", headingRe)
	}
	// A boundary is any heading with <= depth hashes.
	boundary := regexp.MustCompile(`^#{1,` + strconv.Itoa(depth) + `} `)
	end := len(lines)
	for i := start + 1; i < len(lines); i++ {
		if boundary.MatchString(lines[i]) {
			end = i
			break
		}
	}
	return strings.Join(lines[start:end], "\n")
}

// containsAny reports whether hay contains any of needles (case-insensitive).
func containsAny(hay string, needles ...string) bool {
	low := strings.ToLower(hay)
	for _, n := range needles {
		if strings.Contains(low, strings.ToLower(n)) {
			return true
		}
	}
	return false
}

func mustContain(t *testing.T, where, body string, needles ...string) {
	t.Helper()
	for _, n := range needles {
		if !strings.Contains(body, n) {
			t.Errorf("%s: expected to contain %q but it did not", where, n)
		}
	}
}

// TestF14_StoreOfRecordIsSQLiteNotPostgresRiver is the keystone DONE-WHEN check:
// the canonical store sections (DESIGN §12.3, BUILD §3.2) must name SQLite as the
// store of record and must NOT carry a surviving Postgres/River store-of-record claim.
func TestF14_StoreOfRecordIsSQLiteNotPostgresRiver(t *testing.T) {
	design := readDoc(t, "DESIGN.md")
	build := readDoc(t, "BUILD.md")

	// --- DESIGN §12.3 (the store section). ---
	store := section(t, design, `^### 12\.3 Store:`, 3)
	if !containsAny(store, "SQLite") {
		t.Fatalf("DESIGN §12.3 must name SQLite as the store of record")
	}
	if !containsAny(store, "modernc.org/sqlite") {
		t.Errorf("DESIGN §12.3 should reference the pure-Go driver modernc.org/sqlite")
	}
	// The heading itself must say SQLite (not "River/Postgres underneath").
	headLine := strings.SplitN(store, "\n", 2)[0]
	if containsAny(headLine, "River", "Postgres") {
		t.Errorf("DESIGN §12.3 heading still names River/Postgres as the substrate: %q", headLine)
	}

	// Any surviving "River"/"Postgres" line in §12.3 must be an explicit negation /
	// reconciliation note, never a positive store-of-record assertion.
	assertOnlyNegatedSubstrate(t, "DESIGN §12.3", store)

	// --- BUILD §3.2 (the locked library table). ---
	lib := section(t, build, `^### 3\.2 Library choices`, 3)
	if !containsAny(lib, "modernc.org/sqlite") {
		t.Fatalf("BUILD §3.2 must lock the store to modernc.org/sqlite")
	}
	// The Store/DB-driver row must not present pgx/Postgres/River as the live choice.
	assertOnlyNegatedSubstrate(t, "BUILD §3.2", lib)

	// --- Repo-wide: no Makefile/compose Postgres dependency claim left as live. ---
	// (Spot-check the canonical store vocabulary line in DESIGN's reading guide.)
	if !strings.Contains(design, "never Postgres/River") {
		t.Errorf("DESIGN canonical vocabulary should mark the store 'never Postgres/River'")
	}
}

// assertOnlyNegatedSubstrate scans body line-by-line; any line that mentions River or
// Postgres (as a substrate) must also carry an explicit negation/reconciliation marker.
// This is what lets the docs keep "we dropped River" sentences while failing if a
// positive "Flowbee runs River/Postgres as the store" sentence ever returns.
func assertOnlyNegatedSubstrate(t *testing.T, where, body string) {
	t.Helper()
	subRe := regexp.MustCompile(`(?i)\b(river|postgres|pgx|pgxpool|riverqueue|testcontainers)\b`)
	// Markers that make a mention legitimate (a negation or reconciliation note).
	negators := []string{
		"drop", "dropped", "not used", "not required", "no river", "no postgres",
		"no pgx", "replaces", "reconcil", "pre-build", "draft named", "draft inverted",
		"never postgres", "not postgres", "instead of", "in place of", "moot",
		"there is no river", "no external", "without an external", "rather than",
	}
	for _, ln := range strings.Split(body, "\n") {
		if !subRe.MatchString(ln) {
			continue
		}
		if !containsAny(ln, negators...) {
			t.Errorf("%s: line names a dropped substrate without a negation/reconciliation marker:\n  %s", where, strings.TrimSpace(ln))
		}
	}
}

// TestF14_DeterminismRecordedAsInvariant proves I-0 (determinism) is an explicit invariant.
func TestF14_DeterminismRecordedAsInvariant(t *testing.T) {
	design := readDoc(t, "DESIGN.md")
	inv := section(t, design, `^## 15\. Correctness invariants`, 2)
	mustContain(t, "DESIGN §15", inv, "I-0")
	if !containsAny(inv, "deterministic, replayable function of persisted facts") {
		t.Errorf("DESIGN §15 must state I-0 as 'a deterministic, replayable function of persisted facts'")
	}
	// The summary §3.6 must also carry I-0 and the archcheck enforcement.
	sum := section(t, design, `^### 3\.6 The invariants`, 3)
	mustContain(t, "DESIGN §3.6", sum, "I-0", "archcheck")
}

// TestF14_BranchProtectionReframe proves the §8 / I-8 reframe is recorded.
func TestF14_BranchProtectionReframe(t *testing.T) {
	design := readDoc(t, "DESIGN.md")
	bp := section(t, design, `^### 9\.6 Branch protection`, 3)
	// Branch protection is a STRUCTURAL gate, not the reviewer-not-equal-author arbiter.
	if !containsAny(bp, "structural") {
		t.Errorf("DESIGN §9.6 must frame branch protection as a STRUCTURAL gate")
	}
	if !containsAny(bp, "Flowbee owns actor identity", "owns actor identity") {
		t.Errorf("DESIGN §9.6 must state Flowbee owns actor identity")
	}
	if !containsAny(bp, "not", "never") || !containsAny(bp, "arbiter") {
		t.Errorf("DESIGN §9.6 must say branch protection is NOT the reviewer-not-equal-author arbiter")
	}
	// I-8 in §15 must be reframed too.
	i8 := regexp.MustCompile(`(?s)\*\*I-8 —.*?\(§9\.6`).FindString(design)
	if i8 == "" {
		t.Fatalf("DESIGN §15 I-8 entry not found")
	}
	if !containsAny(i8, "structural") {
		t.Errorf("I-8 must call branch protection a STRUCTURAL backstop, got: %q", i8)
	}
}

// TestF14_ModeADefaultAndAuth proves Mode-A-default + PAT/App auth guidance.
func TestF14_ModeADefaultAndAuth(t *testing.T) {
	design := readDoc(t, "DESIGN.md")

	// Mode A is the default (harness-driven claude -p / codex, subscription-covered).
	if !containsAny(design, "Mode A is the default", "Mode A** is the") {
		t.Errorf("DESIGN must lead with Mode A as the default worker mode")
	}
	if !containsAny(design, "subscription-covered") {
		t.Errorf("DESIGN must note claude -p is subscription-covered (the Mode-A driver)")
	}

	// PAT for single operator, App for multi-repo / org scale (§8.3).
	id := section(t, design, `^### 8\.3 Identity:`, 3)
	if !containsAny(id, "PAT") {
		t.Errorf("DESIGN §8.3 must mention a fine-grained repo-scoped PAT for the single operator")
	}
	if !containsAny(id, "single operator", "single-operator") {
		t.Errorf("DESIGN §8.3 must tie the PAT to the single-operator default")
	}
	if !containsAny(id, "App") || !containsAny(id, "multi-repo", "org-scale", "org/multi-repo", "org scale") {
		t.Errorf("DESIGN §8.3 must reserve a GitHub App for multi-repo / org scale")
	}
}

// TestF14_Section14ResolvedBranchB proves THE ONE DECISION is recorded as RESOLVED = Branch B
// in BOTH docs, and tied to the configurable Policy.AllowSelfMerge toggle.
func TestF14_Section14ResolvedBranchB(t *testing.T) {
	design := readDoc(t, "DESIGN.md")
	build := readDoc(t, "BUILD.md")

	// DESIGN §14 heading + body must record the resolution.
	s14 := section(t, design, `^## 14\. THE ONE DECISION`, 2)
	if !containsAny(s14, "RESOLVED") {
		t.Fatalf("DESIGN §14 must record the decision as RESOLVED")
	}
	if !containsAny(s14, "Branch B") {
		t.Errorf("DESIGN §14 must record the resolution as Branch B")
	}
	if !containsAny(s14, "AllowSelfMerge") {
		t.Errorf("DESIGN §14 must name the configurable Policy.AllowSelfMerge toggle")
	}
	if !containsAny(s14, "autonomous merge", "no human", "without a human") {
		t.Errorf("DESIGN §14 must state autonomous merge / no human gate")
	}
	// The §14 heading itself should announce the resolution (not pose it as open).
	head14 := strings.SplitN(s14, "\n", 2)[0]
	if !containsAny(head14, "RESOLVED") {
		t.Errorf("DESIGN §14 heading should announce RESOLVED, got: %q", head14)
	}

	// BUILD must record the resolution too (the reconciliation banner + roadmap note).
	if !containsAny(build, "RESOLVED") || !containsAny(build, "Branch B") {
		t.Errorf("BUILD must record §14 RESOLVED = Branch B")
	}
	if !containsAny(build, "AllowSelfMerge") {
		t.Errorf("BUILD must name the configurable Policy.AllowSelfMerge toggle")
	}

	// Cross-check: the built config field exists with the documented env var, so the
	// doc claim is grounded in code (not aspirational).
	cfg := readDoc(t, filepath.Join("internal", "config", "config.go"))
	mustContain(t, "internal/config/config.go", cfg, "AllowSelfMerge", "FLOWBEE_ALLOW_SELF_MERGE")
}
