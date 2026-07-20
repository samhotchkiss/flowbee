package epicdigest

import "github.com/samhotchkiss/flowbee/internal/attention"

// SummaryRow is one epic's counts-only contribution to the fleet summary (plan
// §15.16c) — the constrained-consumer rollup (`GET /v1/summary`, ETag/304) a Stream
// Deck key or a sleeping laptop can poll cheaply. Injected per active epic.
type SummaryRow struct {
	Blocked  bool // lifecycle 'blocked' (or otherwise not making progress)
	OnTask   bool // the deterministic on_task rollup for this epic
	Stranded bool // stuck in launching / orphaned with no live master (plan §13)
}

// Summary is the fleet counts-only rollup (plan §15.16c). Every field is a small
// number/flag so a constrained consumer can render the whole fleet from one poll.
type Summary struct {
	DigestSeq            int64       `json:"digest_seq"`
	AttentionTotal       int         `json:"attention_total"`
	ByPriority           map[int]int `json:"by_priority"`
	EpicsBlocked         int         `json:"epics_blocked"`
	EpicsOnTask          int         `json:"epics_on_task"`
	DispatchPaused       bool        `json:"dispatch_paused"`
	Stranded             int         `json:"stranded"`
	WorstAccountSeverity string      `json:"worst_account_severity"`
}

// Summarize reduces the fleet's injected rows into the counts-only Summary. PURE. Only
// OPEN attention items count toward the totals (a resolved item is not pending), and
// worst_account_severity honors §12.14 stale suppression — a critical reading that is
// probe_stale does NOT escalate the fleet's worst severity.
func Summarize(seq int64, rows []SummaryRow, items []attention.Item, accounts []AccountSummary, dispatchPaused bool) Summary {
	s := Summary{
		DigestSeq:            seq,
		ByPriority:           map[int]int{},
		DispatchPaused:       dispatchPaused,
		WorstAccountSeverity: "normal",
	}
	for _, r := range rows {
		if r.Blocked {
			s.EpicsBlocked++
		}
		if r.OnTask {
			s.EpicsOnTask++
		}
		if r.Stranded {
			s.Stranded++
		}
	}
	for _, it := range items {
		if !openAttentionState(it.State) {
			continue
		}
		s.AttentionTotal++
		s.ByPriority[it.Priority]++
	}
	for _, a := range accounts {
		if a.Severity == SeverityCritical && !a.ProbeStale {
			s.WorstAccountSeverity = SeverityCritical
		}
	}
	return s
}
