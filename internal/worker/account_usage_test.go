package worker

import "testing"

// TestParseRunTokensCodexJSON proves the F6 token-budget signal is extracted from
// codex `exec --json` output: the turn.completed event carries usage.{input,output,
// reasoning_output}_tokens, summed (cached_input excluded). This is the real codex
// JSONL shape captured from codex-cli 0.142.0.
func TestParseRunTokensCodexJSON(t *testing.T) {
	out := `{"type":"thread.started","thread_id":"abc"}
{"type":"turn.started"}
{"type":"item.completed","item":{"id":"item_0","type":"agent_message","text":"hi"}}
{"type":"turn.completed","usage":{"input_tokens":19907,"cached_input_tokens":6016,"output_tokens":5,"reasoning_output_tokens":3}}`
	if got := parseRunTokens(out); got != 19907+5+3 {
		t.Fatalf("codex json tokens: got %d want %d", got, 19907+5+3)
	}
}

// TestParseRunTokensCodexJSONLastTurnWins proves a multi-turn run reports the LAST
// turn.completed accounting (the final tally for the run), not a stale earlier one.
func TestParseRunTokensCodexJSONLastTurnWins(t *testing.T) {
	out := `{"type":"turn.completed","usage":{"input_tokens":100,"output_tokens":10,"reasoning_output_tokens":0}}
{"type":"turn.completed","usage":{"input_tokens":500,"output_tokens":40,"reasoning_output_tokens":0}}`
	if got := parseRunTokens(out); got != 540 {
		t.Fatalf("last turn wins: got %d want 540", got)
	}
}

// TestParseRunTokensClaudeJSON proves the claude single-object usage path still works.
func TestParseRunTokensClaudeJSON(t *testing.T) {
	out := `{"total_cost_usd":0.12,"usage":{"input_tokens":1000,"output_tokens":250},"result":"done"}`
	if got := parseRunTokens(out); got != 1250 {
		t.Fatalf("claude json tokens: got %d want 1250", got)
	}
}

// TestParseRunTokensPlain proves the codex non-JSON fallback ("tokens used\n<N>"
// with thousands separators) is parsed.
func TestParseRunTokensPlain(t *testing.T) {
	out := "codex\nhi\ntokens used\n13,937\n"
	if got := parseRunTokens(out); got != 13937 {
		t.Fatalf("plain tokens: got %d want 13937", got)
	}
}

func TestParseRunTokensNone(t *testing.T) {
	if got := parseRunTokens("just some prose, no token accounting"); got != 0 {
		t.Fatalf("no tokens: got %d want 0", got)
	}
}

// (the binary rate-limit BACKSTOP, agentHitLimit, is covered by account_limit_test.go.)

// TestParseUsagePct proves the opportunistic provider-% parse (a CLI that exposes a
// live "X% of your usage limit" line — codex emits none today, so it's best-effort).
func TestParseUsagePct(t *testing.T) {
	if pct, ok := parseUsagePct("You have used 73% of your weekly limit."); !ok || pct != 73 {
		t.Fatalf("usage pct: got %d,%v want 73,true", pct, ok)
	}
	if _, ok := parseUsagePct("no percentage here"); ok {
		t.Fatal("parseUsagePct should not match arbitrary text")
	}
}
