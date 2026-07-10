package github

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestPullRequestCapturesFailingCheckURLs(t *testing.T) {
	rollup := `{"commit":{"statusCheckRollup":{"state":"FAILURE","contexts":{"nodes":[` +
		`{"__typename":"CheckRun","name":"backend shard 2","conclusion":"FAILURE","detailsUrl":"https://github.com/acme/api/actions/runs/123"},` +
		`{"__typename":"StatusContext","context":"lint","state":"ERROR","targetUrl":"https://ci.example/lint/456"}` +
		`]}}}}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := `{"data":{"repository":{"pullRequest":{` +
			`"number":42,"headRefOid":"h","baseRefOid":"b","commits":{"nodes":[` + rollup + `]},` +
			`"labels":{"nodes":[]}}}}}`
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	c := &RealClient{
		Owner: "o", Repo: "r",
		Token: func(context.Context) (string, error) { return "t", nil },
		HTTP:  srv.Client(), Endpoint: srv.URL + "/graphql",
	}
	pr, ok, err := c.PullRequest(context.Background(), 42)
	if err != nil {
		t.Fatalf("pull request: %v", err)
	}
	if !ok {
		t.Fatal("pull request not found")
	}
	for name, wantURL := range map[string]string{
		"backend shard 2": "https://github.com/acme/api/actions/runs/123",
		"lint":            "https://ci.example/lint/456",
	} {
		if got := pr.FailingCheckURLs[name]; got != wantURL {
			t.Fatalf("FailingCheckURLs[%q]=%q, want %q", name, got, wantURL)
		}
	}
}
