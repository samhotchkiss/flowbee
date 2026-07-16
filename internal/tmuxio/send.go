package tmuxio

import (
	"context"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
)

// Verification is how confident Send is that the message was actually SUBMITTED
// into the agent (left the input box), not merely typed and left sitting there.
type Verification string

const (
	// Strong: the input box was CONFIRMED cleared by an exact-match check — the
	// sent text is no longer sitting in the prompt. The highest confidence this
	// package offers.
	Strong Verification = "strong"
	// Weak: the pane changed / the message appears to have gone, but the exact-match
	// confirmation was unavailable (a wrapped/multiline message, a menu/dialog on
	// screen, or verification was disabled). Honest uncertainty — treat as
	// "probably submitted, verify if it matters".
	Weak Verification = "weak"
	// Failed: the message is still sitting UNSUBMITTED in the input box after the
	// bounded Enter retries (a menu/dialog is likely swallowing Enter). The caller
	// should inspect and Nudge — NOT re-send (that would duplicate the text).
	Failed Verification = "failed"
)

// SendResult is the typed outcome of Send. Attempts is the number of Enter key
// events sent; Evidence is a human-readable explanation of the verdict (surfaced
// to operators and logs).
type SendResult struct {
	Verification Verification
	Attempts     int
	Evidence     string
}

// SendOptions tunes delivery. The zero value is valid: withDefaults fills sane
// defaults ported from the tmux-send skill (0.4s settle, 4 Enter attempts, 0.5s
// backoff growing by 0.5s).
type SendOptions struct {
	// PreSubmitDelay is the settle time between the paste and the FIRST Enter, so
	// the TUI finishes rendering the pasted text before we submit. Raise toward ~1s
	// for slow/remote panes. Default 400ms.
	PreSubmitDelay time.Duration
	// VerifyBackoff is the initial wait between an Enter and the recapture that
	// checks whether it cleared the box; it grows by 500ms each retry. Default 500ms.
	VerifyBackoff time.Duration
	// MaxAttempts caps how many times Enter is (re-)pressed while the exact-match
	// check still sees the message unsubmitted. Default 4.
	MaxAttempts int
	// BufferName is the tmux paste-buffer base name. Default "flowbee-tmuxio"; a
	// per-call nanosecond suffix keeps concurrent sends from colliding.
	BufferName string
	// NoSubmit pastes the text but never presses Enter (leave it for a human to
	// review/submit). Returns Weak with evidence saying so.
	NoSubmit bool
	// NoVerify presses Enter once and returns immediately without verifying
	// (fire-and-forget). Returns Weak.
	NoVerify bool
}

func (o SendOptions) withDefaults(clock Clock) SendOptions {
	if o.PreSubmitDelay <= 0 {
		o.PreSubmitDelay = 400 * time.Millisecond
	}
	if o.VerifyBackoff <= 0 {
		o.VerifyBackoff = 500 * time.Millisecond
	}
	if o.MaxAttempts <= 0 {
		o.MaxAttempts = 4
	}
	if o.BufferName == "" {
		o.BufferName = "flowbee-tmuxio"
	}
	// Per-call unique suffix from the injected clock (deterministic under the fake).
	o.BufferName = fmt.Sprintf("%s-%d", o.BufferName, clock.Now().UnixNano())
	return o
}

