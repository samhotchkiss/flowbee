package epicspec

import (
	"strings"
	"testing"
)

func TestRenderCriteriaIncludesGoalConstraintsStepsAndInstructions(t *testing.T) {
	spec := Spec{
		Goal:        "Ship the review gate.",
		Constraints: "Do not touch the migration.",
		Steps: []Step{
			{N: 1, Text: "wire the parser", Validate: "go test ./internal/epicspec/..."},
			{N: 2, Text: "wire the gate", Validate: "go test ./internal/project/..."},
		},
	}
	out := RenderCriteria(spec)
	for _, want := range []string{
		"Ship the review gate.",
		"Do not touch the migration.",
		"1. wire the parser",
		"Validate: go test ./internal/epicspec/...",
		"2. wire the gate",
		"Epic-Step: N/M",
		"claimed done but with NO",
		"RUN THE CODE",
		"run targeted tests",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("RenderCriteria output missing %q; got:\n%s", want, out)
		}
	}
}

func TestRenderChecklistShowsCheckedAndUnchecked(t *testing.T) {
	sb := StatusBlock{
		State:    "done",
		Blockers: "",
		Checklist: []ChecklistItem{
			{Step: 1, Checked: true, Text: "wire the parser", Evidence: "go test passed"},
			{Step: 2, Checked: false, Text: "wire the gate"},
		},
	}
	out := RenderChecklist(sb)
	for _, want := range []string{
		"State: done",
		"[x] Step 1 — wire the parser (evidence: go test passed)",
		"[ ] Step 2 — wire the gate (evidence: (NO EVIDENCE GIVEN))",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("RenderChecklist output missing %q; got:\n%s", want, out)
		}
	}
}
