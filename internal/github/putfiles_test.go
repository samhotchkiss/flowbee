package github

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestPutFilesRetriesOnNonFastForward: a concurrent write moves the branch between the ref
// read and the fast-forward update, so the first PATCH 422s (non-fast-forward). PutFiles must
// re-read the new tip, rebuild the tree on it, and retry — not abandon the commit. This is the
// regression the live §F archive hit (a merge landed in the window and the archive was lost).
func TestPutFilesRetriesOnNonFastForward(t *testing.T) {
	var patchCalls, treeCalls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/git/ref/heads/"):
			_ = json.NewEncoder(w).Encode(map[string]any{"object": map[string]string{"sha": "tip"}})
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/git/commits/"):
			_ = json.NewEncoder(w).Encode(map[string]any{"tree": map[string]string{"sha": "basetree"}})
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/git/trees"):
			treeCalls++
			_ = json.NewEncoder(w).Encode(map[string]string{"sha": "newtree"})
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/git/commits"):
			_ = json.NewEncoder(w).Encode(map[string]string{"sha": "newcommit"})
		case r.Method == http.MethodPatch && strings.Contains(r.URL.Path, "/git/refs/heads/"):
			patchCalls++
			if patchCalls == 1 {
				w.WriteHeader(http.StatusUnprocessableEntity) // the tip moved under us
				_, _ = w.Write([]byte(`{"message":"Update is not a fast forward"}`))
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{})
		default:
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	defer srv.Close()

	c := &RealClient{
		Owner: "o", Repo: "r",
		Token:    func(context.Context) (string, error) { return "t", nil },
		HTTP:     srv.Client(),
		RESTBase: srv.URL,
	}
	if err := c.PutFiles(context.Background(),
		map[string][]byte{"docs/history/x.md": []byte("card"), "docs/history/README.md": []byte("toc")},
		"flowbee: archive history for x", "main"); err != nil {
		t.Fatalf("PutFiles must retry past a non-fast-forward and succeed; got %v", err)
	}
	if patchCalls != 2 {
		t.Errorf("expected 2 fast-forward attempts (422 then success), got %d", patchCalls)
	}
	if treeCalls != 2 {
		t.Errorf("expected the tree REBUILT on the retry (re-read tip), got %d tree calls", treeCalls)
	}
}

// TestPutFilesIdempotentUnchangedTree: when the new tree equals the tip's base tree (all files
// already match — a re-drain after a crash), PutFiles makes NO commit and no ref update.
func TestPutFilesIdempotentUnchangedTree(t *testing.T) {
	var commitCalls, patchCalls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/git/ref/heads/"):
			_ = json.NewEncoder(w).Encode(map[string]any{"object": map[string]string{"sha": "tip"}})
		case strings.Contains(r.URL.Path, "/git/commits/") && r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(map[string]any{"tree": map[string]string{"sha": "sametree"}})
		case strings.HasSuffix(r.URL.Path, "/git/trees"):
			_ = json.NewEncoder(w).Encode(map[string]string{"sha": "sametree"}) // unchanged
		case strings.HasSuffix(r.URL.Path, "/git/commits"):
			commitCalls++
			_ = json.NewEncoder(w).Encode(map[string]string{"sha": "c"})
		case r.Method == http.MethodPatch:
			patchCalls++
			_ = json.NewEncoder(w).Encode(map[string]any{})
		}
	}))
	defer srv.Close()
	c := &RealClient{Owner: "o", Repo: "r", Token: func(context.Context) (string, error) { return "t", nil }, HTTP: srv.Client(), RESTBase: srv.URL}
	if err := c.PutFiles(context.Background(), map[string][]byte{"a": []byte("x")}, "m", "main"); err != nil {
		t.Fatalf("PutFiles unchanged: %v", err)
	}
	if commitCalls != 0 || patchCalls != 0 {
		t.Errorf("unchanged tree must make no commit/ref-update; commits=%d patches=%d", commitCalls, patchCalls)
	}
}
