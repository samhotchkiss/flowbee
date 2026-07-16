package acctprobe

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// claudeConfig is the ALLOW-LIST parse of `.claude.json`. Only these non-secret
// fields are decoded; the file's tokens and other sensitive keys have no field to
// land in and are dropped by encoding/json. Do NOT add credential-adjacent fields.
type claudeConfig struct {
	OauthAccount struct {
		AccountUUID               string `json:"accountUuid"`
		EmailAddress              string `json:"emailAddress"`
		OrganizationName          string `json:"organizationName"`
		OrganizationUUID          string `json:"organizationUuid"`
		OrganizationRateLimitTier string `json:"organizationRateLimitTier"`
		SeatTier                  string `json:"seatTier"` // JSON null decodes to "" (fine)
	} `json:"oauthAccount"`
	CachedUsageUtilization struct {
		FetchedAtMs int64             `json:"fetchedAtMs"`
		AccountUUID string            `json:"accountUuid"`
		Utilization claudeUtilization `json:"utilization"`
	} `json:"cachedUsageUtilization"`
}

// claudeUtilization is the shared shape of both the cached blob's `utilization`
// object and the LIVE usage-API response body — so one parser serves both tiers.
type claudeUtilization struct {
	FiveHour   claudeWindowRaw `json:"five_hour"`
	SevenDay   claudeWindowRaw `json:"seven_day"`
	ExtraUsage struct {
		IsEnabled bool `json:"is_enabled"`
	} `json:"extra_usage"`
	Limits []claudeLimitRaw `json:"limits"`
	Spend  struct {
		Percent float64 `json:"percent"`
		Enabled bool    `json:"enabled"`
	} `json:"spend"`
}

type claudeWindowRaw struct {
	Utilization *float64 `json:"utilization"` // pointer: absent ≠ 0%
	ResetsAt    string   `json:"resets_at"`
}

type claudeLimitRaw struct {
	Kind     string   `json:"kind"`
	Percent  *float64 `json:"percent"` // pointer: absent/null ≠ 0% (must not synthesize a fresh 0)
	Severity string   `json:"severity"`
	ResetsAt string   `json:"resets_at"`
	Scope    *struct {
		Model struct {
			DisplayName string `json:"display_name"`
		} `json:"model"`
	} `json:"scope"`
	IsActive bool `json:"is_active"`
}

// DiscoverClaudeDirs returns the Claude config directories under home whose account
// data is readable. It scans for `.claude*` directories containing a `.claude.json`,
// AND always includes the default `<home>/.claude` when the legacy `<home>/.claude.json`
// (a sibling FILE, not inside the dir) exists — on machines predating Claude's config
// migration the default account's real data lives in that legacy file while
// `<home>/.claude/.claude.json` is only a stub. ProbeClaudeDir resolves that fallback;
// discovery just makes sure the default dir is listed. De-duplicated and sorted.
func (p *Prober) DiscoverClaudeDirs(home string) ([]string, error) {
	seen := map[string]bool{}
	entries, err := p.FS.ReadDir(home)
	if err != nil {
		return nil, fmt.Errorf("acctprobe: read home %q: %w", home, err)
	}
	for _, e := range entries {
		if !e.IsDir() || !strings.HasPrefix(e.Name(), ".claude") {
			continue
		}
		dir := filepath.Join(home, e.Name())
		if _, err := p.FS.Stat(filepath.Join(dir, ".claude.json")); err == nil {
			seen[dir] = true
		}
	}
	if _, err := p.FS.Stat(filepath.Join(home, ".claude.json")); err == nil {
		seen[filepath.Join(home, ".claude")] = true
	}
	dirs := make([]string, 0, len(seen))
	for d := range seen {
		dirs = append(dirs, d)
	}
	sort.Strings(dirs)
	return dirs, nil
}

