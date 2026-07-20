package api_test

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestConversationAPIStableThreadMessagesFocusAndDelivery(t *testing.T) {
	ts, _ := decisionAPIServer(t)
	create := map[string]any{
		"id": "thread-api", "conversation_key": "primary", "title": "Default project",
		"interactor_actor_id": "interactor:default", "interactor_binding_id": "binding-1",
		"interactor_incarnation_id": "run-1",
	}
	resp, got := decisionRequest(t, ts.Client(), http.MethodPost,
		ts.URL+"/v1/projects/default/conversations", create, "thread-create-api")
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create status=%d body=%v", resp.StatusCode, got)
	}
	thread, _ := got["conversation"].(map[string]any)
	if thread["id"] != "thread-api" || thread["focus_kind"] != "project" || thread["focus_ref"] != "default" {
		t.Fatalf("thread=%v", thread)
	}
	resp, replay := decisionRequest(t, ts.Client(), http.MethodPost,
		ts.URL+"/v1/projects/default/conversations", create, "thread-create-api")
	if resp.StatusCode != http.StatusCreated || replay["conversation"].(map[string]any)["id"] != "thread-api" {
		t.Fatalf("create replay status=%d body=%v", resp.StatusCode, replay)
	}

	messageBody := map[string]any{"project_id": "default", "content_text": "Keep moving on v2."}
	resp, first := decisionRequest(t, ts.Client(), http.MethodPost,
		ts.URL+"/v1/conversations/thread-api/messages", messageBody, "message-browser-retry")
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("message status=%d body=%v", resp.StatusCode, first)
	}
	message := first["message"].(map[string]any)
	messageID, _ := message["id"].(string)
	if message["role"] != "human" || message["delivery_state"] != "pending" ||
		message["thread_seq"].(float64) != 1 || !strings.HasPrefix(message["content_sha256"].(string), "sha256:") {
		t.Fatalf("message=%v", message)
	}
	resp, retry := decisionRequest(t, ts.Client(), http.MethodPost,
		ts.URL+"/v1/conversations/thread-api/messages", messageBody, "message-browser-retry")
	if resp.StatusCode != http.StatusCreated || retry["message"].(map[string]any)["id"] != messageID {
		t.Fatalf("message retry status=%d body=%v", resp.StatusCode, retry)
	}

	resp, listed := decisionRequest(t, ts.Client(), http.MethodGet,
		ts.URL+"/v1/conversations/thread-api/messages?project_id=default&after=0&limit=1", nil, "")
	if resp.StatusCode != http.StatusOK || listed["schema_version"] != "flowbee.conversation/v1" ||
		len(listed["messages"].([]any)) != 1 || listed["next_after"].(float64) != 1 || listed["digest_seq"].(float64) < 2 {
		t.Fatalf("messages status=%d body=%v", resp.StatusCode, listed)
	}

	artifactSHA := "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	focus := map[string]any{"project_id": "default", "expected_state_version": 1,
		"focus_kind": "artifact", "focus_ref": "artifact://design/1", "focus_artifact_sha256": artifactSHA}
	resp, focused := decisionRequest(t, ts.Client(), http.MethodPost,
		ts.URL+"/v1/conversations/thread-api/focus", focus, "focus-api")
	if resp.StatusCode != http.StatusOK || focused["conversation"].(map[string]any)["state_version"].(float64) != 2 {
		t.Fatalf("focus status=%d body=%v", resp.StatusCode, focused)
	}

	delivery := map[string]any{"project_id": "default", "expected_state_version": 1,
		"state": "routing", "action_id": "conversation-action-1"}
	resp, routed := decisionRequest(t, ts.Client(), http.MethodPost,
		fmt.Sprintf("%s/v1/conversations/thread-api/messages/%s/delivery", ts.URL, messageID), delivery, "delivery-api")
	if resp.StatusCode != http.StatusOK || routed["message"].(map[string]any)["delivery_state"] != "routing" {
		t.Fatalf("delivery status=%d body=%v", resp.StatusCode, routed)
	}

	resp, threads := decisionRequest(t, ts.Client(), http.MethodGet,
		ts.URL+"/v1/projects/default/conversations", nil, "")
	if resp.StatusCode != http.StatusOK || len(threads["conversations"].([]any)) != 1 {
		t.Fatalf("threads status=%d body=%v", resp.StatusCode, threads)
	}
}

func TestConversationSSEReplaysPersistedEventsFromCursor(t *testing.T) {
	ts, _ := decisionAPIServer(t)
	create := map[string]any{
		"id": "thread-stream", "conversation_key": "primary", "title": "Stream",
		"interactor_actor_id": "interactor:default", "interactor_binding_id": "binding-1",
		"interactor_incarnation_id": "run-1",
	}
	resp, got := decisionRequest(t, ts.Client(), http.MethodPost,
		ts.URL+"/v1/projects/default/conversations", create, "thread-stream-create")
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create status=%d body=%v", resp.StatusCode, got)
	}
	resp, got = decisionRequest(t, ts.Client(), http.MethodPost,
		ts.URL+"/v1/conversations/thread-stream/messages",
		map[string]any{"project_id": "default", "content_text": "Persist this."}, "stream-message")
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("message status=%d body=%v", resp.StatusCode, got)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		ts.URL+"/v1/conversations/thread-stream/events?project_id=default&after=1", nil)
	if err != nil {
		t.Fatal(err)
	}
	stream, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Body.Close()
	if stream.StatusCode != http.StatusOK || stream.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("stream status=%d content-type=%q", stream.StatusCode, stream.Header.Get("Content-Type"))
	}
	scanner := bufio.NewScanner(stream.Body)
	var idLine, eventLine, dataLine string
	for scanner.Scan() {
		line := scanner.Text()
		switch {
		case strings.HasPrefix(line, "id: "):
			idLine = line
		case strings.HasPrefix(line, "event: "):
			eventLine = line
		case strings.HasPrefix(line, "data: "):
			dataLine = line
		case line == "" && dataLine != "":
			goto complete
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}

complete:
	if idLine != "id: 2" || eventLine != "event: message_appended" {
		t.Fatalf("id=%q event=%q data=%q", idLine, eventLine, dataLine)
	}
	var event map[string]any
	if err := json.Unmarshal([]byte(strings.TrimPrefix(dataLine, "data: ")), &event); err != nil {
		t.Fatal(err)
	}
	if event["project_id"] != "default" || event["thread_id"] != "thread-stream" || event["kind"] != "message_appended" {
		t.Fatalf("event=%v", event)
	}
}

func TestConversationAPIRejectsChangedIdempotencyBody(t *testing.T) {
	ts, _ := decisionAPIServer(t)
	create := map[string]any{
		"id": "thread-conflict", "conversation_key": "primary", "title": "One",
		"interactor_actor_id": "interactor:default", "interactor_binding_id": "binding-1",
		"interactor_incarnation_id": "run-1",
	}
	resp, _ := decisionRequest(t, ts.Client(), http.MethodPost,
		ts.URL+"/v1/projects/default/conversations", create, "same-key")
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create status=%d", resp.StatusCode)
	}
	create["title"] = "Two"
	resp, got := decisionRequest(t, ts.Client(), http.MethodPost,
		ts.URL+"/v1/projects/default/conversations", create, "same-key")
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("changed retry status=%d body=%v", resp.StatusCode, got)
	}
}
