package github

import (
	"context"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
)

// truncatingServer serves a response that DECLARES more bytes than it writes, so
// the client's io.ReadAll fails mid-body (the same failure shape as a client
// timeout mid-stream / HTTP/2 stream reset / dropped connection under fleet
// load). The server's own "wrote less than declared Content-Length" complaint is
// routed to a discard logger so the test output stays clean.
func truncatingServer(t *testing.T, partial string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", strconv.Itoa(len(partial)+512))
		_, _ = w.Write([]byte(partial))
	}))
	srv.Config.ErrorLog = log.New(io.Discard, "", 0)
	t.Cleanup(srv.Close)
	return srv
}

// TestPullRequestDiffTruncatedBodyErrors: a truncated diff-body read must FAIL the
// fetch, never return ("", nil). The swallowed-ReadAll version returned the partial
// (here: effectively empty) buffer as the PR's AUTHORITATIVE diff — AdoptPRForReview
// then persisted patch_diff='' with diff_empty=1 for a PR with real changes, the
// review grant shipped the stored empty diff, and nothing re-healed it (the sweep's
// adopt gate rightly skips an already-tracked unchanged PR, so the accidental
// per-sweep re-fetch that used to paper over this is gone). Failing here means no
// job is inserted, the gate stays open, and the next sweep retries cleanly. Same
// class as the BoardSweep "unexpected end of JSON input" outage.
//
// The handler truncates ONLY the REST diff GET; the graphQL SHA re-check (a POST)
// answers whole and matching, so the OLD swallowed-read code would sail through
// the re-check and return the partial diff with err=nil — this test discriminates
// the fix, not some later incidental failure.
func TestPullRequestDiffTruncatedBodyErrors(t *testing.T) {
	const partial = "diff --git a/x b/x\n"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost { // the graphQL SHA re-check: valid + matching
			_, _ = w.Write([]byte(`{"data":{"repository":{"pullRequest":{` +
				`"number":77,"baseRefOid":"base","headRefOid":"head"}}},` +
				`"rateLimit":{"limit":5000,"remaining":4999}}`))
			return
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(partial)+512))
		_, _ = w.Write([]byte(partial))
	}))
	srv.Config.ErrorLog = log.New(io.Discard, "", 0)
	defer srv.Close()

	c := &RealClient{
		Owner: "o", Repo: "r",
		Token:    func(context.Context) (string, error) { return "t", nil },
		HTTP:     srv.Client(),
		RESTBase: srv.URL,
		Endpoint: srv.URL,
	}
	diff, err := c.PullRequestDiff(context.Background(), 77, "base", "head")
	if err == nil {
		t.Fatalf("truncated diff read must error, got nil (diff=%q)", diff)
	}
	if diff != "" {
		t.Fatalf("truncated diff read must not return a partial diff: %q", diff)
	}
}

// TestRESTTruncatedBodyErrors: the shared REST helper had the same swallowed
// ReadAll — a truncated 2xx body previously fell through to json.Unmarshal on the
// partial buffer ("unexpected end of JSON input", a permanent-looking decode
// failure on a transport fault). It must surface as the TRANSPORT error (the
// wrapped io.ErrUnexpectedEOF from the broken read) — asserted via errors.Is so
// this test fails against the swallowed-read code, whose json.SyntaxError does
// NOT wrap the read error. The serialized sender classifies a non-ErrGitHub error
// as transient and retries, exactly right for a broken read.
func TestRESTTruncatedBodyErrors(t *testing.T) {
	srv := truncatingServer(t, `{"number":`)
	c := &RealClient{
		Owner: "o", Repo: "r",
		Token:    func(context.Context) (string, error) { return "t", nil },
		HTTP:     srv.Client(),
		RESTBase: srv.URL,
	}
	_, err := c.OpenPR(context.Background(), OpenPRInput{Title: "t", HeadRef: "h", BaseRef: "b"})
	if err == nil {
		t.Fatal("truncated REST body read must error, got nil")
	}
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("truncated REST read must surface the transport error, got: %v", err)
	}
}
