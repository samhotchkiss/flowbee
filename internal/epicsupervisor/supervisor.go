// Package epicsupervisor is the impure orchestration shell for the ONE consolidated
// epic-supervision pass (epic-lane Phase 6b, plan §12.2). It is deliberately a SINGLE
// serialized batch — NOT six tickers — that folds, per pass, over the active epics and
// the attention queue: classify each pane, produce/auto-resolve typed attention items,
// reap stranded launches and expired leases, recover crash-window deliveries, run the
// send-and-ack loop, reap dead masters, and push-to-wake an idle master (plan §1.5/§1.6/
// §12.3/§15.4/§15.10). Every DECISION comes from the pure cores (internal/attention,
// internal/epicdigest); this package only wires them into serialized store writes through
// the injected Pane seam, so it is testable without a live tmux (the acceptance test
// injects a fake Pane) and adds a bounded, batched SQLite write budget (plan §12.2).
package epicsupervisor

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/samhotchkiss/flowbee/internal/attention"
	"github.com/samhotchkiss/flowbee/internal/epicdigest"
	"github.com/samhotchkiss/flowbee/internal/store"
	"github.com/samhotchkiss/flowbee/internal/tmuxio"
	"github.com/samhotchkiss/flowbee/internal/ulid"
	"github.com/samhotchkiss/flowbee/internal/verbs"
)

// Pane is the tmux seam the pass observes and (for push-to-wake) types through. The
// production impl (TmuxPane) wraps a per-host tmuxio.Client; tests inject a fake so the
// store transitions are exercised without a live tmux.
type Pane interface {
	// Classify returns the tmuxio.Classify state token of the epic's session pane and the
	// last non-empty line (evidence), or an error if the pane cannot be captured.
	Classify(ctx context.Context, host, session string) (state string, lastLine string, err error)
	// Deliver types a push-to-wake ping (a closed-template DATA line, never pane content)
	// into a master's IDLE pane and returns the tmuxio verdict.
	Deliver(ctx context.Context, host, session, message string) (verdict string, err error)
	// Stop idempotently stops one exact tmux session and confirms it is absent. A caller
	// must retain the epic's seat/scope reservation when Stop cannot prove that state.
	Stop(ctx context.Context, host, session string) error
}

// ContextProber resolves an epic session's remaining-context % from disk (internal/
// ctxprobe). Optional (nil = skip): the pass never GUESSES a context %; an unresolved
// reading leaves the stored value untouched (plan §12.4).
type ContextProber interface {
	ContextPct(ctx context.Context, epic store.EpicRun) (pct float64, ok bool)
}

// Config tunes the pass. Zero fields fall back to shipped defaults.
type Config struct {
	Policy            attention.Policy
	AckTimeout        time.Duration // T_ack (plan §12.3, default 6m)
	HeartbeatInterval time.Duration // supervisor heartbeat interval (stale = 3× this)
	PingInterval      time.Duration // push-to-wake rate limit T_ping (default 2m)
	LaunchStrandAfter time.Duration // a 'launching' epic older than this is stranded (default 10m)
	CompactionJumpPts float64       // context% jump treated as a compaction, not drift
}

func (c Config) withDefaults() Config {
	if c.Policy.MasterStaleAfter == 0 {
		c.Policy = attention.DefaultPolicy()
	}
	if c.AckTimeout <= 0 {
		c.AckTimeout = 6 * time.Minute
	}
	if c.HeartbeatInterval <= 0 {
		c.HeartbeatInterval = 30 * time.Second
	}
	if c.PingInterval <= 0 {
		c.PingInterval = 2 * time.Minute
	}
	if c.LaunchStrandAfter <= 0 {
		c.LaunchStrandAfter = 10 * time.Minute
	}
	if c.CompactionJumpPts <= 0 {
		c.CompactionJumpPts = epicdigest.DefaultCompactionJumpPoints
	}
	return c
}

// Supervisor holds the pass's injected deps.
type Supervisor struct {
	store   *store.Store
	pane    Pane
	ctx     ContextProber
	cfg     Config
	logger  *slog.Logger
	mu      sync.Mutex
	lastPin map[string]time.Time // per-master push-to-wake rate limit
}

