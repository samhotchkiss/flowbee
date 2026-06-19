package onboarding

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/samhotchkiss/flowbee/internal/config"
	gh "github.com/samhotchkiss/flowbee/internal/github"
	"gopkg.in/yaml.v3"
)

// CheckStatus is one diagnostic's outcome.
type CheckStatus string

const (
	StatusPass CheckStatus = "pass"
	StatusWarn CheckStatus = "warn" // non-fatal (e.g. GitHub skipped offline)
	StatusFail CheckStatus = "fail"
)

// Check is one named diagnostic doctor ran.
type Check struct {
	Name   string
	Status CheckStatus
	Detail string
}

// DoctorReport is the full set of checks. Green reports whether doctor is happy:
// no fails (warnings are allowed — they are the skippable/offline cases).
type DoctorReport struct {
	Checks []Check
}

// Green is true when no check failed. Warnings (skipped reachability offline) do
// not break green — that is the "skippable offline" contract.
func (r DoctorReport) Green() bool {
	for _, c := range r.Checks {
		if c.Status == StatusFail {
			return false
		}
	}
	return true
}

func (r *DoctorReport) add(name string, st CheckStatus, detail string) {
	r.Checks = append(r.Checks, Check{Name: name, Status: st, Detail: detail})
}

// GitHubProbe is the reachability check doctor runs. It is an interface so tests
// inject the in-memory fakeGitHub (NO real creds, NO network) and the CLI injects
// a RealClient. Returning an error => unreachable (fail); nil => reachable (pass).
type GitHubProbe interface {
	BoardSweep(ctx context.Context) (gh.BoardSnapshot, error)
}

// preflighter is the optional deployment-sanity capability (satisfied by the
// RealClient). When the probe implements it, doctor also checks the three misconfigs
// that otherwise silently stall a real run: token write-scope, CI presence, and
// branch protection.
type preflighter interface {
	Preflight(ctx context.Context, branch string) (gh.Preflight, error)
}

// DoctorOptions configures one doctor run. Root is the repo to inspect. Probe is
// the optional GitHub reachability prober (nil + SkipGitHub=false => doctor builds
// a RealClient from FLOWBEE_GITHUB_TOKEN, or warns if no token). SkipGitHub forces
// the reachability check to a warn (the offline path: `flowbee doctor --offline`).
type DoctorOptions struct {
	Root string
	// ConfigPath is the EXACT config file to validate (what `serve` uses via
	// FLOWBEE_CONFIG). When set it wins over Root — doctor reads this file and resolves
	// flows/ next to it — so doctor validates the SAME config serve runs, not a stray
	// cwd/flowbee.yaml. Empty => <Root>/flowbee.yaml (the scaffold/quickstart path).
	ConfigPath string
	Probe      GitHubProbe
	SkipGitHub bool
	// ProbeTimeout bounds a real reachability call. 0 => 10s.
	ProbeTimeout time.Duration
}

// Doctor validates that the scaffolded repo is sound: config parses + passes its
// invariants, the flow files exist and reference identities that exist (with their
// lens files), and GitHub is reachable (or explicitly skipped/warned). It never
// mutates the repo. The returned report is Green when nothing failed.
func Doctor(ctx context.Context, opts DoctorOptions) (DoctorReport, error) {
	var rep DoctorReport
	root := opts.Root

	// honor FLOWBEE_CONFIG (what serve uses): validate the EXACT config file serve runs
	// and resolve flows/ next to it, instead of a possibly-stray <cwd>/flowbee.yaml.
	configPath := opts.ConfigPath
	if configPath == "" {
		configPath = filepath.Join(root, "flowbee.yaml")
	} else {
		root = filepath.Dir(configPath)
	}

	// (1) config: present + parses + valid.
	cfg, _ := checkConfig(configPath, &rep)

	// (2) flows + identities: the flow files exist and every referenced identity
	// resolves to an identity yaml with an existing lens file.
	checkFlows(root, cfg, &rep)

	// (3) GitHub reachability (skippable offline).
	checkGitHub(ctx, opts, cfg, &rep)

	// (4) durability: a single-file SQLite control plane with no backup loses ALL
	// state on disk failure. Surface it at the readiness gate (WARN, not FAIL — it's a
	// production recommendation, not a start-up blocker).
	checkDurability(&rep)

	// (5) worker-API security posture: catch the serve-refuses-to-start misconfig (a
	// non-loopback bind with no auth and no insecure opt-in) at the readiness gate,
	// and surface an OPEN API even when it's the intended (trusted-tailnet) posture.
	checkWorkerAuth(cfg, &rep)

	return rep, nil
}

