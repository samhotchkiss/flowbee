// Package config loads Flowbee's typed configuration from an optional YAML file
// plus FLOWBEE_* environment overrides, and validates invariants.
package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/samhotchkiss/flowbee/internal/content"
	"gopkg.in/yaml.v3"
)

type Config struct {
	DatabaseURL        string `yaml:"database_url"`
	PrivateAddr        string `yaml:"private_addr"`
	HealthAddr         string `yaml:"health_addr"`
	WebhookAddr        string `yaml:"webhook_addr"`
	LeaseTTLS          int    `yaml:"lease_ttl_s"`
	HeartbeatIntervalS int    `yaml:"heartbeat_interval_s"`
	LongPollWaitS      int    `yaml:"long_poll_wait_s"`
	RiverMaxWorkers    int    `yaml:"river_max_workers"`
	LogLevel           string `yaml:"log_level"`
	// NoEligibleWorkerS is how long a `ready` job may sit with no compliant
	// worker before the no_eligible_worker alarm fires (I-6, §6.6).
	NoEligibleWorkerS int `yaml:"no_eligible_worker_s"`

	// WorkerAuthSecret is the HMAC key that signs per-worker bearer tokens
	// (DESIGN §7.6). When set, the private worker API requires mutual auth: every
	// call carries a signed token bound to an enrolled identity, and an unenrolled
	// caller is rejected 401 before it can lease job context. Empty = loopback-only
	// dev (no mutual auth — the listener must stay on 127.0.0.1). Set via
	// FLOWBEE_WORKER_AUTH_SECRET for any non-loopback (Tailscale/LAN) listener.
	WorkerAuthSecret string `yaml:"worker_auth_secret"`
	// EnrolledIdentities is the allowlist of worker identities permitted to
	// authenticate (§7.6). Set via FLOWBEE_ENROLLED_IDENTITIES (comma-separated).
	EnrolledIdentities []string `yaml:"enrolled_identities"`
	// AuthLoopbackBypass lets same-box (127.0.0.1) workers skip the token even when
	// WorkerAuthSecret is set (§12.4 "bearer fallback on loopback"). Default true.
	AuthLoopbackBypass bool `yaml:"auth_loopback_bypass"`

	// AllowSelfMerge is THE ONE DECISION (§14, F2): whether the MVP may merge without
	// a human. Default false = Branch A (every approved job hands off to a human).
	// true = Branch B (autonomous merge): an approved + denylist-clear + CI-green job
	// is self_merge-eligible and Flowbee merges it itself. The safety net stays
	// deterministic — content-integrity gate + CI-green-at-head + the reconciled,
	// SHA-bound verdict. Set via FLOWBEE_ALLOW_SELF_MERGE.
	AllowSelfMerge bool `yaml:"allow_self_merge"`

	// ContentMaxDiffBytes / ContentMaxChangedFiles are the operator-configurable
	// content-integrity ceilings (F2, §9.2c): a diff over either bound fails static
	// checks and is forced to handoff. 0 => the shipped content.DefaultLimits.
	ContentMaxDiffBytes    int `yaml:"content_max_diff_bytes"`
	ContentMaxChangedFiles int `yaml:"content_max_changed_files"`
	// ContentDenyExtra is an installation EXTRA path-prefix denylist (F2, §9.2a) that
	// AUGMENTS — never replaces — the shipped, always-on protected set (CI config,
	// lockfiles, secrets, Flowbee's own source). Any diff touching a configured prefix
	// is forced to the human gate. Set via FLOWBEE_CONTENT_DENY_EXTRA (comma-separated).
	ContentDenyExtra []string `yaml:"content_deny_extra"`
}

// ContentPolicy projects the content-integrity knobs into the content package's
// operator Policy (F2). The zero config yields the zero Policy = shipped defaults.
func (c Config) ContentPolicy() content.Policy {
	return content.Policy{
		Limits: content.Limits{
			MaxDiffBytes:    c.ContentMaxDiffBytes,
			MaxChangedFiles: c.ContentMaxChangedFiles,
		},
		ExtraDenyPrefixes: c.ContentDenyExtra,
	}
}

func Default() Config {
	return Config{
		DatabaseURL:        "flowbee.db",
		PrivateAddr:        ":7070",
		HealthAddr:         ":7001",
		WebhookAddr:        ":8443",
		LeaseTTLS:          300,
		HeartbeatIntervalS: 30,
		LongPollWaitS:      30,
		RiverMaxWorkers:    10,
		LogLevel:           "info",
		NoEligibleWorkerS:  120,
		AuthLoopbackBypass: true,
	}
}

