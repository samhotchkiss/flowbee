package github

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestBoardSweepRejectsAllSkippedCI: GitHub aggregates a head whose every check was SKIPPED to
// statusCheckRollup.state=SUCCESS even though no test ran. The parser must set CIHasRealSuccess
// ONLY when a non-skipped check actually concluded SUCCESS (a CheckRun conclusion or a legacy
// StatusContext state), so the merge gate can never read an all-skipped PR as green.
func TestBoardSweepRejectsAllSkippedCI(t *testing.T) {
	rollup := func(ctxNode string) string {
		return `{"commit":{"statusCheckRollup":{"state":"SUCCESS","contexts":{"nodes":[` + ctxNode + `]}}}}`
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		prs := `[` +
			`{"number":10,"headRefOid":"h10","commits":{"nodes":[` + rollup(`{"__typename":"CheckRun","conclusion":"SKIPPED"}`) + `]}},` +
			`{"number":11,"headRefOid":"h11","commits":{"nodes":[` + rollup(`{"__typename":"CheckRun","conclusion":"SUCCESS"}`) + `]}},` +
			`{"number":12,"headRefOid":"h12","commits":{"nodes":[` + rollup(`{"__typename":"StatusContext","state":"SUCCESS"}`) + `]}}` +
			`]`
		body := `{"data":{"repository":{` +
			`"pullRequests":{"pageInfo":{"hasNextPage":false,"endCursor":"C"},"nodes":` + prs + `},` +
			`"mergedPullRequests":{"nodes":[]},` +
			`"issues":{"pageInfo":{"hasNextPage":false,"endCursor":"C"},"nodes":[]}},` +
			`"rateLimit":{"limit":5000,"remaining":4999}}}`
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	c := &RealClient{
		Owner: "o", Repo: "r",
		Token: func(context.Context) (string, error) { return "t", nil },
		HTTP:  srv.Client(), Endpoint: srv.URL + "/graphql",
	}
	snap, err := c.BoardSweep(context.Background())
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	byNum := map[int]PullRequest{}
	for _, pr := range snap.PullRequests {
		byNum[pr.Number] = pr
	}
	if byNum[10].CIRollup != CISuccess || byNum[10].CIHasRealSuccess {
		t.Errorf("all-skipped PR 10: rollup=%q hasRealSuccess=%v — want SUCCESS + false (no test ran)", byNum[10].CIRollup, byNum[10].CIHasRealSuccess)
	}
	if !byNum[11].CIHasRealSuccess {
		t.Error("PR 11 has a real CheckRun SUCCESS — must be CIHasRealSuccess=true")
	}
	if !byNum[12].CIHasRealSuccess {
		t.Error("PR 12 has a real StatusContext SUCCESS — must be CIHasRealSuccess=true")
	}
}