// checkWorkerAuth mirrors serve's §7.6 trust-boundary decision so an operator sees the
// outcome BEFORE `flowbee serve` enforces it: mutual-auth configured (pass), loopback-
// only (pass), a non-loopback bind with no auth and no FLOWBEE_INSECURE (FAIL — serve
// refuses to start), or an explicitly-open API on a trusted network (warn).
func checkWorkerAuth(cfg config.Config, rep *DoctorReport) {
	// serve resolves these from env too (config.Load), but doctor's cfg is the parsed
	// FILE — so apply the same env overrides here, or the check disagrees with what
	// `serve` will actually do.
	addr := cfg.PrivateAddr
	if v := os.Getenv("FLOWBEE_PRIVATE_ADDR"); v != "" {
		addr = v
	}
	if addr == "" {
		addr = ":7070"
	}
	authSecret := cfg.WorkerAuthSecret
	if v := os.Getenv("FLOWBEE_WORKER_AUTH_SECRET"); v != "" {
		authSecret = v
	}
	if authSecret != "" {
		rep.add("worker-auth", StatusPass,
			fmt.Sprintf("worker mutual-auth configured (%d enrolled identities, loopback_bypass=%v)",
				len(cfg.EnrolledIdentities), cfg.AuthLoopbackBypass))
		return
	}
	if isLoopbackBind(addr) {
		rep.add("worker-auth", StatusPass,
			"loopback-only bind ("+addr+") — no auth needed; set worker_auth_secret before binding non-loopback")
		return
	}
	if os.Getenv("FLOWBEE_INSECURE") == "" {
		// WARN, not FAIL: the scaffold is valid (a fresh init stays green) — but serve
		// enforces a security CHOICE before it will bind a non-loopback addr. Surface it
		// so the operator picks one instead of hitting the refusal at `serve`.
		rep.add("worker-auth", StatusWarn,
			fmt.Sprintf("`flowbee serve` will REFUSE to start until you choose a posture: worker API binds %s "+
				"(non-loopback) with NO auth — set worker_auth_secret (recommended), or FLOWBEE_INSECURE=1 on a "+
				"trusted tailnet to accept an open API", addr))
		return
	}
	rep.add("worker-auth", StatusWarn,
		fmt.Sprintf("worker API will be OPEN (no auth) on %s — relying on the network (e.g. Tailscale) as the "+
			"only trust boundary (self_merge=%v). Intended for a trusted tailnet; set worker_auth_secret otherwise",
			addr, cfg.AllowSelfMerge))
}

// isLoopbackBind mirrors serve's isLoopbackAddr: an addr is loopback-only when its host
// is 127.0.0.1/::1/localhost. ":7070" (host empty) binds ALL interfaces — NOT loopback.
func isLoopbackBind(addr string) bool {
	host := addr
	if i := strings.LastIndex(addr, ":"); i >= 0 {
		host = addr[:i]
	}
	switch host {
	case "127.0.0.1", "::1", "localhost":
		return true
	default:
		return false
	}
}

// checkDurability warns when no backup strategy is detected. It PASSES on either a
// Litestream config (the off-disk production answer) or a recent `flowbee backup`
// snapshot (the on-disk floor); otherwise it WARNs. WARN never breaks `green`.
func checkDurability(rep *DoctorReport) {
	for _, p := range []string{
		"/etc/litestream.yml", "/etc/litestream.yaml",
		filepath.Join(homeDir(), ".config", "litestream.yml"),
		filepath.Join(homeDir(), "litestream.yml"),
	} {
		if p != "" && fileExists(p) {
			rep.add("durability", StatusPass, "litestream config found ("+p+") — continuous off-disk replication")
			return
		}
	}
	dir := os.Getenv("FLOWBEE_BACKUP_DIR")
	if dir == "" && homeDir() != "" {
		dir = filepath.Join(homeDir(), ".flowbee", "backups")
	}
	if name, age, ok := recentSnapshot(dir); ok {
		rep.add("durability", StatusPass, fmt.Sprintf("recent backup snapshot (%s, %s old) — on-disk floor; litestream is the off-disk answer", name, age))
		return
	}
	rep.add("durability", StatusWarn, "no backup detected — a disk failure loses ALL state. Set up litestream "+
		"(off-disk, docs/operating.md §6) or schedule `flowbee backup` (on-disk floor)")
}

