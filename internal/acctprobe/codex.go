package acctprobe

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	// codexTailWindow bounds the tail of each rollout log scanned for the last
	// token_count event, so a large session log is never slurped whole.
	codexTailWindow = 512 * 1024
	// codexMaxRollouts bounds how many newest rollout files are tried before giving
	// up (a fresh session with no token_count event yet falls through to older ones).
	codexMaxRollouts = 12
)

// codexAuth is the ALLOW-LIST parse of `auth.json`. account_id/auth_mode are
// non-secret identity. The token fields are read ONLY to compute one-way digests
// (credential/lineage) and to detect auth mode by presence — their values are never
// stored on an exported type, returned, or logged.
type codexAuth struct {
	AuthMode     string `json:"auth_mode"`
	OpenAIAPIKey string `json:"OPENAI_API_KEY"` // presence ⇒ apikey mode; value never used
	Tokens       struct {
		AccountID    string `json:"account_id"`
		AccessToken  string `json:"access_token"`  // transient: credential digest only
		RefreshToken string `json:"refresh_token"` // transient: lineage digest only
		IDToken      string `json:"id_token"`      // presence ⇒ chatgpt (subscription) mode
	} `json:"tokens"`
}

// codexRolloutLine is the ALLOW-LIST parse of a rollout JSONL `event_msg` line whose
// payload is a token_count. rate_limits here is DISPLAY-ONLY (upstream stamping bugs
// openai/codex#16323, #14880) — never scheduled on.
type codexRolloutLine struct {
	Timestamp string `json:"timestamp"`
	Payload   struct {
		Type string `json:"type"`
		Info struct {
			TotalTokenUsage struct {
				TotalTokens int64 `json:"total_tokens"`
			} `json:"total_token_usage"`
			ModelContextWindow int64 `json:"model_context_window"`
		} `json:"info"`
		RateLimits struct {
			PlanType  string           `json:"plan_type"`
			Primary   *codexDiskWindow `json:"primary"`
			Secondary *codexDiskWindow `json:"secondary"`
			Credits   struct {
				Balance string `json:"balance"`
			} `json:"credits"`
			RateLimitReachedType *string `json:"rate_limit_reached_type"`
		} `json:"rate_limits"`
	} `json:"payload"`
}

type codexDiskWindow struct {
	UsedPercent   float64 `json:"used_percent"`
	WindowMinutes int     `json:"window_minutes"`
	ResetsAt      int64   `json:"resets_at"` // unix epoch SECONDS
}

// ProbeCodexHome reads a Codex home's identity (from auth.json + config.toml) and its
// on-disk session telemetry (newest rollout's last token_count). The telemetry is
// DISPLAY-ONLY — Codex's persisted rate_limits carry known upstream stamping bugs, so
// the result is TrustDisplayOnly and NEVER capacity-routable. Use ProbeCodexLive for a
// routable reading. SECURITY: token values are read only for digests, never returned.
func (p *Prober) ProbeCodexHome(dir string) (*Result, error) {
	id, err := p.codexIdentity(dir)
	if err != nil {
		return nil, err
	}
	usage, capturedAt, src := p.latestCodexTelemetry(dir)
	if usage.PlanType != "" && id.Tier == "" {
		id.Tier = usage.PlanType
	}
	return &Result{
		Identity:   id,
		Usage:      usage,
		TrustState: TrustDisplayOnly,
		CapturedAt: capturedAt,
		Source:     src,
	}, nil
}

// codexIdentity builds the local, non-secret Codex identity plus credential/lineage
// digests. account_id is required (it is the durable account key).
func (p *Prober) codexIdentity(dir string) (Identity, error) {
	b, err := p.FS.ReadFile(filepath.Join(dir, "auth.json"))
	if err != nil {
		return Identity{}, held(ReasonIdentityMissing, fmt.Errorf("codex auth %q: %w", dir, err))
	}
	var auth codexAuth
	if err := json.Unmarshal(b, &auth); err != nil {
		return Identity{}, held(ReasonIdentityMissing, fmt.Errorf("parse codex auth %q: %w", dir, err))
	}
	if auth.Tokens.AccountID == "" {
		return Identity{}, held(ReasonIdentityMissing, fmt.Errorf("codex auth %q has no tokens.account_id", dir))
	}
	id := Identity{
		Provider:    ProviderCodex,
		AccountKey:  auth.Tokens.AccountID,
		Fingerprint: fingerprint(auth.Tokens.AccountID),
		ConfigDir:   dir,
		AuthMode:    codexAuthMode(auth),
		Model:       p.codexModel(dir),
	}
	if d, ok := digest16(auth.Tokens.AccessToken); ok {
		id.CredentialDigest = d
	}
	if d, ok := digest16(auth.Tokens.RefreshToken); ok {
		id.LineageDigest = d
	}
	return id, nil
}

