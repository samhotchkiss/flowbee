package api_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/samhotchkiss/flowbee/internal/api"
	"github.com/samhotchkiss/flowbee/internal/clock"
	"github.com/samhotchkiss/flowbee/internal/testutil"
	"github.com/samhotchkiss/flowbee/internal/ulid"
)

func decisionAPIServer(t *testing.T) (*httptest.Server, *clock.Fake) {
	t.Helper()
	st := testutil.NewStore(t)
	clk := clock.NewFake(time.Date(2026, 7, 19, 17, 0, 0, 0, time.UTC))
	srv := api.New(st, clk, ulid.NewMinter(nil), api.Config{}, "decision-test")
	ts := httptest.NewServer(srv.PrivateHandler())
	t.Cleanup(ts.Close)
	return ts, clk
}

func decisionRequest(t *testing.T, client *http.Client, method, url string, body any, key string) (*http.Response, map[string]any) {
	t.Helper()
	var raw []byte
	if body != nil {
		var err error
		raw, err = json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
	}
	req, err := http.NewRequest(method, url, bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Flowbee-Actor", "sam")
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	decoded := map[string]any{}
	_ = json.NewDecoder(resp.Body).Decode(&decoded)
	return resp, decoded
}

func TestDecisionAPIExactSubjectIdempotencyAndGlobalInbox(t *testing.T) {
	ts, _ := decisionAPIServer(t)
	sha := "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	create := map[string]any{
		"id": "decision-api", "project_id": "default", "kind": "plan_review",
		"title": "Approve plan", "prompt": "Review the immutable plan.",
		"expected_response_kinds": []string{"approve", "request_changes"},
		"route_to":                "human",
		"subject_artifact_ref":    "artifact://plan/1", "subject_version": 1,
		"subject_sha256": sha, "options": []any{}, "response_schema": map[string]any{},
		"evidence_refs": []any{},
	}
	resp, got := decisionRequest(t, ts.Client(), http.MethodPost, ts.URL+"/v1/decisions", create, "create-decision-api")
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create status=%d body=%v", resp.StatusCode, got)
	}
	decision, _ := got["decision"].(map[string]any)
	if decision["id"] != "decision-api" || decision["project_id"] != "default" ||
		decision["subject_sha256"] != sha || decision["state"] != "open" {
		t.Fatalf("created decision=%v", decision)
	}

	resp, got = decisionRequest(t, ts.Client(), http.MethodGet, ts.URL+"/v1/decisions", nil, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list status=%d body=%v", resp.StatusCode, got)
	}
	rows, _ := got["decisions"].([]any)
	if got["schema_version"] != "flowbee.decision/v1" || len(rows) != 1 {
		t.Fatalf("global inbox=%v", got)
	}

	viewBody := map[string]any{"project_id": "default", "request_version": 1}
	resp, got = decisionRequest(t, ts.Client(), http.MethodPost, ts.URL+"/v1/decisions/decision-api/view", viewBody, "view-decision-api")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("view status=%d body=%v", resp.StatusCode, got)
	}

	approve := map[string]any{
		"project_id": "default", "request_version": 1, "subject_version": 1,
		"subject_sha256": sha, "value": map[string]any{"approved": true},
		"comment": "Approved as shown.",
	}
	resp, first := decisionRequest(t, ts.Client(), http.MethodPost, ts.URL+"/v1/decisions/decision-api/approve", approve, "browser-submit-1")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("approve status=%d body=%v", resp.StatusCode, first)
	}
	firstResponse := first["response"].(map[string]any)
	if firstResponse["kind"] != "approve" || firstResponse["actor_id"] != "loopback-human" {
		t.Fatalf("approve response=%v", firstResponse)
	}

	resp, replay := decisionRequest(t, ts.Client(), http.MethodPost, ts.URL+"/v1/decisions/decision-api/approve", approve, "browser-submit-1")
	if resp.StatusCode != http.StatusOK || replay["response"].(map[string]any)["id"] != firstResponse["id"] {
		t.Fatalf("idempotent replay status=%d first=%v replay=%v", resp.StatusCode, first, replay)
	}
	approve["comment"] = "changed replay"
	resp, _ = decisionRequest(t, ts.Client(), http.MethodPost, ts.URL+"/v1/decisions/decision-api/approve", approve, "browser-submit-1")
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("changed idempotency replay status=%d want 409", resp.StatusCode)
	}
}

func TestDecisionAPIRejectsStaleDisplayedArtifact(t *testing.T) {
	ts, _ := decisionAPIServer(t)
	sha := "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	create := map[string]any{
		"id": "decision-stale", "project_id": "default", "kind": "design_review",
		"title": "Approve design", "prompt": "Review it.",
		"expected_response_kinds": []string{"approve"},
		"route_to":                "human", "subject_artifact_ref": "artifact://design/2",
		"subject_version": 2, "subject_sha256": sha,
	}
	resp, got := decisionRequest(t, ts.Client(), http.MethodPost, ts.URL+"/v1/decisions", create, "create-decision-stale")
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create status=%d body=%v", resp.StatusCode, got)
	}
	response := map[string]any{
		"project_id": "default", "request_version": 1, "subject_version": 1,
		"subject_sha256": sha, "value": map[string]any{},
	}
	resp, _ = decisionRequest(t, ts.Client(), http.MethodPost, ts.URL+"/v1/decisions/decision-stale/approve", response, "stale-submit")
	if resp.StatusCode != http.StatusPreconditionFailed {
		t.Fatalf("stale artifact response status=%d want 412", resp.StatusCode)
	}
}
