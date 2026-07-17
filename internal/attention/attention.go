// Package attention is the deterministic decision core for the epic-lane attention
// queue (epic-lane Phase 5, plan §1). Like internal/scheduler and internal/lease it
// is a PURE-core package (DESIGN §1.2, enforced by tools/archcheck): every function
// is a total function of injected values — no clock, no randomness, no ID minter, no
// I/O. `time.Time`/`time.Duration` appear only as VALUES (the instant is always
// passed in, never read from a clock). internal/store wires these decisions into the
// serialized store transactions exactly as it wires scheduler.Pick.
//
// The three decisions that live here (plan §14 "New decision logic is pure-core"):
//   - lease-grant   — which of the eligible OPEN items a master may lease now, given
//     max/kinds and the one-in-flight-per-epic rule (GrantLease);
//   - fence         — is a fenced deliver/resolve call from the live incarnation, or a
//     superseded zombie? (FenceOK — the exactly-once predicate);
//   - escalation    — per-kind tiering (human-first vs master-first vs fast-retry) of
//     when an unhandled item must reach a human (plan §15.4), plus the
//     send-and-ack timeout (plan §12.3).
package attention

import (
	"sort"
	"time"
)

// Kind is the attention-item taxonomy (plan §1.3). NOTE: these are ATTENTION kinds,
// not session states — a healthy-but-paused Codex session (`goal_paused`, plan §15.1)
// is a session state the watchdog/digest carries and recovers via the family Resume()
// verb; it is deliberately NOT an attention kind (no master judgment needed), so it is
// absent here by design.
type Kind string

const (
	KindScopeViolation      Kind = "scope_violation"       // prio 5  — epic diff out of scope, or a main-merge into a reserved tree
	KindLaunchFailed        Kind = "launch_failed"         // prio 10 — epic start rollback / launching-reaper
	KindBlockedNonResumable Kind = "blocked_non_resumable" // prio 10 — watchdog blockInfra/SetNeedsOperator
	KindAuthDead            Kind = "auth_dead"             // prio 10 — auth-death classifier; HUMAN-ONLY re-login
	KindMasterAbsent        Kind = "master_absent"         // prio 3  — reaper: high-pri item unleased with no live master
	KindWedgedUI            Kind = "wedged_ui"             // prio 15 — pane stuck in modal/copy-mode
	KindDriftSuspect        Kind = "drift_suspect"         // prio 15 — drift detector after advisor says "off"
	KindUsageCritical       Kind = "usage_critical"        // prio 15 — capacity monitor: assigned account critical
	KindStalled             Kind = "stalled"               // prio 15 — session stalled (plan §15.4)
	KindNeedsInput          Kind = "needs_input"           // prio 20 — pane AWAITING_INPUT (blocking vs non-blocking, plan §15.4)
	KindCIRedOnEpicPR       Kind = "ci_red_on_epic_pr"     // prio 20 — CI red on epic head (real, not flake)
	KindCIInfraIncident     Kind = "ci_infra_incident"     // prio 25 — suspected infra flake; fleet banner, never pages
	KindMergeMainSuggested  Kind = "merge_main_suggested"  // prio 35 — main moved adjacent to active scope
	KindEpicFinished        Kind = "epic_finished"         // prio 40 — ingestion saw State: done/achieved
	KindSendUnverified      Kind = "send_unverified"       // delivery-failed / send-unverified: fast master retry (plan §15.4)
	KindDepFailed           Kind = "dep_failed"            // prio 15 — a queued epic's blocker terminated unsuccessfully (plan §15.12)
)

// knownKinds is the CLOSED attention-kind enum. ValidKind gates any place a kind
// crosses a trust boundary as free text — e.g. the internal/verbs NotifyMaster ping,
// which must never template an unvalidated string into a master's pane (plan §15.10).
var knownKinds = map[Kind]bool{
	KindScopeViolation: true, KindLaunchFailed: true, KindBlockedNonResumable: true,
	KindAuthDead: true, KindMasterAbsent: true, KindWedgedUI: true, KindDriftSuspect: true,
	KindUsageCritical: true, KindStalled: true, KindNeedsInput: true, KindCIRedOnEpicPR: true,
	KindCIInfraIncident: true, KindMergeMainSuggested: true, KindEpicFinished: true,
	KindSendUnverified: true, KindDepFailed: true,
}

