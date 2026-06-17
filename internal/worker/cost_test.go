package worker

import "testing"

// TestParseAgentUsage: the harness reads token/cost usage from a claude --output-format
// json result object (total_cost_usd taken directly — no price table), and ignores
// plain-text output so cost reporting is opt-in by the agent's output format.
func TestParseAgentUsage(t *testing.T) {
	js := `{"type":"result","subtype":"success","total_cost_usd":0.0234,` +
		`"usage":{"input_tokens":1500,"output_tokens":800},"result":"the spec text"}`
	ti, to, micro, ok := parseAgentUsage(js)
	if !ok || ti != 1500 || to != 800 || micro != 23400 {
		t.Fatalf("parseAgentUsage = in=%d out=%d micro=%d ok=%v, want 1500/800/23400/true", ti, to, micro, ok)
	}
	// plain text output: not JSON -> no usage (backward compatible, no spurious cost).
	if _, _, _, ok := parseAgentUsage("just a text response from the agent"); ok {
		t.Fatal("plain text output must not parse as usage")
	}
	if _, _, _, ok := parseAgentUsage(""); ok {
		t.Fatal("empty output must not parse as usage")
	}
}

// TestAgentResultText: the spec_author fallback unwraps claude's JSON `.result` when the
// agent ran with --output-format json, but passes raw text through unchanged — so the
// fallback works in either mode.
func TestAgentResultText(t *testing.T) {
	if got := agentResultText(`{"result":"# Spec\n\nbody","total_cost_usd":0.01}`); got != "# Spec\n\nbody" {
		t.Fatalf("agentResultText(JSON) = %q, want the .result text", got)
	}
	if got := agentResultText("raw markdown spec"); got != "raw markdown spec" {
		t.Fatalf("agentResultText(text) = %q, want raw passthrough", got)
	}
}
