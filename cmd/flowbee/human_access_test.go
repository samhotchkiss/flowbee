package main

import (
	"os"
	"path/filepath"
	"testing"
)

func writeOwnerFile(t *testing.T, name, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestConfiguredHumanAccessFailsClosedForTailnetAndLoadsOwnerFiles(t *testing.T) {
	t.Setenv("FLOWBEE_HUMAN_SESSION_KEY_FILE", "")
	t.Setenv("FLOWBEE_HUMAN_GRANTS_FILE", "")
	t.Setenv("FLOWBEE_HUMAN_LOOPBACK_DEV", "")
	if _, err := configuredHumanAccess("100.80.1.2:7070", nil, true); err == nil {
		t.Fatal("Tailnet Phase 1 started without human access")
	}
	key := writeOwnerFile(t, "human.key", "01234567890123456789012345678901")
	grants := writeOwnerFile(t, "grants", "sam@default=admin\n")
	t.Setenv("FLOWBEE_HUMAN_SESSION_KEY_FILE", key)
	t.Setenv("FLOWBEE_HUMAN_GRANTS_FILE", grants)
	if access, err := configuredHumanAccess("100.80.1.2:7070", nil, true); err != nil || access == nil {
		t.Fatalf("access=%v err=%v", access, err)
	}
}

func TestConfiguredHumanAccessAllowsOnlyExplicitLoopbackDev(t *testing.T) {
	t.Setenv("FLOWBEE_HUMAN_SESSION_KEY_FILE", "")
	t.Setenv("FLOWBEE_HUMAN_GRANTS_FILE", "")
	t.Setenv("FLOWBEE_HUMAN_LOOPBACK_DEV", "1")
	if _, err := configuredHumanAccess("127.0.0.1:7070", nil, true); err != nil {
		t.Fatal(err)
	}
	if _, err := configuredHumanAccess("0.0.0.0:7070", nil, true); err == nil {
		t.Fatal("loopback dev bypass widened to non-loopback")
	}
}