// codexAuthMode classifies how this Codex home authenticates: "chatgpt" (subscription
// login with usage windows), "apikey" (metered — no subscription windows to route),
// or "unknown".
func codexAuthMode(a codexAuth) string {
	m := strings.ToLower(strings.TrimSpace(a.AuthMode))
	if m == "apikey" {
		return "apikey"
	}
	if a.Tokens.IDToken != "" {
		return "chatgpt"
	}
	if a.OpenAIAPIKey != "" {
		return "apikey"
	}
	if m != "" {
		return m
	}
	return "unknown"
}

// codexAccessToken returns the Codex access token transiently (digest use only).
func (p *Prober) codexAccessToken(dir string) (string, bool) {
	b, err := p.FS.ReadFile(filepath.Join(dir, "auth.json"))
	if err != nil {
		return "", false
	}
	var auth codexAuth
	if json.Unmarshal(b, &auth) != nil || auth.Tokens.AccessToken == "" {
		return "", false
	}
	return auth.Tokens.AccessToken, true
}

// codexModel returns the top-level `model` from config.toml (best-effort; "" when
// absent — Codex has a built-in default).
func (p *Prober) codexModel(dir string) string {
	b, err := p.FS.ReadFile(filepath.Join(dir, "config.toml"))
	if err != nil {
		return ""
	}
	return parseCodexModel(b)
}

// parseCodexModel extracts the top-level (pre-section) `model = "..."` key from a
// minimal TOML file — enough for the one scalar we need without a TOML dependency.
func parseCodexModel(b []byte) string {
	section := ""
	for _, raw := range strings.Split(string(b), "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "[") {
			section = line
			continue
		}
		if section != "" {
			continue // only the top-level model key
		}
		key, val, ok := strings.Cut(line, "=")
		if !ok || strings.TrimSpace(key) != "model" {
			continue
		}
		return strings.Trim(strings.TrimSpace(val), `"'`)
	}
	return ""
}

// latestCodexTelemetry scans the newest rollout logs (bounded) for the last
// token_count event and folds it into a display-only Usage, returning also the
// event's capture time and source path. Empty Usage when none is found.
func (p *Prober) latestCodexTelemetry(dir string) (Usage, time.Time, string) {
	for _, f := range p.recentRolloutFiles(dir) {
		if usage, captured, ok := p.parseRolloutTail(f); ok {
			return usage, captured, f
		}
	}
	return Usage{}, time.Time{}, ""
}

// recentRolloutFiles returns up to codexMaxRollouts rollout paths, newest first,
// walking sessions/<year>/<month>/<day>/ in descending order. Zero-padded names sort
// lexically = chronologically, so a descending walk yields global newest-first.
func (p *Prober) recentRolloutFiles(dir string) []string {
	var out []string
	root := filepath.Join(dir, "sessions")
	years := p.descendingDirs(root)
	for _, y := range years {
		for _, m := range p.descendingDirs(filepath.Join(root, y)) {
			for _, d := range p.descendingDirs(filepath.Join(root, y, m)) {
				dayDir := filepath.Join(root, y, m, d)
				entries, err := p.FS.ReadDir(dayDir)
				if err != nil {
					continue
				}
				var files []string
				for _, e := range entries {
					if !e.IsDir() && strings.HasPrefix(e.Name(), "rollout-") && strings.HasSuffix(e.Name(), ".jsonl") {
						files = append(files, e.Name())
					}
				}
				sort.Sort(sort.Reverse(sort.StringSlice(files)))
				for _, f := range files {
					out = append(out, filepath.Join(dayDir, f))
					if len(out) >= codexMaxRollouts {
						return out
					}
				}
			}
		}
	}
	return out
}