// Send delivers message into the agent at target and VERIFIES submission.
//
// The sequence, ported from the tmux-send skill and hardened with
// internal/watchdog's exact-match lesson:
//  1. Exit copy/scroll mode if the pane is in it (keystrokes never reach the app
//     otherwise) — best-effort.
//  2. Bracketed-paste the text (set-buffer + paste-buffer -p) so an embedded
//     newline can never submit early.
//  3. Settle (PreSubmitDelay), then send Enter as a SEPARATE key event.
//  4. Verify: re-capture and check whether the input box still holds the EXACT
//     text. If it does, the Enter was swallowed — re-press with growing backoff,
//     up to MaxAttempts. The exact-match is the ONLY thing that drives a retry; a
//     fragment/Contains match never does (it could press Enter under a human's
//     edited input) and only ever lowers confidence to Weak.
//
// Blind spots are reported honestly, never as a false Strong: a wrapped long line
// or a multiline message defeats the exact-match (the last visible line is only a
// TAIL of the payload), so those verify by change-detection and cap at Weak; a
// dialog/menu on screen caps at Weak and says so; text still sitting after all
// retries is Failed.
//
// target must be a caller-validated tmux target; it is shQuote'd regardless.
func (c *Client) Send(ctx context.Context, target, message string, opts SendOptions) (SendResult, error) {
	opts = opts.withDefaults(c.clock)
	message = stripTrailingNewlines(message)
	if message == "" {
		return SendResult{}, fmt.Errorf("tmuxio: empty message (use Nudge to press Enter only)")
	}

	pane, err := c.resolvePane(ctx, target)
	if err != nil {
		return SendResult{}, err
	}

	// Snapshot before we touch anything, for change-detection fallback.
	preCap, err := c.Capture(ctx, target, 0)
	if err != nil {
		return SendResult{}, err
	}

	// Exit copy/scroll mode — otherwise the paste and Enter go nowhere.
	if pane.inMode {
		_, _ = c.run(ctx, "send-keys -t "+shQuote(target)+" -X cancel")
	}

	// Bracketed paste: set the buffer to the literal message, paste it (deleting
	// the buffer afterward with -d).
	if _, err := c.run(ctx, "set-buffer -b "+shQuote(opts.BufferName)+" -- "+shQuote(message)); err != nil {
		return SendResult{}, err
	}
	if _, err := c.run(ctx, "paste-buffer -p -d -b "+shQuote(opts.BufferName)+" -t "+shQuote(target)); err != nil {
		return SendResult{}, err
	}

	c.clock.Sleep(ctx, opts.PreSubmitDelay)

	// Look at the pane after the paste: did the text land, and is a dialog up?
	afterPaste, err := c.Capture(ctx, target, 0)
	if err != nil {
		return SendResult{}, err
	}
	wrapRisk := isWrapRisk(message, pane.width)
	hazardBefore := isMenuHazard(afterPaste.Raw)
	// Did the pasted text visibly land in the input box? If we can CONFIRM it did
	// (exact single-line match, or a fragment for anything else), a later "box
	// cleared" reading is trustworthy Strong evidence. If we never saw it land — the
	// paste may have been consumed by a menu/dialog (the brief's prompt-box-absence
	// hazard) — a "cleared" box proves nothing, so Strong is downgraded to Weak.
	landed := inputLineHoldsExactly(afterPaste.Raw, message) || fragmentPresent(afterPaste.Raw, message)

	if opts.NoSubmit {
		return SendResult{Weak, 0, "text pasted, submission skipped (NoSubmit)"}, nil
	}

	if opts.NoVerify {
		if err := c.sendEnter(ctx, target); err != nil {
			return SendResult{}, err
		}
		return SendResult{Weak, 1, "Enter sent, verification disabled (NoVerify)"}, nil
	}

	if wrapRisk {
		return c.verifyWrapped(ctx, target, message, preCap.Normalized, hazardBefore)
	}
	return c.verifyExact(ctx, target, message, opts, hazardBefore, landed)
}

// verifyExact runs the exact-match retry loop for a single-line message that fits
// on one input line. It is the only path that re-presses Enter, and only while the
// input box still holds the EXACT text. landed reports whether the paste was
// confirmed to have reached the input box before the first Enter; when it was not,
// a cleared box is only Weak evidence (the paste may have gone into a menu).
func (c *Client) verifyExact(ctx context.Context, target, message string, opts SendOptions, hazardBefore, landed bool) (SendResult, error) {
	backoff := opts.VerifyBackoff
	for attempt := 1; attempt <= opts.MaxAttempts; attempt++ {
		if err := c.sendEnter(ctx, target); err != nil {
			return SendResult{}, err
		}
		c.clock.Sleep(ctx, backoff)
		after, err := c.Capture(ctx, target, 0)
		if err != nil {
			return SendResult{}, err
		}
		if inputLineHoldsExactly(after.Raw, message) {
			// Still sitting unsubmitted — the Enter was swallowed. Re-press.
			backoff += 500 * time.Millisecond
			continue
		}
		// The input line no longer holds the exact text: the box cleared.
		if hazardBefore || isMenuHazard(after.Raw) {
			return SendResult{Weak, attempt, fmt.Sprintf(
				"input box cleared after %d Enter press(es), but the pane shows a dialog/menu (AWAITING_INPUT) — the text may have been consumed by the menu rather than submitted to the agent", attempt)}, nil
		}
		if !landed {
			return SendResult{Weak, attempt, fmt.Sprintf(
				"input box is clear after %d Enter press(es), but the pasted text was never confirmed in the box beforehand — submission is likely yet unverified (possible menu/dialog swallowed the paste)", attempt)}, nil
		}
		return SendResult{Strong, attempt, fmt.Sprintf(
			"input box cleared (exact-match): the sent text is no longer in the prompt after %d Enter press(es)", attempt)}, nil
	}
	// Exhausted: the exact text is STILL sitting in the input box.
	return SendResult{Failed, opts.MaxAttempts, fmt.Sprintf(
		"message still sitting unsubmitted in the input line after %d Enter presses — a menu/dialog is likely swallowing Enter; inspect the pane and Nudge, do NOT re-send", opts.MaxAttempts)}, nil
}

