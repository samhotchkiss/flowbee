package epicdigest

import (
	"testing"
	"time"

	"github.com/samhotchkiss/flowbee/internal/attention"
)

var testNow = time.Date(2026, 7, 16, 12, 0, 0, 0, time.UTC)

// happyInput is an epic that IS on-task: working pane, no attention, no drift, a
// normal account with headroom, context well above the floor.
func happyInput() Input {
	return Input{
		Now: testNow,
		Epic: Epic{
			Slug:         "frob",
			PaneState:    paneWorking,
			ContextPct:   80,
			LastCommitAt: testNow.Add(-2 * time.Minute),
		},
		Account: AccountSummary{Bound: true, Severity: "normal", WeeklyPct: 40, ProbeStale: false},
	}
}

func TestOnTask_HappyPath(t *testing.T) {
	if ok, reason := onTask(happyInput()); !ok {
		t.Fatalf("expected on-task, got false: %s", reason)
	}
}

func TestOnTask_Branches(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*Input)
		want   bool
	}{
		{"working pane", func(in *Input) { in.Epic.PaneState = paneWorking }, true},
		{"idle with recent commit", func(in *Input) {
			in.Epic.PaneState = paneIdleAtPrompt
			in.Epic.LastCommitAt = testNow.Add(-5 * time.Minute)
		}, true},
		{"idle with recent status update", func(in *Input) {
			in.Epic.PaneState = paneIdleAtPrompt
			in.Epic.LastCommitAt = time.Time{}
			in.Epic.StatusUpdatedAt = testNow.Add(-3 * time.Minute)
		}, true},
		{"idle with STALE progress", func(in *Input) {
			in.Epic.PaneState = paneIdleAtPrompt
			in.Epic.LastCommitAt = testNow.Add(-2 * time.Hour)
			in.Epic.StatusUpdatedAt = testNow.Add(-2 * time.Hour)
		}, false},
		{"unknown pane", func(in *Input) { in.Epic.PaneState = "unknown" }, false},
		{"awaiting input pane", func(in *Input) { in.Epic.PaneState = "awaiting_input" }, false},
		{"open halting attention item", func(in *Input) {
			in.Attention = []attention.Item{{Kind: attention.KindDriftSuspect, State: attention.StateOpen}}
		}, false},
		{"resolved attention item does not count", func(in *Input) {
			in.Attention = []attention.Item{{Kind: attention.KindDriftSuspect, State: attention.StateResolved}}
		}, true},
		{"non-halting attention item (epic_finished)", func(in *Input) {
			in.Attention = []attention.Item{{Kind: attention.KindEpicFinished, State: attention.StateOpen}}
		}, true},
		{"non-halting attention item (master_absent)", func(in *Input) {
			in.Attention = []attention.Item{{Kind: attention.KindMasterAbsent, State: attention.StateOpen}}
		}, true},
		{"fired drift signal", func(in *Input) { in.Epic.DriftSignals = []string{"claim_exceeds_commits"} }, false},
		{"account critical non-stale", func(in *Input) { in.Account.Severity = "critical" }, false},
		{"account critical but STALE (suppressed)", func(in *Input) {
			in.Account.Severity = "critical"
			in.Account.ProbeStale = true
		}, true},
		{"unbound account never critical", func(in *Input) {
			in.Account = AccountSummary{Bound: false, Severity: "critical"}
		}, true},
		{"context below floor", func(in *Input) { in.Epic.ContextPct = 8 }, false},
		{"context at floor is ok", func(in *Input) { in.Epic.ContextPct = 15 }, true},
		{"context unknown not held against", func(in *Input) { in.Epic.ContextPct = -1 }, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			in := happyInput()
			c.mutate(&in)
			got := OnTask(in)
			if got != c.want {
				_, reason := onTask(in)
				t.Fatalf("OnTask = %v, want %v (reason: %q)", got, c.want, reason)
			}
		})
	}
}

func TestCompactionJumped(t *testing.T) {
	cases := []struct {
		prev, cur, thr float64
		want           bool
	}{
		{prev: 20, cur: 80, thr: 15, want: true},  // classic compaction: freed context
		{prev: 20, cur: 30, thr: 15, want: false}, // small rise: not compaction
		{prev: 80, cur: 20, thr: 15, want: false}, // a DROP is never compaction
		{prev: -1, cur: 80, thr: 15, want: false}, // unknown prev
		{prev: 20, cur: -1, thr: 15, want: false}, // unknown cur
		{prev: 20, cur: 40, thr: 0, want: true},   // 0 threshold -> default 15; 20pt rise matches
		{prev: 20, cur: 30, thr: 0, want: false},  // default 15; 10pt rise does not
	}
	for _, c := range cases {
		if got := CompactionJumped(c.prev, c.cur, c.thr); got != c.want {
			t.Errorf("CompactionJumped(%v,%v,%v)=%v want %v", c.prev, c.cur, c.thr, got, c.want)
		}
	}
}