// New builds a Supervisor. pane MUST be non-nil (the pass classifies panes); ctxProber may
// be nil (context% probing skipped).
func New(st *store.Store, pane Pane, ctxProber ContextProber, cfg Config, logger *slog.Logger) *Supervisor {
	if logger == nil {
		logger = slog.Default()
	}
	return &Supervisor{store: st, pane: pane, ctx: ctxProber, cfg: cfg.withDefaults(),
		logger: logger, lastPin: map[string]time.Time{}}
}

// Pass runs one full consolidated supervision batch at `now`. Every sub-step is wrapped so
// one epic's / one item's error is logged and skipped — a wedged pane must never blind the
// whole pass (the same degrade-to-inert posture the watchdog and status ingestion take).
//
// PANIC HARDENING (M1, mirroring watchdog.Watcher.Pass): this pass parses UNTRUSTED
// builder-pushed markdown (status ingestion, upstream of this call) and TYPES KEYSTROKES
// into live panes, all inside the control-plane process. A panic here — a nil pane edge,
// a bad IANA name, a malformed capture — must NEVER take `flowbee serve` down (with
// Restart=always a persistent trigger would crashloop the WHOLE control plane, not just
// epic supervision). TWO layers: a per-EPIC recover (observeEpicSafe) so one malformed
// pane skips while the rest process, and this per-PASS recover as the backstop for the
// non-per-epic steps. The next 2-minute tick starts clean.
func (s *Supervisor) Pass(ctx context.Context, now time.Time) {
	defer func() {
		if r := recover(); r != nil {
			s.logger.Error("epic supervision: PANIC recovered — pass skipped", "panic", r)
		}
	}()
	epics, err := s.store.ListActiveEpicRuns(ctx)
	if err != nil {
		s.logger.Error("epic supervision: list active epics", "err", err)
		return
	}
	// paneStates + workingTransition let the ack loop reuse this pass's classification (and
	// whether the pane transitioned INTO working this pass) without re-capturing.
	paneStates := map[string]string{}
	workingTransition := map[string]bool{}
	for _, e := range epics {
		st, transitioned := s.observeEpicSafe(ctx, e, now)
		paneStates[e.ID] = st
		workingTransition[e.ID] = transitioned
	}
	s.reapStrandedLaunches(ctx, epics, now)
	s.recoverStrandedDeliveries(ctx, now)
	s.reapExpiredLeases(ctx, now)
	s.runAckLoop(ctx, workingTransition, now)
	s.reapDeadMasters(ctx, now)
	s.pushToWake(ctx, now)
}

// observeEpicSafe wraps observeAndProduce in a PER-EPIC recover (M1): a panic while
// classifying/producing for ONE epic (a nil pane, a malformed capture) logs and skips that
// epic, and the loop continues with the rest — one bad pane never aborts the whole batch.
// On panic it returns the epic's prior pane state and no transition (a safe ack-loop no-op).
func (s *Supervisor) observeEpicSafe(ctx context.Context, e store.EpicRun, now time.Time) (state string, transitioned bool) {
	state = e.PaneState
	defer func() {
		if r := recover(); r != nil {
			s.logger.Error("epic supervision: per-epic PANIC recovered — epic skipped", "epic", e.ID, "panic", r)
		}
	}()
	return s.observeAndProduce(ctx, e, now)
}

// observeAndProduce classifies one epic's pane, writes its disk/pane-derived runtime facts,
// and produces/auto-resolves the typed attention items for its condition (plan §2 producers
// + §12.13 taxonomy). Returns the classified pane state and whether the pane TRANSITIONED
// INTO working this pass (the send-and-ack "processed, not merely busy" signal, §12.3 — a
// pane already working that merely stays working is NOT a transition). A compaction (a
// context% jump) is recognized and NOT treated as drift (plan §15.3).
func (s *Supervisor) observeAndProduce(ctx context.Context, e store.EpicRun, now time.Time) (string, bool) {
	prior := e.PaneState
	paneState := prior
	lastLine := ""
	if st, ll, err := s.pane.Classify(ctx, "", e.TmuxName); err != nil {
		s.logger.Warn("epic supervision: classify pane", "epic", e.ID, "err", err)
	} else {
		paneState = st
		lastLine = ll
	}
	transitionedToWorking := prior != string(tmuxio.StateWorking) && paneState == string(tmuxio.StateWorking)

	contextPct := e.ContextPct
	if s.ctx != nil {
		if pct, ok := s.ctx.ContextPct(ctx, e); ok {
			// A jump UP is a self-compaction (context freed), recognized and NOT drift.
			if epicdigest.CompactionJumped(e.ContextPct, pct, s.cfg.CompactionJumpPts) {
				s.logger.Info("epic supervision: context compaction (not drift)", "epic", e.ID, "from", e.ContextPct, "to", pct)
			}
			contextPct = pct
		}
	}

	// Write the runtime facts (preserving auth/commit, which this pass does not recompute).
	if err := s.store.SetEpicRuntimeState(ctx, e.ID, store.EpicRuntimeState{
		ContextPct: contextPct, PaneState: paneState, AuthState: e.AuthState, LastCommitAt: e.LastCommitAt,
	}, now); err != nil {
		s.logger.Warn("epic supervision: set runtime state", "epic", e.ID, "err", err)
	}

	s.produceForEpic(ctx, e, paneState, lastLine, now)
	return paneState, transitionedToWorking
}

