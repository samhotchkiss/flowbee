package store

import "testing"

// TestCollapseByPhase: a ready<->leased build-retry churn and a review oscillation
// collapse into one Build / one Review row with an attempt count, instead of many rows.
func TestCollapseByPhase(t *testing.T) {
	raw := []StageTiming{
		{Stage: "ready", DurationS: 0}, {Stage: "leased", DurationS: 15},
		{Stage: "ready", DurationS: 0}, {Stage: "leased", DurationS: 42},
		{Stage: "building", DurationS: 1},
		{Stage: "review_pending", DurationS: 60}, {Stage: "code_review", DurationS: 40},
		{Stage: "mergeable", DurationS: 0}, {Stage: "merging", DurationS: 12},
		{Stage: "done", DurationS: 0, Open: true},
	}
	out := collapseByPhase(raw)
	if len(out) != 4 {
		t.Fatalf("collapsed to %d rows, want 4 (Build, Review, Merge, Done): %+v", len(out), out)
	}
	if out[0].Stage != "Build" || out[0].Attempts != 5 {
		t.Fatalf("row0=%+v, want Build ×5 (the ready/leased/building churn)", out[0])
	}
	if out[0].DurationS != 58 {
		t.Fatalf("Build duration=%d, want summed 58s", out[0].DurationS)
	}
	if out[1].Stage != "Review" || out[1].Attempts != 2 {
		t.Fatalf("row1=%+v, want Review ×2", out[1])
	}
	if out[3].Stage != "Done" || !out[3].Open {
		t.Fatalf("row3=%+v, want open Done", out[3])
	}
}
