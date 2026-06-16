package intake

import "testing"

func TestTaskFromIssueBody_Sections(t *testing.T) {
	body := `Add a /healthz endpoint that returns 200.

## Spec
It must be wired into the router and covered by a test.

## Acceptance Criteria
- GET /healthz returns 200
- a test proves it
`
	got := TaskFromIssueBody(body)
	if got.Text != "Add a /healthz endpoint that returns 200." {
		t.Fatalf("task=%q", got.Text)
	}
	if got.Spec != "It must be wired into the router and covered by a test." {
		t.Fatalf("spec=%q", got.Spec)
	}
	want := "- GET /healthz returns 200\n- a test proves it"
	if got.AcceptanceCriteria != want {
		t.Fatalf("acceptance=%q want %q", got.AcceptanceCriteria, want)
	}
}

func TestTaskFromIssueBody_NoHeadings(t *testing.T) {
	got := TaskFromIssueBody("just do the thing")
	if got.Text != "just do the thing" || got.Spec != "" || got.AcceptanceCriteria != "" {
		t.Fatalf("got %+v", got)
	}
}

func TestTaskFromIssueBody_DoneWhenAlias(t *testing.T) {
	got := TaskFromIssueBody("Fix bug\n\n## Done When\nthe test passes")
	if got.Text != "Fix bug" {
		t.Fatalf("task=%q", got.Text)
	}
	if got.AcceptanceCriteria != "the test passes" {
		t.Fatalf("acceptance=%q", got.AcceptanceCriteria)
	}
}
