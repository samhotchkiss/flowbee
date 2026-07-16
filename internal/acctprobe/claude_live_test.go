package acctprobe

import (
	"context"
	"errors"
	"net/http"
	"runtime"
	"testing"
	"time"
)

const liveLimitsBody = `{"limits":[
 {"kind":"session","percent":24,"severity":"normal","resets_at":"2026-07-16T23:50:00+00:00","is_active":true},
 {"kind":"weekly_all","percent":40,"severity":"normal","resets_at":"2026-07-22T00:00:00+00:00","is_active":false},
 {"kind":"weekly_scoped","percent":5,"severity":"normal","resets_at":"2026-07-22T00:00:00+00:00","scope":{"model":{"display_name":"Fable"}},"is_active":false}
]}`

func liveProber(doer HTTPDoer) *Prober {
	return NewWith(OSFS{}, fakeExec{fn: func(context.Context, string, ...string) ([]byte, error) {
		return nil, errors.New("exec must not run in this test")
	}}, doer, nil, fakeClock())
}

func TestProbeClaudeLiveVerified(t *testing.T) {
	var gotReq *http.Request
	doer := fakeHTTP{fn: func(r *http.Request) (*http.Response, error) {
		gotReq = r
		return httpResp(http.StatusOK,
			map[string]string{"anthropic-organization-id": "org-live-response-xyz"},
			liveLimitsBody), nil
	}}
	res, err := liveProber(doer).ProbeClaudeLive(context.Background(), td("claude", "live_dir"), "")
	if err != nil {
		t.Fatal(err)
	}
	if res.TrustState != TrustVerified || !res.Identity.Verified {
		t.Errorf("trust=%v verified=%v want verified", res.TrustState, res.Identity.Verified)
	}
	if res.Source != "anthropic_usage_api" {
		t.Errorf("source=%q", res.Source)
	}
	if s, ok := res.Usage.Windows.SessionPct(); !ok || s != 24 {
		t.Errorf("session=%v ok=%v want 24", s, ok)
	}
	if wk, ok := res.Usage.Windows.WeeklyPct(); !ok || wk != 40 {
		t.Errorf("weekly=%v ok=%v want 40", wk, ok)
	}
	if res.UsageOrgFingerprint != fingerprint("org-live-response-xyz") {
		t.Errorf("usage org fingerprint=%q", res.UsageOrgFingerprint)
	}
	// credential digest bound; the raw token must never be it.
	if res.Identity.CredentialDigest == "" {
		t.Error("credential digest should be set from the token")
	}
	// request carries the three required headers and the bearer.
	if got := gotReq.Header.Get("authorization"); got != "Bearer FAKE-live-access-token-not-a-real-secret" {
		t.Errorf("authorization header=%q", got)
	}
	if gotReq.Header.Get("anthropic-beta") != "oauth-2025-04-20" ||
		gotReq.Header.Get("anthropic-version") != "2023-06-01" {
		t.Errorf("missing anthropic headers: %v", gotReq.Header)
	}
}

func TestProbeClaudeLiveLegacyBody(t *testing.T) {
	body := `{"five_hour":{"utilization":12,"resets_at":"2026-07-16T20:00:00+00:00"},"seven_day":{"utilization":33,"resets_at":"2026-07-22T00:00:00+00:00"}}`
	doer := fakeHTTP{fn: func(*http.Request) (*http.Response, error) {
		return httpResp(http.StatusOK, map[string]string{"anthropic-organization-id": "org-x"}, body), nil
	}}
	res, err := liveProber(doer).ProbeClaudeLive(context.Background(), td("claude", "live_dir"), "")
	if err != nil {
		t.Fatal(err)
	}
	if s, ok := res.Usage.Windows.SessionPct(); !ok || s != 12 {
		t.Errorf("legacy session=%v ok=%v want 12", s, ok)
	}
	if wk, ok := res.Usage.Windows.WeeklyPct(); !ok || wk != 33 {
		t.Errorf("legacy weekly=%v ok=%v want 33", wk, ok)
	}
}

func TestProbeClaudeLiveErrors(t *testing.T) {
	dir := td("claude", "live_dir")
	cases := []struct {
		name       string
		status     int
		headers    map[string]string
		pin        string
		wantReason HoldReason
	}{
		{"throttled 429", http.StatusTooManyRequests, map[string]string{"Retry-After": "120"}, "", ReasonThrottled},
		{"rejected 401", http.StatusUnauthorized, nil, "", ReasonTokenRejected},
		{"rejected 403", http.StatusForbidden, nil, "", ReasonTokenRejected},
		{"org unverifiable", http.StatusOK, nil, "", ReasonOrgUnverifiable},
		{"org changed", http.StatusOK, map[string]string{"anthropic-organization-id": "org-different"}, "some-other-pin", ReasonOrgChanged},
		{"other status", http.StatusInternalServerError, nil, "", ReasonUnrecognizedPayload},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			doer := fakeHTTP{fn: func(*http.Request) (*http.Response, error) {
				return httpResp(c.status, c.headers, liveLimitsBody), nil
			}}
			_, err := liveProber(doer).ProbeClaudeLive(context.Background(), dir, c.pin)
			var hold *HoldError
			if !errors.As(err, &hold) || hold.Reason != c.wantReason {
				t.Fatalf("err=%v want reason %s", err, c.wantReason)
			}
			if c.wantReason == ReasonThrottled && hold.RetryAt != fixedNow.Add(120*time.Second) {
				t.Errorf("RetryAt=%v want now+120s", hold.RetryAt)
			}
		})
	}
}

