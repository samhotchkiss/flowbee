package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"github.com/samhotchkiss/flowbee/internal/deadman"
)

func runWatchdog(args []string) error {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	return runWatchdogContext(ctx, args)
}

func runWatchdogContext(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("watchdog", flag.ContinueOnError)
	healthURL := fs.String("health-url", envOr("FLOWBEE_WATCHDOG_HEALTH_URL", "http://127.0.0.1:7001/healthz"), "independently reachable Flowbee /healthz URL")
	stateFile := fs.String("state-file", envOr("FLOWBEE_WATCHDOG_STATE_FILE", defaultWatchdogStatePath()), "owner-only durable watchdog state file")
	webhookURL := fs.String("webhook-url", os.Getenv("FLOWBEE_ALERT_WEBHOOK_URL"), "alert receiver URL")
	secretFile := fs.String("secret-file", os.Getenv("FLOWBEE_ALERT_WEBHOOK_SECRET_FILE"), "owner-only file containing the HMAC webhook key")
	watchdogID := fs.String("id", os.Getenv("FLOWBEE_EXTERNAL_WATCHDOG_ID"), "stable identity for this external watchdog")
	interval := fs.Duration("interval", envDuration("FLOWBEE_WATCHDOG_INTERVAL", 30*time.Second), "health polling interval")
	timeout := fs.Duration("timeout", envDuration("FLOWBEE_WATCHDOG_TIMEOUT", 5*time.Second), "per-request timeout")
	once := fs.Bool("once", false, "perform one durable probe/delivery pass and exit (for cron/testing)")
	systemd := fs.Bool("systemd", false, "print a hardened independent systemd installation template")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	if *systemd {
		printWatchdogSystemd()
		return nil
	}
	if *watchdogID == "" {
		return errors.New("--id or FLOWBEE_EXTERNAL_WATCHDOG_ID is required")
	}
	if *webhookURL == "" {
		return errors.New("--webhook-url or FLOWBEE_ALERT_WEBHOOK_URL is required")
	}
	if *secretFile == "" {
		return errors.New("--secret-file or FLOWBEE_ALERT_WEBHOOK_SECRET_FILE is required (the key must be file-backed)")
	}
	if *interval <= 0 || *timeout <= 0 {
		return errors.New("--interval and --timeout must be positive")
	}
	if err := validateHTTPURL(*healthURL, "health"); err != nil {
		return err
	}
	if err := validateHTTPURL(*webhookURL, "webhook"); err != nil {
		return err
	}
	secret, err := deadman.ReadOwnerOnlySecret(*secretFile)
	if err != nil {
		return fmt.Errorf("read alert webhook secret: %w", err)
	}
	state := deadman.FileStore{Path: *stateFile}
	lock, err := state.Lock()
	if err != nil {
		return err
	}
	defer lock.Close()
	client := &http.Client{Timeout: *timeout}
	runner := deadman.Runner{
		WatchdogID: *watchdogID, Target: *healthURL, Store: state,
		Probe:     deadman.HTTPProbe{URL: *healthURL, Client: client},
		Publisher: deadman.WebhookPublisher{URL: *webhookURL, Secret: secret, Client: client},
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, nil)).With("component", "external-deadman", "watchdog_id", *watchdogID)
	pass := func() error {
		report, err := runner.RunOnce(ctx)
		attrs := []any{"healthy", report.Observation.Healthy, "reason", report.Observation.Reason,
			"incident_id", report.IncidentID, "notifications_sent", report.NotificationsSent,
			"notifications_pending", report.NotificationsLeft}
		if err != nil {
			log.Error("dead-man pass failed", append(attrs, "err", err)...)
			return err
		}
		if report.IncidentStarted {
			log.Error("Flowbee control plane incident opened", attrs...)
		} else if report.IncidentResolved {
			log.Info("Flowbee control plane incident resolved", attrs...)
		} else {
			log.Info("dead-man pass complete", attrs...)
		}
		return nil
	}
	if *once {
		return pass()
	}
	// A failed alert delivery must not kill the independent observer. The durable
	// queue is retried on the next tick with the exact same key and body.
	_ = pass()
	ticker := time.NewTicker(*interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			_ = pass()
		}
	}
}

func validateHTTPURL(raw, label string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return fmt.Errorf("invalid %s URL %q (want http:// or https:// with a host)", label, raw)
	}
	return nil
}

func envDuration(name string, fallback time.Duration) time.Duration {
	value := os.Getenv(name)
	if value == "" {
		return fallback
	}
	if parsed, err := time.ParseDuration(value); err == nil {
		return parsed
	}
	if seconds, err := strconv.Atoi(value); err == nil {
		return time.Duration(seconds) * time.Second
	}
	return fallback
}

func defaultWatchdogStatePath() string {
	if base := os.Getenv("XDG_STATE_HOME"); base != "" {
		return filepath.Join(base, "flowbee", "watchdog.json")
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return "flowbee-watchdog.json"
	}
	return filepath.Join(home, ".local", "state", "flowbee", "watchdog.json")
}

func printWatchdogSystemd() {
	self, err := os.Executable()
	if err != nil {
		self = os.Args[0]
	}
	fmt.Printf(`# Install this on an independently supervised host (preferably not the
# Flowbee control-plane host). No database or tmux access is required.

# 1. Install the binary, then create the unprivileged service account and
# owner-only HMAC key file:
sudo install -o root -g root -m 0755 %q /usr/local/bin/flowbee
sudo useradd --system --home /var/lib/flowbee-watchdog --shell /usr/sbin/nologin flowbee-watchdog
sudo install -d -o flowbee-watchdog -g flowbee-watchdog -m 0700 /var/lib/flowbee-watchdog
sudo install -o flowbee-watchdog -g flowbee-watchdog -m 0600 /dev/null /etc/flowbee-watchdog.secret
# Write the shared webhook HMAC key into /etc/flowbee-watchdog.secret (no newline requirement).

# 2. Write /etc/flowbee-watchdog.env (this file contains references, not the key):
FLOWBEE_EXTERNAL_WATCHDOG_ID=<stable-host-id>
FLOWBEE_WATCHDOG_HEALTH_URL=http://<tailnet-control-plane-host>:7001/healthz
FLOWBEE_ALERT_WEBHOOK_URL=https://<alert-receiver>/flowbee
FLOWBEE_ALERT_WEBHOOK_SECRET_FILE=/etc/flowbee-watchdog.secret
FLOWBEE_WATCHDOG_STATE_FILE=/var/lib/flowbee-watchdog/state.json
FLOWBEE_WATCHDOG_INTERVAL=30s
FLOWBEE_WATCHDOG_TIMEOUT=5s

# 3. Write /etc/systemd/system/flowbee-watchdog.service:
[Unit]
Description=Independent Flowbee control-plane dead-man
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=flowbee-watchdog
Group=flowbee-watchdog
EnvironmentFile=/etc/flowbee-watchdog.env
ExecStart=/usr/local/bin/flowbee watchdog
Restart=always
RestartSec=5
NoNewPrivileges=true
PrivateTmp=true
ProtectSystem=strict
ProtectHome=true
ReadOnlyPaths=/etc/flowbee-watchdog.secret
ReadWritePaths=/var/lib/flowbee-watchdog

[Install]
WantedBy=multi-user.target

# 4. Enable, inspect, and prove the negative path before relying on it:
sudo systemctl daemon-reload && sudo systemctl enable --now flowbee-watchdog
journalctl -u flowbee-watchdog -f
# Stop Flowbee (or point health URL at an unused port), confirm one firing alert,
# restore it, and confirm one resolved alert with the same incident ID.
`, self)
}