// ValidKind reports whether k is a member of the closed attention-kind enum.
func ValidKind(k string) bool { return knownKinds[Kind(k)] }

// Item is the projection of an attention_items row the decisions read. The store
// folds a row into this; the pure core never sees the DB.
type Item struct {
	ID            string
	Kind          Kind
	EpicID        string
	Priority      int // lower = more urgent (0021 convention)
	State         string
	Blocking      bool
	FirstSeenAt   time.Time // when the condition first opened (aging clock)
	AwaitingSince time.Time // when it entered awaiting_ack (the send-and-ack clock)
	ItemEpoch     int
	LeasedBy      string
}

// Item states (mirror the attention_items.state machine).
const (
	StateOpen        = "open"
	StateLeased      = "leased"
	StateDelivering  = "delivering"
	StateAwaitingAck = "awaiting_ack"
	StateResolved    = "resolved"
)

// ── priority ordering ──

// Order returns items most-urgent first: priority ascending (lower = more urgent),
// then oldest FirstSeenAt (a genuinely-aged item outranks a fresh one at the same
// band — the "aged? keep simple: priority then age" rule), then ID for determinism.
// Pure and stable; does not mutate the input.
func Order(items []Item) []Item {
	out := append([]Item(nil), items...)
	sort.SliceStable(out, func(i, k int) bool {
		if out[i].Priority != out[k].Priority {
			return out[i].Priority < out[k].Priority
		}
		if !out[i].FirstSeenAt.Equal(out[k].FirstSeenAt) {
			return out[i].FirstSeenAt.Before(out[k].FirstSeenAt)
		}
		return out[i].ID < out[k].ID
	})
	return out
}

// ── lease-grant decision ──

// GrantLease chooses which OPEN items a master may lease this batch (plan §1.4).
// Rules, all structural:
//   - only state=open items are eligible;
//   - if kinds is non-empty, only those kinds are eligible;
//   - ONE in-flight item per epic across all masters — an item whose epic already has
//     an in-flight (leased/delivering/awaiting_ack) item is skipped, and within this
//     same batch at most one item per epic is granted (never two masters, and never one
//     master twice, driving one pane);
//   - items with an empty EpicID (e.g. master_absent) are NOT epic-scoped, so the
//     one-per-epic rule never suppresses them;
//   - most-urgent first (Order), capped at max.
//
// inFlightEpics is the set of epic ids that ALREADY hold an in-flight item; it is read
// only (not mutated). Returns the granted items in grant order.
func GrantLease(open []Item, inFlightEpics map[string]bool, max int, kinds []string) []Item {
	if max <= 0 {
		return nil
	}
	want := kindSet(kinds)
	claimed := make(map[string]bool, len(inFlightEpics))
	for e := range inFlightEpics {
		claimed[e] = true
	}
	var granted []Item
	for _, it := range Order(open) {
		if len(granted) >= max {
			break
		}
		if it.State != StateOpen {
			continue
		}
		if want != nil && !want[it.Kind] {
			continue
		}
		if it.EpicID != "" && claimed[it.EpicID] {
			continue
		}
		granted = append(granted, it)
		if it.EpicID != "" {
			claimed[it.EpicID] = true
		}
	}
	return granted
}

func kindSet(kinds []string) map[Kind]bool {
	if len(kinds) == 0 {
		return nil
	}
	m := make(map[Kind]bool, len(kinds))
	for _, k := range kinds {
		m[Kind(k)] = true
	}
	return m
}

// ── fence decision ──

// Fence carries the claimed vs live facts a fenced call presents (plan §1.5 step 1).
type Fence struct {
	State                string // the item's current state
	ExpectState          string // the state the call requires (leased for BeginDelivery, delivering for the verdict)
	LeasedBy             string // supervisors.id currently holding the lease
	Caller               string // the caller's master_id
	ClaimItemEpoch       int
	LiveItemEpoch        int
	ClaimSupervisorEpoch int
	LiveSupervisorEpoch  int
}

