package github

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestEnqueueMergeQueueBaseModifiedIsRetryable: GitHub's 405 "Base branch was modified.
// Review and try the merge again." (a sibling PR merged first, moving main between the
// mergeability check and this merge) must surface as the RETRYABLE ErrMergeBaseModified,
// not a conflict and not a permanent *ErrGitHub — so project-out retries once the base
// settles instead of dead-lettering the loser of a concurrent merge to needs_human.
func TestEnqueueMergeQueueBaseModifiedIsRetryable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusMethodNotAllowed)
		_, _ = w.Write([]byte(`{"message":"Base branch was modified. Review and try the merge again.","status":"405"}`))
	}))
	defer srv.Close()

	c := &RealClient{
		Owner: "o", Repo: "r",
		Token:    func(context.Context) (string, error) { return "t", nil },
		HTTP:     srv.Client(),
		RESTBase: srv.URL,
	}
	err := c.EnqueueMergeQueue(context.Background(), 5)
	if !errors.Is(err, ErrMergeBaseModified) {
		t.Fatalf("base-modified 405 err=%v, want ErrMergeBaseModified", err)
	}
	// must NOT be misclassified as a conflict (which would spin up a resolver)...
	if errors.Is(err, ErrMergeConflict) {
		t.Fatal("base-modified must not be an ErrMergeConflict")
	}
	// ...nor as a permanent *ErrGitHub (which project-out would dead-letter).
	var ghErr *ErrGitHub
	if errors.As(err, &ghErr) {
		t.Fatal("base-modified must not surface as a permanent *ErrGitHub")
	}
}

// TestBaseModifiedDistinctFromConflict: a real conflict 405 still classifies as a
// conflict, not base-modified — the two retry paths must stay separate.
func TestBaseModifiedDistinctFromConflict(t *testing.T) {
	conflict := errors.New(`405: {"message":"Pull Request is not mergeable"}`)
	if isBaseModified(conflict) {
		t.Error("a not-mergeable conflict must not be classified as base-modified")
	}
	baseMod := errors.New(`405: {"message":"Base branch was modified. Review and try the merge again."}`)
	if isMergeConflict(baseMod) {
		t.Error("a base-modified 405 must not be classified as a conflict")
	}
	if !isBaseModified(baseMod) || !strings.Contains(strings.ToLower(baseMod.Error()), "base branch was modified") {
		t.Error("base-modified classifier failed on the canonical message")
	}
}
