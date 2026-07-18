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
	// CheckoutPath is a fully RESOLVED literal path (e.g. "/home/ops/dev/russ"),
	// not a "$HOME/..." template — the caller resolves the box's home directory via
	// HomeDirCmd first (see its doc for why: a "$HOME" left in the string would get
	// shQuote'd into inertness here, since every command below embeds CheckoutPath
	// as a quoted argument). The convention is <home>/dev/<repo> (the per-repo base
	// checkout shared by every isolated epic worktree on that host; each epic runner cuts
	// epic/<slug> from main in its own worktree per epics/INSTRUCTIONS.md).
	CheckoutPath string
	// DiskProbePath is the path `df` measures free space AT — it must be a path
	// that ALREADY EXISTS on the box (the caller passes the resolved home
	// directory). It is deliberately NOT CheckoutPath (review MAJOR M1): the disk
	// check runs BEFORE the clone step below creates the checkout, and df against
	// a nonexistent path emits nothing — parsed as 0 free KB, so every FIRST
	// launch onto a fresh box was refused with a misleading "0.0G free" (and
	// worse, self-healed on retry because the refused pass had already cloned).
	// Home and the checkout live on the same filesystem under the ~/dev/<repo>
	// convention, so measuring at home answers the same question.
	DiskProbePath string
	OwnerRepo     string // "owner/repo", used only if a fresh clone is needed
}

type PreflightResult struct {
	GhAuthOK    bool
	DiskFreeKB  int64
	ClonedFresh bool // true if CheckoutPath did not exist and Preflight cloned it
}