// verifyWrapped handles a multiline or wrapped-long message, where the exact-match
// is unavailable (the last visible line is only a TAIL of the payload). It presses
// Enter ONCE — re-pressing is unsafe without exact-match confirmation (it could
// double-submit) — and classifies by change-detection and fragment presence,
// capping confidence at Weak and never returning a false Strong.
func (c *Client) verifyWrapped(ctx context.Context, target, message, preNorm string, hazardBefore bool) (SendResult, error) {
	if err := c.sendEnter(ctx, target); err != nil {
		return SendResult{}, err
	}
	c.clock.Sleep(ctx, 700*time.Millisecond)
	after, err := c.Capture(ctx, target, 0)
	if err != nil {
		return SendResult{}, err
	}
	changed := after.Normalized != preNorm
	stillPresent := fragmentPresent(after.Raw, message)
	hazard := hazardBefore || isMenuHazard(after.Raw)

	switch {
	case stillPresent && !changed:
		return SendResult{Failed, 1,
			"long/multiline message still visible in the input box and the pane did not change after Enter — submission could not be confirmed (wrapped input defeats exact-match); inspect and Nudge, do NOT re-send"}, nil
	case hazard:
		return SendResult{Weak, 1,
			"long/multiline message: a dialog/menu is on screen — exact-match unavailable for wrapped input, submission unconfirmed (menu hazard)"}, nil
	case changed:
		return SendResult{Weak, 1,
			"long/multiline message: the pane changed and no message fragment remains — likely submitted, but exact-match verification is unavailable for wrapped input"}, nil
	default:
		return SendResult{Failed, 1,
			"long/multiline message: no pane change and submission unconfirmed for wrapped input — inspect and Nudge, do NOT re-send"}, nil
	}
}

// Nudge presses Enter once against target — the recovery for text left sitting
// UNSUBMITTED in the input box (the tmux-send `--nudge`). It captures before and
// after and reports Weak on any change, Failed on none. It never types text, so it
// cannot duplicate a message; it is safe to call after a Failed Send.
func (c *Client) Nudge(ctx context.Context, target string) (SendResult, error) {
	before, err := c.Capture(ctx, target, 0)
	if err != nil {
		return SendResult{}, err
	}
	if err := c.sendEnter(ctx, target); err != nil {
		return SendResult{}, err
	}
	c.clock.Sleep(ctx, 500*time.Millisecond)
	after, err := c.Capture(ctx, target, 0)
	if err != nil {
		return SendResult{}, err
	}
	if after.Normalized != before.Normalized {
		return SendResult{Weak, 1, "pane changed after the nudge Enter — the stuck text was likely submitted"}, nil
	}
	return SendResult{Failed, 1, "no pane change after the nudge Enter — nothing was submitted (the pane may not have had unsubmitted text, or Enter is still being swallowed)"}, nil
}

// sendEnter sends a single bare Enter key event to target.
func (c *Client) sendEnter(ctx context.Context, target string) error {
	_, err := c.run(ctx, "send-keys -t "+shQuote(target)+" Enter")
	return err
}

// resolvedPane holds the pane facts Send needs, fetched in one display-message.
type resolvedPane struct {
	id     string
	width  int
	inMode bool
}

// resolvePane fetches the target's pane id, width, and copy-mode flag in one call.
// It also validates that the target exists (a missing target surfaces as an error
// here, before any keystroke is sent).
func (c *Client) resolvePane(ctx context.Context, target string) (resolvedPane, error) {
	format := strings.Join([]string{"#{pane_id}", "#{pane_width}", "#{pane_in_mode}"}, fieldSep)
	out, err := c.run(ctx, "display-message -p -t "+shQuote(target)+" "+shQuote(format))
	if err != nil {
		return resolvedPane{}, err
	}
	f := strings.Split(strings.TrimSpace(out), fieldSep)
	if len(f) != 3 {
		return resolvedPane{}, fmt.Errorf("tmuxio: unexpected display-message output %q for target %q", out, target)
	}
	return resolvedPane{id: f[0], width: atoiOr(f[1], 0), inMode: strings.TrimSpace(f[2]) == "1"}, nil
}