// produceForEpic upserts (or auto-resolves) the attention items a single epic's observed
// condition implies. Every producer is idempotent: a re-seen condition refreshes the one
// deduped row; a cleared condition auto-resolves it (plan §1.3 dedup discipline, both
// directions).
func (s *Supervisor) produceForEpic(ctx context.Context, e store.EpicRun, paneState, lastLine string, now time.Time) {
	// needs_input — the pane is showing a dialog/menu/permission prompt (AWAITING_INPUT).
	if paneState == string(tmuxio.StateAwaitingInput) {
		s.upsert(ctx, e, attention.KindNeedsInput, 20, e.ID+":needs_input", true,
			map[string]string{"pane_state": paneState, "last_line": clip(lastLine, 200)}, now)
	} else {
		s.clear(ctx, e.ID+":needs_input", now)
	}

	// blocked_non_resumable — the lifecycle state reached 'blocked'.
	if e.State == "blocked" {
		s.upsert(ctx, e, attention.KindBlockedNonResumable, 10, e.ID+":blocked", true,
			map[string]string{"blockers": clip(e.StatusBlockers, 300)}, now)
	} else {
		s.clear(ctx, e.ID+":blocked", now)
	}

	// auth_dead — a distinct human-only state (plan §12.4/§12.13): never auto-resumed.
	if e.AuthState == "auth_dead" {
		s.upsert(ctx, e, attention.KindAuthDead, 10, e.ID+":auth_dead", true,
			map[string]string{"detail": "auth_dead — human re-login required"}, now)
	} else {
		s.clear(ctx, e.ID+":auth_dead", now)
	}

	// usage_critical — the BOUND account is critically capped AND the reading is trustworthy
	// (plan §12.14: a probe_stale critical is SUPPRESSED, never a phantom off flaky ssh).
	if e.AccountKey != "" {
		if aw, ok, err := s.store.GetAccountWindow(ctx, e.AccountKey); err == nil && ok {
			if aw.CriticalNonStale() {
				s.upsert(ctx, e, attention.KindUsageCritical, 15, e.ID+":usage:"+e.AccountKey, false,
					map[string]string{"account": e.AccountKey, "weekly_pct": ftoa(aw.WeeklyPct), "severity": aw.Severity}, now)
			} else {
				s.clear(ctx, e.ID+":usage:"+e.AccountKey, now)
			}
		}
	}
}