// ProbeClaudeDir reads one Claude config dir's CACHED (on-disk, fallback-tier)
// account identity and usage. The config JSON is `<dir>/.claude.json`, except that
// the default `<dir=~/.claude>` — whose inner file is often a stub with no account —
// falls back to the legacy sibling `<home>/.claude.json`.
//
// The cache only refreshes while that dir's CLI is running, so the reading is
// stamped with its capture time (fetchedAtMs) and trusted at most VerifiedLocal,
// downgraded to Stale past the freshness bound; a dir with identity but no usable
// window is returned Held (identity still populated for the dashboard). A hard error
// (unreadable/unparsable/no account) returns a *HoldError. SECURITY: only allow-
// listed non-secret fields are decoded; no token is read here.
func (p *Prober) ProbeClaudeDir(dir string) (*Result, error) {
	cfg, src, err := p.resolveClaudeConfig(dir)
	if err != nil {
		return nil, held(ReasonIdentityMissing, fmt.Errorf("probe claude dir %q: %w", dir, err))
	}

	id := Identity{
		Provider:    ProviderClaude,
		AccountKey:  cfg.OauthAccount.AccountUUID,
		Fingerprint: fingerprint(cfg.OauthAccount.AccountUUID),
		Email:       cfg.OauthAccount.EmailAddress,
		Org:         cfg.OauthAccount.OrganizationName,
		OrgKey:      cfg.OauthAccount.OrganizationUUID,
		ConfigDir:   dir,
		Tier:        cfg.OauthAccount.OrganizationRateLimitTier,
		SeatTier:    cfg.OauthAccount.SeatTier,
		Verified:    false, // local metadata only
	}

	usage := claudeUsage(cfg.CachedUsageUtilization.Utilization)
	res := &Result{Identity: id, Usage: usage, Source: src}
	if cfg.CachedUsageUtilization.FetchedAtMs > 0 {
		res.CapturedAt = time.UnixMilli(cfg.CachedUsageUtilization.FetchedAtMs).UTC()
	}
	p.stampLocalTrust(res)
	return res, nil
}

// stampLocalTrust sets TrustState for a LOCAL cache reading: Held when it carried no
// usable window; else VerifiedLocal, downgraded to Stale past the freshness bound OR
// when the capture time is UNKNOWN (zero) — an unknown age must never read as fresh,
// so a cache with no fetchedAtMs can never stay routable forever.
func (p *Prober) stampLocalTrust(res *Result) {
	if len(res.Usage.Windows) == 0 {
		res.TrustState = TrustHeld
		res.Hold = ReasonUnrecognizedPayload
		return
	}
	res.TrustState = TrustVerifiedLocal
	d, ok := res.Staleness(p.Clock.Now())
	if !ok || d > p.staleAfter() { // unknown age (ok=false) or too old ⇒ stale
		res.TrustState = TrustStale
	}
}

// errClaudeStub marks a `.claude.json` that parsed but carried no account (the
// 178-byte default stub). It is distinct from a missing file and from a parse error
// so resolveClaudeConfig can fall back to the legacy sibling for the stub/missing
// cases only — never masking a real parse error of a present, populated file.
var errClaudeStub = errors.New("no oauthAccount.accountUuid")

// resolveClaudeConfig returns the usable `.claude.json` for dir: the inner
// `<dir>/.claude.json` normally, falling back to the legacy sibling `<home>/.claude.json`
// ONLY for the default `.claude` dir AND only when the inner file is ABSENT or a STUB
// (never on a parse error, which is surfaced). NOTE: if a default dir's inner file
// were itself a valid-but-different account from the legacy sibling, this prefers the
// inner one and does not probe the sibling; that divergence is not observed on real
// installs (the migrated default inner is always a stub, or the legacy file is absent)
// and a single-Result API has nowhere to surface a second account, so it is a
// documented non-goal rather than a handled case.
func (p *Prober) resolveClaudeConfig(dir string) (claudeConfig, string, error) {
	cfg, src, err := p.readUsableClaudeConfig(filepath.Join(dir, ".claude.json"))
	if err == nil {
		return cfg, src, nil
	}
	if filepath.Base(dir) == ".claude" && (errors.Is(err, os.ErrNotExist) || errors.Is(err, errClaudeStub)) {
		return p.readUsableClaudeConfig(filepath.Join(filepath.Dir(dir), ".claude.json"))
	}
	return cfg, "", err
}

