package watchdog

import "testing"

// TestParseStatus_ExactSamples pins ParseStatus against the EXACT captured pane
// samples from the task brief — the one contract this parser must never silently
// drift on, since the TUI format churns weekly and this function is the ENTIRE
// blast radius of that churn (§ package doc).
func TestParseStatus_ExactSamples(t *testing.T) {
	cases := []struct {
		name       string
		pane       string
		wantState  State
		wantDetail string
	}{
		{
			name:       "pursuing",
			pane:       "  gpt-5.6-terra high · ~/dev/russ · Main [default]                    Pursuing goal (2d 4h 12m)",
			wantState:  StatePursuing,
			wantDetail: "2d 4h 12m",
		},
		{
			name:       "blocked",
			pane:       "  gpt-5.6-sol medium · ~/dev/russ                                     Goal blocked (/goal resume)",
			wantState:  StateBlocked,
			wantDetail: "/goal resume",
		},
		{
			name:       "achieved",
			pane:       "  gpt-5.6-sol medium · ~/dev/russ                                     Goal achieved (1h 52m)",
			wantState:  StateAchieved,
			wantDetail: "1h 52m",
		},
		{
			name:       "working mid-turn line",
			pane:       "• Working (30m 48s • esc to interrupt) · 1 background terminal running · /ps to view · /stop to close",
			wantState:  StateWorking,
			wantDetail: "30m 48s",
		},
		{
			name:       "empty pane",
			pane:       "",
			wantState:  StateUnknown,
			wantDetail: "",
		},
		{
			name:       "whitespace-only pane",
			pane:       "   \n\n   \n",
			wantState:  StateUnknown,
			wantDetail: "",
		},
		{
			name:       "garbage unrelated text",
			pane:       "some random shell prompt\n$ ls -la\ntotal 0\n",
			wantState:  StateUnknown,
			wantDetail: "",
		},
		{
			name:       "status line buried above trailing blank padding (tmux pane height)",
			pane:       "  gpt-5.6-terra high · ~/dev/russ · Main [default]                    Pursuing goal (5m)\n\n\n\n",
			wantState:  StatePursuing,
			wantDetail: "5m",
		},
		{
			name:       "line merely containing the word working, no bullet prefix, must not match",
			pane:       "the build is still working through the queue",
			wantState:  StateUnknown,
			wantDetail: "",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			state, detail := ParseStatus(tc.pane)
			if state != tc.wantState {
				t.Errorf("state = %q, want %q", state, tc.wantState)
			}
			if detail != tc.wantDetail {
				t.Errorf("detail = %q, want %q", detail, tc.wantDetail)
			}
		})
	}
}
