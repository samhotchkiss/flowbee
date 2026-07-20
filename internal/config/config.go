// Package config loads Flowbee's typed configuration from an optional YAML file
// plus FLOWBEE_* environment overrides, and validates invariants.
package config

import (
	"encoding/json"
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
	// WorkerAttestations is the production authorization policy for capabilities
	// claimed by each enrolled worker. Authentication proves who called; this map
	// separately bounds what that identity may do. Values use the scheduler's
	// canonical role:/model_family:/tool: capability strings. An enrolled identity
	// omitted from this map may authenticate (capacity collectors need that) but
	// attests no scheduling capability. FLOWBEE_WORKER_ATTESTATIONS_JSON is the
	// environment form, for example {"reviewer-russ":["role:code_reviewer",
	// "model_family:grok"]}.
	WorkerAttestations map[string][]string `yaml:"worker_attestations"`
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

	// RequiredReviewers is the F5 multi-reviewer consensus size: how many DISTINCT
	// reviewers must approve at the current head before a verdict mints (all-must-pass).
	// 0 or 1 (the default) = the single-reviewer gate (first approval mints) — unchanged.
	// N>1 makes an approval below N re-arm the job for the next distinct reviewer; the Nth
	// approval mints. A changes_requested (any-veto) or a SHA move resets the round. Set via
	// FLOWBEE_REQUIRED_REVIEWERS.
	RequiredReviewers int `yaml:"required_reviewers"`

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

	// BackupIntervalS controls the control plane's built-in auto-backup loop: `flowbee
	// serve` takes a verified, pruned VACUUM-INTO snapshot of the DB every interval, so an
	// operator gets the on-disk durability floor with ZERO extra services (no cron, no
	// litestream) — the orchestrator backs itself up. 0 (unset) => default 6h. A NEGATIVE
	// value DISABLES auto-backup (the operator runs their own cron/litestream). Set via
	// FLOWBEE_BACKUP_INTERVAL_S. (Litestream to object storage is still the off-disk
	// production answer; this is the floor — see docs/operating.md §6.)
	BackupIntervalS int `yaml:"backup_interval_s"`
	// BackupKeep is how many recent snapshots the auto-backup loop (and `flowbee backup`)
	// retain in the backup dir, pruning older ones. 0 (unset) => 7. Set via
	// FLOWBEE_BACKUP_KEEP.
	BackupKeep int `yaml:"backup_keep"`

	// SelfUnblockDisabled is the kill-switch for the self-unblock janitor (0023): the
	// forward-progress watchdog's sibling that auto-requeues MECHANICALLY-stuck jobs
	// (currently `stall`) out of the needs_human sink, bounded + breaker-gated, so a
	// transient stall no longer needs an operator to run `flowbee requeue`. Default false
	// (enabled). Set FLOWBEE_SELF_UNBLOCK to a falsey value (0/false/off/no) to disable and
	// restore the old operator-only behavior — the reversible switch every rung ships with.
	SelfUnblockDisabled bool `yaml:"self_unblock_disabled"`

	// SessionWatchDisabled is the kill-switch for the goal-session watchdog (epic-lane
	// Phase 1, 0025_goal_sessions.sql): the 2-minute-tick poller that watches registered
	// tmux "goal" sessions (long-running codex CLI agents), self-serves `/goal resume`
	// after a usage-limit window resets, and flags an operator for anything it must not
	// touch on its own (infra breakage, a real weekly cap, 3-strikes rate limiting).
	// Default false (enabled). Set FLOWBEE_SESSION_WATCH to a falsey value (0/false/off/no)
	// to disable — mirrors FLOWBEE_SELF_UNBLOCK's reversible-switch convention exactly.
	SessionWatchDisabled bool `yaml:"session_watch_disabled"`

	// EpicSupervisionDisabled is the kill-switch for the consolidated epic-supervision
	// ticker (epic-lane Phase 6b, plan §12.2): the ONE 2-minute-tick batch that classifies
	// each epic pane, produces/auto-resolves attention items, reaps stranded launches +
	// expired leases, recovers crash-window deliveries, runs the send-and-ack loop, reaps
	// dead masters, and pings an idle master (plan §1/§12.3/§15.10). Default false (enabled).
	// Set FLOWBEE_EPIC_SUPERVISION to a falsey value (0/false/off/no) to disable — mirrors
	// FLOWBEE_SESSION_WATCH's reversible-switch convention exactly.
	EpicSupervisionDisabled bool `yaml:"epic_supervision_disabled"`

	// AdvisorEnabled turns on Rung E: the read-only, single-shot LLM advisor consulted for a
	// job the deterministic janitor could not rescue (a stall past its mechanical unblock
	// cap). It NOMINATES an action {PLAN,CORRECTION,REPROMPT,STOP}; the store re-authorizes.
	// OFF by default (it spends model budget + needs an agent CLI on the serve box). Enable
	// via FLOWBEE_ADVISOR=on. AdvisorCmd overrides the CLI (default `claude -p`, or set the
	// codex form on a codex box) via FLOWBEE_ADVISOR_CMD.
	AdvisorEnabled bool   `yaml:"advisor_enabled"`
	AdvisorCmd     string `yaml:"advisor_cmd"`

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

	// BootstrapProjects is the explicit origin→project admission map used only by
	// the no-argument bootstrap path. A repository id is never assumed to also be
	// a project id. Every lifecycle/workspace/capacity identity is operator data;
	// bootstrap has no model, path, account, or seat defaults.
	BootstrapProjects []BootstrapProjectConfig `yaml:"bootstrap_projects"`
}

