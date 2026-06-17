package worker

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/samhotchkiss/flowbee/client"
)

func TestIsReviewRole(t *testing.T) {
	for _, r := range []string{"spec_author", "spec_reviewer", "code_reviewer"} {
		if !IsReviewRole(r) {
			t.Fatalf("%s should be a review role", r)
		}
	}
	for _, r := range []string{"eng_worker", "conflict_resolver", ""} {
		if IsReviewRole(r) {
			t.Fatalf("%s should NOT be a review role", r)
		}
	}
}

func TestReadVerdict(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "verdict.json")
	if err := os.WriteFile(p, []byte(`{"decision":"approved","disposition":"self_merge","meets_style":true,"notes":"lgtm"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	v, err := readVerdict(p)
	if err != nil {
		t.Fatal(err)
	}
	if v.Decision != "approved" || v.Disposition != "self_merge" || !v.MeetsStyle {
		t.Fatalf("parsed wrong: %+v", v)
	}
	if _, err := readVerdict(filepath.Join(dir, "missing.json")); err == nil {
		t.Fatal("expected error for missing file")
	}
}

func TestRenderReviewBriefCodeReviewer(t *testing.T) {
	c := &client.LeaseContext{Identity: "senior_code_reviewer", Lens: "correctness", Task: "Add CHANGELOG", Diff: "diff"}
	brief := renderReviewBrief("job-1", "code_reviewer", c)
	for _, want := range []string{"code_reviewer", "senior_code_reviewer", "$FLOWBEE_DIFF_FILE", "$FLOWBEE_VERDICT_FILE", "approved|changes_requested", "Add CHANGELOG"} {
		if !strings.Contains(brief, want) {
			t.Fatalf("code_reviewer brief missing %q\n%s", want, brief)
		}
	}
}

func TestRenderReviewBriefSpecReviewer(t *testing.T) {
	c := &client.LeaseContext{Identity: "engineering_manager", Spec: "# Feature\n\nbody"}
	brief := renderReviewBrief("job-2", "spec_reviewer", c)
	for _, want := range []string{"issue-reviewer", "signed_off|amended|needs_design", "# Feature", "$FLOWBEE_VERDICT_FILE"} {
		if !strings.Contains(brief, want) {
			t.Fatalf("spec_reviewer brief missing %q\n%s", want, brief)
		}
	}
}

// TestRenderReviewBriefSpecAuthor pins the imperative file-write instruction: the
// first live spec_author runs re-claimed because a generic `claude -p` agent emitted
// the spec to stdout instead of writing $FLOWBEE_SPEC_FILE. The brief must tell it to
// WRITE the file (with a concrete shell example) and not just print — and explicitly
// warn that a run writing nothing is discarded.
func TestRenderReviewBriefSpecAuthor(t *testing.T) {
	c := &client.LeaseContext{Identity: "product_speccer", Task: "Add a board command", AcceptanceCriteria: "prints a table"}
	brief := renderReviewBrief("job-3", "spec_author", c)
	for _, want := range []string{"$FLOWBEE_SPEC_FILE", "Add a board command", "prints a table"} {
		if !strings.Contains(brief, want) {
			t.Fatalf("spec_author brief missing %q\n%s", want, brief)
		}
	}
	// it must be imperative about writing the file, not printing.
	if !strings.Contains(brief, "REQUIRED") || !strings.Contains(brief, "Do NOT just print") {
		t.Fatalf("spec_author brief must force a file write, got:\n%s", brief)
	}
}
