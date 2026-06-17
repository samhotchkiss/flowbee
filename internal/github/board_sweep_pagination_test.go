package github

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestBoardSweepPaginates: a board with more than one page of open issues must be read
// to exhaustion, not truncated at the first page. The server hands out two issue pages
// (page 0 sets hasNextPage + an endCursor; page 1 closes it), includes MERGED PRs only
// when asked (first page), and BoardSweep must accumulate every open PR, every merged PR
// exactly once, and every open issue across both pages.
func TestBoardSweepPaginates(t *testing.T) {
	var sawMergedRequests int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var req struct {
			Variables struct {
				IssueCursor   *string `json:"issueCursor"`
				IncludeMerged bool    `json:"includeMerged"`
			} `json:"variables"`
		}
		_ = json.Unmarshal(raw, &req)
		if req.Variables.IncludeMerged {
			sawMergedRequests++
		}

		// page 0: issueCursor==nil; page 1: issueCursor=="ISSUE_C1".
		first := req.Variables.IssueCursor == nil
		mergedNodes := `[]`
		if req.Variables.IncludeMerged {
			// only requested on the first page
			mergedNodes = `[{"number":900,"merged":true},{"number":901,"merged":true}]`
		}
		var openPRs, issues, issuePI string
		if first {
			openPRs = `[{"number":10},{"number":11}]`
			issues = `[{"number":1},{"number":2}]`
			issuePI = `{"hasNextPage":true,"endCursor":"ISSUE_C1"}`
		} else {
			openPRs = `[]` // open PRs exhausted on page 0
			issues = `[{"number":3},{"number":4}]`
			issuePI = `{"hasNextPage":false,"endCursor":"ISSUE_C2"}`
		}
		body := `{"data":{"repository":{` +
			`"pullRequests":{"pageInfo":{"hasNextPage":false,"endCursor":"PR_C1"},"nodes":` + openPRs + `},` +
			`"mergedPullRequests":{"nodes":` + mergedNodes + `},` +
			`"issues":{"pageInfo":` + issuePI + `,"nodes":` + issues + `}},` +
			`"rateLimit":{"limit":5000,"remaining":4999}}}`
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	c := &RealClient{
		Owner: "o", Repo: "r",
		Token:    func(context.Context) (string, error) { return "t", nil },
		HTTP:     srv.Client(),
		Endpoint: srv.URL + "/graphql",
	}
	snap, err := c.BoardSweep(context.Background())
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}

	// 2 open + 2 merged PRs, all four open issues across both pages.
	if got := len(snap.PullRequests); got != 4 {
		t.Errorf("PRs: got %d want 4 (2 open + 2 merged): %+v", got, snap.PullRequests)
	}
	if got := len(snap.Issues); got != 4 {
		t.Errorf("issues: got %d want 4 (paginated across 2 pages): %+v", got, snap.Issues)
	}
	nums := map[int]bool{}
	for _, iss := range snap.Issues {
		nums[iss.Number] = true
	}
	for _, want := range []int{1, 2, 3, 4} {
		if !nums[want] {
			t.Errorf("missing issue #%d (second page dropped?)", want)
		}
	}
	// merged must be fetched exactly once (first page), not re-pulled every page.
	if sawMergedRequests != 1 {
		t.Errorf("merged PRs requested %d times, want exactly 1 (first page only)", sawMergedRequests)
	}
}