// reapStrandedLaunches abandons any epic stuck in 'launching' past LaunchStrandAfter (plan
// §13 — AddEpicRun writes 'launching' BEFORE the tmux session is confirmed up, so a crash
// mid-launch strands the row and permanently holds the host/scope reservation). It first
// stops the remote agent session (when any) and the local attach, and releases capacity
// only after both exact sessions are confirmed absent. A launch_failed item routes cleanup
// failures and retries without ever overbooking around a possibly-live agent.
func (s *Supervisor) reapStrandedLaunches(ctx context.Context, epics []store.EpicRun, now time.Time) {
	for _, e := range epics {
		if e.State != "launching" {
			continue
		}
		started := store.ParseTimeOrZero(e.CreatedAt)
		if started.IsZero() || now.Sub(started) < s.cfg.LaunchStrandAfter {
			continue
		}
		if e.TmuxName == "" {
			s.upsert(ctx, e, attention.KindLaunchFailed, 10, e.ID+":launch_failed", true,
				map[string]string{"detail": "launch stranded but has no registered tmux session; capacity retained because cleanup cannot be confirmed"}, now)
			continue
		}
		if e.Host != "" {
			if err := s.pane.Stop(ctx, e.Host, e.TmuxName); err != nil {
				s.upsert(ctx, e, attention.KindLaunchFailed, 10, e.ID+":launch_failed", true,
					map[string]string{"detail": "launch stranded; remote tmux cleanup unconfirmed, so host/scope capacity remains reserved: " + err.Error()}, now)
				s.logger.Warn("epic supervision: stranded launch remote cleanup unconfirmed; capacity retained", "epic", e.ID, "host", e.Host, "err", err)
				continue
			}
		}
		if err := s.pane.Stop(ctx, "", e.TmuxName); err != nil {
			s.upsert(ctx, e, attention.KindLaunchFailed, 10, e.ID+":launch_failed", true,
				map[string]string{"detail": "launch stranded; local attach cleanup unconfirmed, so host/scope capacity remains reserved: " + err.Error()}, now)
			s.logger.Warn("epic supervision: stranded launch local cleanup unconfirmed; capacity retained", "epic", e.ID, "err", err)
			continue
		}
		if err := s.store.AbandonEpicRun(ctx, e.ID, now); err != nil {
			s.logger.Warn("epic supervision: reap stranded launch", "epic", e.ID, "err", err)
			continue
		}
		s.upsert(ctx, e, attention.KindLaunchFailed, 10, e.ID+":launch_failed", true,
			map[string]string{"detail": "launch stranded in 'launching' — host/scope released"}, now)
		s.logger.Warn("epic supervision: reaped stranded launch (host/scope released)", "epic", e.ID)
	}
}

// recoverStrandedDeliveries handles the crash-window (plan §1.5): a master that crashed
// between a verified send and recording the verdict leaves an item 'delivering'. The pass
// re-captures the pane and asks whether the steer appears to have LANDED — if the pane is
// WORKING (the steer was absorbed), recover to awaiting_ack idempotently (NEVER a second
// send); otherwise reopen for a fresh decision. It never re-sends.
func (s *Supervisor) recoverStrandedDeliveries(ctx context.Context, now time.Time) {
	stranded, err := s.store.ListStrandedDeliveries(ctx, now)
	if err != nil {
		s.logger.Warn("epic supervision: list stranded deliveries", "err", err)
		return
	}
	for _, it := range stranded {
		landed := false
		if it.EpicID != "" {
			if e, gerr := s.store.GetEpicRun(ctx, it.EpicID); gerr == nil {
				if st, _, cerr := s.pane.Classify(ctx, "", e.TmuxName); cerr == nil {
					landed = st == string(tmuxio.StateWorking)
				}
			}
		}
		var rerr error
		if landed {
			rerr = s.store.RecoverStrandedAwaitingAck(ctx, it.ID, it.DeliveryKey, now)
		} else {
			rerr = s.store.ReopenStranded(ctx, it.ID, now)
		}
		if rerr != nil {
			s.logger.Warn("epic supervision: recover stranded delivery", "item", it.ID, "landed", landed, "err", rerr)
		}
	}
}

// reapExpiredLeases returns leased-but-expired items to open (plan §1.4/§1.6) — a master
// that died/stalled and let the lease TTL pass. Durable items simply wait for the next
// live master.
func (s *Supervisor) reapExpiredLeases(ctx context.Context, now time.Time) {
	if _, err := s.store.ReapExpiredLeases(ctx, now); err != nil {
		s.logger.Warn("epic supervision: reap expired leases", "err", err)
	}
}

// runAckLoop closes the send-and-ack loop (plan §12.3): for each awaiting_ack item, if the
// epic ADVANCED in response (pane WORKING now, or status/commit moved since the send) →
// resolve as acked; else if past T_ack with no change → reopen as steer_not_processed (a
// politely-stalling agent that absorbed a nudge and kept drifting must not look handled).
func (s *Supervisor) runAckLoop(ctx context.Context, workingTransition map[string]bool, now time.Time) {
	items, err := s.store.ListOpenAttention(ctx, attention.StateAwaitingAck, nil, "")
	if err != nil {
		s.logger.Warn("epic supervision: list awaiting_ack", "err", err)
		return
	}
	for _, it := range items {
		since := store.ParseTimeOrZero(it.AwaitingSince)
		if s.epicAdvancedSince(ctx, it.EpicID, workingTransition[it.EpicID], since) {
			if aerr := s.store.AckAttention(ctx, it.ID, now); aerr != nil {
				s.logger.Warn("epic supervision: ack", "item", it.ID, "err", aerr)
			}
			continue
		}
		pure := attention.Item{State: it.State, AwaitingSince: since}
		if attention.AckExpired(pure, now, s.cfg.AckTimeout) {
			if rerr := s.store.ReopenUnacked(ctx, it.ID, now); rerr != nil {
				s.logger.Warn("epic supervision: reopen unacked", "item", it.ID, "err", rerr)
			}
		}
	}
}

