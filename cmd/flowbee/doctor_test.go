package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// initTestRepo makes a temp git repo and runs flowbee Init on it.
func initTestRepo(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	run := func(args ...string) {
		cmd := exec.Command("git", append([]string{"-C", root}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init")
	run("remote", "add", "origin", "git@github.com:acme/widgets.git")
	if err := runInit([]string{"--dir", root}); err != nil {
		t.Fatalf("runInit: %v", err)
	}
	return root
}

func TestDoctorJSONOutput(t *testing.T) {
	root := initTestRepo(t)
	t.Setenv("FLOWBEE_SKIP_ORIGIN_FETCH", "1")
	t.Setenv("FLOWBEE_CONFIG", "")
	t.Setenv("FLOWBEE_GITHUB_TOKEN", "")

	out := captureStdout(t, func() {
		_ = runDoctor([]string{"--dir", root, "--offline", "--json"})
	})

	var checks []struct {
		Name   string `json:"name"`
		Status string `json:"status"`
		Detail string `json:"detail"`
	}
	if err := json.Unmarshal([]byte(out), &checks); err != nil {
		t.Fatalf("--json output did not parse as JSON: %v\noutput: %q", err, out)
	}
	if len(checks) == 0 {
		t.Fatal("--json output is an empty array; expected at least one check")
	}
	for i, c := range checks {
		if c.Name == "" {
			t.Errorf("check[%d] has empty name", i)
		}
		if c.Status == "" {
			t.Errorf("check[%d] has empty status", i)
		}
		switch c.Status {
		case "pass", "warn", "fail":
		default:
			t.Errorf("check[%d] has unexpected status %q", i, c.Status)
		}
		// detail may legitimately be empty for some checks, so no assertion there.
	}

	// Verify known checks are present with expected stable names.
	named := map[string]string{}
	for _, c := range checks {
		named[c.Name] = c.Status
	}
	for _, want := range []string{"config", "flow", "identities"} {
		if _, ok := named[want]; !ok {
			t.Errorf("expected check %q to be present in JSON output", want)
		}
	}
	// github check must be present; offline run yields "warn", not "fail".
	if s, ok := named["github"]; !ok {
		t.Error("expected check \"github\" to be present in JSON output")
	} else if s != "warn" {
		t.Errorf("offline github check should be \"warn\", got %q", s)
	}
}

func TestDoctorJSONExitCodeOnFail(t *testing.T) {
	root := t.TempDir() // no flowbee.yaml → config check fails
	t.Setenv("FLOWBEE_SKIP_ORIGIN_FETCH", "1")
	t.Setenv("FLOWBEE_CONFIG", "")
	var doctorErr error
	captureStdout(t, func() {
		doctorErr = runDoctor([]string{"--dir", root, "--offline", "--json"})
	})
	if doctorErr == nil {
		t.Fatal("expected non-nil error when a check fails under --json")
	}
}

func TestDoctorJSONWinsOverQuiet(t *testing.T) {
	root := initTestRepo(t)
	cfgPath := filepath.Join(root, "flowbee.yaml")
	t.Setenv("FLOWBEE_SKIP_ORIGIN_FETCH", "1")
	t.Setenv("FLOWBEE_CONFIG", cfgPath)
	t.Setenv("FLOWBEE_GITHUB_TOKEN", "")

	out := captureStdout(t, func() {
		_ = runDoctor([]string{"--dir", root, "--offline", "--json", "--quiet"})
	})
	// when --json wins, output must be valid JSON (not the human summary line).
	var checks []map[string]interface{}
	if err := json.Unmarshal([]byte(out), &checks); err != nil {
		t.Fatalf("--json --quiet should emit JSON, got: %q\nerr: %v", out, err)
	}
}

func TestRunningConfigCheckUsesWorkerToken(t *testing.T) {
	const token = "operator-token"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer "+token {
			http.Error(w, "missing auth", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"version":"test-version","pid":123,"database_url":"db","private_addr":":7070","allow_self_merge":true}`))
	}))
	defer srv.Close()

	t.Setenv("FLOWBEE_URL", srv.URL)
	t.Setenv("FLOWBEE_WORKER_TOKEN", token)

	check := runningConfigCheck(t.Context())
	if check.Status != "pass" {
		t.Fatalf("running-config status=%s detail=%s", check.Status, check.Detail)
	}
	if !strings.Contains(check.Detail, "version=test-version") {
		t.Fatalf("running config detail missing served config: %s", check.Detail)
	}
}

func TestRunningConfigCheckWarnsWhenRunningBinaryBehind(t *testing.T) {
	behind := 2
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"version":               "dev-0cef2e5ac1f6+dirty",
			"source_commit":         "0cef2e5ac1f6aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			"tree_dirty":            true,
			"behind_origin_main_by": behind,
			"origin_main_warning":   "WARN: running binary is 2 commits behind origin/main (built from 0cef2e5ac1f6, dirty=true) - merged fixes may be missing",
			"pid":                   123,
			"database_url":          "db",
			"private_addr":          ":7070",
			"allow_self_merge":      true,
		})
	}))
	defer srv.Close()

	t.Setenv("FLOWBEE_URL", srv.URL)
	check := runningConfigCheck(t.Context())
	if check.Status != "warn" {
		t.Fatalf("running-config status=%s detail=%s", check.Status, check.Detail)
	}
	for _, want := range []string{
		"source_commit=0cef2e5ac1f6aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		"tree_dirty=true",
		"behind_origin_main_by=2",
		"WARN: running binary is 2 commits behind origin/main",
	} {
		if !strings.Contains(check.Detail, want) {
			t.Fatalf("running-config detail missing %q: %s", want, check.Detail)
		}
	}
}
