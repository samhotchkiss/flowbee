package github

import (
	"errors"
	"fmt"
	"testing"
)

// TestIsMergeConflictDetection: a GitHub 405 "merge conflicts" / "not mergeable" is
// recognized as an unmergeable conflict (route to a resolver), while transient errors
// and other 405s are not (so they keep their normal retry semantics).
func TestIsMergeConflictDetection(t *testing.T) {
	conflict := []string{
		`rest PUT /pulls/78/merge: 405: {"message":"Pull Request has merge conflicts"}`,
		`rest PUT /pulls/9/merge: 405: {"message":"Pull Request is not mergeable"}`,
	}
	for _, s := range conflict {
		if !isMergeConflict(errors.New(s)) {
			t.Errorf("should be a merge conflict: %q", s)
		}
	}
	notConflict := []string{
		`rest PUT /pulls/1/merge: 500: server error`,
		`dial tcp: connection refused`,
		`405: {"message":"Required status check is expected"}`, // a 405, but not a conflict
	}
	for _, s := range notConflict {
		if isMergeConflict(errors.New(s)) {
			t.Errorf("should NOT be a merge conflict: %q", s)
		}
	}
}

// TestFakeMergeConflictIsTyped: the fake surfaces ErrMergeConflict for a flagged PR so
// project-out tests can drive the route-to-resolver path.
func TestFakeMergeConflictIsTyped(t *testing.T) {
	f := NewFake()
	f.SetMergeConflict(42)
	err := f.EnqueueMergeQueue(nil, 42, "")
	if !errors.Is(err, ErrMergeConflict) {
		t.Fatalf("flagged PR merge err=%v, want ErrMergeConflict", err)
	}
	if err := f.EnqueueMergeQueue(nil, 7, ""); err != nil {
		t.Fatalf("unflagged PR merge err=%v, want nil", err)
	}
	_ = fmt.Sprint(err)
}