type BootstrapProjectConfig struct {
	ProjectID     string                      `yaml:"project_id"`
	Name          string                      `yaml:"name"`
	RepositoryIDs []string                    `yaml:"repository_ids"`
	ControlPlane  BootstrapControlPlaneConfig `yaml:"control_plane"`
	Interactor    BootstrapInteractorConfig   `yaml:"interactor"`
	Orchestrator  BootstrapOrchestratorConfig `yaml:"orchestrator"`
	LocalSeats    []BootstrapSeatConfig       `yaml:"local_seats"`
}

// BootstrapControlPlaneConfig is the exact Driver-owned lifecycle target for
// the Flowbee server. The Driver profile owns argv and executable selection;
// Flowbee supplies only stable lifecycle/workspace identity and the reserved
// human-facing presentation name through the v3 Ensure contract.
type BootstrapControlPlaneConfig struct {
	InstanceRef           string `yaml:"instance_ref"`
	LifecycleKey          string `yaml:"lifecycle_key"`
	TargetEpoch           int64  `yaml:"target_epoch"`
	ProfileID             string `yaml:"profile_id"`
	WorkspaceRootID       string `yaml:"workspace_root_id"`
	WorkspaceRelativePath string `yaml:"workspace_relative_path"`
	TmuxServerInstanceID  string `yaml:"tmux_server_instance_id"`
}

type BootstrapInteractorConfig struct {
	ActorID                       string `yaml:"actor_id"`
	PresentationName              string `yaml:"presentation_name"`
	Operation                     string `yaml:"operation"`
	InstanceRef                   string `yaml:"instance_ref"`
	LifecycleKey                  string `yaml:"lifecycle_key"`
	TargetEpoch                   int64  `yaml:"target_epoch"`
	ProfileID                     string `yaml:"profile_id"`
	ExternalWatchID               string `yaml:"external_watch_id"`
	ExistingSessionID             string `yaml:"existing_session_id"`
	ExpectedPaneInstanceID        string `yaml:"expected_pane_instance_id"`
	ExpectedAgentRunID            string `yaml:"expected_agent_run_id"`
	WorkspaceRootID               string `yaml:"workspace_root_id"`
	WorkspaceRelativePath         string `yaml:"workspace_relative_path"`
	RecoveryProfileID             string `yaml:"recovery_profile_id"`
	RecoveryWorkspaceRootID       string `yaml:"recovery_workspace_root_id"`
	RecoveryWorkspaceRelativePath string `yaml:"recovery_workspace_relative_path"`
	TmuxServerInstanceID          string `yaml:"tmux_server_instance_id"`
}