func homeDir() string {
	h, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return h
}

func fileExists(p string) bool {
	info, err := os.Stat(p)
	return err == nil && !info.IsDir()
}

// recentSnapshot returns the newest flowbee-*.db in dir and its rounded age, when one
// exists within the last 25h (a daily backup cadence with slack). ok=false otherwise.
func recentSnapshot(dir string) (name, age string, ok bool) {
	if dir == "" {
		return "", "", false
	}
	matches, err := filepath.Glob(filepath.Join(dir, "flowbee-*.db"))
	if err != nil || len(matches) == 0 {
		return "", "", false
	}
	var newest os.FileInfo
	var newestPath string
	for _, m := range matches {
		fi, err := os.Stat(m)
		if err != nil {
			continue
		}
		if newest == nil || fi.ModTime().After(newest.ModTime()) {
			newest, newestPath = fi, m
		}
	}
	if newest == nil {
		return "", "", false
	}
	since := time.Since(newest.ModTime())
	if since > 25*time.Hour {
		return "", "", false
	}
	return filepath.Base(newestPath), since.Round(time.Minute).String(), true
}

func checkConfig(path string, rep *DoctorReport) (config.Config, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		rep.add("config", StatusFail, fmt.Sprintf("config not found at %s (run `flowbee init`, or set FLOWBEE_CONFIG): %v", path, err))
		return config.Config{}, false
	}
	cfg := config.Default()
	if err := yaml.Unmarshal(b, &cfg); err != nil {
		rep.add("config", StatusFail, fmt.Sprintf("flowbee.yaml does not parse: %v", err))
		return config.Config{}, false
	}
	if err := cfg.Validate(); err != nil {
		rep.add("config", StatusFail, fmt.Sprintf("flowbee.yaml fails validation: %v", err))
		return cfg, false
	}
	rep.add("config", StatusPass, fmt.Sprintf("flowbee.yaml parses; lease_ttl_s=%d heartbeat=%d allow_self_merge=%v",
		cfg.LeaseTTLS, cfg.HeartbeatIntervalS, cfg.AllowSelfMerge))

	// cost-ceiling: informational — both armed and off are valid postures.
	if cfg.CostCeilingUSD > 0 {
		rep.add("cost-ceiling", StatusPass, fmt.Sprintf("$%.2f per job (over-budget jobs escalate to needs_human)", cfg.CostCeilingUSD))
	} else {
		rep.add("cost-ceiling", StatusPass, "off (jobs bounded by attempts/bounces only)")
	}

	// coords: a multi-repo registry is the production layout; single-repo coords (filled
	// or env) are the quickstart; neither is a warn (user must wire one before serve).
	switch {
	case len(cfg.Repos) > 0:
		ids := make([]string, 0, len(cfg.Repos))
		for _, r := range cfg.Repos {
			ids = append(ids, r.ID)
		}
		rep.add("repo-coords", StatusPass, fmt.Sprintf("multi-repo registry: %d repos (%s)", len(cfg.Repos), strings.Join(ids, ", ")))
	case cfg.GithubOwner != "" && cfg.GithubRepo != "":
		rep.add("repo-coords", StatusPass, fmt.Sprintf("%s/%s", cfg.GithubOwner, cfg.GithubRepo))
	case os.Getenv("FLOWBEE_GITHUB_OWNER") != "" && os.Getenv("FLOWBEE_GITHUB_REPO") != "":
		rep.add("repo-coords", StatusPass, "from FLOWBEE_GITHUB_OWNER/REPO env")
	default:
		rep.add("repo-coords", StatusWarn, "no repos configured — set a repos: registry, or github_owner/github_repo, before serve")
	}
	return cfg, true
}

// identityFile is the subset of a flows/identities/*.yaml doctor reads: the id and
// the lens path it points at.
type identityFile struct {
	ID   string `yaml:"id"`
	Lens string `yaml:"lens"`
}

