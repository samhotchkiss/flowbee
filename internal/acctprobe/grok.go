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
	// grokAuthIssuerSep is the separator in an auth.json top-level key. grok keys each
	// logged-in principal by "<oidc_issuer>::<uuid>", e.g.
	// "https://auth.x.ai::b1a00492-…" (live-verified on the seat box). We match on the
	// "::" separator rather than a hardcoded issuer host so a future issuer change does
	// not silently drop the identity.
	grokAuthIssuerSep = "::"
	// grokLogBillingMarker is the substring of the unified.jsonl line the grok CLI writes
	// every time a user runs `/usage`. The fallback tier tails for the LAST such line.
	grokLogBillingMarker = "billing: fetched credits config"
	// grokLogTailWindow bounds the tail of unified.jsonl scanned for the last billing
	// line, so a large log is never slurped whole (mirrors codexTailWindow).
	grokLogTailWindow = 512 * 1024
)

// grokAuthEntry is the ALLOW-LIST parse of ONE auth.json principal object. It carries the
// NON-secret identity fields PLUS the two secrets (Key/RefreshToken) that are read
// TRANSIENTLY — Key for the live billing Bearer and a credential digest, RefreshToken for
// a lineage digest — exactly the codex posture: their raw values are never stored on an
// exported type, returned, or logged. ExpiresAt gates the never-refresh live pre-check.
// Do NOT add a field that would let a secret's raw VALUE escape onto a Result.
type grokAuthEntry struct {
	AuthMode      string `json:"auth_mode"`      // "oidc"
	Email         string `json:"email"`          // the account's email
	UserID        string `json:"user_id"`        // durable per-account id (== principal_id for a User)
	TeamID        string `json:"team_id"`        // grok's team ≈ org
	PrincipalType string `json:"principal_type"` // "User" / "Team"
	PrincipalID   string `json:"principal_id"`
	ExpiresAt     string `json:"expires_at"`    // RFC3339 bearer expiry (non-secret)
	Key           string `json:"key"`           // SECRET: transient — live Bearer + credential digest only
	RefreshToken  string `json:"refresh_token"` // SECRET: transient — lineage digest only
}

// grokBillingResponse / grokBillingConfig is the ALLOW-LIST parse of the live
// GET /v1/billing?format=credits body (`{"config":{…}}`) AND the unified.jsonl
// "billing: fetched credits config" log line's ctx.config — the SAME shape, so one parser
// serves both tiers (live-verified against the seat box). Only non-secret usage fields.
//
// grok has EXACTLY ONE window — the account-wide weekly (or monthly) billing period; there
// is no 5h/session/rolling window (unlike Claude 5h / Codex primary+secondary). So a grok
// reading carries a single KindWeeklyAll window. creditUsagePercent is OPTIONAL: the CLI
// omits it on accounts with no metered cap, and the TUI then shows "Weekly limit: 0%", so
// an ABSENT percent means a real 0% (a fresh capped account), not "unknown".
type grokBillingResponse struct {
	Config grokBillingConfig `json:"config"`
}

type grokBillingConfig struct {
	CreditUsagePercent *float64 `json:"creditUsagePercent"` // OPTIONAL; absent ⇒ 0% (grok has no rich cache)
	SubscriptionTier   string   `json:"subscriptionTier"`   // optional
	BillingPeriodEnd   string   `json:"billingPeriodEnd"`   // RFC3339; == currentPeriod.end
	CurrentPeriod      struct {
		End  string `json:"end"`  // RFC3339 reset time
		Type string `json:"type"` // USAGE_PERIOD_TYPE_WEEKLY | USAGE_PERIOD_TYPE_MONTHLY
	} `json:"currentPeriod"`
}

// grokLogLine is the ALLOW-LIST parse of a unified.jsonl line: the timestamp (captured_at
// for the fallback reading), the msg discriminator, and the ctx.config billing object.
type grokLogLine struct {
	TS  string `json:"ts"`  // RFC3339 (e.g. "2026-07-17T03:56:06.705Z")
	Msg string `json:"msg"` // "billing: fetched credits config"
	Ctx struct {
		Config grokBillingConfig `json:"config"`
	} `json:"ctx"`
}

