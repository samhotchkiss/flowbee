package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/samhotchkiss/flowbee/internal/alertingress"
)

func TestWatchdogOnePassWiresHealthStateAndAuthenticatedWebhook(t *testing.T) {
	health := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(w, `{"status":"degraded","reconciler_overdue":1,"reconciler_overdue_names":["review_handoff"]}`)
	}))
	defer health.Close()
	secret := "watchdog-test-key"
	heartbeatCalls, alertCalls := 0, 0
	webhook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != alertingress.ControlAlertIngressPath || r.URL.RawQuery != "" {
			t.Errorf("watchdog posted outside exact Flowbee ingress: %s", r.URL.String())
		}
		body, _ := io.ReadAll(r.Body)
		var envelope struct {
			ProjectID string `json:"project_id"`
			Kind      string `json:"kind"`
		}
		if err := json.Unmarshal(body, &envelope); err != nil || envelope.ProjectID != "russ" {
			t.Errorf("webhook project=%q err=%v", envelope.ProjectID, err)
		}
		mac := hmac.New(sha256.New, []byte(secret))
		_, _ = mac.Write(body)
		if r.Header.Get("X-Flowbee-Signature") != "sha256="+hex.EncodeToString(mac.Sum(nil)) {
			t.Error("missing exact HMAC signature")
		}
		switch envelope.Kind {
		case "external_watchdog_heartbeat":
			heartbeatCalls++
			if !strings.HasPrefix(r.Header.Get("Idempotency-Key"), "deadman-heartbeat:russ:test-observer:") {
				t.Error("missing stable heartbeat idempotency key")
			}
		case "external_deadman":
			alertCalls++
			if !strings.HasSuffix(r.Header.Get("Idempotency-Key"), ":firing") {
				t.Error("missing stable firing idempotency key")
			}
		default:
			t.Errorf("unexpected envelope kind %q", envelope.Kind)
		}
		hash := sha256.Sum256(body)
		ackBody, _ := json.Marshal(alertingress.Acknowledgement{
			FormatVersion: alertingress.AckFormatVersion,
			Status:        "accepted", IdempotencyKey: r.Header.Get("Idempotency-Key"),
			BodySHA256: hex.EncodeToString(hash[:]),
		})
		ackMAC := hmac.New(sha256.New, []byte(secret))
		_, _ = ackMAC.Write(ackBody)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Flowbee-Signature", "sha256="+hex.EncodeToString(ackMAC.Sum(nil)))
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write(ackBody)
	}))
	defer webhook.Close()
	dir := t.TempDir()
	secretPath := filepath.Join(dir, "secret")
	if err := os.WriteFile(secretPath, []byte(secret), 0600); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(dir, "state.json")
	args := []string{"--once", "--id", "test-observer", "--project-id", "russ", "--health-url", health.URL,
		"--webhook-url", webhook.URL + alertingress.ControlAlertIngressPath,
		"--secret-file", secretPath, "--state-file", statePath}
	if err := runWatchdogContext(context.Background(), args); err != nil {
		t.Fatal(err)
	}
	if err := runWatchdogContext(context.Background(), args); err != nil {
		t.Fatal(err)
	}
	if alertCalls != 1 || heartbeatCalls != 2 {
		t.Fatalf("alert calls=%d heartbeat calls=%d, want one incident and one heartbeat per pass", alertCalls, heartbeatCalls)
	}
	if info, err := os.Stat(statePath); err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("state file info=%v err=%v", info, err)
	}
}

func TestWatchdogSystemdTemplateUsesSecretFileNotSecretValue(t *testing.T) {
	t.Setenv("FLOWBEE_ALERT_WEBHOOK_SECRET", "must-not-leak")
	out := captureStdout(t, printWatchdogSystemd)
	for _, want := range []string{"FLOWBEE_ALERT_WEBHOOK_SECRET_FILE=", "FLOWBEE_WATCHDOG_PROJECT_ID=", "FLOWBEE_WATCHDOG_HEALTH_URL=http://<tailnet-flowbee-host>:7001/healthz", "FLOWBEE_ALERT_WEBHOOK_URL=https://<tailnet-flowbee-host>:7443/v1/control-alerts/ingress", "flowbee-watchdog.service", "ExecStart=/usr/local/bin/flowbee watchdog", "ReadWritePaths=/var/lib/flowbee-watchdog", "install -o root -g root -m 0755"} {
		if !strings.Contains(out, want) {
			t.Errorf("systemd template missing %q\n%s", want, out)
		}
	}
	if strings.Contains(out, "must-not-leak") || strings.Contains(out, "FLOWBEE_ALERT_WEBHOOK_SECRET=") {
		t.Fatalf("systemd template exposed or requested an inline secret\n%s", out)
	}
	if strings.Contains(out, ">/flowbee") {
		t.Fatalf("systemd template still points at removed legacy receiver path\n%s", out)
	}
}

func TestWatchdogRequiresExplicitStableProjectID(t *testing.T) {
	t.Setenv("FLOWBEE_WATCHDOG_PROJECT_ID", "")
	if err := runWatchdogContext(context.Background(), []string{"--once", "--id", "observer"}); err == nil ||
		!strings.Contains(err.Error(), "--project-id") {
		t.Fatalf("missing project error=%v", err)
	}
	if err := runWatchdogContext(context.Background(), []string{"--once", "--id", "observer", "--project-id", "Russ/Main"}); err == nil ||
		!strings.Contains(err.Error(), "watchdog project id") {
		t.Fatalf("invalid project error=%v", err)
	}
}

func TestWatchdogRequiresExactFlowbeeControlAlertIngressURL(t *testing.T) {
	health := "http://flowbee.tailnet.example:7001/healthz"
	valid := "https://flowbee.tailnet.example:7443" + alertingress.ControlAlertIngressPath
	if err := validateControlAlertIngressURL(valid, health); err != nil {
		t.Fatalf("exact Flowbee ingress rejected: %v", err)
	}
	for _, raw := range []string{
		"https://flowbee.tailnet.example/",
		"https://flowbee.tailnet.example/hooks/alerts",
		valid + "/",
		valid + "?relay=provider",
		valid + "#human-sink",
		"https://user@flowbee.tailnet.example" + alertingress.ControlAlertIngressPath,
		"https://flowbee.tailnet.example/v1/control-alerts/%69ngress",
		"https://evil.example" + alertingress.ControlAlertIngressPath,
	} {
		if err := validateControlAlertIngressURL(raw, health); err == nil {
			t.Errorf("non-Flowbee alert destination accepted: %q", raw)
		}
	}
}

func TestWatchdogIntervalCannotOutliveReadinessLease(t *testing.T) {
	err := runWatchdogContext(context.Background(), []string{"--once", "--id", "observer",
		"--project-id", "russ", "--webhook-url", "http://127.0.0.1:1" + alertingress.ControlAlertIngressPath,
		"--secret-file", "/does/not/matter", "--interval", "61s"})
	if err == nil || !strings.Contains(err.Error(), "must be <= 1m0s") {
		t.Fatalf("overlong watchdog interval error=%v", err)
	}
}
