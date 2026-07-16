package acctprobe

import (
	"os"
	"path/filepath"
	"testing"
)

// TestLiveSmoke is an OPT-IN read-only sanity check against the real home dir,
// gated by ACCTPROBE_LIVE=1. It probes only the on-disk caches (no network, no
// Keychain prompts) and asserts that NON-SECRET aggregate fields parse: it never
// reads a token, never logs an email or credential, and only reports counts and
// percentages. Run with:  ACCTPROBE_LIVE=1 go test ./internal/acctprobe -run LiveSmoke -v
func TestLiveSmoke(t *testing.T) {
	if os.Getenv("ACCTPROBE_LIVE") != "1" {
		t.Skip("set ACCTPROBE_LIVE=1 to run the live smoke test against the real home dir")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("home dir: %v", err)
	}
	p := New()

	dirs, err := p.DiscoverClaudeDirs(home)
	if err != nil {
		t.Fatalf("discover claude dirs: %v", err)
	}
	t.Logf("discovered %d claude config dir(s)", len(dirs))
	for _, dir := range dirs {
		res, err := p.ProbeClaudeDir(dir)
		if err != nil {
			t.Logf("claude dir %s held: %v", filepath.Base(dir), err)
			continue
		}
		if res.Identity.AccountKey == "" {
			t.Errorf("claude dir %s: empty account key", filepath.Base(dir))
		}
		assertSaneWindows(t, res)
		t.Logf("claude %s: trust=%s windows=%d", filepath.Base(dir), res.TrustState, len(res.Usage.Windows))
	}

	codexHome := filepath.Join(home, ".codex")
	if _, err := os.Stat(filepath.Join(codexHome, "auth.json")); err == nil {
		res, err := p.ProbeCodexHome(codexHome)
		if err != nil {
			t.Logf("codex held: %v", err)
		} else {
			if res.Identity.AccountKey == "" {
				t.Error("codex: empty account key")
			}
			assertSaneWindows(t, res)
			t.Logf("codex: trust=%s authMode=%s windows=%d", res.TrustState, res.Identity.AuthMode, len(res.Usage.Windows))
		}
	}
}

// assertSaneWindows checks only non-secret aggregate invariants of the parsed usage.
func assertSaneWindows(t *testing.T, res *Result) {
	t.Helper()
	for _, w := range res.Usage.Windows {
		if w.Percent < 0 || w.Percent > 200 {
			t.Errorf("window %s percent out of sane range: %v", w.Kind, w.Percent)
		}
	}
}
