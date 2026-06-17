package github

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// newDraftTestClient points a RealClient's GraphQL endpoint at a test handler.
func newDraftTestClient(t *testing.T, handler http.HandlerFunc) *RealClient {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return &RealClient{
		Owner:    "o",
		Repo:     "r",
		Token:    func(context.Context) (string, error) { return "tok", nil },
		HTTP:     srv.Client(),
		Endpoint: srv.URL,
	}
}

// TestConvertToDraftResolvesNodeIDThenMutates: an open PR is drafted back via the
// GraphQL convertPullRequestToDraft mutation — first resolving the node ID from the
// number, then mutating. Proves the M11 zombie compensation actually flips the PR
// (the old REST `draft:true` PATCH was silently a no-op).
func TestConvertToDraftResolvesNodeIDThenMutates(t *testing.T) {
	var sawNodeQuery, sawMutation bool
	var mutatedID string
	c := newDraftTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body struct {
			Query     string         `json:"query"`
			Variables map[string]any `json:"variables"`
		}
		_ = json.Unmarshal(raw, &body)
		switch {
		case strings.Contains(body.Query, "convertPullRequestToDraft"):
			sawMutation = true
			mutatedID, _ = body.Variables["id"].(string)
			_, _ = io.WriteString(w, `{"data":{"convertPullRequestToDraft":{"pullRequest":{"isDraft":true}}}}`)
		case strings.Contains(body.Query, "pullRequest(number"):
			sawNodeQuery = true
			if n, _ := body.Variables["number"].(float64); int(n) != 42 {
				t.Errorf("node query number=%v want 42", body.Variables["number"])
			}
			_, _ = io.WriteString(w, `{"data":{"repository":{"pullRequest":{"id":"PR_node_123","isDraft":false}}}}`)
		default:
			t.Errorf("unexpected query: %s", body.Query)
		}
	})

	if err := c.ConvertToDraft(context.Background(), 42); err != nil {
		t.Fatalf("ConvertToDraft: %v", err)
	}
	if !sawNodeQuery || !sawMutation {
		t.Fatalf("expected node-id query AND mutation; got query=%v mutation=%v", sawNodeQuery, sawMutation)
	}
	if mutatedID != "PR_node_123" {
		t.Fatalf("mutation used id=%q, want PR_node_123 (the resolved node id)", mutatedID)
	}
}

// TestConvertToDraftIdempotentWhenAlreadyDraft: a PR already in draft is left alone
// (no mutation), so the project-OUT sender can safely retry the ActionDraftPR step.
func TestConvertToDraftIdempotentWhenAlreadyDraft(t *testing.T) {
	mutated := false
	c := newDraftTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body struct {
			Query string `json:"query"`
		}
		_ = json.Unmarshal(raw, &body)
		if strings.Contains(body.Query, "convertPullRequestToDraft") {
			mutated = true
			_, _ = io.WriteString(w, `{"data":{"convertPullRequestToDraft":{"pullRequest":{"isDraft":true}}}}`)
			return
		}
		_, _ = io.WriteString(w, `{"data":{"repository":{"pullRequest":{"id":"PR_node_9","isDraft":true}}}}`)
	})

	if err := c.ConvertToDraft(context.Background(), 9); err != nil {
		t.Fatalf("ConvertToDraft (already draft): %v", err)
	}
	if mutated {
		t.Fatalf("must NOT mutate a PR that is already draft")
	}
}

// TestConvertToDraftErrorsWhenPRMissing: a number that resolves to no PR is a hard
// error, not a silent success — the caller must know the compensation didn't land.
func TestConvertToDraftErrorsWhenPRMissing(t *testing.T) {
	c := newDraftTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"data":{"repository":{"pullRequest":null}}}`)
	})
	if err := c.ConvertToDraft(context.Background(), 404); err == nil {
		t.Fatalf("expected an error when the PR does not resolve, got nil")
	}
}
