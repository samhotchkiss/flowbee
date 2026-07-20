package tmuxio

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"sync/atomic"
	"time"
	"unicode"
)

// Verification is how confident Send is that the message was actually SUBMITTED
// into the agent (left the input box), not merely typed and left sitting there.
type Verification string

const (
	// Strong: the input-prompt line was LOCATED and is now EMPTY — the sent text
	// left the box. The highest confidence this package offers, and (per review M1)
	// it requires POSITIVE corroboration: a located, empty prompt. Absence of the
	// message alone is never Strong.
	Strong Verification = "strong"
	// Weak: honest uncertainty — the box appears to have changed but submission could
	// not be positively corroborated (the prompt line could not be located after
	// Enter, a wrapped/multiline message defeats the single-line match, a menu/dialog
	// is on screen, or verification was disabled). Treat as "probably submitted,
	// verify if it matters".
	Weak Verification = "weak"
	// Failed: the message is still sitting UNSUBMITTED in the located input box after
	// the bounded Enter retries (a menu/dialog is likely swallowing Enter). The caller
	// should inspect and Nudge — NOT re-Send (that would duplicate the text).
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

// maxMessageBytes caps a single Send payload. The message is handed to tmux as an
// argv element (set-buffer/send-keys), so it is bounded by ARG_MAX — and MORE
// tightly over ssh, where the whole command is shQuote-nested twice. 64 KiB is far
// under any real ARG_MAX (typically 256 KiB–2 MiB) even doubled, while still
// comfortably fitting any agent instruction (review m7). A larger payload should be
// written to a file the agent reads, not pasted.
const maxMessageBytes = 64 * 1024

// keysDeliveryMaxWidth is the display-width ceiling for delivering a single-line
// message as literal keystrokes rather than a bracketed paste (review m6, mirroring
// the tmux-send reference's 400-char cutoff).
const keysDeliveryMaxWidth = 400

// SendOptions tunes delivery. The zero value is valid: withDefaults fills sane
// defaults ported from the tmux-send skill (0.4s settle, 4 Enter attempts, 0.5s
// backoff growing by 0.5s).
type SendOptions struct {
	// PreSubmitDelay is the settle time between delivering the text and the FIRST
	// Enter, so the TUI finishes rendering before we submit. Raise toward ~1s for
	// slow/remote panes. Default 400ms.
	PreSubmitDelay time.Duration
	// VerifyBackoff is the initial wait between an Enter and the recapture that
	// checks whether it cleared the box; it grows by 500ms each retry. Default 500ms.
	VerifyBackoff time.Duration
	// MaxAttempts caps how many times Enter is (re-)pressed while the exact-match
	// check still sees the message sitting in the located input box. Default 4.
	MaxAttempts int
	// BufferName is the tmux paste-buffer base name (paste path only). Default
	// "flowbee-tmuxio"; a per-call nanosecond+counter suffix keeps concurrent sends
	// from colliding on the buffer.
	BufferName string
	// NoSubmit delivers the text but never presses Enter (leave it for a human to
	// review/submit). Returns Weak with evidence saying so.
	NoSubmit bool
	// NoVerify presses Enter once and returns immediately without verifying
	// (fire-and-forget). Returns Weak.
	NoVerify bool
}

// bufferSeq makes each paste-buffer name process-unique even when two Sends land in
// the same clock tick (or under a fake clock that never advances) — review m14.
var bufferSeq atomic.Uint64

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
	o.BufferName = fmt.Sprintf("%s-%d-%d", o.BufferName, clock.Now().UnixNano(), bufferSeq.Add(1))
	return o
}

// pastedUnsubmittedEvidence is returned WITH the error when a step fails AFTER the
// text was delivered but before submission was verified — so a caller can tell
// "nothing was sent" (bare error, empty result) from "text may be sitting
// unsubmitted" and does NOT naively re-Send (which would double the text). Review m11.
const pastedUnsubmittedEvidence = "an error occurred after the text was delivered but before submission was confirmed — it may be sitting UNSUBMITTED in the input box; inspect and Nudge, do NOT re-Send"

func deliveredErr(attempts int, err error) (SendResult, error) {
	return SendResult{Failed, attempts, pastedUnsubmittedEvidence}, err
}

