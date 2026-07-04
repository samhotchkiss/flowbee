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
		"<<<<<<<",            // the brief explains the marker format
		"KEEPING BOTH sides", // resolve mechanically, keep both
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

// TestRenderTaskMarkdownCapsOversizedConflictDiff is the regression for the conflict_resolver
// analogue of the review-brief argv bug: this path previously embedded c.Diff UNCONDITIONALLY
// with no size check at all (unlike the review brief, which at least had — and needed a fix
// to — a cap). Any moderately large conflict diff guaranteed "Argument list too long" on
// EVERY attempt, burning all retries and escalating to needs_human no matter how many times
// it was requeued — commit 7b5cc91's lease-release fix only stopped it from thrashing
// silently for hours; it did not stop the escalation. A diff that blows the total brief
// budget must fall back to the .flowbee/original-diff.patch file reference instead.
func TestRenderTaskMarkdownCapsOversizedConflictDiff(t *testing.T) {
	huge := strings.Repeat("x", maxTotalBriefBytes+1)
	c := &client.LeaseContext{Role: "conflict_resolver", Task: "resolve it", Conflict: true, Diff: huge}
	md := renderTaskMarkdown("job-3", c)
	if strings.Contains(md, huge) {
		t.Fatalf("an oversized conflict diff (%d bytes) must NOT be embedded inline — it would "+
			"blow the OS exec argv-string limit and fail every resolve attempt", len(huge))
	}
	if !strings.Contains(md, "original-diff.patch") {
		t.Fatalf("an oversized conflict diff must fall back to the original-diff.patch file reference\n%s", md)
	}

	small := "diff --git a/x.go b/x.go\n@@\n-old\n+new\n"
	md = renderTaskMarkdown("job-4", &client.LeaseContext{Role: "conflict_resolver", Task: "resolve it", Conflict: true, Diff: small})
	if !strings.Contains(md, small) {
		t.Fatalf("a small conflict diff must stay inline\n%s", md)
	}
}