// epicAdvancedSince reports whether an epic PROCESSED a steer since it entered awaiting_ack:
// its pane TRANSITIONED into working THIS pass (a real behavior change, not mere busyness —
// m3: a pane already working that merely stays working is NOT proof it acted on the steer),
// OR its ## Status / newest commit advanced after the send. This is the "processed, not
// merely submitted" proof (plan §12.3); a politely-stalling agent that swallows a nudge and
// keeps doing what it was already doing is NOT acked (it ack-expires and reopens).
func (s *Supervisor) epicAdvancedSince(ctx context.Context, epicID string, transitionedToWorking bool, since time.Time) bool {
	if epicID == "" {
		return false
	}
	if transitionedToWorking {
		return true
	}
	e, err := s.store.GetEpicRun(ctx, epicID)
	if err != nil {
		return false
	}
	if t := store.ParseTimeOrZero(e.StatusUpdatedAt); !t.IsZero() && !since.IsZero() && t.After(since) {
		return true
	}
	if t := store.ParseTimeOrZero(e.LastCommitAt); !t.IsZero() && !since.IsZero() && t.After(since) {
		return true
	}
	return false
}

// reapDeadMasters marks heartbeat-stale masters stale (reaping their leases back to open,
// plan §1.6) then raises master_absent when the pure core says a human is warranted (a live
// master would lease the items itself). Liveness = max(last_heartbeat) over non-stale
// supervisors (any live master keeps the alarm dark).
func (s *Supervisor) reapDeadMasters(ctx context.Context, now time.Time) {
	stale, err := s.store.ListStaleSupervisors(ctx, s.cfg.HeartbeatInterval, now)
	if err != nil {
		s.logger.Warn("epic supervision: list stale supervisors", "err", err)
		return
	}
	for _, sup := range stale {
		if merr := s.store.MarkSupervisorStale(ctx, sup.ID, now); merr != nil {
			s.logger.Warn("epic supervision: mark supervisor stale", "master", sup.ID, "err", merr)
		}
	}
	sups, err := s.store.ListSupervisors(ctx)
	if err != nil {
		return
	}
	freshest := time.Time{}
	for _, sup := range sups {
		if sup.State != "active" {
			continue
		}
		if t := store.ParseTimeOrZero(sup.LastHeartbeatAt); t.After(freshest) {
			freshest = t
		}
	}
	open, err := s.store.ListOpenAttention(ctx, "", nil, "")
	if err != nil {
		return
	}
	items := make([]attention.Item, 0, len(open))
	for _, it := range open {
		items = append(items, attention.Item{
			Kind: attention.Kind(it.Kind), State: it.State, Priority: it.Priority,
			Blocking: it.Blocking, FirstSeenAt: store.ParseTimeOrZero(it.FirstSeenAt),
		})
	}
	if attention.ShouldRaiseMasterAbsent(items, freshest, now, s.cfg.Policy) {
		s.upsertRaw(ctx, attention.KindMasterAbsent, 3, "master_absent", "", "", false,
			map[string]string{"detail": "no live master while human-warranting items are open"}, now)
	} else {
		s.clear(ctx, "master_absent", now)
	}
}