// ── verification helpers ──

// inputLineHoldsExactly reports whether the pane's last non-empty line, with the
// prompt glyph stripped, EXACTLY equals message — i.e. the message is sitting
// unsubmitted in the input box. Ported from internal/watchdog.paneShowsUnsubmittedText
// (review MAJOR #2b): exact match, not Contains, so it never fires on codex's own
// hint text or under a human's edited input. Only defined for a single-line
// message (a multiline payload can never occupy a single input line verbatim).
func inputLineHoldsExactly(capture, message string) bool {
	if strings.Contains(message, "\n") {
		return false
	}
	return stripPromptGlyph(lastNonEmptyLine(capture)) == message
}

// isMenuHazard reports whether the captured pane is showing a dialog/menu that
// would capture keystrokes (StateAwaitingInput) — the classic delivery hazard.
func isMenuHazard(capture string) bool {
	st, _ := Classify(capture)
	return st == StateAwaitingInput
}

// isWrapRisk reports whether message is at risk of wrapping across multiple input
// lines (multiline, or wider than the input box), which defeats the single-line
// exact-match. Conservative: the input box is a few columns narrower than the pane
// (prompt glyph + padding), so any message within 4 columns of the pane width is
// treated as a wrap risk. A zero/unknown width falls back to 76.
func isWrapRisk(message string, paneWidth int) bool {
	if strings.Contains(message, "\n") {
		return true
	}
	limit := paneWidth - 4
	if paneWidth <= 0 {
		limit = 76
	}
	return utf8.RuneCountInString(message) > limit
}

// fragmentPresent reports whether a recognizable fragment of message is still
// visible in the pane's input region — the tmux-send fragment heuristic, kept ONLY
// as a Weak-confidence signal for wrapped messages (it never drives an Enter
// retry). Fragments: the head of the first line, the tail of the last line, and
// the "[Pasted text …]" placeholder tmux/agents collapse big pastes into.
func fragmentPresent(capture, message string) bool {
	region := inputRegion(capture)
	for _, f := range fragmentsOf(message) {
		if f != "" && strings.Contains(region, f) {
			return true
		}
	}
	return false
}

// fragmentsOf builds the fragment candidates for message, mirroring the tmux-send
// skill's build_fragments.
func fragmentsOf(message string) []string {
	var frags []string
	lines := strings.Split(message, "\n")
	first := lines[0]
	last := ""
	for i := len(lines) - 1; i >= 0; i-- {
		if strings.TrimSpace(lines[i]) != "" {
			last = lines[i]
			break
		}
	}
	if utf8.RuneCountInString(first) >= 6 {
		frags = append(frags, headRunes(first, 24))
	}
	if last != first && utf8.RuneCountInString(last) >= 6 {
		frags = append(frags, headRunes(last, 24))
	}
	if utf8.RuneCountInString(last) > 30 {
		frags = append(frags, tailRunes(last, 20))
	}
	frags = append(frags, "asted text") // "[Pasted text #1 +8 lines]" placeholder
	return frags
}

// inputRegion returns the pane's input area: from the last line that looks like a
// prompt (glyph, optionally behind a box border) to the end. Falls back to the
// last few non-empty lines when no prompt line is found. Mirrors the tmux-send
// awk input_region.
func inputRegion(capture string) string {
	lines := strings.Split(capture, "\n")
	promptRe := classifyPromptLineRe
	start := -1
	for i, ln := range lines {
		if promptRe.MatchString(ln) {
			start = i
		}
	}
	if start >= 0 {
		return strings.Join(lines[start:], "\n")
	}
	// Fallback: last 4 non-empty-ish lines.
	from := len(lines) - 4
	if from < 0 {
		from = 0
	}
	return strings.Join(lines[from:], "\n")
}

func headRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

func tailRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[len(r)-n:])
}

// stripTrailingNewlines removes trailing newline characters — Enter is sent as a
// separate key event on purpose, so a trailing newline in the payload must not
// ride along in the paste.
func stripTrailingNewlines(s string) string {
	return strings.TrimRight(s, "\n")
}
