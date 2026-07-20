package tmuxio

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"strings"
)

// Capture is a snapshot of a pane's text. Raw is exactly what tmux emitted;
// Normalized is Raw with trailing whitespace stripped per line and runs of blank
// lines collapsed (so cosmetic churn — a blinking cursor's trailing space, a
// redraw that adds a blank line — does not read as a content change); Hash is a
// stable content hash of Normalized, for cheap change-detection.
type Capture struct {
	Raw        string
	Normalized string
	Hash       string // hex sha256 of Normalized
}

// Capture reads a pane's visible content plus `history` lines of scrollback
// (history <= 0 captures just the visible screen). Pass history=0 for liveness
// polling: capture the visible screen repeatedly and compare Hash — an unchanged
// Hash across N polls is a stalled UI. Pass history>0 to read back the reason text
// that scrolled past (e.g. classifying WHY a pane is blocked).
//
// target is any tmux target (pane id "%5", "session:win.pane", or a session name);
// it must be a caller-validated identifier and is shQuote'd regardless.
func (c *Client) Capture(ctx context.Context, target string, history int) (Capture, error) {
	if err := c.validateIdent("target", target); err != nil {
		return Capture{}, err
	}
	sub := "capture-pane -p -t " + shQuote(exactTarget(target))
	if history > 0 {
		sub += " -S -" + strconv.Itoa(history)
	}
	raw, err := c.run(ctx, sub)
	if err != nil {
		return Capture{}, err
	}
	return newCapture(raw), nil
}

// newCapture builds a Capture from raw pane text (normalizing and hashing). Split
// out so tests can exercise normalization/hashing without a tmux server.
func newCapture(raw string) Capture {
	norm := normalize(raw)
	sum := sha256.Sum256([]byte(norm))
	return Capture{Raw: raw, Normalized: norm, Hash: hex.EncodeToString(sum[:])}
}

// normalize strips trailing whitespace from every line, drops trailing blank
// lines, and collapses any run of 2+ blank lines to a single blank line. This is
// what the change-detection Hash is taken over, so two captures that differ only
// in trailing padding or cursor-blink whitespace hash identically. Leading blank
// lines are preserved (they can be meaningful vertical position); only trailing
// padding — which tmux always adds to fill the pane height — is removed.
func normalize(raw string) string {
	lines := strings.Split(raw, "\n")
	out := make([]string, 0, len(lines))
	blankRun := 0
	for _, ln := range lines {
		ln = strings.TrimRight(ln, " \t\r")
		if ln == "" {
			blankRun++
			if blankRun >= 2 {
				continue // collapse consecutive blanks
			}
		} else {
			blankRun = 0
		}
		out = append(out, ln)
	}
	// Drop trailing blank lines (tmux pads to pane height).
	for len(out) > 0 && out[len(out)-1] == "" {
		out = out[:len(out)-1]
	}
	return strings.Join(out, "\n")
}
