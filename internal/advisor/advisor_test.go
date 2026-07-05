package advisor

import (
	"context"
	"testing"
)

func TestParseVerdict(t *testing.T) {
	cases := []struct {
		name       string
		stdout     string
		wantAction Action
		wantNote   string
		wantErr    bool
	}{
		{
			name:       "claude json envelope",
			stdout:     `{"type":"result","result":"{\"action\":\"PLAN\",\"note\":\"split into two steps\"}","total_cost_usd":0.01}`,
			wantAction: ActionPlan, wantNote: "split into two steps",
		},
		{
			name:       "plain json (codex)",
			stdout:     `{"action":"CORRECTION","note":"fix the import path"}`,
			wantAction: ActionCorrection, wantNote: "fix the import path",
		},
		{
			name:       "json wrapped in prose",
			stdout:     "Here is my decision:\n\n{\"action\":\"STOP\",\"note\":\"needs a human\"}\n\nThanks!",
			wantAction: ActionStop, wantNote: "needs a human",
		},
		{
			name:       "unknown action fails safe to stop",
			stdout:     `{"action":"REQUEUE","note":"..."}`,
			wantAction: ActionStop,
		},
		{
			name:       "reprompt",
			stdout:     `{"action":"reprompt","note":"be explicit about the file"}`,
			wantAction: ActionReprompt, wantNote: "be explicit about the file",
		},
		{
			name:    "no json is an error",
			stdout:  "I could not decide.",
			wantErr: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v, err := ParseVerdict(tc.stdout)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("want error, got verdict %+v", v)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if v.Action != tc.wantAction {
				t.Fatalf("action=%q want %q", v.Action, tc.wantAction)
			}
			if tc.wantNote != "" && v.Note != tc.wantNote {
				t.Fatalf("note=%q want %q", v.Note, tc.wantNote)
			}
		})
	}
}

// TestCLIAdvisorFailSafe: a command that exits non-zero (or prints nothing usable) must
// yield STOP with an error — a broken advisor can only ever leave a job parked.
func TestCLIAdvisorFailSafe(t *testing.T) {
	a := &CLIAdvisor{Cmd: "exit 1", Timeout: 5_000_000_000}
	v, err := a.Consult(context.Background(), StuckJob{JobID: "j", Reason: "stall"})
	if err == nil {
		t.Fatal("want error from a failing advisor command")
	}
	if v.Action != ActionStop {
		t.Fatalf("fail-safe action=%q, want STOP", v.Action)
	}
}

// TestCLIAdvisorParsesEcho: the command echoes a valid verdict — proves the prompt-file
// plumbing + parse round-trips end-to-end without a real model.
func TestCLIAdvisorParsesEcho(t *testing.T) {
	a := &CLIAdvisor{Cmd: `printf '{"action":"PLAN","note":"do the thing"}'`, Timeout: 5_000_000_000}
	v, err := a.Consult(context.Background(), StuckJob{JobID: "j", Reason: "stall"})
	if err != nil {
		t.Fatalf("consult: %v", err)
	}
	if v.Action != ActionPlan || v.Note != "do the thing" {
		t.Fatalf("got %+v, want PLAN/do the thing", v)
	}
}

// TestBuildPromptClosedSet: the prompt must constrain to the closed action set and tell the
// model to default to STOP — the behavioral contract the store relies on.
func TestBuildPromptClosedSet(t *testing.T) {
	p := BuildPrompt(StuckJob{JobID: "j", Reason: "stall", Kind: "build", Task: "do X"})
	for _, want := range []string{"PLAN", "CORRECTION", "REPROMPT", "STOP", "single-line JSON", "Default to STOP"} {
		if !contains(p, want) {
			t.Fatalf("prompt missing %q", want)
		}
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (func() bool {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	})()
}
