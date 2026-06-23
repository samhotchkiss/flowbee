package github

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestEnqueueMergeQueueRequiredCheckExpectedIsRetryable: a ruleset 405 saying a
// required status check is still "expected" is a pending-CI race, not a poison 4xx.
func TestEnqueueMergeQueueRequiredCheckExpectedIsRetryable(t *testing.T) {
	err := mergeErrorFromREST(t, `{"message":"Repository rule violations found\nRequired status check \"Migration version guard\" is expected.","status":"405"}`)
	if !errors.Is(err, ErrMergeRuleViolationPending) {
		t.Fatalf("required-check ruleset 405 err=%v, want ErrMergeRuleViolationPending", err)
	}
	if errors.Is(err, ErrMergeConflict) || errors.Is(err, ErrMergeBaseModified) {
		t.Fatalf("required-check ruleset 405 must not be conflict/base-modified: %v", err)
	}
	var ghErr *ErrGitHub
	if errors.As(err, &ghErr) {
		t.Fatal("required-check ruleset 405 must not surface as permanent *ErrGitHub")
	}
	if IsMergeRuleBehind(err) {
		t.Fatal("required-check pending must not request update-branch")
	}
}

// TestEnqueueMergeQueueBehindRuleViolationIsRetryable: a ruleset 405 requiring the branch
// to be up to date is also retryable, and carries the signal project-out uses to FF it.
func TestEnqueueMergeQueueBehindRuleViolationIsRetryable(t *testing.T) {
	err := mergeErrorFromREST(t, `{"message":"Repository rule violations found\nThis branch must be up to date with the base branch before merging.","status":"405"}`)
	if !errors.Is(err, ErrMergeRuleViolationPending) {
		t.Fatalf("behind ruleset 405 err=%v, want ErrMergeRuleViolationPending", err)
	}
	if !IsMergeRuleBehind(err) {
		t.Fatal("behind ruleset 405 must request update-branch")
	}
	var ghErr *ErrGitHub
	if errors.As(err, &ghErr) {
		t.Fatal("behind ruleset 405 must not surface as permanent *ErrGitHub")
	}
}

func mergeErrorFromREST(t *testing.T, body string) error {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusMethodNotAllowed)
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	c := &RealClient{
		Owner: "o", Repo: "r",
		Token:    func(context.Context) (string, error) { return "t", nil },
		HTTP:     srv.Client(),
		RESTBase: srv.URL,
	}
	return c.EnqueueMergeQueue(context.Background(), 5, "")
}
