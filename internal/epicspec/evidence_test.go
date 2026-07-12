package epicspec

import (
	"strings"
	"testing"
)

func evidenceSpec() Spec {
	return Spec{
		Frontmatter: Frontmatter{Title: "t", Scope: []string{"internal/foo/**"}},
		Goal:        "goal",
		Steps: []Step{
			{N: 1, Text: "do the first thing", Validate: "go test ./internal/foo/..."},
			{N: 2, Text: "do the second thing", Validate: "go test ./internal/foo/bar/..."},
		},
	}
}

func TestCheckEvidenceAllGreen(t *testing.T) {
	sb := StatusBlock{
		State: "done",
		Checklist: []ChecklistItem{
			{Step: 1, Checked: true, Evidence: "go test ./internal/foo/... passed"},
			{Step: 2, Checked: true, Evidence: "go test ./internal/foo/bar/... passed"},
		},
	}
	got := CheckEvidence(evidenceSpec(), sb)
	if !got.Clear {
		t.Fatalf("want Clear, got failures: %v", got.Failures)
	}
	if len(got.Failures) != 0 {
		t.Fatalf("want no failures, got %v", got.Failures)
	}
}

func TestCheckEvidenceUncheckedStep(t *testing.T) {
	sb := StatusBlock{
		State: "done",
		Checklist: []ChecklistItem{
			{Step: 1, Checked: true, Evidence: "ran the test"},
			{Step: 2, Checked: false, Evidence: ""},
		},
	}
	got := CheckEvidence(evidenceSpec(), sb)
	if got.Clear {
		t.Fatal("want NOT clear (step 2 unchecked)")
	}
	if !containsSubstring(got.Failures, "step 2") || !containsSubstring(got.Failures, "unchecked") {
		t.Fatalf("failures should name step 2 as unchecked: %v", got.Failures)
	}
}

func TestCheckEvidenceEmptyEvidence(t *testing.T) {
	sb := StatusBlock{
		State: "done",
		Checklist: []ChecklistItem{
			{Step: 1, Checked: true, Evidence: "ran the test"},
			{Step: 2, Checked: true, Evidence: ""},
		},
	}
	got := CheckEvidence(evidenceSpec(), sb)
	if got.Clear {
		t.Fatal("want NOT clear (step 2 has no evidence)")
	}
	if !containsSubstring(got.Failures, "step 2") || !containsSubstring(got.Failures, "no evidence") {
		t.Fatalf("failures should name step 2's missing evidence: %v", got.Failures)
	}
}

func TestCheckEvidenceMissingStepFromChecklist(t *testing.T) {
	sb := StatusBlock{
		State: "done",
		Checklist: []ChecklistItem{
			{Step: 1, Checked: true, Evidence: "ran the test"},
			// step 2 never appears in the checklist at all.
		},
	}
	got := CheckEvidence(evidenceSpec(), sb)
	if got.Clear {
		t.Fatal("want NOT clear (step 2 missing from checklist)")
	}
	if !containsSubstring(got.Failures, "step 2") || !containsSubstring(got.Failures, "missing") {
		t.Fatalf("failures should name step 2 as missing: %v", got.Failures)
	}
}

func TestCheckEvidenceStateNotDone(t *testing.T) {
	sb := StatusBlock{
		State: "building",
		Checklist: []ChecklistItem{
			{Step: 1, Checked: true, Evidence: "e1"},
			{Step: 2, Checked: true, Evidence: "e2"},
		},
	}
	got := CheckEvidence(evidenceSpec(), sb)
	if got.Clear {
		t.Fatal("want NOT clear (State: building, not done)")
	}
	if !containsSubstring(got.Failures, "State: building") {
		t.Fatalf("failures should name the wrong state: %v", got.Failures)
	}
}

func TestCheckEvidenceStateCaseInsensitive(t *testing.T) {
	sb := StatusBlock{
		State: "DONE",
		Checklist: []ChecklistItem{
			{Step: 1, Checked: true, Evidence: "e1"},
			{Step: 2, Checked: true, Evidence: "e2"},
		},
	}
	got := CheckEvidence(evidenceSpec(), sb)
	if !got.Clear {
		t.Fatalf("State: DONE should match case-insensitively, got failures: %v", got.Failures)
	}
}

func TestCheckEvidenceBlockersPresent(t *testing.T) {
	sb := StatusBlock{
		State:    "done",
		Blockers: "waiting on a design decision",
		Checklist: []ChecklistItem{
			{Step: 1, Checked: true, Evidence: "e1"},
			{Step: 2, Checked: true, Evidence: "e2"},
		},
	}
	got := CheckEvidence(evidenceSpec(), sb)
	if got.Clear {
		t.Fatal("want NOT clear (blockers present)")
	}
	if !containsSubstring(got.Failures, "Blockers:") {
		t.Fatalf("failures should include the blockers text: %v", got.Failures)
	}
}

func TestCheckEvidenceMultipleFailuresAllNamed(t *testing.T) {
	sb := StatusBlock{
		State: "blocked",
		Checklist: []ChecklistItem{
			{Step: 1, Checked: false, Evidence: ""},
		},
	}
	got := CheckEvidence(evidenceSpec(), sb)
	if got.Clear {
		t.Fatal("want NOT clear")
	}
	// State wrong, step 1 unchecked, step 2 missing entirely — three distinct reasons.
	if len(got.Failures) != 3 {
		t.Fatalf("want 3 distinct failures, got %d: %v", len(got.Failures), got.Failures)
	}
}

func TestCheckScopeAllInScope(t *testing.T) {
	scope := []string{"internal/foo/**", "cmd/bar/**.go"}
	touched := []string{"internal/foo/a.go", "internal/foo/sub/b.go", "cmd/bar/main.go"}
	out := CheckScope(scope, touched)
	if len(out) != 0 {
		t.Fatalf("want no out-of-scope paths, got %v", out)
	}
}

func TestCheckScopeOneOutOfScope(t *testing.T) {
	scope := []string{"internal/foo/**"}
	touched := []string{"internal/foo/a.go", "internal/bar/evil.go"}
	out := CheckScope(scope, touched)
	if len(out) != 1 || out[0] != "internal/bar/evil.go" {
		t.Fatalf("want exactly [internal/bar/evil.go], got %v", out)
	}
}

func TestCheckScopeEmptyScopeCoversNothing(t *testing.T) {
	out := CheckScope(nil, []string{"internal/foo/a.go"})
	if len(out) != 1 {
		t.Fatalf("an empty scope: must cover nothing, got %v", out)
	}
}

func TestCheckScopeNoTouchedPaths(t *testing.T) {
	out := CheckScope([]string{"internal/foo/**"}, nil)
	if len(out) != 0 {
		t.Fatalf("no touched paths -> no violations, got %v", out)
	}
}

func containsSubstring(haystack []string, needle string) bool {
	for _, h := range haystack {
		if strings.Contains(h, needle) {
			return true
		}
	}
	return false
}
