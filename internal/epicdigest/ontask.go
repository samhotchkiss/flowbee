package epicdigest

import (
	"time"

	"github.com/samhotchkiss/flowbee/internal/attention"
)

// Pane-state tokens the rollup keys on (mirror internal/tmuxio.State values; kept as
// local literals so this core package does not import tmuxio, which shells out).
const (
	paneWorking      = "working"
	paneIdleAtPrompt = "idle_at_prompt"
)

// nonHaltingKinds are the attention kinds an OPEN item may carry WITHOUT knocking an
// epic off-task: fleet-level notices and the terminal-good "finished" signal. Every
// other (valid) kind is a "someone must look" condition that means the builder is not
// simply progressing — so on_task=false while it is open.
var nonHaltingKinds = map[attention.Kind]bool{
	attention.KindMasterAbsent:       true, // fleet-level: no master registered
	attention.KindCIInfraIncident:    true, // fleet-level: suspected infra flake banner
	attention.KindMergeMainSuggested: true, // advisory: main moved adjacent to scope
	attention.KindEpicFinished:       true, // terminal-good: handled via the review handoff
}

// openAttentionStates are the item states that still demand attention (an item the
// master has already resolved no longer knocks the epic off-task).
func openAttentionState(state string) bool {
	switch state {
	case attention.StateOpen, attention.StateLeased, attention.StateDelivering, attention.StateAwaitingAck:
		return true
	}
	return false
}

// OnTask is the deterministic §2.1 rollup: true iff the pane is WORKING (or IDLE with
// recent progress) AND no open halting attention item AND no fired drift signal AND the
// bound account is not critically capped (non-stale) AND context_pct is above the floor
// (an UNKNOWN context is never held against the epic — plan §2.3 "NOT a false stall").
// PURE over its Input. Lets a master eyeball a fleet and descend only where false.
func OnTask(in Input) bool {
	ok, _ := onTask(in)
	return ok
}

// onTask returns the rollup plus the first disqualifying reason ("" when on-task) — the
// reason is what the table-driven tests assert against, and a caller may surface it.
func onTask(in Input) (bool, string) {
	cfg := in.Config.withDefaults()
	e := in.Epic

	switch e.PaneState {
	case paneWorking:
		// a running turn is progress.
	case paneIdleAtPrompt:
		if !recentProgress(e, in.Now, cfg.RecentProgressWindow) {
			return false, "idle at prompt with no recent commit or status update"
		}
	default:
		// unknown / awaiting_input / goal_blocked: not observably progressing.
		return false, "pane not working and not idle-with-recent-progress"
	}

	if hasOpenHaltingItem(in.Attention) {
		return false, "an open halting attention item is pending"
	}
	if len(e.DriftSignals) > 0 {
		return false, "a deterministic drift signal has fired"
	}
	if in.Account.CriticalNonStale() {
		return false, "the bound account is critically capped (non-stale)"
	}
	if e.ContextPct >= 0 && e.ContextPct < cfg.ContextFloorPct {
		return false, "context_pct is below the floor"
	}
	return true, ""
}

// recentProgress reports whether the epic committed or updated its status within the
// window — the signal that an IDLE_AT_PROMPT pane is between turns of real work rather
// than genuinely stalled.
func recentProgress(e Epic, now time.Time, window time.Duration) bool {
	if !e.LastCommitAt.IsZero() && now.Sub(e.LastCommitAt) >= 0 && now.Sub(e.LastCommitAt) <= window {
		return true
	}
	if !e.StatusUpdatedAt.IsZero() && now.Sub(e.StatusUpdatedAt) >= 0 && now.Sub(e.StatusUpdatedAt) <= window {
		return true
	}
	return false
}

// hasOpenHaltingItem reports whether any injected attention item is OPEN (still
// demanding attention) and of a HALTING kind (not one of the fleet/terminal-good
// exceptions). An item with an unknown kind is treated as halting (conservative — an
// unclassifiable pending condition should surface, not be silently ignored).
func hasOpenHaltingItem(items []attention.Item) bool {
	for _, it := range items {
		if !openAttentionState(it.State) {
			continue
		}
		if nonHaltingKinds[it.Kind] {
			continue
		}
		return true
	}
	return false
}

// DefaultCompactionJumpPoints is the remaining-context rise (in percentage points)
// between two observations that reads as a self-compaction event rather than drift.
const DefaultCompactionJumpPoints = 15.0

// CompactionJumped reports whether remaining-context RISING from prev to cur by at least
// thresholdPoints is a compaction (plan §15.3): the running session summarized/compacted
// its own context, FREEING tokens — so context_pct jumps UP. This must NOT be read as a
// stall or drift, and a steer sent mid-compaction is swallowed. Both readings must be
// KNOWN (>= 0); an unknown reading (-1) never signals compaction. A non-positive
// threshold falls back to the default so a caller cannot accidentally match every tick.
// PURE — the ticker calls it to suppress wrong actions on a context rise.
func CompactionJumped(prev, cur, thresholdPoints float64) bool {
	if prev < 0 || cur < 0 {
		return false
	}
	if thresholdPoints <= 0 {
		thresholdPoints = DefaultCompactionJumpPoints
	}
	return cur-prev >= thresholdPoints
}