// readUsableClaudeConfig reads and allow-list-parses path, erroring unless it yielded
// a real account (non-empty oauthAccount.accountUuid). A stub returns errClaudeStub; a
// missing file wraps os.ErrNotExist; a malformed file returns a parse error — three
// distinct outcomes so resolveClaudeConfig only falls back on the first two.
func (p *Prober) readUsableClaudeConfig(path string) (claudeConfig, string, error) {
	var cfg claudeConfig
	b, err := p.FS.ReadFile(path)
	if err != nil {
		return cfg, "", err // wraps os.ErrNotExist when absent
	}
	if err := json.Unmarshal(b, &cfg); err != nil {
		return cfg, "", fmt.Errorf("parse %q: %w", path, err)
	}
	if cfg.OauthAccount.AccountUUID == "" {
		return cfg, "", fmt.Errorf("%q: %w", path, errClaudeStub)
	}
	return cfg, path, nil
}

// claudeUsage folds a utilization blob (cached OR live — same shape) into the
// provider-neutral Usage. The authoritative window list is `limits[]`; when it is
// absent (older caches) it falls back to the five_hour/seven_day summaries. An
// absent summary window is OMITTED, never synthesized as 0%.
func claudeUsage(u claudeUtilization) Usage {
	usage := Usage{
		ExtraUsageEnabled: u.ExtraUsage.IsEnabled,
		SpendPct:          u.Spend.Percent,
		SpendEnabled:      u.Spend.Enabled,
	}
	if len(u.Limits) > 0 {
		for _, l := range u.Limits {
			// A missing/null percent, or one out of the 0..100 range, proves nothing —
			// SKIP it rather than synthesize a fresh 0% (which would read an exhausted
			// account as idle). Mirrors five_hour/seven_day and Codex bucketWindow.
			if l.Percent == nil || !validClaudePercent(*l.Percent) {
				continue
			}
			usage.Windows = append(usage.Windows, LimitWindow{
				Kind:          claudeKind(l.Kind),
				Percent:       *l.Percent,
				Severity:      severityOf(l.Severity),
				ResetsAt:      parseRFC3339(l.ResetsAt),
				Scope:         claudeScope(l),
				WindowMinutes: claudeMinutes(l.Kind),
				Active:        l.IsActive,
			})
		}
	} else {
		if u.FiveHour.Utilization != nil && validClaudePercent(*u.FiveHour.Utilization) {
			usage.Windows = append(usage.Windows, LimitWindow{
				Kind: KindSession, Percent: *u.FiveHour.Utilization,
				Severity: SeverityNormal, ResetsAt: parseRFC3339(u.FiveHour.ResetsAt),
				WindowMinutes: 300, Active: true,
			})
		}
		if u.SevenDay.Utilization != nil && validClaudePercent(*u.SevenDay.Utilization) {
			usage.Windows = append(usage.Windows, LimitWindow{
				Kind: KindWeeklyAll, Percent: *u.SevenDay.Utilization,
				Severity: SeverityNormal, ResetsAt: parseRFC3339(u.SevenDay.ResetsAt),
				WindowMinutes: 10080,
			})
		}
	}
	if maxPct, ok := usage.Windows.MaxPct(); ok && maxPct >= 100 {
		usage.RateLimited = true
	}
	return usage
}

// validClaudePercent bounds a server-reported percent to the sane 0..100 range.
func validClaudePercent(p float64) bool { return p >= 0 && p <= 100 }

func claudeKind(kind string) WindowKind {
	switch kind {
	case "session":
		return KindSession
	case "weekly_all":
		return KindWeeklyAll
	case "weekly_scoped":
		return KindWeeklyScoped
	default:
		return WindowKind(kind)
	}
}

// claudeMinutes maps a Claude limit kind to its window duration (session=5h,
// weekly=7d). Claude does not carry the minutes explicitly the way Codex does.
func claudeMinutes(kind string) int {
	switch kind {
	case "session":
		return 300
	case "weekly_all", "weekly_scoped":
		return 10080
	default:
		return 0
	}
}

func claudeScope(l claudeLimitRaw) string {
	if l.Scope == nil {
		return ""
	}
	return l.Scope.Model.DisplayName
}

func severityOf(s string) Severity {
	if s == "critical" {
		return SeverityCritical
	}
	return SeverityNormal
}

// parseRFC3339 best-effort parses a server timestamp (accepts fractional seconds
// and a numeric offset, e.g. "2026-07-16T23:50:00.238598+00:00"); a zero time on any
// failure lets callers treat "reset unknown" uniformly.
func parseRFC3339(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t.UTC()
	}
	return time.Time{}
}
