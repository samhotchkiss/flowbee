package acctprobe

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// grokLiveProber builds a Prober whose HTTP is the supplied fake and whose clock is fixed.
func grokLiveProber(fn func(*http.Request) (*http.Response, error)) *Prober {
	return NewWith(OSFS{}, nil, fakeHTTP{fn: fn}, nil, fakeClock())
}

// grokBillingJSON renders a live billing response body. pctField is either "" (percent
// absent → 0%) or a `"creditUsagePercent": N,` fragment.
func grokBillingJSON(pctField string) string {
	return `{"config":{` + pctField +
		`"subscriptionTier":"SUBSCRIPTION_TIER_PRO",` +
		`"currentPeriod":{"type":"USAGE_PERIOD_TYPE_WEEKLY","start":"2026-07-16T00:00:00Z","end":"2026-07-23T00:00:00Z"},` +
		`"billingPeriodEnd":"2026-07-23T00:00:00Z"}}`
}

// TestProbeGrokHomeIdentity: the LOCAL (unified.jsonl cache) tier resolves identity +
// digests and folds the last billing line's weekly window as a fresh VerifiedLocal.
func TestProbeGrokHomeIdentity(t *testing.T) {
	res, err := osProber().ProbeGrokHome(td("grok", "home_oidc"))
	if err != nil {
		t.Fatal(err)
	}
	id := res.Identity
	if id.Provider != ProviderGrok {
		t.Errorf("provider=%q want grok", id.Provider)
	}
	if id.AccountKey != "0784922d-0000-0000-0000-fakeuser0001" {
		t.Errorf("accountKey=%q want the user_id", id.AccountKey)
	}
	if id.Email != "grok-tester@example.com" {
		t.Errorf("email=%q", id.Email)
	}
	if id.Org != "d706f09b-0000-0000-0000-faketeam0001" || id.OrgKey != id.Org {
		t.Errorf("team/org=%q/%q want the team_id", id.Org, id.OrgKey)
	}
	if id.AuthMode != "oidc" || id.PrincipalType != "User" {
		t.Errorf("authMode=%q principalType=%q want oidc/User", id.AuthMode, id.PrincipalType)
	}
	if id.Fingerprint == "" || id.CredentialDigest == "" || id.LineageDigest == "" {
		t.Errorf("fingerprint/credential/lineage digests must all be set: %+v", id)
	}
	if id.CredentialDigest == id.LineageDigest {
		t.Error("credential (key) and lineage (refresh_token) digests must differ")
	}
	// fresh log line (11:45, 15m before fixedNow) ⇒ VerifiedLocal + a routable 37.5% weekly.
	if res.TrustState != TrustVerifiedLocal || !res.Routable() {
		t.Errorf("trust=%q routable=%v want verified_local/routable", res.TrustState, res.Routable())
	}
	wk, ok := res.Usage.Windows.WeeklyPct()
	if !ok || wk != 37.5 {
		t.Errorf("weekly=%v ok=%v want 37.5", wk, ok)
	}
	if len(res.Usage.Windows) != 1 || res.Usage.Windows[0].Kind != KindWeeklyAll {
		t.Errorf("grok must have exactly ONE weekly window: %+v", res.Usage.Windows)
	}
	if res.Usage.Windows[0].WindowMinutes != 10080 {
		t.Errorf("weekly window minutes=%d want 10080", res.Usage.Windows[0].WindowMinutes)
	}
	if res.Usage.Windows.Critical() {
		t.Error("37.5%% weekly must not be critical")
	}
}

// TestProbeGrokNeverReturnsSecrets: no Result field may contain either fake secret, and
// the credential digest is a 16-hex fingerprint of the key — never the key itself.
func TestProbeGrokNeverReturnsSecrets(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(td("grok", "home_oidc"), "auth.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "FAKE-SECRET-KEY") || !strings.Contains(string(raw), "FAKE-SECRET-REFRESH") {
		t.Fatal("fixture must contain the fake secrets for this guard to be meaningful")
	}
	res, err := osProber().ProbeGrokHome(td("grok", "home_oidc"))
	if err != nil {
		t.Fatal(err)
	}
	id := res.Identity
	for _, field := range []string{
		id.AccountKey, id.Email, id.Org, id.OrgKey, id.AuthMode, id.PrincipalType,
		id.CredentialDigest, id.LineageDigest, id.Model, id.Tier, id.SeatTier, res.Source,
	} {
		if strings.Contains(field, "FAKE-SECRET-KEY") || strings.Contains(field, "FAKE-SECRET-REFRESH") {
			t.Fatalf("a Result field leaked a grok secret: %q", field)
		}
	}
	if len(id.CredentialDigest) != 16 {
		t.Errorf("credential digest len=%d want 16 (a digest, not the token)", len(id.CredentialDigest))
	}
	// CredentialDigest dispatch returns the SAME digest and never the token.
	d := osProber().CredentialDigest(context.Background(), ProviderGrok, td("grok", "home_oidc"))
	if d != id.CredentialDigest || d == "" {
		t.Fatalf("CredentialDigest(grok)=%q want the identity's credential digest %q", d, id.CredentialDigest)
	}
}