// FenceOK is the pure exactly-once fence predicate — the same fencing that stops two
// workers double-completing a job (internal/lease). A fenced deliver/resolve is
// honored ONLY when the item is in the expected state, the caller holds the lease, the
// item_epoch matches, AND the caller's supervisor epoch matches the live one. ANY
// mismatch => the caller is a superseded incarnation (a post-`/clear` or crashed-and-
// re-registered master) and the store rejects it with lease.ErrStaleEpoch (409 fenced).
func FenceOK(f Fence) bool {
	return f.State == f.ExpectState &&
		f.LeasedBy != "" && f.LeasedBy == f.Caller &&
		f.ClaimItemEpoch == f.LiveItemEpoch &&
		f.ClaimSupervisorEpoch == f.LiveSupervisorEpoch
}

// ── escalation decision (plan §15.4) ──

// Tier classifies HOW an unhandled item reaches a human. It is a DESIGN-FIXED property
// of the kind (not operator config); only the windows below are tunable.
type Tier int

const (
	// TierMasterFirst: a master decision by design. Escalate to a human ONLY if the
	// item is still unleased past the kind's window (human-immediate would cry wolf —
	// usage_critical is the canonical example, plan §15.4).
	TierMasterFirst Tier = iota
	// TierHumanImmediate: no master window — escalate to a human as soon as it is open
	// (auth_dead, wedged_ui, master_absent, launch_failed).
	TierHumanImmediate
	// TierFastRetry: a delivery-failed / send-unverified item the master should re-attempt
	// on a short budget (~2-5m), NOT a long TTL; only after the retry window is exhausted
	// does the caller escalate (as blocked_non_resumable).
	TierFastRetry
	// TierNeverPage: surfaced but never pages a human (epic_finished, merge_main_suggested,
	// ci_infra_incident — plan §1.6 "low-priority items do not page").
	TierNeverPage
)

// TierFor maps a kind to its escalation tier (plan §15.4).
func TierFor(kind Kind) Tier {
	switch kind {
	case KindAuthDead, KindWedgedUI, KindMasterAbsent, KindLaunchFailed:
		return TierHumanImmediate
	case KindSendUnverified:
		return TierFastRetry
	case KindEpicFinished, KindMergeMainSuggested, KindCIInfraIncident:
		return TierNeverPage
	default:
		// usage_critical, drift_suspect, ci_red_on_epic_pr, needs_input, stalled,
		// scope_violation, blocked_non_resumable.
		return TierMasterFirst
	}
}

// Policy holds the tunable escalation windows. DefaultPolicy is the shipped posture
// (plan §15.4); tests override individual fields.
type Policy struct {
	// Window is the per-kind master-first escalation window; a missing kind falls back
	// to DefaultMasterWindow.
	Window map[Kind]time.Duration
	// NeedsInput is tiered by the item's Blocking flag (plan §15.4).
	NeedsInputBlockingWindow    time.Duration
	NeedsInputNonBlockingWindow time.Duration
	// DefaultMasterWindow backstops any master-first kind not in Window.
	DefaultMasterWindow time.Duration
	// FastRetryWindow bounds the delivery-failed fast-retry tier.
	FastRetryWindow time.Duration
	// MasterStaleAfter: a heartbeat older than this means no live master (plan §1.6),
	// the "no master" input to ShouldRaiseMasterAbsent.
	MasterStaleAfter time.Duration
}

// DefaultPolicy is the shipped escalation posture (plan §15.4).
func DefaultPolicy() Policy {
	return Policy{
		Window: map[Kind]time.Duration{
			KindUsageCritical:       10 * time.Minute,
			KindDriftSuspect:        15 * time.Minute,
			KindCIRedOnEpicPR:       15 * time.Minute,
			KindStalled:             15 * time.Minute,
			KindDepFailed:           15 * time.Minute, // a queued epic's blocker failed: master re-plans/drops/forces
			KindScopeViolation:      5 * time.Minute,  // a contract breach: short master window, then human
			KindBlockedNonResumable: 10 * time.Minute,
		},
		NeedsInputBlockingWindow:    10 * time.Minute,
		NeedsInputNonBlockingWindow: 30 * time.Minute,
		DefaultMasterWindow:         15 * time.Minute,
		FastRetryWindow:             5 * time.Minute,
		MasterStaleAfter:            3 * time.Minute,
	}
}

