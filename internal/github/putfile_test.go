package github

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestPutFile covers the three Contents-API paths the §F history archive relies on:
// a NEW file (404 GET -> PUT with no sha = create), an UNCHANGED file (GET matches ->
// NO PUT, idempotent no-op so a crash-replay doesn't mint a redundant commit), and a
// CHANGED file (GET differs -> PUT carrying the blob sha = update in place).
func TestPutFile(t *testing.T) {
	cases := []struct {
		name       string
		getStatus  int
		getContent string // existing content (raw) when getStatus==200
		newContent string
		wantPut    bool   // a PUT is expected
		wantSHA    string // the sha the PUT must carry ("" = create)
	}{
		{name: "create (404)", getStatus: 404, newContent: "hello", wantPut: true, wantSHA: ""},
		{name: "unchanged -> no-op", getStatus: 200, getContent: "hello", newContent: "hello", wantPut: false},
		{name: "update", getStatus: 200, getContent: "old", newContent: "new", wantPut: true, wantSHA: "blob-sha-1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var putBody map[string]any
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/contents/docs/history/x.md"):
					if tc.getStatus == 404 {
						w.WriteHeader(404)
						_, _ = w.Write([]byte(`{"message":"Not Found"}`))
						return
					}
					_, _ = w.Write([]byte(`{"sha":"blob-sha-1","content":"` +
						base64.StdEncoding.EncodeToString([]byte(tc.getContent)) + `"}`))
				case r.Method == http.MethodPut && strings.Contains(r.URL.Path, "/contents/docs/history/x.md"):
					b, _ := io.ReadAll(r.Body)
					_ = json.Unmarshal(b, &putBody)
					_, _ = w.Write([]byte(`{"commit":{"sha":"c1"}}`))
				default:
					t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
					w.WriteHeader(500)
				}
			}))
			defer srv.Close()

			c := &RealClient{
				Owner: "o", Repo: "r",
				Token:    func(context.Context) (string, error) { return "t", nil },
				HTTP:     srv.Client(),
				RESTBase: srv.URL,
			}
			if err := c.PutFile(context.Background(), "docs/history/x.md", []byte(tc.newContent), "msg", "main"); err != nil {
				t.Fatalf("PutFile: %v", err)
			}
			if !tc.wantPut {
				if putBody != nil {
					t.Fatalf("unchanged content must NOT PUT (idempotent), but PUT fired: %v", putBody)
				}
				return
			}
			if putBody == nil {
				t.Fatal("expected a PUT, none fired")
			}
			// the content is sent base64-encoded; the branch rides along.
			if got, _ := base64.StdEncoding.DecodeString(putBody["content"].(string)); string(got) != tc.newContent {
				t.Fatalf("PUT content=%q want %q", got, tc.newContent)
			}
			if putBody["branch"] != "main" {
				t.Fatalf("PUT branch=%v want main", putBody["branch"])
			}
			sha, _ := putBody["sha"].(string)
			if sha != tc.wantSHA {
				t.Fatalf("PUT sha=%q want %q (create carries no sha; update carries the blob sha)", sha, tc.wantSHA)
			}
		})
	}
}
