package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWatchdogOnePassWiresHealthStateAndAuthenticatedWebhook(t *testing.T) {
	health := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(w, `{"status":"degraded","reconciler_overdue":1,"reconciler_overdue_names":["review_handoff"]}`)
	}))
	defer health.Close()
	secret := "watchdog-test-key"
	webhookCalls := 0
	webhook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mac := hmac.New(sha256.New, []byte(secret))
		_, _ = mac.Write(body)
		if r.Header.Get("X-Flowbee-Signature") != "sha256="+hex.EncodeToString(mac.Sum(nil)) {
			t.Error("missing exact HMAC signature")
		}
		if !strings.HasSuffix(r.Header.Get("Idempotency-Key"), ":firing") {
			t.Error("missing stable firing idempotency key")
		}
		webhookCalls++
		w.WriteHeader(http.StatusNoContent)
	}))
	defer webhook.Close()
	dir := t.TempDir()
	secretPath := filepath.Join(dir, "secret")
	if err := os.WriteFile(secretPath, []byte(secret), 0600); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(dir, "state.json")
	args := []string{"--once", "--id", "test-observer", "--health-url", health.URL,
		"--webhook-url", webhook.URL, "--secret-file", secretPath, "--state-file", statePath}
	if err := runWatchdogContext(context.Background(), args); err != nil {
		t.Fatal(err)
	}
	if err := runWatchdogContext(context.Background(), args); err != nil {
		t.Fatal(err)
	}
	if webhookCalls != 1 {
		t.Fatalf("webhook calls=%d, want one across independent one-pass invocations", webhookCalls)
	}
	if info, err := os.Stat(statePath); err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("state file info=%v err=%v", info, err)
	}
}

func TestWatchdogSystemdTemplateUsesSecretFileNotSecretValue(t *testing.T) {
	t.Setenv("FLOWBEE_ALERT_WEBHOOK_SECRET", "must-not-leak")
	out := captureStdout(t, printWatchdogSystemd)
	for _, want := range []string{"FLOWBEE_ALERT_WEBHOOK_SECRET_FILE=", "flowbee-watchdog.service", "ExecStart=/usr/local/bin/flowbee watchdog", "ReadWritePaths=/var/lib/flowbee-watchdog", "install -o root -g root -m 0755"} {
		if !strings.Contains(out, want) {
			t.Errorf("systemd template missing %q\n%s", want, out)
		}
	}
	if strings.Contains(out, "must-not-leak") || strings.Contains(out, "FLOWBEE_ALERT_WEBHOOK_SECRET=") {
		t.Fatalf("systemd template exposed or requested an inline secret\n%s", out)
	}
}
