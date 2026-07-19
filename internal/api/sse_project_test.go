package api

import (
	"encoding/json"
	"testing"
)

func TestPhase2ProjectSSECarriesExplicitScope(t *testing.T) {
	b := NewBroker()
	id, ch := b.subscribe()
	defer b.unsubscribe(id)
	b.Publish(LifeEvent{ProjectID: "mail", State: "projects", Event: "project_state_changed"})
	blob := <-ch
	if topic := topicOf(blob); topic != "projects" {
		t.Fatalf("topic=%q", topic)
	}
	var got LifeEvent
	if err := json.Unmarshal(blob, &got); err != nil {
		t.Fatal(err)
	}
	if got.ProjectID != "mail" || got.Event != "project_state_changed" {
		t.Fatalf("event lost project scope: %+v", got)
	}
}
