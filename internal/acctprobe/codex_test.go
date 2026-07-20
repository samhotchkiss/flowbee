package acctprobe

import (
	"strings"
	"testing"
	"time"
)

func TestProbeCodexHomeDisplayOnly(t *testing.T) {
	res, err := osProber().ProbeCodexHome(td("codex", "home_chatgpt"))
	if err != nil {
		t.Fatal(err)
	}
	if res.TrustState != TrustDisplayOnly {
		t.Errorf("trust=%v want display_only (on-disk telemetry is never routable)", res.TrustState)
	}
	if res.Routable() {
		t.Error("display-only telemetry must not be routable")
	}
	if res.Identity.AccountKey != "codex-acct-0000-0000-000000000009" {
		t.Errorf("accountKey=%q", res.Identity.AccountKey)
	}
	if res.Identity.AuthMode != "chatgpt" {
		t.Errorf("authMode=%q want chatgpt", res.Identity.AuthMode)
	}
	if res.Identity.Model != "gpt-5.6-sol" {
		t.Errorf("model=%q", res.Identity.Model)
	}
	if res.Identity.Fingerprint == "" || res.Identity.CredentialDigest == "" || res.Identity.LineageDigest == "" {
		t.Errorf("digests/fingerprint should all be set: %+v", res.Identity)
	}
	if res.Identity.CredentialDigest == res.Identity.LineageDigest {
		t.Error("credential (access) and lineage (refresh) digests must differ")
	}
	// LAST token_count wins: weekly 39, not the earlier 30. secondary is null -> no session.
	if wk, ok := res.Usage.Windows.WeeklyPct(); !ok || wk != 39 {
		t.Errorf("weekly=%v ok=%v want 39", wk, ok)
	}
	if _, ok := res.Usage.Windows.SessionPct(); ok {
		t.Error("no session window should be present (secondary was null)")
	}
	// bucketed as weekly by the 10080 duration.
	if res.Usage.Windows[0].WindowMinutes != 10080 || res.Usage.Windows[0].Kind != KindWeeklyAll {
		t.Errorf("window=%+v want 10080/weekly_all", res.Usage.Windows[0])
	}
	if res.Usage.PlanType != "pro" || res.Usage.CreditBalance != "0" {
		t.Errorf("plan=%q balance=%q", res.Usage.PlanType, res.Usage.CreditBalance)
	}
	if res.Usage.TotalTokens != 15410 || res.Usage.ContextWindow != 258400 {
		t.Errorf("tokens=%d ctx=%d", res.Usage.TotalTokens, res.Usage.ContextWindow)
	}
	// capture time comes from the event, and the source is the 16:00 file (the newer
	// 18:00 rollout has no token_count, so the scan falls through to it).
	if !res.CapturedAt.Equal(time.Date(2026, 7, 16, 16, 5, 0, 0, time.UTC)) {
		t.Errorf("capturedAt=%v want 16:05:00Z", res.CapturedAt)
	}
	if !strings.Contains(res.Source, "16-00-00") {
		t.Errorf("source=%q want the 16:00 rollout (fresh 18:00 file has no token_count)", res.Source)
	}
}

func TestProbeCodexApikeyIdentity(t *testing.T) {
	res, err := osProber().ProbeCodexHome(td("codex", "home_apikey"))
	if err != nil {
		t.Fatal(err)
	}
	if res.Identity.AuthMode != "apikey" {
		t.Errorf("authMode=%q want apikey", res.Identity.AuthMode)
	}
	if len(res.Usage.Windows) != 0 {
		t.Errorf("apikey home has no sessions -> no windows, got %d", len(res.Usage.Windows))
	}
}

func TestCodexModelParsedFromConfig(t *testing.T) {
	if m := osProber().codexModel(td("codex", "home_chatgpt")); m != "gpt-5.6-sol" {
		t.Errorf("model=%q", m)
	}
	if m := osProber().codexModel(td("codex", "home_apikey")); m != "" {
		t.Errorf("no config.toml -> empty model, got %q", m)
	}
}
