package watchdog

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// PreflightParams / PreflightResult / Preflight run the epic-lane launch's
// on-host checks (§ task brief point 3 "Preflight on the host") via the SAME
// Runner abstraction the goal-session watchdog itself uses — local (box="") or
// ssh (box!="") is entirely remoteWrap's concern, never this function's. Preflight
// never fails on a GATE condition (auth missing, disk low) — it reports what it
// found and lets the caller (cmd/flowbee's runEpicStart) decide whether to refuse
// the launch; it only returns an error when the Runner itself couldn't be used
// (an ssh/tmux-adjacent plumbing failure), since that's a launch-blocking problem
// no gate policy can meaningfully evaluate around.
type PreflightParams struct {
	Box string
	// CheckoutPath is a fully RESOLVED literal path (e.g. "/home/ops/epics/russ"),
	// not a "$HOME/..." template — the caller resolves the box's home directory via
	// HomeDirCmd first (see its doc for why: a "$HOME" left in the string would get
	// shQuote'd into inertness here, since every command below embeds CheckoutPath
	// as a quoted argument).
	CheckoutPath string
	OwnerRepo    string // "owner/repo", used only if a fresh clone is needed
}

type PreflightResult struct {
	GhAuthOK    bool
	DiskFreeKB  int64
	ClonedFresh bool // true if CheckoutPath did not exist and Preflight cloned it
}

// Preflight runs `gh auth status`, checks free disk space, and ensures the repo
// checkout exists at CheckoutPath (cloning fresh via `gh repo clone` if not) —
// exactly the three checks the design doc names, in that order, so an unauthenticated
// gh or a nearly-full disk is discovered BEFORE spending time on a clone that would
// then have nowhere useful to land its output.
func Preflight(ctx context.Context, r Runner, p PreflightParams) (PreflightResult, error) {
	var out PreflightResult

	authOut, authErr := r.Run(ctx, GhAuthStatusCmd(p.Box))
	out.GhAuthOK = authErr == nil
	_ = authOut // diagnostic only; the caller's error message may want it — see cmd/flowbee/epic.go

	diskOut, err := r.Run(ctx, DiskFreeKBCmd(p.Box, p.CheckoutPath))
	if err != nil {
		return out, fmt.Errorf("check disk space: %w", err)
	}
	if kb, perr := strconv.ParseInt(strings.TrimSpace(diskOut), 10, 64); perr == nil {
		out.DiskFreeKB = kb
	}
	// a df failure/garbage output (kb stays 0) is NOT swallowed silently — 0 free
	// KB reads as "definitely under any sane threshold", so the caller's ≥10G gate
	// naturally refuses rather than launching blind into an unmeasured disk.

	existsOut, err := r.Run(ctx, RepoCheckoutExistsCmd(p.Box, p.CheckoutPath))
	if err != nil {
		return out, fmt.Errorf("check checkout presence: %w", err)
	}
	if strings.TrimSpace(existsOut) != "yes" {
		if _, err := r.Run(ctx, CloneRepoCmd(p.Box, p.OwnerRepo, p.CheckoutPath)); err != nil {
			return out, fmt.Errorf("clone checkout: %w", err)
		}
		out.ClonedFresh = true
	}
	return out, nil
}

// LaunchParams / LaunchEpicSession start the coding agent and hand it the epic's
// goal, reusing EXACTLY the Phase-1 double-Enter submit-verify mechanics
// (Watcher.autoResume): settle, recapture, and if the input line still shows the
// unsubmitted text verbatim, send one bare Enter. Also settles BEFORE sending the
// goal (Phase 1 has no equivalent — autoResume always targets an ALREADY-RUNNING
// session, whereas this creates a brand-new tmux pane and the TUI needs a moment
// to boot and render before typed keys land in its input line rather than on a
// blank pane).
type LaunchParams struct {
	Box, TmuxName, Dir, StartCmd, Goal string
	// SettleDelay is the pause after tmux-new-session (TUI boot) and again after
	// sending the goal (pre-verify settle) — zero in tests for speed, ~500ms-1s in
	// production (New()'s default, reused here rather than inventing a second knob).
	SettleDelay time.Duration
}

// LaunchEpicSession creates the tmux session, sends the goal, and verifies
// submission. Returns an error only for a Runner failure on session-creation or
// goal-send (both launch-fatal — the caller has no epic to register if either
// fails); a failed submit-VERIFY capture is logged by returning verified=false
// with no error, matching Phase 1's posture of failing toward "no extra keystroke,
// re-evaluate later" rather than treating an unclear pane as fatal.
func LaunchEpicSession(ctx context.Context, r Runner, p LaunchParams) (verified bool, err error) {
	if _, err := r.Run(ctx, NewTmuxSessionCmd(p.Box, p.TmuxName, p.Dir, p.StartCmd)); err != nil {
		return false, fmt.Errorf("create tmux session: %w", err)
	}
	if err := sleepCtx(ctx, p.SettleDelay); err != nil {
		return false, err
	}
	if _, err := r.Run(ctx, SendGoalCmd(p.Box, p.TmuxName, p.Goal)); err != nil {
		return false, fmt.Errorf("send goal: %w", err)
	}
	if err := sleepCtx(ctx, p.SettleDelay); err != nil {
		return false, err
	}
	pane, err := r.Run(ctx, capturePaneCmd(p.Box, p.TmuxName))
	if err != nil {
		return false, nil // capture-for-verify failure: not fatal, see doc above
	}
	if paneShowsUnsubmittedText(pane, p.Goal) {
		if _, err := r.Run(ctx, sendEnterCmd(p.Box, p.TmuxName)); err != nil {
			return false, nil // same non-fatal posture
		}
	}
	return true, nil
}

// sleepCtx sleeps for d, returning ctx.Err() if the context is cancelled first
// (mirrors the select{ctx.Done / time.After} pattern Watcher.autoResume uses
// inline; factored out here since LaunchEpicSession needs it twice).
func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(d):
		return nil
	}
}
