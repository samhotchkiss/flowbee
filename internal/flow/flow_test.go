package flow

import (
	"os"
	"strings"
	"testing"
)

// TestDefaultFlowsParseClean: the shipped flows/flows.yaml loads with zero
// neutrality violations (providers appear only in model_family:* tags and
// lens.prompt_ref paths).
func TestDefaultFlowsParseClean(t *testing.T) {
	data, err := os.ReadFile("../../flows/flows.yaml")
	if err != nil {
		t.Fatalf("read default flows: %v", err)
	}
	if _, err := Parse(data); err != nil {
		t.Fatalf("default flows should lint clean, got: %v", err)
	}
}

// TestLintFailsOnPlantedProviderLiteral: a provider literal in a control position
// (a `when:` predicate) FAILS the lint, and Parse returns an error (the build
// fails). This is the §5.6 DONE-WHEN at the unit level.
func TestLintFailsOnPlantedProviderLiteral(t *testing.T) {
	planted := `
roles:
  eng_worker:
    requires: ["role:eng_worker", "model_family:*"]
    lens: { prompt_ref: lenses/eng_worker.md }
  code_reviewer:
    requires: ["role:code_reviewer", "model_family:*"]
    lens: { prompt_ref: lenses/code_review.md }
flows:
  build:
    stages:
      build: { role: eng_worker }
      review:
        role: code_reviewer
        when: "model == 'codex'"     # ← PLANTED provider literal in control position
`
	if _, err := Parse([]byte(planted)); err == nil {
		t.Fatal("Parse must FAIL on a provider literal in a when: predicate (§5.6)")
	} else if !strings.Contains(err.Error(), "codex") {
		t.Fatalf("error should name the offending literal, got: %v", err)
	}
}

// TestAllowlistedPositions: providers in the two allowlisted positions
// (model_family:* tag, lens.prompt_ref path) do NOT trip the lint.
func TestAllowlistedPositions(t *testing.T) {
	ok := `
roles:
  eng_worker:
    requires: ["role:eng_worker", "model_family:codex"]   # allowlisted capability tag
    lens: { prompt_ref: lenses/opus_review.md }            # allowlisted prompt path
flows:
  build:
    stages:
      build: { role: eng_worker }
`
	if _, err := Parse([]byte(ok)); err != nil {
		t.Fatalf("allowlisted provider positions must lint clean, got: %v", err)
	}
}

// TestLintCatchesLiteralInIndependenceAndRole: literals in an independence term,
// a role name, and a requires tag (non-model_family) are all caught.
func TestLintCatchesLiteralInIndependenceAndRole(t *testing.T) {
	for name, doc := range map[string]string{
		"role name": `
roles:
  opus_reviewer: { requires: ["role:x"], lens: { prompt_ref: a.md } }
flows: {}`,
		"requires tag": `
roles:
  r: { requires: ["role:claude_only"], lens: { prompt_ref: a.md } }
flows: {}`,
		"independence term": `
roles:
  r: { requires: ["model_family:*"], lens: { prompt_ref: a.md } }
flows:
  build:
    stages: { build: { role: r } }
    independence: ["eng_worker.model_family != gpt"]`,
	} {
		if _, err := Parse([]byte(doc)); err == nil {
			t.Fatalf("[%s] expected a neutrality violation", name)
		}
	}
}

// TestWordBoundaries: a provider substring inside a larger non-provider word does
// NOT trip the lint (e.g. "octopus" must not match "opus").
func TestWordBoundaries(t *testing.T) {
	if findProviderLiteral("octopus garden") != "" {
		t.Fatal("'octopus' must not match the 'opus' literal")
	}
	if findProviderLiteral("model:opus") != "opus" {
		t.Fatal("'model:opus' should match the 'opus' literal at a boundary")
	}
}
