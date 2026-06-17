package main

import (
	"io"
	"os"
	"strings"
	"testing"
	"time"
)

// captureStdout runs f with os.Stdout redirected to a pipe and returns what it wrote.
func captureStdout(t *testing.T, f func()) string {
	t.Helper()
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	f()
	_ = w.Close()
	os.Stdout = old
	out, _ := io.ReadAll(r)
	return string(out)
}

// TestFleetSystemdTemplatesRequiredRepoURL: the --systemd env template MUST include
// FLOWBEE_REPO_URL — `flowbee fleet` hard-fails at startup without it, so omitting it
// (the prior bug) printed a unit that died on enable. With nothing in the env, a clear
// placeholder appears; the worker-auth secret line is always templated (as a
// placeholder, never a live value).
func TestFleetSystemdTemplatesRequiredRepoURL(t *testing.T) {
	t.Setenv("FLOWBEE_REPO_URL", "")
	t.Setenv("FLOWBEE_GITHUB_OWNER", "")
	t.Setenv("FLOWBEE_GITHUB_REPO", "")
	out := captureStdout(t, func() {
		printFleetSystemd("http://cp:7070", 3, "claude -p x", "claude -p y")
	})
	for _, want := range []string{
		"FLOWBEE_REPO_URL=git@github.com:OWNER/REPO.git", // required-to-start var, placeholder
		"FLOWBEE_WORKER_AUTH_SECRET=<shared-worker-secret>",
		"FLOWBEE_URL=http://cp:7070",
		"ExecStart=",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("fleet --systemd output missing %q\n--- output ---\n%s", want, out)
		}
	}
}

// TestFleetSystemdEchoesResolvedRepoURL: when FLOWBEE_REPO_URL is set, the template
// echoes the real value so the installed unit starts as-is.
func TestFleetSystemdEchoesResolvedRepoURL(t *testing.T) {
	t.Setenv("FLOWBEE_REPO_URL", "git@github.com:samhotchkiss/flowbee.git")
	out := captureStdout(t, func() {
		printFleetSystemd("http://cp:7070", 2, "a", "b")
	})
	if !strings.Contains(out, "FLOWBEE_REPO_URL=git@github.com:samhotchkiss/flowbee.git") {
		t.Errorf("must echo the resolved repo url; got:\n%s", out)
	}
}

// TestNextRespawnBackoff: the supervisor's respawn delay doubles and caps at 30s, so a
// crash-looping worker backs off instead of hot-spinning the box.
func TestNextRespawnBackoff(t *testing.T) {
	cases := []struct{ in, want time.Duration }{
		{1 * time.Second, 2 * time.Second},
		{2 * time.Second, 4 * time.Second},
		{16 * time.Second, 30 * time.Second}, // 32 -> capped
		{30 * time.Second, 30 * time.Second}, // stays capped
	}
	for _, c := range cases {
		if got := nextRespawnBackoff(c.in); got != c.want {
			t.Fatalf("nextRespawnBackoff(%s)=%s, want %s", c.in, got, c.want)
		}
	}
}