// flowDoc is the subset of flows/default.yaml doctor walks to find every identity
// referenced by a stage or a reviewer fan-out.
type flowDoc struct {
	Stages map[string]struct {
		Identity  string `yaml:"identity"`
		Reviewers []struct {
			Identity string `yaml:"identity"`
		} `yaml:"reviewers"`
	} `yaml:"stages"`
}

func checkFlows(root string, cfg config.Config, rep *DoctorReport) {
	flowsDir := filepath.Join(root, "flows")
	defaultPath := filepath.Join(flowsDir, "default.yaml")
	fb, err := os.ReadFile(defaultPath)
	if err != nil {
		// A multi-repo control-plane config (repos: registry) legitimately ships NO
		// flows/ dir — it runs the embedded default flows. Only the single-repo
		// `flowbee init` layout must scaffold flows/, so missing-flows is a hard fail
		// THERE but an informational note for a multi-repo deployment.
		if len(cfg.Repos) > 0 {
			rep.add("flow", StatusWarn, "no flows/ dir — multi-repo control plane uses the embedded default flows (add flows/ to customize)")
			return
		}
		rep.add("flow", StatusFail, fmt.Sprintf("flows/default.yaml not found (run `flowbee init`): %v", err))
		return
	}
	var fd flowDoc
	if err := yaml.Unmarshal(fb, &fd); err != nil {
		rep.add("flow", StatusFail, fmt.Sprintf("flows/default.yaml does not parse: %v", err))
		return
	}

	// the identities the flow references.
	referenced := map[string]bool{}
	for _, st := range fd.Stages {
		if st.Identity != "" {
			referenced[st.Identity] = true
		}
		for _, rv := range st.Reviewers {
			if rv.Identity != "" {
				referenced[rv.Identity] = true
			}
		}
	}
	if len(referenced) == 0 {
		rep.add("flow", StatusFail, "flows/default.yaml references no identities")
		return
	}
	rep.add("flow", StatusPass, fmt.Sprintf("flows/default.yaml parses; references %d identities", len(referenced)))

	// every referenced identity must resolve to a yaml with an existing lens.
	var missing []string
	ids := make([]string, 0, len(referenced))
	for id := range referenced {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		idPath := filepath.Join(flowsDir, "identities", id+".yaml")
		ib, err := os.ReadFile(idPath)
		if err != nil {
			missing = append(missing, id+" (no identities/"+id+".yaml)")
			continue
		}
		var idf identityFile
		if err := yaml.Unmarshal(ib, &idf); err != nil {
			missing = append(missing, id+" (identity yaml does not parse)")
			continue
		}
		if idf.ID != id {
			missing = append(missing, fmt.Sprintf("%s (id field is %q)", id, idf.ID))
			continue
		}
		if idf.Lens != "" {
			lensPath := filepath.Join(flowsDir, filepath.FromSlash(idf.Lens))
			if _, err := os.Stat(lensPath); err != nil {
				missing = append(missing, id+" (lens "+idf.Lens+" missing)")
				continue
			}
		}
	}
	if len(missing) > 0 {
		rep.add("identities", StatusFail, "unresolved: "+strings.Join(missing, "; "))
		return
	}
	rep.add("identities", StatusPass, fmt.Sprintf("all %d flow identities exist with lenses", len(ids)))
}

