// Package config loads Flowbee's typed configuration from an optional YAML file
// plus FLOWBEE_* environment overrides, and validates invariants.
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/samhotchkiss/flowbee/internal/content"
	"gopkg.in/yaml.v3"
)

// defaultDBPath is the standard single-file DB location, ~/.flowbee/flowbee.db —
// matching the ~/.flowbee/ convention used for mirrors and config. Using this as the
// default (rather than a cwd-relative "flowbee.db") means a CLI command like `flowbee
// board` finds the live control-plane DB on the host without FLOWBEE_CONFIG set,
// instead of silently creating an empty ./flowbee.db and erroring "no such table".
func defaultDBPath() string {
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		return filepath.Join(home, ".flowbee", "flowbee.db")
	}
	return "flowbee.db"
}

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
	// An entry MAY bind the identity's model family as "identity:family" (e.g.
	// "reviewer-bob:claude-opus"). When bound, the control plane clamps that worker's
	// self-asserted model_family to the declared value, grounding the §5.5 anti-affinity
	// exclusion (a same-family reviewer can't rubber-stamp) in the credential instead of
	// the worker's word. A bare "identity" leaves model_family worker-asserted (legacy).
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

	// CostCeilingUSD is the optional per-job cost circuit-breaker (§6.7, I-15): when
	// > 0, every newly-metered job inherits it as a ceiling, and the FIRST worker
	// cost report whose accumulated total reaches it revokes the lease (epoch++) and
	// escalates the job to needs_human (over_budget). 0 (default) = no $ ceiling —
	// cost is still metered for the rollup, but a runaway job is bounded only by
	// attempts/bounces, never by spend. A per-job ceiling seeded at creation still
	// takes precedence. Dollars; converted to micro-USD (×1e6). Set via
	// FLOWBEE_COST_CEILING_USD.
	CostCeilingUSD float64 `yaml:"cost_ceiling_usd"`

	// GithubOwner / GithubRepo are the single-repo coordinates `flowbee init`
	// prefills from the git remote (F13). They are the config-file form of the
	// legacy FLOWBEE_GITHUB_OWNER/REPO env path: when Repos is empty, serve uses
	// these (env still overrides). Empty + no env + no Repos = no GitHub loops
	// (dev/CI with no creds). DefaultBranch defaults to "main".
	GithubOwner         string `yaml:"github_owner"`
	GithubRepo          string `yaml:"github_repo"`
	GithubDefaultBranch string `yaml:"github_default_branch"`

	// Repos is the F9 multi-repo registry: one control plane manages a SET of repos,
	// each with its own GitHub coords + integration branch + its own reconcile-IN /
	// project-OUT loop, over a SHARED, repo-agnostic worker fleet and a GLOBAL
	// scheduler. Empty falls back to the single-repo FLOWBEE_GITHUB_OWNER/REPO env
	// path (the legacy posture). Configured in flowbee.yaml only (a structured list).
	Repos []RepoConfig `yaml:"repos"`
}

// RepoConfig is one managed repo's coordinates in the F9 registry (build-list F9).
// ID is a short stable handle used to scope jobs; Owner/Repo are the GitHub coords;
// DefaultBranch is the integration branch (PR base + I-8 protection target); Token
// is an optional per-repo PAT env-var NAME (not the secret itself) — empty falls
// back to FLOWBEE_GITHUB_TOKEN (one shared operator PAT across repos is the common
// single-operator case).
type RepoConfig struct {
	ID            string `yaml:"id"`
	Owner         string `yaml:"owner"`
	Repo          string `yaml:"repo"`
	DefaultBranch string `yaml:"default_branch"`
	TokenEnv      string `yaml:"token_env"`
	// Active defaults to true; set false to register-but-park a repo.
	Active *bool `yaml:"active"`
	// AllowOwnSourceMerge relaxes the `flowbee_source` content-denylist class
	// (internal/, cmd/flowbee/, tools/, flows/, flowbee.yaml, content.go) for THIS
	// repo. That class exists to stop Flowbee autonomously merging changes to its OWN
	// control-plane source; it is correct ONLY for the repo that actually contains
	// Flowbee's source. For any OTHER managed repo those are the repo's own paths (most
	// Go repos have internal/ + cmd/), so leaving it on wrongly forces every such change
	// to the human gate. Set true for a managed repo that is NOT the Flowbee control
	// plane, so its own internal//cmd/ changes can self-merge. Default false = fully
	// protected (the control-plane-self posture; never relax the repo that IS Flowbee).
	// Universal classes (CI, lockfiles, dockerfiles, secrets) are NEVER relaxed.
	AllowOwnSourceMerge bool `yaml:"allow_own_source_merge"`
}