// pushToWake types the fixed-template ping into an IDLE registered master's pane when a
// master-first-or-higher item is open (plan §15.10) — turning attention latency from the
// master's poll cadence into immediate. Rate-limited per master (T_ping); skipped when the
// master pane is not IDLE_AT_PROMPT (WORKING/compacting panes are left alone). The ping is
// resolved through the family verb table (NotifyMaster), so it can NEVER carry pane content.
func (s *Supervisor) pushToWake(ctx context.Context, now time.Time) {
	open, err := s.store.ListOpenAttention(ctx, "open", nil, "")
	if err != nil || len(open) == 0 {
		return
	}
	// most-urgent actionable open item that pages a master (tier != never-page).
	var top store.AttentionItem
	found := false
	for _, it := range open {
		if attention.TierFor(attention.Kind(it.Kind)) == attention.TierNeverPage {
			continue
		}
		if !found || it.Priority < top.Priority {
			top = it
			found = true
		}
	}
	if !found {
		return
	}
	sups, err := s.store.ListSupervisors(ctx)
	if err != nil {
		return
	}
	for _, sup := range sups {
		if sup.State != "active" || sup.TmuxName == "" {
			continue
		}
		s.mu.Lock()
		last := s.lastPin[sup.ID]
		s.mu.Unlock()
		if !last.IsZero() && now.Sub(last) < s.cfg.PingInterval {
			continue
		}
		st, _, cerr := s.pane.Classify(ctx, sup.Box, sup.TmuxName)
		if cerr != nil || st != string(tmuxio.StateIdleAtPrompt) {
			continue // WORKING/compacting/unreachable: the item waits in the queue it will poll
		}
		table, verr := verbs.For(sup.ModelFamily)
		if verr != nil {
			continue
		}
		send, verr := table.NotifyMaster(len(open), top.Kind)
		if verr != nil {
			continue // an unknown top kind is never templated into a master pane
		}
		if _, derr := s.pane.Deliver(ctx, sup.Box, sup.TmuxName, send.Text); derr != nil {
			s.logger.Warn("epic supervision: push-to-wake", "master", sup.ID, "err", derr)
			continue
		}
		s.mu.Lock()
		s.lastPin[sup.ID] = now
		s.mu.Unlock()
	}
}

// ── item construction helpers ──

func (s *Supervisor) upsert(ctx context.Context, e store.EpicRun, kind attention.Kind, prio int, dedup string, blocking bool, evidence map[string]string, now time.Time) {
	s.upsertRaw(ctx, kind, prio, dedup, e.ID, e.Repo, blocking, evidence, now)
}

func (s *Supervisor) upsertRaw(ctx context.Context, kind attention.Kind, prio int, dedup, epicID, repo string, blocking bool, evidence map[string]string, now time.Time) {
	if _, _, err := s.store.UpsertAttentionItem(ctx, store.AttentionItem{
		ID: ulid.New(), Kind: string(kind), EpicID: epicID, Repo: repo, Priority: prio,
		DedupKey: dedup, Blocking: blocking, Evidence: evidence,
	}, now); err != nil {
		s.logger.Warn("epic supervision: upsert attention", "kind", kind, "dedup", dedup, "err", err)
	}
}

func (s *Supervisor) clear(ctx context.Context, dedup string, now time.Time) {
	if _, err := s.store.AutoResolveCleared(ctx, dedup, now); err != nil {
		s.logger.Warn("epic supervision: auto-resolve cleared", "dedup", dedup, "err", err)
	}
}

// ── production Pane over tmuxio ──

// TmuxPane is the production Pane: it builds a per-host tmuxio.Client (local when host is
// empty — the launch ladder's local attach session, plan §15.15) and drives the merged
// classify + delivery-verified send primitives.
type TmuxPane struct{}

func (TmuxPane) client(host string) *tmuxio.Client {
	if host == "" {
		return tmuxio.New()
	}
	return tmuxio.New(tmuxio.WithHost(host))
}

func (p TmuxPane) Classify(ctx context.Context, host, session string) (string, string, error) {
	capt, err := p.client(host).Capture(ctx, session, 0)
	if err != nil {
		return "", "", err
	}
	st, _ := tmuxio.Classify(capt.Raw)
	return string(st), lastNonEmptyLine(capt.Raw), nil
}

func (p TmuxPane) Deliver(ctx context.Context, host, session, message string) (string, error) {
	res, err := p.client(host).Send(ctx, session, message, tmuxio.SendOptions{})
	if err != nil && res.Verification == "" {
		return "", err
	}
	return string(res.Verification), nil
}

func (p TmuxPane) Stop(ctx context.Context, host, session string) error {
	client := p.client(host)
	exists, err := client.HasSession(ctx, session)
	if err != nil {
		return err
	}
	if exists {
		if err := client.KillSession(ctx, session); err != nil {
			return err
		}
	}
	exists, err = client.HasSession(ctx, session)
	if err != nil {
		return err
	}
	if exists {
		return fmt.Errorf("tmux session %q still exists after kill", session)
	}
	return nil
}

// ── tiny string helpers ──

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

func ftoa(f float64) string { return strconv.FormatFloat(f, 'f', 1, 64) }

func lastNonEmptyLine(raw string) string {
	lines := strings.Split(raw, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if t := strings.TrimSpace(lines[i]); t != "" {
			return t
		}
	}
	return ""
}
