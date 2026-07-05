package advisor

import (
	"context"
	"os"
	"testing"
	"time"
)

// TestCLIAdvisorLive exercises the REAL prompt -> `claude -p` -> parse round-trip against a
// live model. Skipped unless FLOWBEE_ADVISOR_LIVE_TEST=1 (it makes a billed model call), so
// it never runs in the normal suite. Run: FLOWBEE_ADVISOR_LIVE_TEST=1 go test ./internal/advisor/ -run Live -v
func TestCLIAdvisorLive(t *testing.T) {
	if os.Getenv("FLOWBEE_ADVISOR_LIVE_TEST") != "1" {
		t.Skip("set FLOWBEE_ADVISOR_LIVE_TEST=1 to run the live model round-trip")
	}
	a := NewCLIAdvisor("", 120*time.Second) // default claude -p
	v, err := a.Consult(context.Background(), StuckJob{
		JobID: "01TEST", Reason: "attempts", Kind: "build",
		Task:           "Add a --json flag to the `flowbee status` command that prints the status as JSON.",
		Acceptance:     "flowbee status --json prints valid JSON with fleet + job counts.",
		LastCIFailures: "golangci-lint: undefined: statusJSON\ngo test: ./cmd/flowbee: build failed",
		Attempts:       5, MaxAttempts: 5, UnblockAttempts: 0,
	})
	if err != nil {
		t.Fatalf("live consult failed: %v", err)
	}
	switch v.Action {
	case ActionPlan, ActionCorrection, ActionReprompt, ActionStop:
		t.Logf("LIVE advisor verdict: action=%s note=%q", v.Action, v.Note)
	default:
		t.Fatalf("unexpected action %q", v.Action)
	}
}
