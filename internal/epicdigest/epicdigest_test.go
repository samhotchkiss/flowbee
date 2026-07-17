package epicdigest

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/samhotchkiss/flowbee/internal/attention"
)

func TestAssemble_StepsAgesAndClamp(t *testing.T) {
	in := happyInput()
	in.Epic.CurrentStep = 3
	in.Epic.StepsTotal = 8
	in.Epic.Checklist = []ChecklistItem{
		{Step: 1, Checked: true}, {Step: 2, Checked: true}, {Step: 3, Checked: false},
	}
	in.Epic.LastPaneChangeAt = testNow.Add(-90 * time.Second)
	in.Epic.StatusUpdatedAt = testNow.Add(-10 * time.Second)
	in.Epic.LastCommitAt = testNow.Add(-120 * time.Second)
	in.Epic.Tmux = "epic-frob"
	in.RecentInterventions = []Intervention{
		{Actor: "master", Summary: "one"}, {Actor: "operator", Summary: "two"},
		{Actor: "master", Summary: "three"}, {Actor: "operator", Summary: "four"},
	}

	d := Assemble(in)

	if d.Steps.Current != 3 || d.Steps.Total != 8 || d.Steps.Checked != 2 {
		t.Fatalf("steps: %+v", d.Steps)
	}
	if d.Ages.PaneChangeS != 90 || d.Ages.StatusUpdateS != 10 || d.Ages.LastCommitS != 120 {
		t.Fatalf("ages: %+v", d.Ages)
	}
	if d.Tmux != "epic-frob" {
		t.Fatalf("tmux jump target missing: %q", d.Tmux)
	}
	// clamped to the last 3 interventions, freshest preserved.
	if len(d.RecentInterventions) != 3 || d.RecentInterventions[0].Summary != "two" || d.RecentInterventions[2].Summary != "four" {
		t.Fatalf("interventions clamp: %+v", d.RecentInterventions)
	}
	if !d.OnTask {
		t.Fatalf("expected on_task true for the happy baseline")
	}
}

func TestAssemble_UnknownAgeIsMinusOne(t *testing.T) {
	in := happyInput()
	in.Epic.LastPaneChangeAt = time.Time{}           // unknown
	in.Epic.StatusUpdatedAt = testNow.Add(time.Hour) // future (clock skew)
	d := Assemble(in)
	if d.Ages.PaneChangeS != -1 {
		t.Fatalf("unknown pane age should be -1, got %d", d.Ages.PaneChangeS)
	}
	if d.Ages.StatusUpdateS != -1 {
		t.Fatalf("future status age should be -1, got %d", d.Ages.StatusUpdateS)
	}
}

// TestAssemble_JSONNeverNull guards the §15.16d contract: slices serialize as [] never
// null, so a constrained external consumer never has to null-check.
func TestAssemble_JSONNeverNull(t *testing.T) {
	d := Assemble(Input{Now: testNow, Epic: Epic{Slug: "x", ContextPct: -1}})
	b, err := json.Marshal(d)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, k := range []string{"drift_signals", "recent_interventions"} {
		if string(m[k]) != "[]" {
			t.Errorf("%s = %s, want []", k, m[k])
		}
	}
	if string(m["steps"]) == "" {
		t.Fatal("steps missing")
	}
	// account.windows must be [] never null even when no account is bound.
	var acct map[string]json.RawMessage
	if err := json.Unmarshal(m["account"], &acct); err != nil {
		t.Fatalf("account unmarshal: %v", err)
	}
	if string(acct["windows"]) != "[]" {
		t.Errorf("account.windows = %s, want []", acct["windows"])
	}
}

func TestAssemble_WindowsPassthrough(t *testing.T) {
	in := happyInput()
	in.Account.Windows = []Window{
		{Kind: "session", Percent: 40, Severity: "normal"},
		{Kind: "weekly_all", Percent: 72, Severity: "normal", ResetsAt: "2026-07-20T00:00:00Z"},
		{Kind: "weekly_scoped", Percent: 88, Severity: "critical", Scope: "Fable"},
	}
	d := Assemble(in)
	if len(d.Account.Windows) != 3 || d.Account.Windows[2].Scope != "Fable" || d.Account.Windows[2].Percent != 88 {
		t.Fatalf("windows[] not passed through verbatim: %+v", d.Account.Windows)
	}
	b, _ := json.Marshal(d)
	if !strings.Contains(string(b), `"kind":"weekly_scoped"`) || !strings.Contains(string(b), `"scope":"Fable"`) {
		t.Fatalf("windows[] JSON shape missing scoped detail: %s", b)
	}
}

func TestSummarize(t *testing.T) {
	rows := []SummaryRow{
		{OnTask: true},
		{Blocked: true},
		{Stranded: true, Blocked: true},
		{OnTask: true},
	}
	items := []attention.Item{
		{Kind: attention.KindAuthDead, Priority: 10, State: attention.StateOpen},
		{Kind: attention.KindDriftSuspect, Priority: 15, State: attention.StateLeased},
		{Kind: attention.KindDriftSuspect, Priority: 15, State: attention.StateResolved}, // resolved: not counted
		{Kind: attention.KindStalled, Priority: 15, State: attention.StateOpen},
	}
	accts := []AccountSummary{
		{Severity: "normal"},
		{Severity: "critical", ProbeStale: true}, // stale critical: suppressed
	}

	s := Summarize(7, rows, items, accts, true)
	if s.DigestSeq != 7 || !s.DispatchPaused {
		t.Fatalf("seq/paused: %+v", s)
	}
	if s.EpicsOnTask != 2 || s.EpicsBlocked != 2 || s.Stranded != 1 {
		t.Fatalf("epic counts: %+v", s)
	}
	if s.AttentionTotal != 3 {
		t.Fatalf("attention_total should exclude resolved: %+v", s)
	}
	if s.ByPriority[10] != 1 || s.ByPriority[15] != 2 {
		t.Fatalf("by_priority: %+v", s.ByPriority)
	}
	if s.WorstAccountSeverity != "normal" {
		t.Fatalf("stale critical must not escalate worst severity: %+v", s)
	}

	// a non-stale critical DOES escalate.
	accts[1].ProbeStale = false
	if got := Summarize(7, rows, items, accts, false).WorstAccountSeverity; got != "critical" {
		t.Fatalf("worst severity = %q, want critical", got)
	}
}
