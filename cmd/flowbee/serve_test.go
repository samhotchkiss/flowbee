package main

import (
	"strings"
	"testing"
)

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
