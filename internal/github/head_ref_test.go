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

func TestPullRequestReadsHeadOwnershipFactsOnSweepAndRefetch(t *testing.T) {
	var requests int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		raw, _ := io.ReadAll(r.Body)
		var req struct {
			Query string `json:"query"`
		}
		if err := json.Unmarshal(raw, &req); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(req.Query, "headRefName") || !strings.Contains(req.Query, "isCrossRepository") {
			t.Fatalf("ownership fields absent from GraphQL query: %s", req.Query)
		}
		pr := `{"number":71,"headRefName":"epic/owned","isCrossRepository":true,"headRefOid":"head","baseRefOid":"base"}`
		if strings.Contains(req.Query, "query BoardSweep") {
			_, _ = w.Write([]byte(`{"data":{"repository":{"pullRequests":{"pageInfo":{"hasNextPage":false},"nodes":[` + pr + `]},"mergedPullRequests":{"nodes":[]},"closedPullRequests":{"nodes":[]},"issues":{"pageInfo":{"hasNextPage":false},"nodes":[]}},"rateLimit":{"limit":5000,"remaining":4999}}}`))
			return
		}
		_, _ = w.Write([]byte(`{"data":{"repository":{"pullRequest":` + pr + `}}}`))
	}))
	defer srv.Close()

	c := &RealClient{
		Owner: "o", Repo: "r",
		Token: func(context.Context) (string, error) { return "token", nil },
		HTTP:  srv.Client(), Endpoint: srv.URL,
	}
	snap, err := c.BoardSweep(context.Background())
	if err != nil || len(snap.PullRequests) != 1 {
		t.Fatalf("sweep=%+v err=%v", snap, err)
	}
	if got := snap.PullRequests[0]; got.HeadRefName != "epic/owned" || !got.IsCrossRepository {
		t.Fatalf("sweep ownership facts=%+v", got)
	}
	pr, ok, err := c.PullRequest(context.Background(), 71)
	if err != nil || !ok || pr.HeadRefName != "epic/owned" || !pr.IsCrossRepository {
		t.Fatalf("refetch ownership facts=%+v ok=%v err=%v", pr, ok, err)
	}
	if requests != 2 {
		t.Fatalf("requests=%d want 2", requests)
	}
}