// IsActive reports whether the repo is active (default true when unset).
func (r RepoConfig) IsActive() bool { return r.Active == nil || *r.Active }

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

// CostCeilingMicroUSD projects the dollars-denominated config knob into the
// micro-USD unit the engine ceiling predicate (job.CostExceeded) speaks. 0 =>
// no default ceiling (per-job ceilings seeded at creation still apply).
func (c Config) CostCeilingMicroUSD() int64 {
	if c.CostCeilingUSD <= 0 {
		return 0
	}
	return int64(c.CostCeilingUSD * 1_000_000)
}

func Default() Config {
	return Config{
		DatabaseURL: defaultDBPath(),
		PrivateAddr: ":7070",
		HealthAddr:  ":7001",
		WebhookAddr: ":8443",
		// LeaseTTLS is also the absolute lease cap (Rung-3, un-gameable): a worker can
		// hold a lease at most this long, even while heartbeating. It MUST exceed a real
		// agent build's wall time, or a multi-minute build is revoked mid-run and its
		// pushed result fenced 409 (the #40 churn). 20 min covers real agent builds; a
		// crashed worker is still caught sooner by the soft heartbeat rungs.
		LeaseTTLS:          1200,
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
	if v := os.Getenv("FLOWBEE_COST_CEILING_USD"); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			c.CostCeilingUSD = f
		}
	}
	if v := os.Getenv("FLOWBEE_GITHUB_OWNER"); v != "" {
		c.GithubOwner = v
	}
	if v := os.Getenv("FLOWBEE_GITHUB_REPO"); v != "" {
		c.GithubRepo = v
	}
	if v := os.Getenv("FLOWBEE_GITHUB_DEFAULT_BRANCH"); v != "" {
		c.GithubDefaultBranch = v
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
	if c.CostCeilingUSD < 0 {
		return fmt.Errorf("cost_ceiling_usd (%.2f) must be >= 0", c.CostCeilingUSD)
	}
	// the F9 multi-repo registry: each repo needs a unique handle + GitHub coords, or it
	// silently fails at runtime (no mirror, no API URL — the loops just no-op). Catch the
	// typos (dup id, missing owner/repo, reserved id) HERE, before serve, not as a silent
	// dead repo in production.
	seen := map[string]bool{}
	for i, r := range c.Repos {
		id := strings.TrimSpace(r.ID)
		if id == "" {
			return fmt.Errorf("repos[%d]: id is required (the short stable handle that scopes jobs)", i)
		}
		if id == "default" {
			return fmt.Errorf("repos[%d]: id %q is reserved", i, id)
		}
		if seen[id] {
			return fmt.Errorf("repos: duplicate id %q — each repo needs a unique handle", id)
		}
		seen[id] = true
		if strings.TrimSpace(r.Owner) == "" || strings.TrimSpace(r.Repo) == "" {
			return fmt.Errorf("repos[%q]: owner and repo are required (the GitHub coords)", id)
		}
	}
	return nil
}

func (c Config) LeaseTTL() time.Duration { return time.Duration(c.LeaseTTLS) * time.Second }
func (c Config) HeartbeatInterval() time.Duration {
	return time.Duration(c.HeartbeatIntervalS) * time.Second
}
func (c Config) LongPollWait() time.Duration { return time.Duration(c.LongPollWaitS) * time.Second }
