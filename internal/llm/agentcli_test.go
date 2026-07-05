package llm

import "testing"

func TestApplyResolvedAgentModelReplacesClaudeModelFlag(t *testing.T) {
	cmd := `claude --model sonnet --output-format json -p 'do work'`
	got := applyResolvedAgentModel(cmd, "opus")
	want := `claude --model opus --output-format json -p 'do work'`
	if got != want {
		t.Fatalf("command = %q, want %q", got, want)
	}
}

func TestApplyResolvedAgentModelAppendsMissingClaudeModelFlag(t *testing.T) {
	cmd := `claude -p 'do work'`
	got := applyResolvedAgentModel(cmd, "claude-opus-4-6")
	want := `claude -p 'do work' --model claude-opus-4-6`
	if got != want {
		t.Fatalf("command = %q, want %q", got, want)
	}
}

func TestApplyResolvedAgentModelLeavesNonClaudeCommandsAlone(t *testing.T) {
	cmd := `codex exec --dangerously-bypass-approvals-and-sandbox 'do work'`
	if got := applyResolvedAgentModel(cmd, "opus"); got != cmd {
		t.Fatalf("command = %q, want unchanged %q", got, cmd)
	}
}