// NoEligibleWorker is the alarm window as a duration.
func (c Config) NoEligibleWorker() time.Duration {
	return time.Duration(c.NoEligibleWorkerS) * time.Second
}

// Load reads defaults, then flowbee.yaml (or $FLOWBEE_CONFIG), then FLOWBEE_* env
// overrides, then validates.
func Load() (Config, error) {
	c := Default()

	path := os.Getenv("FLOWBEE_CONFIG")
	if path == "" {
		if _, err := os.Stat("flowbee.yaml"); err == nil {
			path = "flowbee.yaml"
		}
	}
	if path != "" {
		b, err := os.ReadFile(path)
		if err != nil {
			return c, fmt.Errorf("read config %s: %w", path, err)
		}
		if err := yaml.Unmarshal(b, &c); err != nil {
			return c, fmt.Errorf("parse config %s: %w", path, err)
		}
	}

	applyEnv(&c)
	if err := c.Validate(); err != nil {
		return c, err
	}
	return c, nil
}

func applyEnv(c *Config) {
	if v := os.Getenv("FLOWBEE_DATABASE_URL"); v != "" {
		c.DatabaseURL = v
	}
	if v := os.Getenv("FLOWBEE_PRIVATE_ADDR"); v != "" {
		c.PrivateAddr = v
	}
	if v := os.Getenv("FLOWBEE_HEALTH_ADDR"); v != "" {
		c.HealthAddr = v
	}
	if v := os.Getenv("FLOWBEE_WEBHOOK_ADDR"); v != "" {
		c.WebhookAddr = v
	}
	if v := os.Getenv("FLOWBEE_LOG_LEVEL"); v != "" {
		c.LogLevel = v
	}
	if v := envInt("FLOWBEE_LEASE_TTL_S"); v > 0 {
		c.LeaseTTLS = v
	}
	if v := envInt("FLOWBEE_HEARTBEAT_INTERVAL_S"); v > 0 {
		c.HeartbeatIntervalS = v
	}
	if v := envInt("FLOWBEE_LONG_POLL_WAIT_S"); v > 0 {
		c.LongPollWaitS = v
	}
	if v := envInt("FLOWBEE_RIVER_MAX_WORKERS"); v > 0 {
		c.RiverMaxWorkers = v
	}
	if v := envInt("FLOWBEE_NO_ELIGIBLE_WORKER_S"); v > 0 {
		c.NoEligibleWorkerS = v
	}
	if v := os.Getenv("FLOWBEE_WORKER_AUTH_SECRET"); v != "" {
		c.WorkerAuthSecret = v
	}
	if v := os.Getenv("FLOWBEE_ENROLLED_IDENTITIES"); v != "" {
		c.EnrolledIdentities = splitCSV(v)
	}
	if v := os.Getenv("FLOWBEE_AUTH_LOOPBACK_BYPASS"); v != "" {
		c.AuthLoopbackBypass = v == "1" || v == "true"
	}
	if v := os.Getenv("FLOWBEE_ALLOW_SELF_MERGE"); v != "" {
		c.AllowSelfMerge = v == "1" || v == "true"
	}
	if v := envInt("FLOWBEE_CONTENT_MAX_DIFF_BYTES"); v > 0 {
		c.ContentMaxDiffBytes = v
	}
	if v := envInt("FLOWBEE_CONTENT_MAX_CHANGED_FILES"); v > 0 {
		c.ContentMaxChangedFiles = v
	}
	if v := os.Getenv("FLOWBEE_CONTENT_DENY_EXTRA"); v != "" {
		c.ContentDenyExtra = splitCSV(v)
	}
}

func splitCSV(v string) []string {
	var out []string
	for _, p := range strings.Split(v, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func envInt(key string) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return 0
}

// Validate enforces DESIGN invariants, notably §6.3.3: TTL = k*heartbeat, k>=3.
func (c Config) Validate() error {
	if c.DatabaseURL == "" {
		return errors.New("database_url is required")
	}
	if c.HeartbeatIntervalS <= 0 {
		return errors.New("heartbeat_interval_s must be > 0")
	}
	if c.LeaseTTLS < 3*c.HeartbeatIntervalS {
		return fmt.Errorf("lease_ttl_s (%d) must be >= 3*heartbeat_interval_s (%d) per DESIGN §6.3.3",
			c.LeaseTTLS, 3*c.HeartbeatIntervalS)
	}
	return nil
}

func (c Config) LeaseTTL() time.Duration { return time.Duration(c.LeaseTTLS) * time.Second }
func (c Config) HeartbeatInterval() time.Duration {
	return time.Duration(c.HeartbeatIntervalS) * time.Second
}
func (c Config) LongPollWait() time.Duration { return time.Duration(c.LongPollWaitS) * time.Second }