func TestProbeClaudeLiveExpiredTokenNoNetwork(t *testing.T) {
	doer := fakeHTTP{fn: func(*http.Request) (*http.Response, error) {
		t.Fatal("HTTP must not be called for an already-expired token")
		return nil, nil
	}}
	_, err := liveProber(doer).ProbeClaudeLive(context.Background(), td("claude", "expired_dir"), "")
	var hold *HoldError
	if !errors.As(err, &hold) || hold.Reason != ReasonTokenExpired {
		t.Fatalf("err=%v want token_expired", err)
	}
}

func TestProbeClaudeTieredFallsBackToCacheOnThrottle(t *testing.T) {
	// live throttled -> ProbeClaude serves the on-disk cache, tagged with the reason.
	doer := fakeHTTP{fn: func(*http.Request) (*http.Response, error) {
		return httpResp(http.StatusTooManyRequests, map[string]string{"Retry-After": "60"}, ""), nil
	}}
	p := liveProber(doer)
	res, err := p.ProbeClaude(context.Background(), td("claude", "live_dir"), "")
	if err != nil {
		t.Fatal(err)
	}
	if res.TrustState == TrustVerified {
		t.Error("a throttled-then-cached read must not claim Verified")
	}
	if res.TrustState != TrustVerifiedLocal {
		t.Errorf("fresh cache fallback should be verified_local, got %v", res.TrustState)
	}
	if res.Hold != ReasonThrottled {
		t.Errorf("cache carry should record why live was unavailable, got %q", res.Hold)
	}
	if res.Identity.Email != "live@example.com" {
		t.Errorf("cache identity wrong: %q", res.Identity.Email)
	}
	if wk, ok := res.Usage.Windows.WeeklyPct(); !ok || wk != 21 {
		t.Errorf("cache weekly=%v ok=%v want 21", wk, ok)
	}
}

// ── credential access (Keychain / .credentials.json) ──

func TestClaudeOAuthPrefersFileOverKeychain(t *testing.T) {
	p := NewWith(OSFS{}, fakeExec{fn: func(context.Context, string, ...string) ([]byte, error) {
		t.Fatal("keychain must not be consulted when .credentials.json is present")
		return nil, nil
	}}, nil, nil, fakeClock())
	oauth, err := p.claudeOAuthFor(context.Background(), td("claude", "live_dir"))
	if err != nil {
		t.Fatal(err)
	}
	if oauth.AccessToken != "FAKE-live-access-token-not-a-real-secret" {
		t.Errorf("token=%q", oauth.AccessToken)
	}
}

func TestClaudeKeychainNamespacedBeforeLegacy(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("keychain path is darwin-only")
	}
	dir := t.TempDir() // no .credentials.json here -> forces the keychain path
	namespaced := claudeKeychainService(dir)
	var calls []string
	p := NewWith(OSFS{}, fakeExec{fn: func(_ context.Context, name string, args ...string) ([]byte, error) {
		if name != "security" {
			t.Fatalf("unexpected exec %q", name)
		}
		svc := serviceArg(args)
		calls = append(calls, svc)
		if svc == namespaced {
			return []byte(`{"claudeAiOauth":{"accessToken":"ns-tok"}}`), nil
		}
		return nil, errors.New("exit status 44")
	}}, nil, nil, fakeClock())

	oauth, err := p.claudeOAuthFor(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	if oauth.AccessToken != "ns-tok" {
		t.Errorf("token=%q want ns-tok", oauth.AccessToken)
	}
	if len(calls) == 0 || calls[0] != namespaced {
		t.Errorf("first keychain probe=%v want the namespaced service first", calls)
	}
}

func TestClaudeKeychainServiceDerivation(t *testing.T) {
	a := claudeKeychainService("/Users/x/homes/a")
	b := claudeKeychainService("/Users/x/homes/b")
	if a == b {
		t.Error("distinct dirs must derive distinct services")
	}
	if len(a) != len(claudeKeychainServiceBase)+1+8 {
		t.Errorf("service %q not base + '-' + 8 hex", a)
	}
	if claudeKeychainService("") != "" {
		t.Error("empty dir -> empty service")
	}
}

func TestNoRedirectClient(t *testing.T) {
	c := newNoRedirectClient(0)
	if c.CheckRedirect == nil {
		t.Fatal("CheckRedirect must be set to forbid following redirects")
	}
	if err := c.CheckRedirect(nil, nil); err != http.ErrUseLastResponse {
		t.Errorf("CheckRedirect=%v want ErrUseLastResponse (never forward the bearer)", err)
	}
}

// serviceArg returns the value following "-s" in a security argv.
func serviceArg(args []string) string {
	for i, a := range args {
		if a == "-s" && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}