// windowFor resolves the escalation window for a master-first item.
func windowFor(pol Policy, item Item) time.Duration {
	if item.Kind == KindNeedsInput {
		if item.Blocking {
			return pol.NeedsInputBlockingWindow
		}
		return pol.NeedsInputNonBlockingWindow
	}
	if w, ok := pol.Window[item.Kind]; ok {
		return w
	}
	return pol.DefaultMasterWindow
}

// age is the item's open age, guarded against a negative (clock-skew / zero) reading.
func age(item Item, now time.Time) time.Duration {
	if item.FirstSeenAt.IsZero() {
		return 0
	}
	d := now.Sub(item.FirstSeenAt)
	if d < 0 {
		return 0
	}
	return d
}

// NeedsHuman reports whether an OPEN item warrants human attention now, per its tier.
// An item that is leased/delivering/awaiting_ack/resolved is BEING handled -> false
// (the master or the ack loop owns it). Pure in (item, now, pol).
func NeedsHuman(item Item, now time.Time, pol Policy) bool {
	if item.State != StateOpen {
		return false
	}
	switch TierFor(item.Kind) {
	case TierHumanImmediate:
		return true
	case TierMasterFirst:
		return age(item, now) >= windowFor(pol, item)
	default: // TierFastRetry, TierNeverPage
		return false
	}
}

// ShouldMasterRetry reports whether a delivery-failed / send-unverified item is still
// within its fast-retry budget (the master should re-attempt). Past the window it is no
// longer a fast retry and the caller escalates it (plan §15.4). Pure.
func ShouldMasterRetry(item Item, now time.Time, pol Policy) bool {
	if item.State != StateOpen || TierFor(item.Kind) != TierFastRetry {
		return false
	}
	return age(item, now) < pol.FastRetryWindow
}

// MasterLive reports whether a heartbeat at lastHeartbeat is still live at now (plan
// §1.6). A zero heartbeat (never registered / never beat) is never live.
func MasterLive(lastHeartbeat, now time.Time, pol Policy) bool {
	if lastHeartbeat.IsZero() {
		return false
	}
	return now.Sub(lastHeartbeat) <= pol.MasterStaleAfter
}

// ShouldRaiseMasterAbsent is the reaper's decision to raise the master_absent alarm
// (plan §1.6): the master is NOT live AND at least one open item already needs human
// attention (a human-immediate kind, or a master-first kind aged past its window). A
// live master will lease the items itself, so the alarm stays dark. The alarm item's
// own kind (master_absent) is skipped so the check never feeds on itself.
//
// lastHeartbeat is a SINGLE liveness reading: with a FLEET of masters the caller passes
// max(last_heartbeat) over the non-stale supervisors (the freshest live heartbeat) — if
// any master is live, the fleet is live and the alarm stays dark. The tier model above
// (plan §15.4) subsumes the older "priority ≤ 15 past T_absent" rule; the plan doc has
// been reconciled to it, so no additional threshold input is needed here.
func ShouldRaiseMasterAbsent(items []Item, lastHeartbeat, now time.Time, pol Policy) bool {
	if MasterLive(lastHeartbeat, now, pol) {
		return false
	}
	for _, it := range items {
		if it.Kind == KindMasterAbsent {
			continue
		}
		if NeedsHuman(it, now, pol) {
			return true
		}
	}
	return false
}

// AckExpired is the send-and-ack timeout (plan §12.3): an awaiting_ack item whose steer
// was not confirmed PROCESSED within tAck should be reopened as steer_not_processed (a
// politely-stalling agent that absorbed a nudge and kept drifting must not look
// handled). Delivery verification proved SUBMISSION; this proves PROCESSING. Pure.
func AckExpired(item Item, now time.Time, tAck time.Duration) bool {
	if item.State != StateAwaitingAck || item.AwaitingSince.IsZero() {
		return false
	}
	return now.Sub(item.AwaitingSince) >= tAck
}
