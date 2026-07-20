package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/samhotchkiss/flowbee/internal/config"
)

func TestServeHelpIsSideEffectFree(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "must-not-exist.db")
	configPath := filepath.Join(dir, "flowbee.yaml")
	if err := os.WriteFile(configPath, []byte("database_url: "+dbPath+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FLOWBEE_CONFIG", configPath)

	var runErr error
	out := captureStdout(t, func() { runErr = runServe([]string{"--help"}) })
	if runErr != nil {
		t.Fatalf("serve --help: %v", runErr)
	}
	if !strings.Contains(out, "usage: flowbee serve") {
		t.Fatalf("serve help missing usage: %q", out)
	}
	if _, err := os.Stat(dbPath); !os.IsNotExist(err) {
		t.Fatalf("serve --help touched database %s: %v", dbPath, err)
	}
	if matches, err := filepath.Glob(filepath.Join(dir, "*.writer.lock")); err != nil || len(matches) != 0 {
		t.Fatalf("serve --help created writer state: matches=%v err=%v", matches, err)
	}
}

func TestServeRejectsUnknownArgumentBeforeSideEffects(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "must-not-exist.db")
	configPath := filepath.Join(dir, "flowbee.yaml")
	if err := os.WriteFile(configPath, []byte("database_url: "+dbPath+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FLOWBEE_CONFIG", configPath)

	err := runServe([]string{"--definitely-unknown"})
	if err == nil || !strings.Contains(err.Error(), "unknown argument") {
		t.Fatalf("serve unknown argument error=%v", err)
	}
	if _, statErr := os.Stat(dbPath); !os.IsNotExist(statErr) {
		t.Fatalf("serve unknown argument touched database %s: %v", dbPath, statErr)
	}
}

// TestServeSystemdDefaultsToStartable: with no security env set, the --systemd
// template must still produce a unit that BOOTS. `flowbee serve` refuses to start on
// its default non-loopback bind without either worker auth or an explicit insecure
// opt-in, so the template defaults to FLOWBEE_INSECURE=1 (trusted-tailnet) with a
// loud pick-one comment — never an env that dies on `systemctl enable`.
func TestServeSystemdDefaultsToStartable(t *testing.T) {
	t.Setenv("FLOWBEE_WORKER_AUTH_SECRET", "")
	t.Setenv("FLOWBEE_INSECURE", "")
	out := captureStdout(t, printServeSystemd)
	if !strings.Contains(out, "FLOWBEE_INSECURE=1") {
		t.Errorf("no-security template must default to a startable FLOWBEE_INSECURE=1\n%s", out)
	}
	if !strings.Contains(out, "Pick ONE") {
		t.Errorf("expected the pick-one security guidance comment\n%s", out)
	}
}

// TestServeSystemdCarriesAuthChoice: when the operator runs with worker auth set, the
// template emits the auth-secret placeholder (not the insecure opt-in) and never the
// live secret value.
func TestServeSystemdCarriesAuthChoice(t *testing.T) {
	t.Setenv("FLOWBEE_WORKER_AUTH_SECRET", "super-secret-live-value")
	t.Setenv("FLOWBEE_INSECURE", "")
	out := captureStdout(t, printServeSystemd)
	if !strings.Contains(out, "FLOWBEE_WORKER_AUTH_SECRET=<shared-worker-secret>") {
		t.Errorf("auth path must template the secret placeholder\n%s", out)
	}
	if strings.Contains(out, "super-secret-live-value") {
		t.Fatalf("must NEVER print the live worker-auth secret value\n%s", out)
	}
	// the auth path should not ALSO drop in a bare insecure opt-in line.
	for _, line := range strings.Split(out, "\n") {
		if strings.TrimSpace(line) == "FLOWBEE_INSECURE=1" {
			t.Errorf("auth path must not emit FLOWBEE_INSECURE=1\n%s", out)
		}
	}
}

func TestServeSystemdCarriesV2FileBackedTrustConfiguration(t *testing.T) {
	t.Setenv("FLOWBEE_EPIC_REVIEW_HANDOFF_V2", "1")
	t.Setenv("FLOWBEE_PHASE1_DASHBOARD", "1")
	t.Setenv("FLOWBEE_ALERT_WEBHOOK_SECRET", "inline-secret-must-never-print")
	t.Setenv("FLOWBEE_ALERT_WEBHOOK_SECRET_FILE", "/etc/flowbee/control-alert-ingress.key")
	t.Setenv("FLOWBEE_WATCHDOG_PROJECT_ID", "russ")
	t.Setenv(config.DriverEndpointsFileEnv, "/etc/flowbee/driver-endpoints.json")
	t.Setenv("FLOWBEE_HUMAN_SESSION_KEY_FILE", "/etc/flowbee/human.key")
	t.Setenv("FLOWBEE_HUMAN_GRANTS_FILE", "/etc/flowbee/human.grants")
	out := captureStdout(t, printServeSystemd)
	for _, want := range []string{
		"FLOWBEE_EPIC_REVIEW_HANDOFF_V2=1",
		"FLOWBEE_PHASE1_DASHBOARD=1",
		"FLOWBEE_ALERT_WEBHOOK_SECRET_FILE=/etc/flowbee/control-alert-ingress.key",
		"FLOWBEE_WATCHDOG_PROJECT_ID=russ",
		"FLOWBEE_DRIVER_ENDPOINTS_FILE=/etc/flowbee/driver-endpoints.json",
		"FLOWBEE_HUMAN_SESSION_KEY_FILE=/etc/flowbee/human.key",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("v2 template missing %q\n%s", want, out)
		}
	}
	if strings.Contains(out, "inline-secret-must-never-print") || strings.Contains(out, "FLOWBEE_ALERT_WEBHOOK_SECRET=") {
		t.Fatalf("v2 template leaked or encouraged an inline alert secret\n%s", out)
	}
	if strings.Contains(out, "FLOWBEE_ALERT_WEBHOOK_URL=") {
		t.Fatalf("v2 serve template must not configure an outbound human-notification webhook; alerts route to the project Interactor\n%s", out)
	}
	if strings.Contains(out, "FLOWBEE_DRIVER_SOCKET=") || strings.Contains(out, "FLOWBEE_DRIVER_TOKEN_FILE=") ||
		strings.Contains(out, "FLOWBEE_DRIVER_INSTANCE_REF=") {
		t.Fatalf("v2 template emitted a legacy single-endpoint fallback\n%s", out)
	}
}

// TestRepoTokenWarning covers the multi-repo token footgun classifier: a repo with
// no token at all (no-op), a declared token_env that's unset (silent shared fallback),
// and the healthy cases (per-repo token present, or shared token with no token_env).
func TestRepoTokenWarning(t *testing.T) {
	cases := []struct {
		name, id, tokenEnv, shared, perRepo string
		wantSub                             string // "" => no warning expected
	}{
		{"no token at all, no env", "web", "", "", "", "has NO GitHub token"},
		{"no token at all, env declared", "web", "WEB_PAT", "", "", "WEB_PAT (or FLOWBEE_GITHUB_TOKEN)"},
		{"env declared but unset, shared present", "web", "WEB_PAT", "shared", "", "token_env WEB_PAT is unset"},
		{"per-repo token present", "web", "WEB_PAT", "shared", "tok", ""},
		{"shared token, no env declared", "web", "", "shared", "", ""},
		{"per-repo present, no shared", "web", "WEB_PAT", "", "tok", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := repoTokenWarning(c.id, c.tokenEnv, c.shared, c.perRepo)
			if c.wantSub == "" {
				if got != "" {
					t.Fatalf("expected no warning, got %q", got)
				}
				return
			}
			if !strings.Contains(got, c.wantSub) {
				t.Fatalf("warning %q must contain %q", got, c.wantSub)
			}
			if !strings.Contains(got, c.id) {
				t.Fatalf("warning %q must name the repo id %q", got, c.id)
			}
		})
	}
}