// ProbeGrokHome reads a grok home's identity (auth.json) and its LOCAL (fallback-tier)
// weekly usage from the LAST "billing: fetched credits config" line the CLI wrote to
// logs/unified.jsonl the last time a user ran `/usage`. This is the cache analogue of
// ProbeClaudeDir / ProbeCodexHome: it refreshes only while that home's CLI runs `/usage`,
// so the reading is stamped with the log line's timestamp and trusted at most
// VerifiedLocal, downgraded to Stale past the freshness bound; a home with identity but no
// billing line yet is returned Held (identity still populated for the dashboard). Use
// ProbeGrok for the authoritative LIVE reading. SECURITY: only non-secret fields leave the
// package; the auth secrets are read only to compute digests.
func (p *Prober) ProbeGrokHome(dir string) (*Result, error) {
	id, err := p.grokIdentity(dir)
	if err != nil {
		return nil, err
	}
	usage, capturedAt, src := p.latestGrokBillingLog(dir)
	if usage.PlanType != "" && id.Tier == "" {
		id.Tier = usage.PlanType
	}
	res := &Result{Identity: id, Usage: usage, CapturedAt: capturedAt, Source: src}
	p.stampGrokTrust(res)
	return res, nil
}

// stampGrokTrust sets TrustState for a LOCAL grok reading (mirrors claude's
// stampLocalTrust): Held when it carried no usable weekly window; else VerifiedLocal,
// downgraded to Stale past the freshness bound OR when the capture time is UNKNOWN (an
// unknown-age log line must never read as fresh forever).
func (p *Prober) stampGrokTrust(res *Result) {
	if len(res.Usage.Windows) == 0 {
		res.TrustState = TrustHeld
		res.Hold = ReasonUnrecognizedPayload
		return
	}
	res.TrustState = TrustVerifiedLocal
	if d, ok := res.Staleness(p.Clock.Now()); !ok || d > p.staleAfter() {
		res.TrustState = TrustStale
	}
}

// grokIdentity builds grok's non-secret identity (with credential/lineage digests) from
// ~/.grok/auth.json. The durable account key is the user_id (stable per account; ==
// principal_id for a User principal). Errors ReasonIdentityMissing when the file is
// absent/unparsable or carries no usable principal.
func (p *Prober) grokIdentity(dir string) (Identity, error) {
	entry, err := p.readGrokAuth(dir)
	if err != nil {
		return Identity{}, err
	}
	return grokIdentityFromEntry(entry, dir), nil
}

// grokIdentityFromEntry builds the Identity from an already-parsed auth entry, computing
// the credential (Key) and lineage (RefreshToken) digests one-way — the raw secrets stay
// in the caller's transient entry and never land on the Identity.
func grokIdentityFromEntry(entry grokAuthEntry, dir string) Identity {
	id := Identity{
		Provider:      ProviderGrok,
		AccountKey:    entry.UserID,
		Fingerprint:   fingerprint(entry.UserID),
		Email:         entry.Email,
		Org:           entry.TeamID, // grok's team is its org-equivalent
		OrgKey:        entry.TeamID,
		ConfigDir:     dir,
		AuthMode:      strings.TrimSpace(entry.AuthMode),
		PrincipalType: strings.TrimSpace(entry.PrincipalType),
		Verified:      false, // local metadata only (no network verification)
	}
	if d, ok := digest16(entry.Key); ok {
		id.CredentialDigest = d
	}
	if d, ok := digest16(entry.RefreshToken); ok {
		id.LineageDigest = d
	}
	return id
}