type BootstrapOrchestratorConfig struct {
	ActorID               string `yaml:"actor_id"`
	PresentationName      string `yaml:"presentation_name"`
	InstanceRef           string `yaml:"instance_ref"`
	LifecycleKey          string `yaml:"lifecycle_key"`
	TargetEpoch           int64  `yaml:"target_epoch"`
	ProfileID             string `yaml:"profile_id"`
	WorkspaceRootID       string `yaml:"workspace_root_id"`
	WorkspaceRelativePath string `yaml:"workspace_relative_path"`
	TmuxServerInstanceID  string `yaml:"tmux_server_instance_id"`
}

type BootstrapSeatConfig struct {
	SeatID string `yaml:"seat_id"`
	// Box is intentionally required empty for the local-only first release.
	// HostID is Driver's authenticated stable host identity, never an SSH alias.
	Box                   string `yaml:"box"`
	HostID                string `yaml:"host_id"`
	AgentFamily           string `yaml:"agent_family"`
	ConfigDir             string `yaml:"config_dir"`
	CodexHome             string `yaml:"codex_home"`
	MaxConcurrent         int    `yaml:"max_concurrent"`
	AccountKey            string `yaml:"account_key"`
	CredentialLineage     string `yaml:"credential_lineage"`
	ReservePct            int    `yaml:"reserve_pct"`
	AccountMaximum        int    `yaml:"account_maximum"`
	InstanceRef           string `yaml:"instance_ref"`
	TmuxServerDomainID    string `yaml:"tmux_server_domain_id"`
	TmuxServerInstanceID  string `yaml:"tmux_server_instance_id"`
	ProfileID             string `yaml:"profile_id"`
	WorkspaceRootID       string `yaml:"workspace_root_id"`
	WorkspaceRelativeBase string `yaml:"workspace_relative_base"`
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
	// ArchiveHistory opts this repo into the §F durable history archive: on every merge,
	// Flowbee lands docs/history/<id>.md + a regenerated TOC on the integration branch
	// (the "compounding memory" — in-repo provenance of how each issue was built). Default
	// false: it commits to the repo's main on every merge, so enable it only for a repo
	// whose owner wants that. Set via `archive_history: true` in the repo's registry entry.
	ArchiveHistory bool `yaml:"archive_history"`
	// RequiredReviewers overrides the global RequiredReviewers (F5 consensus panel) for THIS
	// repo: how many DISTINCT reviewers must approve before a verdict mints. 0 = inherit the
	// global setting. Lets one repo run an N-reviewer panel while others stay single-reviewer.
	RequiredReviewers int `yaml:"required_reviewers"`
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

	if err := applyEnv(&c); err != nil {
		return c, err
	}
	if err := c.Validate(); err != nil {
		return c, err
	}
	return c, nil
}

