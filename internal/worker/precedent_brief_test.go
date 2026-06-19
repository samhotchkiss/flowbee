package worker

import (
	"strings"
	"testing"

	"github.com/samhotchkiss/flowbee/client"
)

// TestRenderTaskMarkdownPrecedentPointer: the PRODUCING roles (eng_worker, spec_author)
// are pointed at the durable §F issue archive (docs/history) so the agent itself judges
// relevant precedent — the cross-issue compounding-memory read. The judging roles
// (code_reviewer, conflict_resolver) are NOT (they act on a specific diff, not a fresh
// approach).
func TestRenderTaskMarkdownPrecedentPointer(t *testing.T) {
	for _, role := range []string{"eng_worker", "spec_author"} {
		md := renderTaskMarkdown("j", &client.LeaseContext{Role: role, Task: "do the thing"})
		if !strings.Contains(md, "docs/history") || !strings.Contains(md, "Precedent") {
			t.Errorf("role %s: producing brief must point at the issue archive\n%s", role, md)
		}
	}
	for _, role := range []string{"code_reviewer", "conflict_resolver"} {
		md := renderTaskMarkdown("j", &client.LeaseContext{Role: role, Task: "judge the diff"})
		if strings.Contains(md, "consult the issue archive") {
			t.Errorf("role %s: judging brief must NOT get the precedent pointer\n%s", role, md)
		}
	}
}