// readGrokAuth reads + allow-list-parses ~/.grok/auth.json and selects the logged-in
// principal. The returned entry carries the two secrets TRANSIENTLY (digest/bearer use
// only). Errors ReasonIdentityMissing on absent/unparsable/no-principal.
func (p *Prober) readGrokAuth(dir string) (grokAuthEntry, error) {
	b, err := p.FS.ReadFile(filepath.Join(dir, "auth.json"))
	if err != nil {
		return grokAuthEntry{}, held(ReasonIdentityMissing, fmt.Errorf("grok auth %q: %w", dir, err))
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(b, &raw); err != nil {
		return grokAuthEntry{}, held(ReasonIdentityMissing, fmt.Errorf("parse grok auth %q: %w", dir, err))
	}
	entry, ok := selectGrokEntry(raw)
	if !ok {
		return grokAuthEntry{}, held(ReasonIdentityMissing, fmt.Errorf("grok auth %q has no logged-in principal", dir))
	}
	if entry.UserID == "" {
		return grokAuthEntry{}, held(ReasonIdentityMissing, fmt.Errorf("grok auth %q principal has no user_id", dir))
	}
	return entry, nil
}

// selectGrokEntry picks the logged-in principal out of a parsed auth.json map. grok keys
// each principal by "<issuer>::<uuid>"; a normal home has exactly one. When more than one
// is present the lexically-first key is chosen deterministically (a single-Result API has
// nowhere to surface a second principal — the same documented single-account posture as
// resolveClaudeConfig). Keys without the "::" issuer separator are ignored.
func selectGrokEntry(raw map[string]json.RawMessage) (grokAuthEntry, bool) {
	keys := make([]string, 0, len(raw))
	for k := range raw {
		if strings.Contains(k, grokAuthIssuerSep) {
			keys = append(keys, k)
		}
	}
	if len(keys) == 0 {
		return grokAuthEntry{}, false
	}
	sort.Strings(keys)
	var e grokAuthEntry
	if err := json.Unmarshal(raw[keys[0]], &e); err != nil {
		return grokAuthEntry{}, false
	}
	return e, true
}

// latestGrokBillingLog tails logs/unified.jsonl for the LAST billing line and folds it
// into a Usage, returning also the line's timestamp and the source path. Empty Usage when
// none is found. Bounded: it reads at most grokLogTailWindow bytes from the end.
func (p *Prober) latestGrokBillingLog(dir string) (Usage, time.Time, string) {
	path := filepath.Join(dir, "logs", "unified.jsonl")
	buf, ok := p.grokTail(path, grokLogTailWindow)
	if !ok {
		return Usage{}, time.Time{}, ""
	}
	lines := bytes.Split(buf, []byte("\n"))
	for i := len(lines) - 1; i >= 0; i-- {
		line := bytes.TrimSpace(lines[i])
		if len(line) == 0 || !bytes.Contains(line, []byte(grokLogBillingMarker)) {
			continue
		}
		var ll grokLogLine
		if json.Unmarshal(line, &ll) != nil || ll.Msg != grokLogBillingMarker {
			continue
		}
		usage := grokUsageFromConfig(ll.Ctx.Config)
		if len(usage.Windows) == 0 {
			continue
		}
		return usage, parseRFC3339(ll.TS), path
	}
	return Usage{}, time.Time{}, ""
}

// grokTail reads the last window bytes of path (or the whole file if smaller), dropping a
// possibly-partial leading line when it did not start at byte 0. ok=false on any error.
func (p *Prober) grokTail(path string, window int64) ([]byte, bool) {
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
		if i := bytes.IndexByte(buf, '\n'); i >= 0 {
			buf = buf[i+1:]
		}
	}
	return buf, true
}

// grokUsageFromConfig folds a grok billing config into a Usage carrying its single
// account-wide window. The window is emitted whenever a billing period is present (even at
// 0%), so a metered account with no accrued usage is a real routable 0% — never a
// fabricated one from a truly-empty config. An out-of-range percent is rejected (no
// window) rather than mis-reported.
func grokUsageFromConfig(cfg grokBillingConfig) Usage {
	usage := Usage{PlanType: cfg.SubscriptionTier}
	if !grokConfigPresent(cfg) {
		return usage // no billing config at all ⇒ no window (unknown, never a synthesized 0)
	}
	pct := 0.0
	if cfg.CreditUsagePercent != nil {
		pct = *cfg.CreditUsagePercent
	}
	if pct < 0 || pct > 100 {
		return usage // out-of-range percent ⇒ reject the window rather than mis-report
	}
	sev := SeverityNormal
	if pct >= 100 {
		sev = SeverityCritical
	}
	usage.Windows = append(usage.Windows, LimitWindow{
		Kind:          KindWeeklyAll,
		Percent:       pct,
		Severity:      sev,
		ResetsAt:      parseRFC3339(firstNonEmptyStr(cfg.CurrentPeriod.End, cfg.BillingPeriodEnd)),
		WindowMinutes: grokPeriodMinutes(cfg.CurrentPeriod.Type),
		Active:        true,
	})
	if pct >= 100 {
		usage.RateLimited = true
	}
	return usage
}

// grokConfigPresent reports whether a parsed billing config actually carried a billing
// period — the signal that a window should be emitted. A zero-value config (no period, no
// percent, no tier) means "no billing data", which must yield NO window (unknown), not a
// synthesized 0%.
func grokConfigPresent(cfg grokBillingConfig) bool {
	return cfg.CreditUsagePercent != nil ||
		cfg.CurrentPeriod.Type != "" ||
		cfg.CurrentPeriod.End != "" ||
		cfg.BillingPeriodEnd != "" ||
		cfg.SubscriptionTier != ""
}

// grokPeriodMinutes maps grok's billing period type to a window duration (weekly=7d,
// monthly≈30d), or 0 when unknown. Both map onto the single account-wide KindWeeklyAll
// window (grok has no shorter window); the minutes are recorded for the dashboard.
func grokPeriodMinutes(periodType string) int {
	switch strings.ToUpper(strings.TrimSpace(periodType)) {
	case "USAGE_PERIOD_TYPE_WEEKLY":
		return 10080
	case "USAGE_PERIOD_TYPE_MONTHLY":
		return 43200
	}
	return 0
}

// firstNonEmptyStr returns the first non-empty string of its arguments.
func firstNonEmptyStr(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
