package acctprobe

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/samhotchkiss/flowbee/internal/clock"
)

// defaultFetchedMs matches testdata/claude/home_default/.claude.json fetchedAtMs.
const defaultFetchedMs = 1784200000000

func TestDiscoverClaudeDirs(t *testing.T) {
	p := osProber()
	home := td("claude", "home_default")
	dirs, err := p.DiscoverClaudeDirs(home)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		filepath.Join(home, ".claude"),
		filepath.Join(home, ".claude-work-example"),
	}
	if len(dirs) != len(want) {
		t.Fatalf("discovered %v want %v", dirs, want)
	}
	for i := range want {
		if dirs[i] != want[i] {
			t.Errorf("dir[%d]=%q want %q", i, dirs[i], want[i])
		}
	}
}

func TestProbeClaudeDefaultViaLegacySibling(t *testing.T) {
	// dir=<home>/.claude has only a stub inner .claude.json; the probe must fall
	// back to the legacy sibling <home>/.claude.json for the real account.
	now := time.UnixMilli(defaultFetchedMs).Add(5 * time.Minute)
	p := NewWith(OSFS{}, nil, nil, nil, clock.NewFake(now))
	home := td("claude", "home_default")
	res, err := p.ProbeClaudeDir(filepath.Join(home, ".claude"))
	if err != nil {
		t.Fatal(err)
	}
	if res.Identity.Email != "default@example.com" {
		t.Errorf("email=%q", res.Identity.Email)
	}
	if res.Identity.AccountKey != "acct-default-0000-0000-000000000001" {
		t.Errorf("accountKey=%q", res.Identity.AccountKey)
	}
	if res.Identity.Fingerprint == "" {
		t.Error("fingerprint should be derived from organizationUuid")
	}
	if res.Source != filepath.Join(home, ".claude.json") {
		t.Errorf("source=%q want the legacy sibling", res.Source)
	}
	if res.TrustState != TrustVerifiedLocal {
		t.Errorf("trust=%v want verified_local (fresh cache)", res.TrustState)
	}
	// windows: session 24, weekly 14, scoped Fable 2.
	if s, ok := res.Usage.Windows.SessionPct(); !ok || s != 24 {
		t.Errorf("session=%v ok=%v want 24", s, ok)
	}
	if wk, ok := res.Usage.Windows.WeeklyPct(); !ok || wk != 14 {
		t.Errorf("weekly=%v ok=%v want 14", wk, ok)
	}
	var scoped *LimitWindow
	for i := range res.Usage.Windows {
		if res.Usage.Windows[i].Kind == KindWeeklyScoped {
			scoped = &res.Usage.Windows[i]
		}
	}
	if scoped == nil || scoped.Scope != "Fable" || scoped.Percent != 2 {
		t.Errorf("scoped window=%+v want Fable 2%%", scoped)
	}
}

func TestProbeClaudeStaleDowngrade(t *testing.T) {
	// a cache older than the freshness bound downgrades to Stale (shown, not routed).
	now := time.UnixMilli(defaultFetchedMs).Add(90 * time.Minute)
	p := NewWith(OSFS{}, nil, nil, nil, clock.NewFake(now))
	res, err := p.ProbeClaudeDir(filepath.Join(td("claude", "home_default"), ".claude"))
	if err != nil {
		t.Fatal(err)
	}
	if res.TrustState != TrustStale {
		t.Errorf("trust=%v want stale", res.TrustState)
	}
	if res.Routable() {
		t.Error("a stale reading must not be routable")
	}
}

// TestProbeClaudeUnknownAgeIsStale is the M2 guard: a cache with NO fetchedAtMs has an
// UNKNOWN age (CapturedAt zero), which must never read as fresh/VerifiedLocal — it is
// forced Stale (non-routable) so an ageless cache can't stay routable forever.
func TestProbeClaudeUnknownAgeIsStale(t *testing.T) {
	res, err := osProber().ProbeClaudeDir(td("claude", "no_ts_dir"))
	if err != nil {
		t.Fatal(err)
	}
	if !res.CapturedAt.IsZero() {
		t.Fatalf("fixture should have no fetchedAtMs, got CapturedAt=%v", res.CapturedAt)
	}
	if len(res.Usage.Windows) == 0 {
		t.Fatal("fixture has windows; the test needs them present to exercise the age branch")
	}
	if res.TrustState != TrustStale {
		t.Errorf("unknown-age cache trust=%v want stale", res.TrustState)
	}
	if res.Routable() {
		t.Error("an unknown-age cache must not be routable")
	}
}

func TestProbeClaudeVariantDir(t *testing.T) {
	p := osProber()
	dir := filepath.Join(td("claude", "home_default"), ".claude-work-example")
	res, err := p.ProbeClaudeDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if res.Identity.Email != "work@example.com" {
		t.Errorf("email=%q", res.Identity.Email)
	}
	if res.Identity.SeatTier != "enterprise" {
		t.Errorf("seatTier=%q want enterprise", res.Identity.SeatTier)
	}
	if !res.Usage.Windows.Critical() {
		t.Error("work account weekly window is severity=critical")
	}
	if !res.Usage.ExtraUsageEnabled {
		t.Error("extra usage should be enabled for the work fixture")
	}
}

// TestClaudeAllowListDropsSecrets proves the parser ignores credential-adjacent
// fields present in the file: the default fixture carries bogus top-level
// "accessToken"/"credentials" keys that must never surface anywhere on the Result.
func TestClaudeAllowListDropsSecrets(t *testing.T) {
	p := osProber()
	res, err := p.ProbeClaudeDir(filepath.Join(td("claude", "home_default"), ".claude"))
	if err != nil {
		t.Fatal(err)
	}
	// The typed structs have no field for a token, so there is simply nowhere for it
	// to appear. Assert the identity fields we DO expose carry no fixture secret.
	for _, v := range []string{res.Identity.Email, res.Identity.AccountKey, res.Identity.Org, res.Source} {
		if strings.Contains(v, "FAKE-must-be-ignored") {
			t.Errorf("a credential-adjacent value leaked into %q", v)
		}
	}
}
