package client

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestCreateSpecWireContract: CreateSpec POSTs the work item to /v1/specs with the exact
// field names the server's specCreate handler decodes, and returns the seeded job id + state.
func TestCreateSpecWireContract(t *testing.T) {
	var gotPath string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"job_id":"01J-SPEC","state":"spec_pending"}`))
	}))
	defer srv.Close()

	c := New(srv.URL)
	id, state, err := c.CreateSpec(context.Background(), SpecRequest{
		Task: "add request timeouts", Title: "HTTP timeouts", Acceptance: "ctx honored", Repo: "flowbee",
	})
	if err != nil {
		t.Fatalf("CreateSpec: %v", err)
	}
	if id != "01J-SPEC" || state != "spec_pending" {
		t.Fatalf("got (%q,%q), want (01J-SPEC, spec_pending)", id, state)
	}
	if gotPath != "/v1/specs" {
		t.Errorf("posted to %q, want /v1/specs", gotPath)
	}
	// field names must match the server's decoder (task/title/acceptance/repo).
	for k, want := range map[string]string{"task": "add request timeouts", "title": "HTTP timeouts", "acceptance": "ctx honored", "repo": "flowbee"} {
		if gotBody[k] != want {
			t.Errorf("body[%q]=%v, want %q", k, gotBody[k], want)
		}
	}
}

// TestCreateSpecNon200 surfaces a server rejection (e.g. 409 conflict) as an error, not a
// silent empty job id.
func TestCreateSpecNon200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "duplicate id", http.StatusConflict)
	}))
	defer srv.Close()

	if _, _, err := New(srv.URL).CreateSpec(context.Background(), SpecRequest{Task: "x"}); err == nil {
		t.Fatal("expected an error on 409, got nil")
	}
}

func TestAdoptPRParsesRearmedResult(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/adopt" {
			t.Fatalf("posted to %q, want /v1/adopt", r.URL.Path)
		}
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"job_id":"adopt-pr-repo-russ-4153","rearmed":true}`))
	}))
	defer srv.Close()

	jobID, already, rearmed, status, err := New(srv.URL).AdoptPR(context.Background(), "russ", 4153)
	if err != nil {
		t.Fatalf("AdoptPR: %v", err)
	}
	if status != http.StatusOK || jobID != "adopt-pr-repo-russ-4153" || already || !rearmed {
		t.Fatalf("status/job/already/rearmed=%d/%q/%v/%v", status, jobID, already, rearmed)
	}
	if gotBody["repo"] != "russ" || gotBody["pr"] != float64(4153) {
		t.Fatalf("request body=%v, want repo russ pr 4153", gotBody)
	}
}
