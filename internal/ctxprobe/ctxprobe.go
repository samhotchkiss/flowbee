// Package ctxprobe reads a running agent SESSION's context-window occupancy from
// disk — the per-session context% the digest surfaces (plan §2.1, §12.4), a launch-
// selection input ("don't hand a big goal to a 23%-context session"), and the soft
// `context_low` drift input. It is a SEPARATE concern from internal/acctprobe:
//
//   - acctprobe answers "which ACCOUNT, and how much of its USAGE QUOTA is spent" —
//     it must touch OAuth tokens (to make the live call) and treats the Codex on-disk
//     rate_limits as display-only (known upstream stamping bugs). ctxprobe answers
//     "how full is THIS SESSION's context window right now" — a plain token count with
//     NO credential surface at all (it reads only token-count telemetry / transcript
//     usage; it never opens auth.json / .credentials.json / .claude.json). Keeping it a
//     separate package keeps that clean security boundary and lets the consolidated
//     ticker probe context independently of account identity.
//   - the granularity differs: acctprobe keys on a config dir / account; ctxprobe keys
//     on the specific rollout (Codex) or the per-cwd transcript (Claude) of ONE session.
//
// Disk sources:
//   - Codex: the newest rollout log's LAST `token_count` event —
//     payload.info.total_token_usage.total_tokens vs .model_context_window. Reading the
//     TAIL means a mid-session self-compaction (which writes a fresh, LOWER token_count
//     after summarizing) yields the POST-compaction number (plan §15.3 accuracy).
//   - Claude: the per-cwd transcript JSONL (config_dir/projects/<cwd-slug>/*.jsonl); the
//     LAST assistant message's usage (input + cache-read + cache-creation input tokens)
//     is the current context occupancy. The context WINDOW size is usually NOT on disk
//     in the transcript — when it is absent ctxprobe returns it UNKNOWN and NEVER
//     guesses (plan §12.4), so RemainingPct reports ok=false rather than a fabricated %.
//   - Grok: the newest session's signals.json under grok_home/sessions/<enc-cwd>/<uuid>/,
//     whose contextTokensUsed / contextWindowTokens fields give BOTH the occupancy and the
//     real window size directly (so grok — unlike Claude — needs no window guess; the
//     window is READ, never the 500000 constant hardcoded). <enc-cwd> is the cwd
//     encodeURIComponent-encoded (see EncodeCwd), grok's own session-dir naming.
//
// Injected FS/clock (mirroring acctprobe) so the same probes run locally or over a
// remote FS, and tests drive fixtures deterministically.
package ctxprobe

import (
	"io"
	"io/fs"
	"os"
	"time"
)

// Provider names the agent CLI a reading came from.
type Provider string

const (
	ProviderClaude Provider = "claude"
	ProviderCodex  Provider = "codex"
	ProviderGrok   Provider = "grok"
)

// Reading is one session's context-window occupancy. UsedTokens is the tokens currently
// occupying the context; ContextWindow is the model's window size (0 = UNKNOWN — never
// guessed). CapturedAt is the reading's own timestamp off disk (zero if unknown); Source
// is the file it came from.
type Reading struct {
	Provider      Provider
	UsedTokens    int64
	ContextWindow int64 // 0 = unknown
	CapturedAt    time.Time
	Source        string
}

// UsedPct returns the percent of the context window OCCUPIED (0..100) and ok. ok=false
// when the window size is unknown (Claude on-disk transcript without a window field) —
// callers must treat that as "context% unknown", never as 0.
func (r Reading) UsedPct() (float64, bool) {
	if r.ContextWindow <= 0 {
		return 0, false
	}
	pct := float64(r.UsedTokens) / float64(r.ContextWindow) * 100
	if pct < 0 {
		pct = 0
	}
	if pct > 100 {
		pct = 100 // a session can momentarily exceed its own window pre-compaction
	}
	return pct, true
}

// RemainingPct returns the percent of the context window still AVAILABLE (0..100) and
// ok — the digest's `context_pct` (higher = healthier; the `context_low` floor is a
// LOW remaining %). ok=false when the window is unknown. A self-compaction RAISES this
// (context freed), which the ticker recognizes via epicdigest.CompactionJumped (§15.3).
func (r Reading) RemainingPct() (float64, bool) {
	used, ok := r.UsedPct()
	if !ok {
		return 0, false
	}
	return 100 - used, true
}

// ── injected dependencies (mirror acctprobe.FS/File so a remote FS is a drop-in) ──

// FS is the read-only filesystem access ctxprobe needs, over ABSOLUTE paths.
type FS interface {
	ReadFile(name string) ([]byte, error)
	ReadDir(name string) ([]fs.DirEntry, error)
	Stat(name string) (fs.FileInfo, error)
	Open(name string) (File, error)
}

// File is an open file supporting tail reads (Seek), so a large rollout/transcript is
// scanned from the end without slurping it whole.
type File interface {
	io.ReadSeekCloser
	Stat() (fs.FileInfo, error)
}

// OSFS is the local filesystem, reading absolute paths straight through os.*.
type OSFS struct{}

func (OSFS) ReadFile(name string) ([]byte, error)       { return os.ReadFile(name) }
func (OSFS) ReadDir(name string) ([]fs.DirEntry, error) { return os.ReadDir(name) }
func (OSFS) Stat(name string) (fs.FileInfo, error)      { return os.Stat(name) }
func (OSFS) Open(name string) (File, error) {
	f, err := os.Open(name) //nolint:gosec // absolute paths are caller-supplied config dirs, read-only
	if err != nil {
		return nil, err
	}
	return f, nil
}

// Prober carries the injected FS and exposes the per-provider probes. Build the local
// default with New(); inject a fake FS with NewWith for tests or a remote mode.
type Prober struct {
	FS FS
}

// New returns a Prober wired to the local filesystem.
func New() *Prober { return &Prober{FS: OSFS{}} }

// NewWith builds a Prober with an explicit FS (nil defaults to OSFS).
func NewWith(filesystem FS) *Prober {
	if filesystem == nil {
		filesystem = OSFS{}
	}
	return &Prober{FS: filesystem}
}

const (
	// tailWindow bounds the tail of a rollout/transcript scanned for the last usage
	// event, so a large session log is never slurped whole.
	tailWindow = 512 * 1024
	// maxRollouts bounds how many newest Codex rollout files are tried before giving up
	// (a fresh session with no token_count event yet falls through to older ones).
	maxRollouts = 12
)

// readTail returns the last tailWindow bytes of path (the whole file if smaller), with a
// possibly-partial leading line dropped when the read did not start at byte 0. Returns
// ok=false on any read error.
func (p *Prober) readTail(path string) ([]byte, bool) {
	f, err := p.FS.Open(path)
	if err != nil {
		return nil, false
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return nil, false
	}
	size := info.Size()
	window := int64(tailWindow)
	if window > size {
		window = size
	}
	if _, err := f.Seek(size-window, io.SeekStart); err != nil {
		return nil, false
	}
	buf := make([]byte, window)
	if _, err := io.ReadFull(f, buf); err != nil {
		return nil, false
	}
	if window < size {
		if i := indexByte(buf, '\n'); i >= 0 {
			buf = buf[i+1:]
		}
	}
	return buf, true
}

func indexByte(b []byte, c byte) int {
	for i := range b {
		if b[i] == c {
			return i
		}
	}
	return -1
}

// parseRFC3339 best-effort parses a disk timestamp to UTC (zero on any failure).
func parseRFC3339(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t.UTC()
	}
	return time.Time{}
}