func applyEnv(c *Config) error {
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
	if v := os.Getenv("FLOWBEE_WORKER_ATTESTATIONS_JSON"); v != "" {
		var policy map[string][]string
		if err := json.Unmarshal([]byte(v), &policy); err != nil {
			return fmt.Errorf("parse FLOWBEE_WORKER_ATTESTATIONS_JSON: %w", err)
		}
		c.WorkerAttestations = policy
	}
	if v := os.Getenv("FLOWBEE_AUTH_LOOPBACK_BYPASS"); v != "" {
		c.AuthLoopbackBypass = v == "1" || v == "true"
	}
	if v := os.Getenv("FLOWBEE_ALLOW_SELF_MERGE"); v != "" {
		c.AllowSelfMerge = v == "1" || v == "true"
	}
	if v := envInt("FLOWBEE_REQUIRED_REVIEWERS"); v > 0 {
		c.RequiredReviewers = v
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
	// backup overrides parse the raw string (not envInt) so a NEGATIVE value can disable
	// auto-backup — envInt's >0 convention can't express "explicitly off".
	if v := os.Getenv("FLOWBEE_BACKUP_INTERVAL_S"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.BackupIntervalS = n
		}
	}
	if v := envInt("FLOWBEE_BACKUP_KEEP"); v > 0 {
		c.BackupKeep = v
	}
	// self-unblock kill-switch: any falsey value disables the janitor (a truthy/empty value
	// leaves it enabled, the default). Parsed as a string so "off" is expressible.
	if v := os.Getenv("FLOWBEE_SELF_UNBLOCK"); v != "" {
		switch strings.ToLower(strings.TrimSpace(v)) {
		case "0", "false", "off", "no", "disable", "disabled":
			c.SelfUnblockDisabled = true
		default:
			c.SelfUnblockDisabled = false
		}
	}
	// session-watch kill-switch: same falsey-string convention as FLOWBEE_SELF_UNBLOCK.
	if v := os.Getenv("FLOWBEE_SESSION_WATCH"); v != "" {
		switch strings.ToLower(strings.TrimSpace(v)) {
		case "0", "false", "off", "no", "disable", "disabled":
			c.SessionWatchDisabled = true
		default:
			c.SessionWatchDisabled = false
		}
	}
	// epic-supervision kill-switch: same falsey-string convention.
	if v := os.Getenv("FLOWBEE_EPIC_SUPERVISION"); v != "" {
		switch strings.ToLower(strings.TrimSpace(v)) {
		case "0", "false", "off", "no", "disable", "disabled":
			c.EpicSupervisionDisabled = true
		default:
			c.EpicSupervisionDisabled = false
		}
	}
	// Rung-E advisor is opt-in: any truthy value enables it.
	if v := os.Getenv("FLOWBEE_ADVISOR"); v != "" {
		switch strings.ToLower(strings.TrimSpace(v)) {
		case "1", "true", "on", "yes", "enable", "enabled":
			c.AdvisorEnabled = true
		default:
			c.AdvisorEnabled = false
		}
	}
	if v := os.Getenv("FLOWBEE_ADVISOR_CMD"); v != "" {
		c.AdvisorCmd = v
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
	return nil
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
	enrolledFamilies := make(map[string]string, len(c.EnrolledIdentities))
	for _, entry := range c.EnrolledIdentities {
		id, family, _ := strings.Cut(strings.TrimSpace(entry), ":")
		if id == "" {
			return errors.New("enrolled_identities contains an empty identity")
		}
		if prior, exists := enrolledFamilies[id]; exists && prior != family {
			return fmt.Errorf("enrolled identity %q declares conflicting model families", id)
		}
		enrolledFamilies[id] = family
	}
	for identity, capabilities := range c.WorkerAttestations {
		identity = strings.TrimSpace(identity)
		family, enrolled := enrolledFamilies[identity]
		if identity == "" || !enrolled {
			return fmt.Errorf("worker_attestations identity %q is not enrolled", identity)
		}
		seenCaps := map[string]bool{}
		for _, capability := range capabilities {
			capability = strings.TrimSpace(capability)
			if capability == "" || (!strings.HasPrefix(capability, "role:") &&
				!strings.HasPrefix(capability, "model_family:") && !strings.HasPrefix(capability, "tool:")) {
				return fmt.Errorf("worker_attestations[%q] has invalid capability %q", identity, capability)
			}
			if strings.HasSuffix(capability, ":") || seenCaps[capability] {
				return fmt.Errorf("worker_attestations[%q] has empty or duplicate capability %q", identity, capability)
			}
			seenCaps[capability] = true
			if family != "" && strings.HasPrefix(capability, "model_family:") &&
				capability != "model_family:"+family {
				return fmt.Errorf("worker_attestations[%q] model family conflicts with enrolled identity family %q", identity, family)
			}
		}
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
	bootstrapProjects, bootstrapRepos := map[string]bool{}, map[string]string{}
	configuredRepos := map[string]RepoConfig{}
	for _, repo := range c.Repos {
		configuredRepos[repo.ID] = repo
	}
	if len(c.Repos) == 0 && c.GithubOwner != "" && c.GithubRepo != "" {
		configuredRepos[c.GithubRepo] = RepoConfig{ID: c.GithubRepo, Owner: c.GithubOwner, Repo: c.GithubRepo}
	}
	for i, project := range c.BootstrapProjects {
		prefix := fmt.Sprintf("bootstrap_projects[%d]", i)
		if project.ProjectID == "" || project.Name == "" || bootstrapProjects[project.ProjectID] {
			return fmt.Errorf("%s: unique project_id and name are required", prefix)
		}
		bootstrapProjects[project.ProjectID] = true
		if len(project.RepositoryIDs) == 0 {
			return fmt.Errorf("%s: repository_ids is required", prefix)
		}
		seenProjectRepo := map[string]bool{}
		for _, repoID := range project.RepositoryIDs {
			repo, exists := configuredRepos[repoID]
			if repoID == "" || !exists || !repo.IsActive() || seenProjectRepo[repoID] {
				return fmt.Errorf("%s: repository %q must be unique, configured, and active", prefix, repoID)
			}
			seenProjectRepo[repoID] = true
			if prior := bootstrapRepos[repoID]; prior != "" && prior != project.ProjectID {
				return fmt.Errorf("%s: repository %q is ambiguously mapped to projects %q and %q", prefix, repoID, prior, project.ProjectID)
			}
			bootstrapRepos[repoID] = project.ProjectID
		}
		cp := project.ControlPlane
		if cp.InstanceRef == "" || cp.LifecycleKey == "" || cp.TargetEpoch < 1 ||
			cp.ProfileID != "flowbee_control" || cp.WorkspaceRootID == "" ||
			cp.WorkspaceRelativePath == "" || cp.TmuxServerInstanceID == "" {
			return fmt.Errorf("%s: control_plane requires exact managed endpoint/lifecycle/workspace identity and profile flowbee_control", prefix)
		}
		i := project.Interactor
		commonInteractor := i.ActorID != "" && i.InstanceRef != "" && i.LifecycleKey != "" &&
			i.TargetEpoch > 0 && i.ProfileID != "" && i.TmuxServerInstanceID != "" &&
			i.PresentationName == project.ProjectID+"-interactor"
		switch i.Operation {
		case "adopt":
			if !commonInteractor || i.ExternalWatchID == "" || i.ExistingSessionID == "" ||
				i.ExpectedPaneInstanceID == "" || i.ExpectedAgentRunID == "" ||
				i.WorkspaceRootID != "" || i.WorkspaceRelativePath != "" ||
				i.RecoveryProfileID != "claude_interactor_managed" || i.RecoveryWorkspaceRootID == "" ||
				i.RecoveryWorkspaceRelativePath == "" {
				return fmt.Errorf("%s: Interactor adopt requires exact external watch/session/pane/run authority, a managed v3 recovery workspace, no launch workspace, and reserved presentation_name %q",
					prefix, project.ProjectID+"-interactor")
			}
		case "ensure":
			if !commonInteractor || i.ProfileID != "claude_interactor_managed" ||
				i.WorkspaceRootID == "" || i.WorkspaceRelativePath == "" || i.ExternalWatchID != "" ||
				i.ExistingSessionID != "" || i.ExpectedPaneInstanceID != "" || i.ExpectedAgentRunID != "" ||
				i.RecoveryProfileID != "" || i.RecoveryWorkspaceRootID != "" || i.RecoveryWorkspaceRelativePath != "" {
				return fmt.Errorf("%s: Interactor ensure requires exact managed workspace authority, profile claude_interactor_managed, no adopt identity, and reserved presentation_name %q",
					prefix, project.ProjectID+"-interactor")
			}
		default:
			return fmt.Errorf("%s: Interactor operation must be adopt or ensure", prefix)
		}
		o := project.Orchestrator
		if o.ActorID == "" || o.PresentationName != project.ProjectID+"-orchestrator" || o.InstanceRef == "" ||
			o.LifecycleKey == "" || o.TargetEpoch < 1 || o.ProfileID != "codex_orchestrator" || o.WorkspaceRootID == "" ||
			o.WorkspaceRelativePath == "" || o.TmuxServerInstanceID == "" {
			return fmt.Errorf("%s: Orchestrator requires exact actor/presentation/endpoint/lifecycle/workspace identity and profile codex_orchestrator", prefix)
		}
		seatIDs := map[string]bool{}
		var builderFamilies, reviewerFamilies []string
		for j, seat := range project.LocalSeats {
			validFamilyPath := seat.AgentFamily == "codex" && seat.CodexHome != "" && seat.ConfigDir == "" ||
				(seat.AgentFamily == "claude" || seat.AgentFamily == "grok") && seat.ConfigDir != "" && seat.CodexHome == ""
			path := seat.ConfigDir
			if seat.CodexHome != "" {
				path = seat.CodexHome
			}
			workerRoleProfile := seat.AgentFamily + "_builder"
			if seat.ProfileID == seat.AgentFamily+"_reviewer" {
				workerRoleProfile = seat.AgentFamily + "_reviewer"
			}
			if seat.SeatID == "" || seatIDs[seat.SeatID] || seat.Box != "" || !canonicalUUIDPattern.MatchString(seat.HostID) ||
				!validFamilyPath || !filepath.IsAbs(path) || seat.MaxConcurrent < 1 || seat.AccountKey == "" ||
				seat.CredentialLineage == "" || seat.ReservePct < 0 || seat.AccountMaximum < 1 ||
				seat.InstanceRef == "" || seat.TmuxServerDomainID != "flowbee" || seat.TmuxServerInstanceID == "" ||
				seat.ProfileID != workerRoleProfile || seat.WorkspaceRootID == "" || seat.WorkspaceRelativeBase == "" ||
				filepath.IsAbs(seat.WorkspaceRelativeBase) || filepath.Clean(seat.WorkspaceRelativeBase) != seat.WorkspaceRelativeBase ||
				strings.HasPrefix(seat.WorkspaceRelativeBase, ".."+string(filepath.Separator)) {
				return fmt.Errorf("%s.local_seats[%d]: exact unique seat, runtime, capacity, and Driver target identity required", prefix, j)
			}
			seatIDs[seat.SeatID] = true
			if strings.HasSuffix(seat.ProfileID, "_reviewer") {
				reviewerFamilies = append(reviewerFamilies, seat.AgentFamily)
			} else {
				builderFamilies = append(builderFamilies, seat.AgentFamily)
			}
		}
		if len(builderFamilies) == 0 || len(reviewerFamilies) == 0 {
			return fmt.Errorf("%s: local_seats requires exact builder and reviewer profile pools", prefix)
		}
		for _, builderFamily := range builderFamilies {
			distinct := false
			for _, reviewerFamily := range reviewerFamilies {
				distinct = distinct || reviewerFamily != builderFamily
			}
			if !distinct {
				return fmt.Errorf("%s: every builder family requires a distinct reviewer family", prefix)
			}
		}
	}
	return nil
}

func (c Config) LeaseTTL() time.Duration { return time.Duration(c.LeaseTTLS) * time.Second }
func (c Config) HeartbeatInterval() time.Duration {
	return time.Duration(c.HeartbeatIntervalS) * time.Second
}
func (c Config) LongPollWait() time.Duration { return time.Duration(c.LongPollWaitS) * time.Second }

// defaultBackupIntervalS is the auto-backup cadence when unset: every 6h.
const defaultBackupIntervalS = 6 * 3600

// BackupInterval resolves the effective auto-backup cadence and whether it's enabled. A
// negative BackupIntervalS disables the loop (operator runs their own backups); 0 means the
// 6h default; any positive value is honored with a 60s floor so a typo can't busy-loop.
func (c Config) BackupInterval() (time.Duration, bool) {
	if c.BackupIntervalS < 0 {
		return 0, false
	}
	s := c.BackupIntervalS
	if s == 0 {
		s = defaultBackupIntervalS
	}
	if s < 60 {
		s = 60
	}
	return time.Duration(s) * time.Second, true
}

// BackupKeepN is how many snapshots the auto-backup loop retains (0/unset => 7).
func (c Config) BackupKeepN() int {
	if c.BackupKeep <= 0 {
		return 7
	}
	return c.BackupKeep
}
