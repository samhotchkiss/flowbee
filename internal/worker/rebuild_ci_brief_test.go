package worker

import (
	"strings"
	"testing"

	"github.com/samhotchkiss/flowbee/client"
)

// TestRenderTaskMarkdownRebuildCIFailures: a bounced build (Rebuild) that carries the
// NAMES of the checks that failed last time must surface each one in the brief and tell
// the agent to reproduce + fix them — so a re-attempt targets the real violation instead
// of rebuilding blind (the loop that stranded the genuinely-fixable tail in needs_human).
func TestRenderTaskMarkdownRebuildCIFailures(t *testing.T) {
	md := renderTaskMarkdown("j", &client.LeaseContext{
		Role:       "eng_worker",
		Task:       "add the thing",
		Rebuild:    true,
		CIFailures: "Architecture and guardrail lints\ngolangci-lint\nBackend fast tests shard 4",
	})
	if !strings.Contains(md, "RE-ATTEMPT") {
		t.Fatalf("rebuild brief missing the re-attempt header\n%s", md)
	}
	for _, want := range []string{
		"Architecture and guardrail lints", "golangci-lint", "Backend fast tests shard 4",
		"FAILED last time", "Reproduce each failing check LOCALLY",
	} {
		if !strings.Contains(md, want) {
			t.Fatalf("rebuild brief missing %q\n%s", want, md)
		}
	}
}

// A rebuild with NO recorded failing checks (e.g. bounced via changes_requested, or an
// older job from before the column existed) still renders the generic rebuild guidance
// and must NOT emit the empty "these checks failed" section.
func TestRenderTaskMarkdownRebuildNoCIFailures(t *testing.T) {
	md := renderTaskMarkdown("j", &client.LeaseContext{Role: "eng_worker", Task: "x", Rebuild: true})
	if !strings.Contains(md, "RE-ATTEMPT") {
		t.Fatalf("rebuild brief missing the re-attempt header\n%s", md)
	}
	if strings.Contains(md, "FAILED last time") {
		t.Fatalf("rebuild brief with no CIFailures must not emit the failing-checks section\n%s", md)
	}
}
