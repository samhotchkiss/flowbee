package acctprobe

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
)

// grokBillingBaseEnv overrides the cli-chat-proxy base URL (default grokBillingBaseDefault),
// so a test / a future region split can point the probe elsewhere. The path
// "/billing?format=credits" is appended to it.
const grokBillingBaseEnv = "GROK_CLI_CHAT_PROXY_BASE_URL"

// grokBillingBaseDefault is the default cli-chat-proxy base (live-verified: a bare Bearer
// on GET <base>/billing?format=credits returns the weekly billing config).
const grokBillingBaseDefault = "https://cli-chat-proxy.grok.com/v1"

// grokBillingURL builds the live weekly-usage endpoint from the (optionally overridden)
// base. Trailing slashes on the base are tolerated.
func grokBillingURL() string {
	base := strings.TrimSpace(os.Getenv(grokBillingBaseEnv))
	if base == "" {
		base = grokBillingBaseDefault
	}
	return strings.TrimRight(base, "/") + "/billing?format=credits"
}

// ProbeGrokLive reads an account's REAL weekly usage LIVE from grok's cli-chat-proxy
// billing endpoint using that account's own Bearer key (the same call the grok TUI's
// `/usage` makes). This is the authoritative tier; on success TrustState is Verified.
//
// It returns a *HoldError with a typed reason for every non-success a caller must react to
// (token_expired / token_rejected on 401/403 / throttled on 429 with a carry-forward
// RetryAt / unrecognized_payload / credentials_missing). A pure transport failure returns
// a non-HoldError so ProbeGrok can fall back to the unified.jsonl cache. SECURITY: the
// bearer key is read transiently, used only for the Authorization header + the credential
// digest, and never leaves this function; the shared HTTP client refuses redirects so the
// bearer can never be forwarded to a redirect target.
func (p *Prober) ProbeGrokLive(ctx context.Context, dir string) (*Result, error) {
	entry, err := p.readGrokAuth(dir)
	if err != nil {
		return nil, err // ReasonIdentityMissing
	}
	id := grokIdentityFromEntry(entry, dir)
	if entry.Key == "" {
		return nil, held(ReasonCredentialsMissing, errors.New("no grok bearer key in auth.json"))
	}
	// Expiry pre-check: NEVER refresh (racing the CLI's OIDC rotation invalidates its
	// session — the Claude/Codex posture). grok's bearer is short-lived (~hours).
	if exp := parseRFC3339(entry.ExpiresAt); !exp.IsZero() && !exp.After(p.Clock.Now()) {
		return nil, held(ReasonTokenExpired, errors.New("cached grok token expired"))
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, grokBillingURL(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("authorization", "Bearer "+entry.Key)

	resp, err := p.HTTP.Do(req)
	if err != nil {
		// transport failure: not a typed hold — let ProbeGrok fall back to the cache.
		return nil, fmt.Errorf("grok billing request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	switch {
	case resp.StatusCode == http.StatusOK:
		// proceed
	case resp.StatusCode == http.StatusTooManyRequests:
		return nil, &HoldError{Reason: ReasonThrottled, RetryAt: p.retryAfter(resp.Header)}
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return nil, held(ReasonTokenRejected, fmt.Errorf("grok billing HTTP %d", resp.StatusCode))
	default:
		return nil, held(ReasonUnrecognizedPayload, fmt.Errorf("grok billing HTTP %d", resp.StatusCode))
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, held(ReasonUnrecognizedPayload, fmt.Errorf("read grok billing body: %w", err))
	}
	var br grokBillingResponse
	if err := json.Unmarshal(body, &br); err != nil {
		return nil, held(ReasonUnrecognizedPayload, fmt.Errorf("parse grok billing body: %w", err))
	}
	usage := grokUsageFromConfig(br.Config)
	if len(usage.Windows) == 0 {
		return nil, held(ReasonUnrecognizedPayload, errors.New("grok billing carried no weekly window"))
	}
	if usage.PlanType != "" && id.Tier == "" {
		id.Tier = usage.PlanType
	}

	id.Verified = true
	return &Result{
		Identity:   id,
		Usage:      usage,
		TrustState: TrustVerified,
		CapturedAt: p.Clock.Now().UTC(),
		Source:     "grok_billing_api",
	}, nil
}

// ProbeGrok is the tiered read: LIVE first (authoritative), falling back to the on-disk
// unified.jsonl cache only when the live meter is unreachable/throttled or the cached
// token is expired/rejected — cases where the cache is the best available signal but must
// be trusted only as VerifiedLocal/Stale. A typed hold a fallback cannot improve
// (identity missing, unrecognized) is returned as-is. Mirrors ProbeClaude.
func (p *Prober) ProbeGrok(ctx context.Context, dir string) (*Result, error) {
	res, err := p.ProbeGrokLive(ctx, dir)
	if err == nil {
		return res, nil
	}
	var hold *HoldError
	if errors.As(err, &hold) {
		switch hold.Reason {
		case ReasonThrottled, ReasonTokenExpired, ReasonTokenRejected, ReasonCredentialsMissing:
			// live meter unusable right now — serve the cache, recording WHY live was
			// unavailable (never Hold, reserved for a TrustHeld result).
			if cached, cerr := p.ProbeGrokHome(dir); cerr == nil {
				cached.LiveUnavailableReason = hold.Reason
				cached.RetryAt = hold.RetryAt
				return cached, nil
			}
			return nil, err
		default:
			// identity_missing / unrecognized: a fallback cannot make these trustworthy.
			return nil, err
		}
	}
	// transport failure: fall back to the cache.
	if cached, cerr := p.ProbeGrokHome(dir); cerr == nil {
		return cached, nil
	}
	return nil, err
}
