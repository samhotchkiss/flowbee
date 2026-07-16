package acctprobe

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// claudeUsageURL is the same OAuth usage endpoint the Claude Code UI calls.
const claudeUsageURL = "https://api.anthropic.com/api/oauth/usage"

// claudeKeychainServiceBase is the macOS login-Keychain service the Claude CLI
// stores its OAuth token under. Current builds NAMESPACE it per config dir:
// base + "-" + hex(sha256(NFC(config_dir)))[:8]. With no CLAUDE_CONFIG_DIR the CLI
// uses the bare base (legacy shared item).
const claudeKeychainServiceBase = "Claude Code-credentials"

// claudeOAuth is the ALLOW-LIST parse of the credential blob (from
// `.credentials.json` or the Keychain item). AccessToken is a TRANSIENT secret used
// only to build a request/digest and is NEVER stored on any exported type or logged.
type claudeOAuth struct {
	AccessToken      string   `json:"accessToken"`
	ExpiresAt        *float64 `json:"expiresAt"` // ms (tolerate seconds); pointer: absent ≠ 0
	SubscriptionType string   `json:"subscriptionType"`
}

// claudeCredsFile is the `.credentials.json` wrapper.
type claudeCredsFile struct {
	ClaudeAiOauth *claudeOAuth `json:"claudeAiOauth"`
}

