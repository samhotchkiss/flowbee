package project

import (
	"strings"
	"testing"

	"github.com/samhotchkiss/flowbee/internal/job"
)

// TestIssueTitleClampedToGitHubLimit: a one-paragraph `flowbee spec` task has no
// newline, so its "first line" is the whole 1000+ char prompt. GitHub rejects
// titles over 256 chars with a permanent 422 ("title is too long"), which the
// drain dead-letters — silently killing every spec materialization (russ,
// 2026-06-20..30). Rendered titles must never exceed the GitHub cap.
func TestIssueTitleClampedToGitHubLimit(t *testing.T) {
	long := strings.Repeat("build the thing that does the stuff ", 40) // ~1440 chars, one line
	j := job.Job{ID: "spec1", TaskText: long}

	got := issueTitle(j)
	if n := len([]rune(got)); n > gitHubTitleMax {
		t.Fatalf("issueTitle length=%d exceeds GitHub's %d-char cap — issues.create will 422 and dead-letter", n, gitHubTitleMax)
	}
	if !strings.HasSuffix(got, "…") {
		t.Fatalf("clamped title should signal truncation with an ellipsis, got %q", got)
	}
	if !strings.HasPrefix(got, "build the thing") {
		t.Fatalf("clamped title lost its prefix: %q", got)
	}

	if pr := prTitle(j, "spec1"); len([]rune(pr)) > gitHubTitleMax {
		t.Fatalf("prTitle length=%d exceeds GitHub's %d-char cap", len([]rune(pr)), gitHubTitleMax)
	}
}

// A short title passes through untouched — no ellipsis, no trimming.
func TestIssueTitleShortUnchanged(t *testing.T) {
	j := job.Job{ID: "spec2", TaskText: "# Add a widget\n\ndetails..."}
	if got := issueTitle(j); got != "Add a widget" {
		t.Fatalf("issueTitle = %q, want %q", got, "Add a widget")
	}
}
