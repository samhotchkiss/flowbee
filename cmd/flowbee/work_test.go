package main

import (
	"strings"
	"testing"
)

// TestWorkRequiresAgentOrStub proves a bare `flowbee work` with no agent command
// refuses to start instead of silently claiming jobs and bouncing them with empty
// results. The guard returns before any network call, so no server is needed.
func TestWorkRequiresAgentOrStub(t *testing.T) {
	// clear anything the live shell might have set, so the guard sees an empty agent.
	t.Setenv("FLOWBEE_AGENT_CMD", "")
	t.Setenv("FLOWBEE_BUILD_AGENT_CMD", "")
	t.Setenv("FLOWBEE_URL", "http://127.0.0.1:0")
	t.Setenv("FLOWBEE_GITHUB_OWNER", "o")
	t.Setenv("FLOWBEE_GITHUB_REPO", "r")

	err := runWork([]string{"--once"})
	if err == nil {
		t.Fatalf("bare work with no agent must fail loud, got nil")
	}
	if !strings.Contains(err.Error(), "no agent configured") {
		t.Fatalf("error must name the missing agent, got: %v", err)
	}
	// the message must point the operator at the real fix paths.
	for _, want := range []string{"--agent-cmd", "--stub", "flowbee fleet"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("guidance must mention %q, got: %v", want, err)
		}
	}
}