func TestRegistryControlMirrorURL(t *testing.T) {
	t.Setenv("FLOWBEE_GITHUB_TOKEN", "shared-tok")
	t.Setenv("WEB_PAT", "web-tok")

	// primary = first ACTIVE repo by id; per-repo token_env wins over the shared token.
	cfg := config.Config{Repos: []config.RepoConfig{
		{ID: "zrepo", Owner: "o", Repo: "z"},
		{ID: "arepo", Owner: "o", Repo: "a", TokenEnv: "WEB_PAT"},
	}}
	got := registryControlMirrorURL(cfg)
	want := "https://x-access-token:web-tok@github.com/o/a.git"
	if got != want {
		t.Fatalf("primary-by-id with token_env: got %q want %q", got, want)
	}

	// no repos => empty (legacy/no-registry path).
	if u := registryControlMirrorURL(config.Config{}); u != "" {
		t.Fatalf("no repos must yield empty, got %q", u)
	}

	// a parked (inactive) primary is skipped.
	no := false
	cfg2 := config.Config{Repos: []config.RepoConfig{
		{ID: "arepo", Owner: "o", Repo: "a", Active: &no},
		{ID: "brepo", Owner: "o", Repo: "b"},
	}}
	if u := registryControlMirrorURL(cfg2); u != "https://x-access-token:shared-tok@github.com/o/b.git" {
		t.Fatalf("inactive primary must be skipped, got %q", u)
	}
}