func TestProbeGrokHomeMissing(t *testing.T) {
	var he *HoldError
	_, err := osProber().ProbeGrokHome(td("grok", "does-not-exist"))
	if !errors.As(err, &he) || he.Reason != ReasonIdentityMissing {
		t.Fatalf("missing grok home err=%v, want a ReasonIdentityMissing hold", err)
	}
	_, err = osProber().ProbeGrokHome(td("grok", "home_empty"))
	if !errors.As(err, &he) || he.Reason != ReasonIdentityMissing {
		t.Fatalf("empty grok auth err=%v, want a ReasonIdentityMissing hold", err)
	}
}

// TestProbeGrokLiveVerified: a live 200 yields TrustVerified with the weekly window.
func TestProbeGrokLiveVerified(t *testing.T) {
	p := grokLiveProber(func(r *http.Request) (*http.Response, error) {
		if !strings.Contains(r.URL.String(), "/billing?format=credits") {
			t.Errorf("unexpected URL %q", r.URL.String())
		}
		if got := r.Header.Get("authorization"); !strings.HasPrefix(got, "Bearer ") {
			t.Error("missing Bearer authorization header")
		}
		return httpResp(200, nil, grokBillingJSON(`"creditUsagePercent":62.0,`)), nil
	})
	res, err := p.ProbeGrokLive(context.Background(), td("grok", "home_oidc"))
	if err != nil {
		t.Fatal(err)
	}
	if res.TrustState != TrustVerified || !res.Identity.Verified {
		t.Errorf("trust=%q verified=%v want verified", res.TrustState, res.Identity.Verified)
	}
	if wk, ok := res.Usage.Windows.WeeklyPct(); !ok || wk != 62 {
		t.Errorf("weekly=%v ok=%v want 62", wk, ok)
	}
	if res.Source != "grok_billing_api" {
		t.Errorf("source=%q", res.Source)
	}
	if res.Identity.Tier != "SUBSCRIPTION_TIER_PRO" {
		t.Errorf("tier=%q want SUBSCRIPTION_TIER_PRO", res.Identity.Tier)
	}
}

// TestProbeGrokLiveAbsentPercentIsZero: an OMITTED creditUsagePercent is a real routable
// 0% (a fresh capped account), NEVER an unknown/held — the coordinator-confirmed behavior.
func TestProbeGrokLiveAbsentPercentIsZero(t *testing.T) {
	p := grokLiveProber(func(*http.Request) (*http.Response, error) {
		return httpResp(200, nil, grokBillingJSON("")), nil // no creditUsagePercent
	})
	res, err := p.ProbeGrokLive(context.Background(), td("grok", "home_oidc"))
	if err != nil {
		t.Fatal(err)
	}
	if !res.Routable() {
		t.Error("a 0% weekly reading must be routable (grok is a normal capped family)")
	}
	wk, ok := res.Usage.Windows.WeeklyPct()
	if !ok || wk != 0 {
		t.Errorf("weekly=%v ok=%v want a real 0", wk, ok)
	}
	if res.Usage.RateLimited || res.Usage.Windows.Critical() {
		t.Error("0% must be neither rate-limited nor critical")
	}
}

// TestProbeGrokLiveCriticalAt100: a full weekly window is rate-limited + critical.
func TestProbeGrokLiveCriticalAt100(t *testing.T) {
	p := grokLiveProber(func(*http.Request) (*http.Response, error) {
		return httpResp(200, nil, grokBillingJSON(`"creditUsagePercent":100.0,`)), nil
	})
	res, err := p.ProbeGrokLive(context.Background(), td("grok", "home_oidc"))
	if err != nil {
		t.Fatal(err)
	}
	if !res.Usage.RateLimited || !res.Usage.Windows.Critical() {
		t.Errorf("100%% weekly must be rate-limited + critical: %+v", res.Usage)
	}
}

// TestProbeGrokLiveExpiredTokenHeld: the expiry pre-check holds token_expired WITHOUT
// making any HTTP call (never race the CLI's OIDC refresh).
func TestProbeGrokLiveExpiredTokenHeld(t *testing.T) {
	p := grokLiveProber(func(*http.Request) (*http.Response, error) {
		t.Fatal("expired token must be caught BEFORE any HTTP call")
		return nil, nil
	})
	var he *HoldError
	_, err := p.ProbeGrokLive(context.Background(), td("grok", "home_expired"))
	if !errors.As(err, &he) || he.Reason != ReasonTokenExpired {
		t.Fatalf("err=%v want a token_expired hold", err)
	}
}