// Send delivers message into the agent at target and VERIFIES submission.
//
// The sequence, ported from the tmux-send skill and hardened with
// internal/watchdog's exact-match lesson and review M1's polarity fix:
//  1. Exit copy/scroll mode if the pane is in it (keystrokes never reach the app
//     otherwise) — best-effort.
//  2. Deliver the text: literal keystrokes for a short single line (preserves
//     codex slash-command autocomplete, avoids a [Pasted text] placeholder), a
//     bracketed paste for long/multiline (so an embedded newline never submits
//     early). Review m6.
//  3. Settle (PreSubmitDelay), then send Enter as a SEPARATE key event.
//  4. Verify by LOCATING the input-prompt line (Claude Code's bordered `│ > │`
//     box is NOT the last line — its last line is a "? for shortcuts" hint) and
//     reading the text sitting on it: if it still EXACTLY holds the message, the
//     Enter was swallowed — re-press with growing backoff up to MaxAttempts. Only
//     a located, EMPTY prompt counts as Strong; a prompt we cannot locate is Weak,
//     never Strong. The exact-match is the ONLY thing that drives a retry.
//
// Blind spots are reported honestly, never as a false Strong: a wrapped long line
// or a multiline message defeats the single-line match, so those verify by
// change-detection and cap at Weak; a dialog/menu caps at Weak; text still sitting
// after all retries is Failed.
//
// target must be a caller-validated tmux target; it is shQuote'd regardless.
// message must be non-empty (after trailing-newline stripping), NUL-free, and at
// most maxMessageBytes.
func (c *Client) Send(ctx context.Context, target, message string, opts SendOptions) (SendResult, error) {
	if err := c.validateIdent("target", target); err != nil {
		return SendResult{}, err
	}
	if err := validateMessage(message); err != nil {
		return SendResult{}, err
	}
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

	// Exit copy/scroll mode — otherwise the delivery and Enter go nowhere.
	if pane.inMode {
		_, _ = c.run(ctx, "send-keys -t "+shQuote(exactTarget(target))+" -X cancel")
	}

	// Deliver the text (keys or paste). A failure HERE means nothing was submitted
	// and, for the keys path, likely nothing landed — a bare error is honest.
	if err := c.deliverText(ctx, target, message, opts.BufferName); err != nil {
		return SendResult{}, err
	}

	c.clock.Sleep(ctx, opts.PreSubmitDelay)

	// From here the text has been delivered into the box; any error is "delivered,
	// unsubmitted" (review m11).
	afterPaste, err := c.Capture(ctx, target, 0)
	if err != nil {
		return deliveredErr(0, err)
	}
	wrapRisk := isWrapRisk(message, pane.width)
	hazardBefore := isMenuHazard(afterPaste.Raw)
	// Did the text visibly land in the input box? Confirmed by the LOCATED input
	// line holding the message exactly, or by one of the message's OWN text
	// fragments in the input region (review m13: never credited from a bare
	// "[Pasted text]" placeholder that could belong to an old paste). When we never
	// saw it land, a later "box cleared" reading is only Weak evidence.
	obsPaste := observeInput(afterPaste.Raw)
	landed := (obsPaste.located && obsPaste.interior == message) || messageFragmentPresent(afterPaste.Raw, message)

	if opts.NoSubmit {
		return SendResult{Weak, 0, "text delivered, submission skipped (NoSubmit)"}, nil
	}

	if opts.NoVerify {
		if err := c.sendEnter(ctx, target); err != nil {
			return deliveredErr(0, err)
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
// LOCATED input box still holds the EXACT text (the positive trigger — matching the
// watchdog's safe polarity). landed reports whether the delivery was confirmed to
// have reached the box before the first Enter; when it was not, a cleared box is
// only Weak evidence.
func (c *Client) verifyExact(ctx context.Context, target, message string, opts SendOptions, hazardBefore, landed bool) (SendResult, error) {
	backoff := opts.VerifyBackoff
	for attempt := 1; attempt <= opts.MaxAttempts; attempt++ {
		if err := c.sendEnter(ctx, target); err != nil {
			return deliveredErr(attempt-1, err)
		}
		c.clock.Sleep(ctx, backoff)
		after, err := c.Capture(ctx, target, 0)
		if err != nil {
			return deliveredErr(attempt, err)
		}
		obs := observeInput(after.Raw)
		if obs.located && obs.interior == message {
			// Still sitting in the box unsubmitted — the Enter was swallowed. Re-press.
			// (This is the ONLY positive retry trigger, matching the watchdog's safe
			// polarity: presence of the exact text drives the recovery Enter.)
			backoff += 500 * time.Millisecond
			continue
		}
		hazard := hazardBefore || isMenuHazard(after.Raw)
		switch {
		case hazard:
			return SendResult{Weak, attempt, fmt.Sprintf(
				"after %d Enter press(es) the pane shows a dialog/menu (AWAITING_INPUT) — the text may have been consumed by the menu rather than submitted to the agent", attempt)}, nil
		case obs.located && obs.interior == "":
			// Positive corroboration: the prompt line is located AND empty.
			if !landed {
				return SendResult{Weak, attempt, fmt.Sprintf(
					"input box is empty after %d Enter press(es), but the delivered text was never confirmed in the box beforehand — submission is likely yet unverified (a menu/dialog may have swallowed the delivery)", attempt)}, nil
			}
			return SendResult{Strong, attempt, fmt.Sprintf(
				"input box located and empty after %d Enter press(es): the sent text left the prompt", attempt)}, nil
		case obs.located:
			// Located but holding some OTHER non-empty text — our message is gone from
			// the box, but we cannot positively confirm WE submitted it.
			return SendResult{Weak, attempt, fmt.Sprintf(
				"input box no longer holds the sent text after %d Enter press(es) but shows different content — submission likely, unconfirmed", attempt)}, nil
		default:
			// Could not locate the input line at all (a full redraw / spinner replaced
			// the box). Per review M1 this is WEAK, never Strong.
			return SendResult{Weak, attempt, fmt.Sprintf(
				"the input prompt was not locatable after %d Enter press(es) (the pane redrew, e.g. started working) — submission likely, unconfirmed", attempt)}, nil
		}
	}
	// Exhausted: the located input box STILL holds the exact text.
	return SendResult{Failed, opts.MaxAttempts, fmt.Sprintf(
		"message still sitting unsubmitted in the located input box after %d Enter presses — a menu/dialog is likely swallowing Enter; inspect the pane and Nudge, do NOT re-Send", opts.MaxAttempts)}, nil
}

// verifyWrapped handles a multiline or wrapped-long message, where the single-line
// exact-match is unavailable (the text spans multiple input lines / collapses to a
// placeholder). It presses Enter ONCE — re-pressing is unsafe without exact-match
// confirmation (it could double-submit) — and classifies by change-detection and
// fragment/placeholder presence, capping confidence at Weak and never returning a
// false Strong.
func (c *Client) verifyWrapped(ctx context.Context, target, message, preNorm string, hazardBefore bool) (SendResult, error) {
	if err := c.sendEnter(ctx, target); err != nil {
		return deliveredErr(0, err)
	}
	c.clock.Sleep(ctx, 700*time.Millisecond)
	after, err := c.Capture(ctx, target, 0)
	if err != nil {
		return deliveredErr(1, err)
	}
	changed := after.Normalized != preNorm
	// Still-present = the message's own fragments OR a paste placeholder sitting in
	// the input region (anchored to the input region so an old placeholder in
	// scrollback does not count — review m13).
	stillPresent := messageFragmentPresent(after.Raw, message) || placeholderInInputRegion(after.Raw)
	hazard := hazardBefore || isMenuHazard(after.Raw)

	switch {
	case stillPresent && !changed:
		return SendResult{Failed, 1,
			"long/multiline message still visible in the input box and the pane did not change after Enter — submission could not be confirmed (wrapped input defeats exact-match); inspect and Nudge, do NOT re-Send"}, nil
	case hazard:
		return SendResult{Weak, 1,
			"long/multiline message: a dialog/menu is on screen — exact-match unavailable for wrapped input, submission unconfirmed (menu hazard)"}, nil
	case changed:
		return SendResult{Weak, 1,
			"long/multiline message: the pane changed and no message fragment remains — likely submitted, but exact-match verification is unavailable for wrapped input"}, nil
	default:
		return SendResult{Failed, 1,
			"long/multiline message: no pane change and submission unconfirmed for wrapped input — inspect and Nudge, do NOT re-Send"}, nil
	}
}

// Nudge presses Enter once against target — the recovery for text left sitting
// UNSUBMITTED in the input box (the tmux-send `--nudge`). It captures before and
// after and reports Weak on any change, Failed on none. It never types text, so it
// cannot duplicate a message; it is safe to call after a Failed Send.
func (c *Client) Nudge(ctx context.Context, target string) (SendResult, error) {
	if err := c.validateIdent("target", target); err != nil {
		return SendResult{}, err
	}
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

// deliverText puts message into the input box: literal keystrokes for a short
// single line, a bracketed paste for long/multiline (review m6). The keys path
// mirrors the tmux-send reference — it preserves codex slash-command autocomplete
// and never renders as a [Pasted text] placeholder. `-l --` sends the text
// literally and guards a leading-dash payload.
func (c *Client) deliverText(ctx context.Context, target, message, bufferName string) error {
	if !strings.Contains(message, "\n") && displayWidth(message) <= keysDeliveryMaxWidth {
		_, err := c.run(ctx, "send-keys -t "+shQuote(exactTarget(target))+" -l -- "+shQuote(message))
		return err
	}
	if _, err := c.run(ctx, "set-buffer -b "+shQuote(bufferName)+" -- "+shQuote(message)); err != nil {
		return err
	}
	_, err := c.run(ctx, "paste-buffer -p -d -b "+shQuote(bufferName)+" -t "+shQuote(exactTarget(target)))
	return err
}

// sendEnter sends a single bare Enter key event to target.
func (c *Client) sendEnter(ctx context.Context, target string) error {
	_, err := c.run(ctx, "send-keys -t "+shQuote(exactTarget(target))+" Enter")
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
	out, err := c.run(ctx, "display-message -p -t "+shQuote(exactTarget(target))+" "+shQuote(format))
	if err != nil {
		return resolvedPane{}, err
	}
	f := splitTmuxFields(strings.TrimSpace(out))
	if len(f) != 3 {
		return resolvedPane{}, fmt.Errorf("tmuxio: unexpected display-message output %q for target %q", out, target)
	}
	return resolvedPane{id: f[0], width: atoiOr(f[1], 0), inMode: strings.TrimSpace(f[2]) == "1"}, nil
}

// ── verification helpers ──

// inputObservation is what the located input line holds. located=false means no
// prompt line could be found at all.
type inputObservation struct {
	located  bool
	interior string // trimmed input text on the located prompt line ("" = empty box)
}

// observeInput locates the input-prompt line (box-aware — see extractInputLine) and
// returns the text sitting on it. This is the primitive the verifier keys on: the
// message being present drives a retry; the box being located AND empty is the only
// Strong signal.
func observeInput(capture string) inputObservation {
	text, ok := extractInputLine(capture)
	return inputObservation{located: ok, interior: text}
}

// isMenuHazard reports whether the captured pane is showing a dialog/menu that
// would capture keystrokes (StateAwaitingInput) — the classic delivery hazard.
// A codex "Goal blocked/paused" status hint is NOT a hazard (it is StateGoalBlocked)
// — review m8.
func isMenuHazard(capture string) bool {
	st, _ := Classify(capture)
	return st == StateAwaitingInput
}

// isWrapRisk reports whether message is at risk of wrapping across multiple input
// lines (multiline, or wider than the input box), which defeats the single-line
// exact-match. It uses DISPLAY width (review m4: wide CJK/emoji runes occupy ~2
// columns), and a conservative chrome allowance (review m5: a bordered box spends
// ~6–8 columns on `│ > ` + ` │`), so a borderline message is treated as a wrap
// risk (the safe direction — it routes to the never-false-Strong wrapped path).
func isWrapRisk(message string, paneWidth int) bool {
	if strings.Contains(message, "\n") {
		return true
	}
	limit := paneWidth - 8
	if paneWidth <= 0 {
		limit = 72
	}
	return displayWidth(message) > limit
}

// pastedPlaceholderRe matches the collapsed big-paste placeholder Claude Code /
// Codex render, e.g. "[Pasted text #1 +8 lines]". Anchored to "[Pasted text" so it
// cannot fire on unrelated prose like "wasted text" (review m13).
var pastedPlaceholderRe = regexp.MustCompile(`\[Pasted text`)

// placeholderInInputRegion reports whether a paste placeholder is sitting in the
// pane's input region (not scrollback) — a Weak-confidence "still present" signal
// for the wrapped path only.
func placeholderInInputRegion(capture string) bool {
	return pastedPlaceholderRe.MatchString(inputRegion(capture))
}

// messageFragmentPresent reports whether a recognizable fragment of THIS message's
// own text is still visible in the pane's input region — a Weak-confidence signal
// (it never drives an Enter retry). Fragments: the head of the first line and the
// head/tail of the last line. The paste placeholder is deliberately NOT a fragment
// here (see placeholderInInputRegion) so "landed" is only ever credited from the
// message's own content (review m13).
func messageFragmentPresent(capture, message string) bool {
	region := inputRegion(capture)
	for _, f := range fragmentsOf(message) {
		if f != "" && strings.Contains(region, f) {
			return true
		}
	}
	return false
}

// fragmentsOf builds the message's own text-fragment candidates, mirroring the
// tmux-send skill's build_fragments (minus the placeholder, handled separately).
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
	if len([]rune(first)) >= 6 {
		frags = append(frags, headRunes(first, 24))
	}
	if last != first && len([]rune(last)) >= 6 {
		frags = append(frags, headRunes(last, 24))
	}
	if len([]rune(last)) > 30 {
		frags = append(frags, tailRunes(last, 20))
	}
	return frags
}

// inputRegion returns the pane's input area: from the last line that looks like a
// prompt (glyph, optionally behind a box border) to the end. Falls back to the last
// few lines when no prompt line is found. Mirrors the tmux-send awk input_region.
func inputRegion(capture string) string {
	lines := strings.Split(capture, "\n")
	start := -1
	for i, ln := range lines {
		if classifyPromptLineRe.MatchString(ln) {
			start = i
		}
	}
	if start >= 0 {
		return strings.Join(lines[start:], "\n")
	}
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

// displayWidth sums the terminal column width of s, counting wide (CJK/fullwidth)
// and emoji runes as 2 and combining marks as 0 — a compact wcwidth approximation
// (review m4) that avoids pulling in golang.org/x/text.
func displayWidth(s string) int {
	w := 0
	for _, r := range s {
		w += runeWidth(r)
	}
	return w
}

func runeWidth(r rune) int {
	if r == 0 || unicode.Is(unicode.Mn, r) || unicode.Is(unicode.Me, r) {
		return 0
	}
	if isWideRune(r) {
		return 2
	}
	return 1
}

// isWideRune reports whether r occupies two terminal columns (East Asian Wide /
// Fullwidth, plus the emoji/symbol planes). Approximate but sufficient to stop an
// emoji/CJK message from being undercounted into the exact-match path.
func isWideRune(r rune) bool {
	switch {
	case r >= 0x1100 && r <= 0x115F, // Hangul Jamo
		r >= 0x2E80 && r <= 0x303E, // CJK radicals, Kangxi
		r >= 0x3041 && r <= 0x33FF, // Hiragana … CJK symbols
		r >= 0x3400 && r <= 0x4DBF, // CJK Ext A
		r >= 0x4E00 && r <= 0x9FFF, // CJK Unified
		r >= 0xA000 && r <= 0xA4CF, // Yi
		r >= 0xAC00 && r <= 0xD7A3, // Hangul Syllables
		r >= 0xF900 && r <= 0xFAFF, // CJK Compatibility Ideographs
		r >= 0xFE30 && r <= 0xFE4F, // CJK Compatibility Forms
		r >= 0xFF00 && r <= 0xFF60, // Fullwidth Forms
		r >= 0xFFE0 && r <= 0xFFE6,
		r >= 0x1F300 && r <= 0x1FAFF, // emoji & symbols
		r >= 0x20000 && r <= 0x3FFFD: // CJK Ext B+
		return true
	}
	return false
}

// stripTrailingNewlines removes trailing newline characters — Enter is sent as a
// separate key event on purpose, so a trailing newline in the payload must not ride
// along in the delivery.
func stripTrailingNewlines(s string) string {
	return strings.TrimRight(s, "\n")
}

// validateMessage enforces the Send payload contract: NUL-free (a NUL cannot cross
// the argv boundary — review n18) and within maxMessageBytes (review m7).
func validateMessage(message string) error {
	if strings.IndexByte(message, 0) >= 0 {
		return fmt.Errorf("tmuxio: message contains a NUL byte (cannot be delivered)")
	}
	if len(message) > maxMessageBytes {
		return fmt.Errorf("tmuxio: message is %d bytes, over the %d-byte limit (write it to a file for the agent to read)", len(message), maxMessageBytes)
	}
	return nil
}