func checkGitHub(ctx context.Context, opts DoctorOptions, cfg config.Config, rep *DoctorReport) {
	if opts.SkipGitHub {
		rep.add("github", StatusWarn, "skipped (offline): GitHub reachability not checked")
		return
	}
	timeout := opts.ProbeTimeout
	if timeout == 0 {
		timeout = 10 * time.Second
	}

	// The targets to preflight: every registered repo (F9 multi-repo registry — the
	// production layout), or the single-repo coords (legacy / `flowbee init` quickstart)
	// when no registry is configured. Each repo gets its OWN reachability + preflight, so
	// a multi-repo deployment is validated per repo instead of silently skipped.
	type target struct{ owner, repo, branch, label, tokenEnv string }
	var targets []target
	if len(cfg.Repos) > 0 {
		for _, r := range cfg.Repos {
			if !r.IsActive() {
				continue
			}
			br := r.DefaultBranch
			if br == "" {
				br = "main"
			}
			targets = append(targets, target{r.Owner, r.Repo, br, "github[" + r.ID + "]", r.TokenEnv})
		}
	} else {
		owner, repo := cfg.GithubOwner, cfg.GithubRepo
		if v := os.Getenv("FLOWBEE_GITHUB_OWNER"); v != "" {
			owner = v
		}
		if v := os.Getenv("FLOWBEE_GITHUB_REPO"); v != "" {
			repo = v
		}
		targets = append(targets, target{owner, repo, cfg.GithubDefaultBranch, "github", ""})
	}

	for _, t := range targets {
		probe := opts.Probe
		if probe == nil {
			token := os.Getenv("FLOWBEE_GITHUB_TOKEN")
			if t.tokenEnv != "" {
				if v := os.Getenv(t.tokenEnv); v != "" {
					token = v
				}
			}
			if token == "" {
				rep.add(t.label, StatusWarn, "no token (set FLOWBEE_GITHUB_TOKEN or the repo's token_env, or pass --offline)")
				continue
			}
			if t.owner == "" || t.repo == "" {
				rep.add(t.label, StatusWarn, "no repo coords; skipping reachability")
				continue
			}
			probe = gh.NewRealClient(t.owner, t.repo, func(context.Context) (string, error) { return token, nil })
		}
		cctx, cancel := context.WithTimeout(ctx, timeout)
		checkOneRepo(cctx, probe, t.branch, t.label, rep)
		cancel()
	}
}

// checkOneRepo runs the reachability + deployment preflight (token write-scope, CI,
// branch protection, token least-privilege) for a single repo, prefixing each check
// with `label` so a multi-repo report names which repo each result belongs to.
func checkOneRepo(cctx context.Context, probe GitHubProbe, branch, label string, rep *DoctorReport) {
	snap, err := probe.BoardSweep(cctx)
	if err != nil {
		rep.add(label, StatusFail, fmt.Sprintf("GitHub unreachable: %v (re-run with --offline to skip)", err))
		return
	}
	rep.add(label, StatusPass, fmt.Sprintf("reachable; rate-limit remaining=%d, board has %d PRs / %d issues",
		snap.RateLimit.Remaining, len(snap.PullRequests), len(snap.Issues)))

	pf, ok := probe.(preflighter)
	if !ok {
		return
	}
	pre, perr := pf.Preflight(cctx, branch)
	if perr != nil {
		rep.add(label+" write", StatusWarn, fmt.Sprintf("could not read repo permissions: %v", perr))
		return
	}
	if pre.CanWrite {
		rep.add(label+" write", StatusPass, "token can write (push branches / open + merge PRs / close issues)")
	} else {
		rep.add(label+" write", StatusFail, "token lacks WRITE — use a fine-grained PAT with Contents + Pull requests + Issues = write (read-only can't push or merge)")
	}
	switch {
	case pre.CITriggersOnPR:
		rep.add(label+" ci", StatusPass, "a workflow triggers on pull_request (Flowbee's merge gate can go green)")
	case pre.HasCI:
		rep.add(label+" ci", StatusWarn, "workflows exist but NONE trigger on pull_request — Flowbee gates the merge on green PR CI, so PRs will sit forever; add `on: pull_request` to a workflow")
	default:
		rep.add(label+" ci", StatusWarn, "no GitHub Actions workflow found — Flowbee merges ONLY on green CI, so nothing will merge until the repo reports a CI status check on PRs")
	}
	if pre.BranchProtected {
		rep.add(label+" protection", StatusWarn, "integration branch is protected — autonomous merge needs the token to satisfy the required checks, or turn protection off")
	} else {
		rep.add(label+" protection", StatusPass, "integration branch unprotected — autonomous merge OK")
	}
	if pre.TokenScopes != "" {
		rep.add(label+" token", StatusWarn, "broadly-scoped CLASSIC PAT (scopes: "+pre.TokenScopes+") — prefer a fine-grained PAT limited to Contents + Pull requests + Issues (least privilege)")
	} else if !pre.TokenScopesProbed {
		// the scope probe failed (transient/network) — scopes UNKNOWN. Do NOT report
		// least-privilege (a green-when-unknown false signal that could mask a broad PAT).
		rep.add(label+" token", StatusWarn, "could not read token scopes (probe failed) — unable to confirm least-privilege; re-run doctor")
	} else {
		rep.add(label+" token", StatusPass, "fine-grained / least-privilege token")
	}
}
