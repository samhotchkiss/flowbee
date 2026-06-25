package worker

import "testing"

// TestAgentHitLimit pins the usage/rate-limit detection used to gate a maxed account (F6):
// it must fire on the distinctive limit phrases codex/claude emit, and NOT on incidental
// mentions or a clean run.
func TestAgentHitLimit(t *testing.T) {
	maxed := []string{
		"ERROR: You've hit your usage limit. Visit ... try again at Jun 26th",
		"Claude API error: rate limit exceeded",
		"HTTP 429: Too Many Requests",
		"quota exceeded for this account",
		"rate_limit_error",
	}
	for _, s := range maxed {
		if !agentHitLimit(s) {
			t.Errorf("expected limit detected in %q", s)
		}
	}
	clean := []string{
		"OK",
		"completed job -> review_pending pushed",
		`{"result":"done","total_cost_usd":0.04}`,
		"", // empty
		"the function enforces a rate per second internally", // incidental 'rate' without limit/429
	}
	for _, s := range clean {
		if agentHitLimit(s) {
			t.Errorf("false positive on %q", s)
		}
	}
}