// Preflight runs `gh auth status`, checks free disk space (at DiskProbePath —
// see its doc), and ensures the repo checkout exists at CheckoutPath (cloning
// fresh via `gh repo clone` if not) — exactly the three checks the design doc
// names, in that order, so an unauthenticated gh or a nearly-full disk is
// discovered BEFORE spending time on a clone that would then have nowhere useful
// to land its output.
func Preflight(ctx context.Context, r Runner, p PreflightParams) (PreflightResult, error) {
	var out PreflightResult

	authOut, authErr := r.Run(ctx, GhAuthStatusCmd(p.Box))
	out.GhAuthOK = authErr == nil
	_ = authOut // diagnostic only; the caller's error message may want it — see cmd/flowbee/epic.go

	probe := p.DiskProbePath
	if probe == "" {
		probe = p.CheckoutPath // legacy/caller-error fallback; may not exist yet (see M1 note above)
	}
	diskOut, err := r.Run(ctx, DiskFreeKBCmd(p.Box, probe))
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

// EpicBasePath is the SHARED per-repo base checkout on a seat's box — the documented
// <home>/dev/<repo> convention (the one Preflight clones/refreshes). Every epic on the box,
// of any slug, shares this single checkout's .git object store; the launch gate's disk math
// and the clone all target it, so it is derived in ONE place here to keep epicPreflight and
// the abandon-cleanup path from inventing a second notion of it.
func EpicBasePath(home, repo string) string {
	return home + "/dev/" + repo
}

// EpicWorktreePath is one epic's PRIVATE working tree, kept OUTSIDE the base checkout (a
// sibling under <home>/dev/.flowbee-wt/<repo>/<slug>) so `git worktree add` never nests a
// worktree inside its own base and two epics on one box get fully isolated trees + branches
// while sharing only the base's .git objects. It is derived per SLUG, so two epics on the
// same box (same repo) resolve to DISTINCT paths and can never collide. Callers pass this —
// never the base — to the launch ladder as the epic's checkout/cwd.
func EpicWorktreePath(home, repo, slug string) string {
	return home + "/dev/.flowbee-wt/" + repo + "/" + slug
}

// ProvisionEpicWorktree creates the epic's private worktree (WorktreeAddCmd) on the box via
// the SAME Runner abstraction as everything else here. A non-nil error is launch-BLOCKING
// by contract — the caller refuses the launch and rolls back; there is DELIBERATELY no
// fallback to the shared base tree, because letting two epics share one working tree is
// precisely the corruption the worktree isolates against.
func ProvisionEpicWorktree(ctx context.Context, r Runner, box, base, worktree, branch string) error {
	if _, err := r.Run(ctx, WorktreeAddCmd(box, base, worktree, branch)); err != nil {
		return fmt.Errorf("create per-epic worktree at %s: %w", worktree, err)
	}
	return nil
}

// RemoveEpicWorktree tears down an epic's private worktree (WorktreeRemoveCmd). It is used
// only for launch-failure rollback, before the launch is declared verified. Explicit
// abandon stops the tmux session but preserves the worktree for recovery.
func RemoveEpicWorktree(ctx context.Context, r Runner, box, base, worktree string) error {
	if _, err := r.Run(ctx, WorktreeRemoveCmd(box, base, worktree)); err != nil {
		return fmt.Errorf("remove per-epic worktree at %s: %w", worktree, err)
	}
	return nil
}

// LaunchParams / LaunchEpicSession start the coding agent and hand it the epic's
// goal, reusing the Phase-1 double-Enter submit-verify mechanics
// (Watcher.autoResume): settle, recapture, and if the input line still shows the
// unsubmitted text verbatim, send one bare Enter. Also settles BEFORE sending the
// goal (Phase 1 has no equivalent — autoResume always targets an ALREADY-RUNNING
// session, whereas this creates a brand-new tmux pane and the TUI needs a moment
// to boot and render before typed keys land in its input line rather than on a
// blank pane).
type LaunchParams struct {
	Box, TmuxName, Dir, StartCmd, Goal string
	// SettleDelay is the pause after tmux-new-session (TUI boot), after sending
	// the goal (pre-verify settle), and between the two verify passes — zero in
	// tests for speed, ~500ms-1s in production.
	SettleDelay time.Duration
}

// LaunchEpicSession creates the tmux session, sends the goal, and verifies
// submission. Returns an error only for a Runner failure on session-creation or
// goal-send (both launch-fatal — the caller has no epic to register if either
// fails); anything unclear at VERIFY time returns verified=false with no error,
// failing toward "no extra keystroke, tell the operator to look" rather than
// treating an unclear pane as fatal.
//
// The verify is TWO bounded passes (review m5), each of which accepts either
// positive signal:
//   - the pane's last line is the EXACT unsubmitted goal text (the swallowed-Enter
//     failure mode) -> send one bare Enter, verified;
//   - ParseStatus reads the pane as pursuing/working (the goal was submitted and
//     the agent is off) -> verified, nothing sent.
//
// The second pass exists because the exact-match check has a KNOWN blind spot the
// single-shot version silently fell into: a ~90-char goal line WRAPS in a narrow
// pane, so a truly swallowed Enter no longer exact-matches the last pane line —
// the old code then declared verified anyway and the epic sat "running" while its
// agent idled at an unsubmitted prompt. One extra settle+capture gives a slow TUI
// time to render a parseable state before we give up. RESIDUAL RISK (documented,
// accepted): a swallowed Enter in a pane narrow enough to wrap the goal line is
// still undetectable by exact match — after both passes it now returns
// verified=FALSE (operator warned to check by hand) instead of a false
// verified=true, which is the safety-preserving direction; detecting the wrapped
// case positively would need width-aware line reassembly, deliberately out of
// scope for this closed, small-blast-radius verifier.
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
	for attempt := 0; attempt < 2; attempt++ {
		if err := sleepCtx(ctx, p.SettleDelay); err != nil {
			return false, err
		}
		pane, cerr := r.Run(ctx, capturePaneCmd(p.Box, p.TmuxName))
		if cerr != nil {
			return false, nil // capture-for-verify failure: not fatal, see doc above
		}
		if paneShowsUnsubmittedText(pane, p.Goal) {
			// the swallowed-Enter failure mode, caught exactly: one bare Enter
			// submits it (Phase 1's proven recovery).
			if _, err := r.Run(ctx, sendEnterCmd(p.Box, p.TmuxName)); err != nil {
				return false, nil
			}
			return true, nil
		}
		if st, _ := ParseStatus(pane); st == StatePursuing || st == StateWorking {
			return true, nil // positive submission evidence: the agent is off
		}
		// neither signal: settle once more and re-verify (attempt 2), then give up.
	}
	return false, nil
}

// sleepCtx sleeps for d, returning ctx.Err() if the context is cancelled first
// (mirrors the select{ctx.Done / time.After} pattern Watcher.autoResume uses
// inline; factored out here since LaunchEpicSession needs it repeatedly).
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
