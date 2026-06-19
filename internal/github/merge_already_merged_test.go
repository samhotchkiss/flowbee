package github

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestEnqueueMergeQueueAlreadyMerged: a CP crash/restart between the GitHub merge
// succeeding and the outbox row being marked sent re-sends the merge. GitHub answers
// the now-merged PR with a 405 "not mergeable" — indistinguishable by status from a real
// conflict. EnqueueMergeQueue must check the PR's actual state and treat an already-merged
// PR as SUCCESS (so the outbox row consumes cleanly) rather than routing it to a resolver.
func TestEnqueueMergeQueueAlreadyMerged(t *testing.T) {
	cases := []struct {
		name      string
		merged    bool // what the GraphQL PR-state query reports after the 405
		wantErr   bool // expect ErrMergeConflict
		wantNilOK bool // expect nil (merge treated as done)
	}{
		{name: "already merged -> success", merged: true, wantNilOK: true},
		{name: "real conflict -> ErrMergeConflict", merged: false, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.Method == http.MethodPut && strings.HasSuffix(r.URL.Path, "/pulls/5/merge"):
					// every merge attempt 405s "not mergeable" (the ambiguous status).
					w.WriteHeader(http.StatusMethodNotAllowed)
					_, _ = w.Write([]byte(`{"message":"Pull Request is not mergeable"}`))
				case strings.HasSuffix(r.URL.Path, "/graphql"):
					body := `{"data":{"repository":{"pullRequest":{"number":5,"merged":false}}}}`
					if tc.merged {
						body = `{"data":{"repository":{"pullRequest":{"number":5,"merged":true}}}}`
					}
					_, _ = w.Write([]byte(body))
				default:
					t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
					w.WriteHeader(http.StatusInternalServerError)
				}
			}))
			defer srv.Close()

			c := &RealClient{
				Owner: "o", Repo: "r",
				Token:    func(context.Context) (string, error) { return "t", nil },
				HTTP:     srv.Client(),
				Endpoint: srv.URL + "/graphql",
				RESTBase: srv.URL,
			}
			err := c.EnqueueMergeQueue(context.Background(), 5, "")
			switch {
			case tc.wantNilOK && err != nil:
				t.Fatalf("already-merged PR should be success, got err=%v", err)
			case tc.wantErr && !errors.Is(err, ErrMergeConflict):
				t.Fatalf("real conflict should be ErrMergeConflict, got err=%v", err)
			}
		})
	}
}
