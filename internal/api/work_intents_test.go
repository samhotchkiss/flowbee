package api_test

import (
	"net/http"
	"testing"
)

func TestWorkIntentAPIIsIdempotentAndHasNoHumanPromoteEndpoint(t *testing.T) {
	ts, _ := decisionAPIServer(t)
	sha := "sha256:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"
	body := map[string]any{
		"source_conversation_id": "thread-api", "source_message_id": "message-api",
		"source_message_version": 1, "interactor_incarnation_id": "interactor-api-run",
		"title": "Build the next stage", "summary": "No manual send step",
		"artifact_ref": "artifact://intent/api", "artifact_sha256": sha,
		"intent_version": 1, "definition_complete": true,
		"orchestrator_registration": "orchestrator-api-run",
	}
	resp, first := decisionRequest(t, ts.Client(), http.MethodPost,
		ts.URL+"/v1/projects/default/work-intents", body, "capture-api-1")
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create status=%d body=%v", resp.StatusCode, first)
	}
	intent := first["work_intent"].(map[string]any)
	id, _ := intent["id"].(string)
	if id == "" || intent["state"] != "captured" || intent["submission_idempotency_key"] == "" {
		t.Fatalf("created intent=%v", intent)
	}
	resp, replay := decisionRequest(t, ts.Client(), http.MethodPost,
		ts.URL+"/v1/projects/default/work-intents", body, "capture-api-1")
	if resp.StatusCode != http.StatusCreated || replay["work_intent"].(map[string]any)["id"] != id {
		t.Fatalf("capture replay status=%d first=%v replay=%v", resp.StatusCode, first, replay)
	}

	resp, listed := decisionRequest(t, ts.Client(), http.MethodGet,
		ts.URL+"/v1/projects/default/work-intents", nil, "")
	rows, _ := listed["work_intents"].([]any)
	if resp.StatusCode != http.StatusOK || listed["schema_version"] != "flowbee.work-intent/v1" || len(rows) != 1 {
		t.Fatalf("list status=%d body=%v", resp.StatusCode, listed)
	}

	// The designed routine path is reconciler-owned. A human-facing generic
	// promote/send endpoint must not exist.
	resp, _ = decisionRequest(t, ts.Client(), http.MethodPost,
		ts.URL+"/v1/work-intents/"+id+"/promote", map[string]any{}, "forbidden-promote")
	if resp.StatusCode != http.StatusNotFound && resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("human promote endpoint unexpectedly exists: status=%d", resp.StatusCode)
	}
}

func TestWorkIntentAPIPauseResumeUsesStateVersionFence(t *testing.T) {
	ts, _ := decisionAPIServer(t)
	sha := "sha256:eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"
	create := map[string]any{
		"source_message_id": "message-pause", "source_message_version": 1,
		"interactor_incarnation_id": "interactor-pause", "title": "Pausable",
		"artifact_ref": "artifact://intent/pause", "artifact_sha256": sha, "intent_version": 1,
	}
	resp, got := decisionRequest(t, ts.Client(), http.MethodPost,
		ts.URL+"/v1/projects/default/work-intents", create, "capture-pause")
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create status=%d body=%v", resp.StatusCode, got)
	}
	intent := got["work_intent"].(map[string]any)
	id := intent["id"].(string)
	pause := map[string]any{"project_id": "default", "state_version": 1, "reason": "human pause"}
	resp, got = decisionRequest(t, ts.Client(), http.MethodPost,
		ts.URL+"/v1/work-intents/"+id+"/pause", pause, "pause-1")
	if resp.StatusCode != http.StatusOK || got["work_intent"].(map[string]any)["hold_kind"] != "paused" {
		t.Fatalf("pause status=%d body=%v", resp.StatusCode, got)
	}
	resp, _ = decisionRequest(t, ts.Client(), http.MethodPost,
		ts.URL+"/v1/work-intents/"+id+"/resume", map[string]any{
			"project_id": "default", "state_version": 1,
		}, "resume-stale")
	if resp.StatusCode != http.StatusPreconditionFailed {
		t.Fatalf("stale resume status=%d want 412", resp.StatusCode)
	}
	resp, got = decisionRequest(t, ts.Client(), http.MethodPost,
		ts.URL+"/v1/work-intents/"+id+"/resume", map[string]any{
			"project_id": "default", "state_version": 2,
		}, "resume-current")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("resume status=%d body=%v", resp.StatusCode, got)
	}
}
