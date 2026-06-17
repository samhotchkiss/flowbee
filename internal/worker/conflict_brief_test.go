package worker

import (
	"strings"
	"testing"

	"github.com/samhotchkiss/flowbee/client"
)

// TestRenderTaskMarkdownConflictBrief: a conflict_resolver lease (Conflict=true) gets a
// resolution-framed brief that includes the original intended change and tells the agent
// to re-apply that intent on the current code — NOT to re-run the original task (which is
// why the resolver previously "produced no changes" and conflicts never auto-resolved).
func TestRenderTaskMarkdownConflictBrief(t *testing.T) {
	c := &client.LeaseContext{
		Role: "conflict_resolver", Task: "Change phrase X to Y", BaseSHA: "main-tip",
		Conflict: true, Diff: "diff --git a/docs/x.md b/docs/x.md\n@@\n-old phrase\n+new phrase\n",
	}
	md := renderTaskMarkdown("job-1", c)
	for _, want := range []string{
		"CONFLICT RESOLUTION",
		"re-apply your ORIGINAL INTENT",
		"reconciling the two",
		"diff --git a/docs/x.md", // the original patch is shown to the agent
	} {
		if !strings.Contains(md, want) {
			t.Errorf("conflict brief missing %q\n--- rendered ---\n%s", want, md)
		}
	}

	// a normal build (no Conflict) must NOT get the conflict-resolution brief.
	plain := renderTaskMarkdown("job-2", &client.LeaseContext{Role: "eng_worker", Task: "build it"})
	if strings.Contains(plain, "CONFLICT RESOLUTION") {
		t.Errorf("non-conflict build must not get the conflict-resolution brief\n%s", plain)
	}
}
