package store_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/samhotchkiss/flowbee/internal/testutil"
)

// TestAdoptIssueAsBuildParsesAcceptanceCriteria: the flowbee:build intake must parse the issue
// body into task / spec / acceptance the same way the spec-flow adopt path does — otherwise
// the acceptance criteria collapse into TaskText, the worker gets no $FLOWBEE_ACCEPTANCE, and
// the reviewer gate has no done-when to judge the build against (so it builds + merges an
// under-specified thing).
func TestAdoptIssueAsBuildParsesAcceptanceCriteria(t *testing.T) {
	ctx := context.Background()
	st := testutil.NewStore(t)
	body := "Implement a per-IP token bucket on POST /login.\n\n" +
		"## Acceptance Criteria\n- 429 after 5 attempts in 60s\n- successful logins are NOT rate-limited\n\n" +
		"## Spec\nUse a sliding window keyed by client IP.\n"

	id, err := st.AdoptIssueAsBuild(ctx, "acme/api", 42, "Add login rate limiting", body, "base0", time.Unix(1000, 0))
	if err != nil || id == "" {
		t.Fatalf("adopt: id=%q err=%v", id, err)
	}
	j, _ := st.GetJob(ctx, id)

	if !strings.Contains(j.AcceptanceCriteria, "429 after 5 attempts") || !strings.Contains(j.AcceptanceCriteria, "NOT rate-limited") {
		t.Errorf("acceptance criteria not parsed into AcceptanceCriteria; got %q", j.AcceptanceCriteria)
	}
	if !strings.Contains(j.SpecText, "sliding window") {
		t.Errorf("spec not parsed into SpecText; got %q", j.SpecText)
	}
	// the task keeps the title + prose, but NOT the acceptance bullets or the section heading.
	if !strings.Contains(j.TaskText, "Add login rate limiting") || !strings.Contains(j.TaskText, "token bucket") {
		t.Errorf("task should hold title + prose; got %q", j.TaskText)
	}
	if strings.Contains(j.TaskText, "Acceptance Criteria") || strings.Contains(j.TaskText, "429 after 5 attempts") {
		t.Errorf("acceptance content must NOT leak into TaskText (the bug); got %q", j.TaskText)
	}
}

// TestAdoptIssueAsBuildNoSections: a body with no recognized headings stays entirely in the
// task (title + body), with empty spec/acceptance — unchanged from the prior behavior.
func TestAdoptIssueAsBuildNoSections(t *testing.T) {
	ctx := context.Background()
	st := testutil.NewStore(t)
	id, err := st.AdoptIssueAsBuild(ctx, "acme/api", 7, "Fix typo", "Correct 'teh' to 'the' in the README.", "base0", time.Unix(1000, 0))
	if err != nil || id == "" {
		t.Fatalf("adopt: id=%q err=%v", id, err)
	}
	j, _ := st.GetJob(ctx, id)
	if !strings.Contains(j.TaskText, "Fix typo") || !strings.Contains(j.TaskText, "Correct 'teh'") {
		t.Errorf("no-section body should be title + body in task; got %q", j.TaskText)
	}
	if j.AcceptanceCriteria != "" || j.SpecText != "" {
		t.Errorf("no sections => empty spec/acceptance; got spec=%q acc=%q", j.SpecText, j.AcceptanceCriteria)
	}
}
