package github

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestOpenPRRecoversExistingOnConflict: when a PR already exists for the head branch
// (a prior attempt opened it, or an outbox re-send fired after a crash between create
// and recording the number), GitHub answers POST /pulls with a 422 "already exists".
// OpenPR must recover the existing OPEN PR's number rather than surfacing the 422 —
// otherwise the row dead-letters and the job is stranded with no pr_number.
func TestOpenPRRecoversExistingOnConflict(t *testing.T) {
	var listedHead string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/pulls"):
			w.WriteHeader(http.StatusUnprocessableEntity)
			_, _ = w.Write([]byte(`{"message":"Validation Failed","errors":[{"message":"A pull request already exists for samhotchkiss:flowbee/issue-120."}]}`))
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/pulls"):
			listedHead = r.URL.Query().Get("head")
			_, _ = w.Write([]byte(`[{"number":121}]`))
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	defer srv.Close()

	c := &RealClient{
		Owner: "samhotchkiss", Repo: "flowbee",
		Token:    func(context.Context) (string, error) { return "t", nil },
		HTTP:     srv.Client(),
		RESTBase: srv.URL,
	}
	n, err := c.OpenPR(context.Background(), OpenPRInput{
		Title: "x", HeadRef: "flowbee/issue-120", BaseRef: "main",
	})
	if err != nil {
		t.Fatalf("OpenPR should recover the existing PR, got err=%v", err)
	}
	if n != 121 {
		t.Fatalf("recovered PR number = %d, want 121", n)
	}
	if listedHead != "samhotchkiss:flowbee/issue-120" {
		t.Fatalf("head filter = %q, want owner:branch", listedHead)
	}
}

// TestOpenPRSurfacesOtherValidationErrors: a 422 that is NOT "already exists" (e.g. a
// missing base branch) must keep surfacing as an error — only the already-exists case
// is the idempotent recover path.
func TestOpenPRSurfacesOtherValidationErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			t.Errorf("must NOT do the recovery lookup for a non-already-exists 422")
		}
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = w.Write([]byte(`{"message":"Validation Failed","errors":[{"message":"Field base is invalid"}]}`))
	}))
	defer srv.Close()

	c := &RealClient{
		Owner: "o", Repo: "r",
		Token:    func(context.Context) (string, error) { return "t", nil },
		HTTP:     srv.Client(),
		RESTBase: srv.URL,
	}
	if _, err := c.OpenPR(context.Background(), OpenPRInput{HeadRef: "flowbee/issue-1", BaseRef: "nope"}); err == nil {
		t.Fatal("a non-already-exists 422 must surface as an error")
	}
}