// descendingDirs lists sub-directory names of parent, sorted descending.
func (p *Prober) descendingDirs(parent string) []string {
	entries, err := p.FS.ReadDir(parent)
	if err != nil {
		return nil
	}
	var dirs []string
	for _, e := range entries {
		if e.IsDir() {
			dirs = append(dirs, e.Name())
		}
	}
	sort.Sort(sort.Reverse(sort.StringSlice(dirs)))
	return dirs
}

// parseRolloutTail reads the tail of one rollout file and returns the LAST token_count
// event's usage (bucketed by window duration), its capture time, and ok. Bounded: it
// reads at most codexTailWindow bytes from the end.
func (p *Prober) parseRolloutTail(path string) (Usage, time.Time, bool) {
	f, err := p.FS.Open(path)
	if err != nil {
		return Usage{}, time.Time{}, false
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return Usage{}, time.Time{}, false
	}
	size := info.Size()
	window := int64(codexTailWindow)
	if window > size {
		window = size
	}
	if _, err := f.Seek(size-window, io.SeekStart); err != nil {
		return Usage{}, time.Time{}, false
	}
	buf := make([]byte, window)
	if _, err := io.ReadFull(f, buf); err != nil {
		return Usage{}, time.Time{}, false
	}
	// drop a possibly-partial first line when we didn't start at byte 0.
	if window < size {
		if i := bytes.IndexByte(buf, '\n'); i >= 0 {
			buf = buf[i+1:]
		}
	}
	lines := bytes.Split(buf, []byte("\n"))
	for i := len(lines) - 1; i >= 0; i-- {
		line := bytes.TrimSpace(lines[i])
		if len(line) == 0 || !bytes.Contains(line, []byte(`"token_count"`)) {
			continue
		}
		var rl codexRolloutLine
		if json.Unmarshal(line, &rl) != nil || rl.Payload.Type != "token_count" {
			continue
		}
		return codexTelemetryUsage(rl), parseRFC3339(rl.Timestamp), true
	}
	return Usage{}, time.Time{}, false
}

// codexTelemetryUsage folds a rollout token_count into a display-only Usage. Windows
// are bucketed by their real window_minutes, not primary/secondary position; an
// unrecognized-duration window is simply omitted.
func codexTelemetryUsage(rl codexRolloutLine) Usage {
	pl := rl.Payload
	usage := Usage{
		TotalTokens:   pl.Info.TotalTokenUsage.TotalTokens,
		ContextWindow: pl.Info.ModelContextWindow,
		CreditBalance: pl.RateLimits.Credits.Balance,
		PlanType:      pl.RateLimits.PlanType,
	}
	for _, w := range []*codexDiskWindow{pl.RateLimits.Primary, pl.RateLimits.Secondary} {
		if w == nil {
			continue
		}
		if lw, ok := bucketWindow(w.WindowMinutes, w.UsedPercent, unixMaybe(w.ResetsAt)); ok {
			usage.Windows = append(usage.Windows, lw)
		}
	}
	if pl.RateLimits.RateLimitReachedType != nil && *pl.RateLimits.RateLimitReachedType != "" {
		usage.RateLimited = true
	}
	if maxPct, ok := usage.Windows.MaxPct(); ok && maxPct >= 100 {
		usage.RateLimited = true
	}
	return usage
}

// bucketWindow maps a window by its REAL duration onto a standard kind: 300 minutes ⇒
// session (5h), 10080 ⇒ weekly (7d). Any other duration is not a standard capacity
// window and is rejected (ok=false) rather than mis-bucketed. Out-of-range percents
// are rejected too. The server reorders/omits windows, so bucketing must be by
// duration, never by position.
func bucketWindow(minutes int, percent float64, resetsAt time.Time) (LimitWindow, bool) {
	if percent < 0 || percent > 100 {
		return LimitWindow{}, false
	}
	var kind WindowKind
	switch minutes {
	case 300:
		kind = KindSession
	case 10080:
		kind = KindWeeklyAll
	default:
		return LimitWindow{}, false
	}
	sev := SeverityNormal
	if percent >= 100 {
		sev = SeverityCritical
	}
	return LimitWindow{
		Kind: kind, Percent: percent, Severity: sev,
		ResetsAt: resetsAt, WindowMinutes: minutes, Active: true,
	}, true
}

// unixMaybe converts a positive unix-seconds epoch to a UTC time, or zero.
func unixMaybe(sec int64) time.Time {
	if sec <= 0 {
		return time.Time{}
	}
	return time.Unix(sec, 0).UTC()
}