// ProbeClaudeLive reads an account's REAL usage LIVE from the Anthropic OAuth usage
// endpoint using that account's own token, and binds the response to the login via
// the anthropic-organization-id header (trust-on-first-use: pass the previously
// pinned fingerprint as pinnedOrgFP, "" on the first call). This is the
// authoritative tier; on success TrustState is Verified.
//
// It returns a *HoldError with a typed reason for every non-success the caller must
// react to specifically (token_expired / token_rejected / throttled / org_* /
// credentials_missing / unrecognized_payload). A pure transport failure (endpoint
// unreachable) returns a non-HoldError so ProbeClaude can fall back to the cache.
// SECURITY: the token is used only for the Authorization header and the credential
// digest; it never leaves this function.
func (p *Prober) ProbeClaudeLive(ctx context.Context, dir, pinnedOrgFP string) (*Result, error) {
	// Identity from local metadata (no network). Fingerprint is the per-ACCOUNT
	// fingerprint (accountUuid) — org-level binding is UsageOrgFingerprint below.
	cfg, _, cerr := p.resolveClaudeConfig(dir)
	if cerr != nil {
		return nil, held(ReasonIdentityMissing, fmt.Errorf("claude live %q: %w", dir, cerr))
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
	}

	oauth, err := p.claudeOAuthFor(ctx, dir)
	if err != nil {
		return nil, held(ReasonCredentialsMissing, err)
	}
	if oauth.AccessToken == "" {
		return nil, held(ReasonCredentialsMissing, errors.New("no Claude access token available"))
	}
	// Expiry pre-check: NEVER refresh (racing the CLI's rotation invalidates its
	// session). expiresAt is ms in current builds; tolerate plain seconds so a unit
	// change can't mark every fresh token as expired. A mistyped value is not proof.
	if oauth.ExpiresAt != nil {
		exp := *oauth.ExpiresAt
		if exp > 1e11 {
			exp /= 1000.0
		}
		if exp <= float64(p.Clock.Now().Unix()) {
			return nil, held(ReasonTokenExpired, errors.New("cached Claude token expired"))
		}
	}
	if d, ok := digest16(oauth.AccessToken); ok {
		id.CredentialDigest = d
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, claudeUsageURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("authorization", "Bearer "+oauth.AccessToken)
	req.Header.Set("anthropic-beta", "oauth-2025-04-20")
	req.Header.Set("anthropic-version", "2023-06-01")

	resp, err := p.HTTP.Do(req)
	if err != nil {
		// transport failure: not a typed hold — let ProbeClaude fall back to cache.
		return nil, fmt.Errorf("claude usage request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	switch {
	case resp.StatusCode == http.StatusOK:
		// proceed
	case resp.StatusCode == http.StatusTooManyRequests:
		return nil, &HoldError{Reason: ReasonThrottled, RetryAt: p.retryAfter(resp.Header)}
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return nil, held(ReasonTokenRejected, fmt.Errorf("claude usage HTTP %d", resp.StatusCode))
	default:
		return nil, held(ReasonUnrecognizedPayload, fmt.Errorf("claude usage HTTP %d", resp.StatusCode))
	}

	// Bind the response to the login by org header (require it on every response —
	// without it the usage cannot be attributed to this account at all).
	respOrg := resp.Header.Get("anthropic-organization-id")
	respFP := fingerprint(respOrg)
	if respFP == "" {
		return nil, held(ReasonOrgUnverifiable, errors.New("usage response missing anthropic-organization-id"))
	}
	if pinnedOrgFP != "" && respFP != pinnedOrgFP {
		return nil, held(ReasonOrgChanged, fmt.Errorf("usage org fingerprint changed"))
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, held(ReasonUnrecognizedPayload, fmt.Errorf("read usage body: %w", err))
	}
	var util claudeUtilization
	if err := json.Unmarshal(body, &util); err != nil {
		return nil, held(ReasonUnrecognizedPayload, fmt.Errorf("parse usage body: %w", err))
	}
	usage := claudeUsage(util)
	if len(usage.Windows) == 0 {
		return nil, held(ReasonUnrecognizedPayload, errors.New("usage response carried no window"))
	}

	id.Verified = true
	return &Result{
		Identity:            id,
		Usage:               usage,
		TrustState:          TrustVerified,
		CapturedAt:          p.Clock.Now().UTC(),
		Source:              "anthropic_usage_api",
		UsageOrgFingerprint: respFP,
	}, nil
}

// ProbeClaude is the tiered read: LIVE first (authoritative), falling back to the
// on-disk CACHE only when the live meter is unreachable/throttled or the cached
// token is expired/rejected — cases where the cache is the best available signal but
// must be trusted only as VerifiedLocal/Stale. A typed hold that a fallback cannot
// improve (org changed, credentials missing) is returned as-is. The returned
// pinnedOrgFP is threaded back on the next call for trust-on-first-use.
func (p *Prober) ProbeClaude(ctx context.Context, dir, pinnedOrgFP string) (*Result, error) {
	res, err := p.ProbeClaudeLive(ctx, dir, pinnedOrgFP)
	if err == nil {
		return res, nil
	}
	var hold *HoldError
	if errors.As(err, &hold) {
		switch hold.Reason {
		case ReasonThrottled, ReasonTokenExpired, ReasonTokenRejected, ReasonCredentialsMissing:
			// live meter unusable right now — serve the cache, recording WHY live was
			// unavailable in the diagnostic field (never Hold, which is reserved for a
			// TrustHeld result; the cache reading here is VerifiedLocal/Stale).
			if cached, cerr := p.ProbeClaudeDir(dir); cerr == nil {
				cached.LiveUnavailableReason = hold.Reason
				cached.RetryAt = hold.RetryAt
				return cached, nil
			}
			return nil, err
		default:
			// org_changed / org_unverifiable / identity_missing / unrecognized:
			// a fallback cannot make these trustworthy — hold.
			return nil, err
		}
	}
	// transport failure: fall back to the cache.
	if cached, cerr := p.ProbeClaudeDir(dir); cerr == nil {
		return cached, nil
	}
	return nil, err
}

// claudeOAuthFor returns the credential the Claude CLI would use for dir:
// `.credentials.json` (Linux/Windows, or an isolated CLAUDE_CONFIG_DIR home)
// preferred, else the macOS login Keychain (namespaced item first, legacy shared
// item as fallback). The returned token is a transient secret.
func (p *Prober) claudeOAuthFor(ctx context.Context, dir string) (claudeOAuth, error) {
	if b, err := p.FS.ReadFile(filepath.Join(dir, ".credentials.json")); err == nil {
		var f claudeCredsFile
		if json.Unmarshal(b, &f) == nil && f.ClaudeAiOauth != nil && f.ClaudeAiOauth.AccessToken != "" {
			return *f.ClaudeAiOauth, nil
		}
	}
	if runtime.GOOS == "darwin" {
		if oauth, ok := p.claudeKeychainOAuth(ctx, dir); ok {
			return oauth, nil
		}
	}
	return claudeOAuth{}, errors.New("no Claude credential found (file or Keychain)")
}

// claudeKeychainOAuth reads the credential blob out of the macOS login Keychain via
// `security find-generic-password -s <service> -w`. It tries the per-dir NAMESPACED
// item; the legacy SHARED item ("Claude Code-credentials") is consulted ONLY when dir
// is the OS-default ~/.claude. A non-default CLAUDE_CONFIG_DIR with no namespaced item
// must NOT borrow the default account's shared token (that would attribute the default
// account's live usage to a different Identity, verified and routable, undetectable by
// the org pin). Returns ok=false on any error so callers fail closed.
func (p *Prober) claudeKeychainOAuth(ctx context.Context, dir string) (claudeOAuth, bool) {
	services := []string{claudeKeychainService(dir)}
	if isDefaultClaudeDir(dir) {
		services = append(services, claudeKeychainServiceBase)
	}
	seen := map[string]bool{}
	for _, svc := range services {
		if svc == "" || seen[svc] {
			continue
		}
		seen[svc] = true
		out, err := p.Exec.Output(ctx, "security", "find-generic-password", "-s", svc, "-w")
		if err != nil {
			continue
		}
		raw := strings.TrimSpace(string(out))
		if raw == "" {
			continue
		}
		// The item stores {"claudeAiOauth": {...}}; tolerate a bare credential too.
		var wrapped claudeCredsFile
		if json.Unmarshal([]byte(raw), &wrapped) == nil && wrapped.ClaudeAiOauth != nil && wrapped.ClaudeAiOauth.AccessToken != "" {
			return *wrapped.ClaudeAiOauth, true
		}
		var bare claudeOAuth
		if json.Unmarshal([]byte(raw), &bare) == nil && bare.AccessToken != "" {
			return bare, true
		}
	}
	return claudeOAuth{}, false
}

// claudeKeychainService derives the Keychain service name for a config dir. NOTE
// (divergence from headroom): headroom NFC-normalizes the path before hashing;
// this port hashes the raw path bytes to avoid a new module dependency. For ASCII
// paths — the overwhelming norm for config dirs — NFC is the identity, so the digest
// is identical; a non-NFC non-ASCII path would miss the namespaced item and fall
// back to the legacy item (a fail-closed degradation, never a wrong-account read).
func claudeKeychainService(dir string) string {
	if dir == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(dir))
	return claudeKeychainServiceBase + "-" + hex.EncodeToString(sum[:])[:8]
}

// isDefaultClaudeDir reports whether dir is the OS-default Claude config dir
// (~/.claude) — the only dir whose token may live in the legacy SHARED Keychain item.
// Uses os.UserHomeDir because the Keychain path is inherently local (darwin-only).
func isDefaultClaudeDir(dir string) bool {
	home, err := os.UserHomeDir()
	if err != nil {
		return false
	}
	return filepath.Clean(dir) == filepath.Join(home, ".claude")
}

// retryAfter computes the provider retry instant from a 429's Retry-After header
// (delta-seconds or an HTTP-date), defaulting to now+5m when absent/unparseable.
func (p *Prober) retryAfter(h http.Header) time.Time {
	now := p.Clock.Now()
	raw := strings.TrimSpace(h.Get("Retry-After"))
	if raw != "" {
		if secs, err := strconv.Atoi(raw); err == nil && secs >= 0 {
			return now.Add(time.Duration(secs) * time.Second)
		}
		if t, err := http.ParseTime(raw); err == nil {
			if t.After(now) {
				return t
			}
		}
	}
	return now.Add(5 * time.Minute)
}

// CredentialDigest returns the digest of the token the given provider's CLI would
// currently use for dir, for a caller's carry-forward/TOCTOU comparison against a
// prior reading's Identity.CredentialDigest. It reads the secret transiently and
// returns only the digest ("" when unreadable). Never logs or returns the token.
func (p *Prober) CredentialDigest(ctx context.Context, provider Provider, dir string) string {
	switch provider {
	case ProviderClaude:
		if oauth, err := p.claudeOAuthFor(ctx, dir); err == nil {
			if d, ok := digest16(oauth.AccessToken); ok {
				return d
			}
		}
	case ProviderCodex:
		if tok, ok := p.codexAccessToken(dir); ok {
			if d, ok := digest16(tok); ok {
				return d
			}
		}
	}
	return ""
}