// TestProbeGrokLive401Rejected / 429Throttled: HTTP status → typed holds.
func TestProbeGrokLive401Rejected(t *testing.T) {
	p := grokLiveProber(func(*http.Request) (*http.Response, error) {
		return httpResp(401, nil, "unauthorized"), nil
	})
	var he *HoldError
	_, err := p.ProbeGrokLive(context.Background(), td("grok", "home_oidc"))
	if !errors.As(err, &he) || he.Reason != ReasonTokenRejected {
		t.Fatalf("err=%v want token_rejected", err)
	}
}

func TestProbeGrokLive429Throttled(t *testing.T) {
	p := grokLiveProber(func(*http.Request) (*http.Response, error) {
		return httpResp(429, map[string]string{"Retry-After": "120"}, "slow down"), nil
	})
	var he *HoldError
	_, err := p.ProbeGrokLive(context.Background(), td("grok", "home_oidc"))
	if !errors.As(err, &he) || he.Reason != ReasonThrottled {
		t.Fatalf("err=%v want throttled", err)
	}
	if he.RetryAt.IsZero() || !he.RetryAt.After(fixedNow) {
		t.Errorf("throttle must carry a forward RetryAt, got %v", he.RetryAt)
	}
}

// TestProbeGrokFallsBackToCache: a throttled live tier serves the unified.jsonl cache
// reading, stamped with the reason live was unavailable (VerifiedLocal, not Held).
func TestProbeGrokFallsBackToCache(t *testing.T) {
	p := grokLiveProber(func(*http.Request) (*http.Response, error) {
		return httpResp(429, map[string]string{"Retry-After": "30"}, "slow down"), nil
	})
	res, err := p.ProbeGrok(context.Background(), td("grok", "home_oidc"))
	if err != nil {
		t.Fatal(err)
	}
	if res.TrustState != TrustVerifiedLocal {
		t.Errorf("trust=%q want verified_local (served the cache)", res.TrustState)
	}
	if res.LiveUnavailableReason != ReasonThrottled {
		t.Errorf("liveUnavailableReason=%q want throttled", res.LiveUnavailableReason)
	}
	if wk, ok := res.Usage.Windows.WeeklyPct(); !ok || wk != 37.5 {
		t.Errorf("cache weekly=%v ok=%v want 37.5", wk, ok)
	}
	if res.Hold != "" {
		t.Errorf("a served-cache result must not carry a Hold, got %q", res.Hold)
	}
}

// TestGrokUsageFromConfig unit-tests the shared fold: weekly/monthly minutes, absent→0,
// out-of-range reject, and an empty config → no window (unknown, never a synthesized 0).
func TestGrokUsageFromConfig(t *testing.T) {
	pct := func(v float64) *float64 { return &v }
	weekly := grokBillingConfig{CreditUsagePercent: pct(20), CurrentPeriod: struct {
		End  string `json:"end"`
		Type string `json:"type"`
	}{End: "2026-07-23T00:00:00Z", Type: "USAGE_PERIOD_TYPE_WEEKLY"}}
	u := grokUsageFromConfig(weekly)
	if len(u.Windows) != 1 || u.Windows[0].WindowMinutes != 10080 || u.Windows[0].Percent != 20 {
		t.Fatalf("weekly fold: %+v", u.Windows)
	}
	monthly := weekly
	monthly.CurrentPeriod.Type = "USAGE_PERIOD_TYPE_MONTHLY"
	if u := grokUsageFromConfig(monthly); u.Windows[0].WindowMinutes != 43200 || u.Windows[0].Kind != KindWeeklyAll {
		t.Fatalf("monthly fold: %+v", u.Windows)
	}
	// absent percent ⇒ a real 0% window (config present via the period).
	noPct := grokBillingConfig{CurrentPeriod: weekly.CurrentPeriod}
	if u := grokUsageFromConfig(noPct); len(u.Windows) != 1 || u.Windows[0].Percent != 0 {
		t.Fatalf("absent-percent fold must be a real 0%% window: %+v", u.Windows)
	}
	// truly-empty config ⇒ NO window (unknown, not a synthesized 0).
	if u := grokUsageFromConfig(grokBillingConfig{}); len(u.Windows) != 0 {
		t.Fatalf("empty config must yield no window: %+v", u.Windows)
	}
	// out-of-range percent ⇒ rejected.
	bad := weekly
	bad.CreditUsagePercent = pct(150)
	if u := grokUsageFromConfig(bad); len(u.Windows) != 0 {
		t.Fatalf("out-of-range percent must be rejected: %+v", u.Windows)
	}
}
