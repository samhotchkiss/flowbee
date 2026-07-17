package ctxprobe

import (
	"encoding/json"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// grokSignals is the ALLOW-LIST parse of a grok session's signals.json — ONLY the two
// context-window fields. grok persists BOTH the occupancy and the real window size here
// (live-confirmed on grok1@localhost: contextTokensUsed=2301, contextWindowTokens=500000),
// so ctxprobe reads the window from disk and never hardcodes grok's 500k constant. No
// token or credential surface: signals.json carries none, matching ctxprobe's zero-secret
// contract.
type grokSignals struct {
	ContextTokensUsed   int64 `json:"contextTokensUsed"`
	ContextWindowTokens int64 `json:"contextWindowTokens"`
}

// ProbeGrok reads a grok session's context occupancy from the newest session's
// signals.json under grok_home/sessions/<EncodeCwd(cwd)>/<session-uuid>/signals.json.
// cwd is the session's working directory (the epic checkout); grok names its per-cwd
// session directory by encodeURIComponent(cwd) (see EncodeCwd). The context window is
// read from contextWindowTokens on disk — grok records it explicitly, so RemainingPct is
// always well-defined (ok=true) when a session is found, unlike the Claude transcript
// path. Returns ok=false (no error) when the cwd dir / a session with a signals.json is
// absent (a just-launched session that has not written signals yet is "unknown", not a
// failure).
func (p *Prober) ProbeGrok(grokHome, cwd string) (Reading, bool, error) {
	cwdDir := filepath.Join(grokHome, "sessions", EncodeCwd(cwd))
	for _, sessDir := range p.newestGrokSessions(cwdDir) {
		path := filepath.Join(sessDir, "signals.json")
		b, err := p.FS.ReadFile(path)
		if err != nil {
			continue
		}
		var sig grokSignals
		if json.Unmarshal(b, &sig) != nil {
			continue
		}
		if sig.ContextTokensUsed == 0 && sig.ContextWindowTokens == 0 {
			continue // an empty signals.json tells us nothing about occupancy
		}
		captured := time.Time{}
		if info, serr := p.FS.Stat(path); serr == nil {
			captured = info.ModTime().UTC()
		}
		return Reading{
			Provider:      ProviderGrok,
			UsedTokens:    sig.ContextTokensUsed,
			ContextWindow: sig.ContextWindowTokens, // read from disk; never the hardcoded 500000
			CapturedAt:    captured,
			Source:        path,
		}, true, nil
	}
	return Reading{}, false, nil
}

// newestGrokSessions lists the session sub-directories of a cwd's session dir, newest
// first (by mtime, ties broken by name descending — grok session ids are UUIDv7, whose
// lexical order is already chronological, so the two orderings agree). Non-dir siblings
// (prompt_history.jsonl, session_search.sqlite) are skipped.
func (p *Prober) newestGrokSessions(cwdDir string) []string {
	entries, err := p.FS.ReadDir(cwdDir)
	if err != nil {
		return nil
	}
	type cand struct {
		name string
		mod  time.Time
	}
	var cands []cand
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		mod := time.Time{}
		if info, ierr := e.Info(); ierr == nil {
			mod = info.ModTime()
		}
		cands = append(cands, cand{name: e.Name(), mod: mod})
	}
	sort.Slice(cands, func(i, j int) bool {
		if !cands[i].mod.Equal(cands[j].mod) {
			return cands[i].mod.After(cands[j].mod)
		}
		return cands[i].name > cands[j].name
	})
	out := make([]string, 0, len(cands))
	for _, c := range cands {
		out = append(out, filepath.Join(cwdDir, c.name))
		if len(out) >= maxRollouts { // reuse the same "how many newest to try" bound
			break
		}
	}
	return out
}

// EncodeCwd encodes a working directory the way grok names its per-cwd session directory:
// JavaScript encodeURIComponent, i.e. every byte outside the unreserved set
// [A-Za-z0-9-_.!~*'()] is percent-encoded as %XX over its UTF-8 bytes (uppercase hex). So
// "/Users/grok1" -> "%2FUsers%2Fgrok1" (live-verified against the box). Exposed so the
// caller can name the same directory when wiring the probe.
func EncodeCwd(cwd string) string {
	const upperhex = "0123456789ABCDEF"
	var b strings.Builder
	for i := 0; i < len(cwd); i++ {
		c := cwd[i]
		if grokUnreserved(c) {
			b.WriteByte(c)
			continue
		}
		b.WriteByte('%')
		b.WriteByte(upperhex[c>>4])
		b.WriteByte(upperhex[c&0x0f])
	}
	return b.String()
}

// grokUnreserved reports whether a byte is left un-encoded by encodeURIComponent.
func grokUnreserved(c byte) bool {
	switch {
	case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9':
		return true
	}
	switch c {
	case '-', '_', '.', '!', '~', '*', '\'', '(', ')':
		return true
	}
	return false
}
