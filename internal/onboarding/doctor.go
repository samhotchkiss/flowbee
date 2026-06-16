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
	Root       string
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

	// (1) config: present + parses + valid.
	cfg, _ := checkConfig(root, &rep)

	// (2) flows + identities: the flow files exist and every referenced identity
	// resolves to an identity yaml with an existing lens file.
	checkFlows(root, &rep)

	// (3) GitHub reachability (skippable offline).
	checkGitHub(ctx, opts, cfg, &rep)

	return rep, nil
}

func checkConfig(root string, rep *DoctorReport) (config.Config, bool) {
	path := filepath.Join(root, "flowbee.yaml")
	b, err := os.ReadFile(path)
	if err != nil {
		rep.add("config", StatusFail, fmt.Sprintf("flowbee.yaml not found (run `flowbee init`): %v", err))
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

	// coords: prefilled is a pass; blank is a warn (user must fill before serve).
	if cfg.GithubOwner != "" && cfg.GithubRepo != "" {
		rep.add("repo-coords", StatusPass, fmt.Sprintf("%s/%s", cfg.GithubOwner, cfg.GithubRepo))
	} else if os.Getenv("FLOWBEE_GITHUB_OWNER") != "" && os.Getenv("FLOWBEE_GITHUB_REPO") != "" {
		rep.add("repo-coords", StatusPass, "from FLOWBEE_GITHUB_OWNER/REPO env")
	} else {
		rep.add("repo-coords", StatusWarn, "github_owner/github_repo unset (set them in flowbee.yaml before serve)")
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

func checkFlows(root string, rep *DoctorReport) {
	flowsDir := filepath.Join(root, "flows")
	defaultPath := filepath.Join(flowsDir, "default.yaml")
	fb, err := os.ReadFile(defaultPath)
	if err != nil {
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
	probe := opts.Probe
	if probe == nil {
		// build a RealClient from the token, if any. No token => warn (the user has
		// not wired creds yet — not a hard failure for `init`-then-`doctor`).
		token := os.Getenv("FLOWBEE_GITHUB_TOKEN")
		if token == "" {
			rep.add("github", StatusWarn, "FLOWBEE_GITHUB_TOKEN unset; skipping reachability (set it or pass --offline)")
			return
		}
		owner, repo := cfg.GithubOwner, cfg.GithubRepo
		if v := os.Getenv("FLOWBEE_GITHUB_OWNER"); v != "" {
			owner = v
		}
		if v := os.Getenv("FLOWBEE_GITHUB_REPO"); v != "" {
			repo = v
		}
		if owner == "" || repo == "" {
			rep.add("github", StatusWarn, "no repo coords; skipping reachability")
			return
		}
		probe = gh.NewRealClient(owner, repo, func(context.Context) (string, error) { return token, nil })
	}

	timeout := opts.ProbeTimeout
	if timeout == 0 {
		timeout = 10 * time.Second
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	snap, err := probe.BoardSweep(cctx)
	if err != nil {
		rep.add("github", StatusFail, fmt.Sprintf("GitHub unreachable: %v (re-run with --offline to skip)", err))
		return
	}
	rep.add("github", StatusPass, fmt.Sprintf("reachable; rate-limit remaining=%d, board has %d PRs / %d issues",
		snap.RateLimit.Remaining, len(snap.PullRequests), len(snap.Issues)))

	// deployment preflight (the make-or-break misconfigs): token write-scope, CI, and
	// branch protection. Only when the probe is a real client (the fake skips these).
	pf, ok := probe.(preflighter)
	if !ok {
		return
	}
	pre, perr := pf.Preflight(cctx, cfg.GithubDefaultBranch)
	if perr != nil {
		rep.add("github write access", StatusWarn, fmt.Sprintf("could not read repo permissions: %v", perr))
		return
	}
	if pre.CanWrite {
		rep.add("github write access", StatusPass, "token can write (push branches / open + merge PRs / close issues)")
	} else {
		rep.add("github write access", StatusFail, "token lacks WRITE — use a fine-grained PAT with Contents + Pull requests + Issues = write (read-only can't push or merge)")
	}
	switch {
	case pre.CITriggersOnPR:
		rep.add("ci configured", StatusPass, "a workflow triggers on pull_request (Flowbee's merge gate can go green)")
	case pre.HasCI:
		rep.add("ci configured", StatusWarn, "workflows exist but NONE trigger on pull_request — Flowbee gates the merge on green PR CI, so PRs will sit forever; add `on: pull_request` to a workflow")
	default:
		rep.add("ci configured", StatusWarn, "no GitHub Actions workflow found — Flowbee merges ONLY on green CI, so nothing will merge until the repo reports a CI status check on PRs")
	}
	if pre.BranchProtected {
		rep.add("branch protection", StatusWarn, "integration branch is protected — autonomous merge needs the token to satisfy the required checks, or turn protection off")
	} else {
		rep.add("branch protection", StatusPass, "integration branch unprotected — autonomous merge OK")
	}
	if pre.TokenScopes != "" {
		rep.add("token scope", StatusWarn, "broadly-scoped CLASSIC PAT (scopes: "+pre.TokenScopes+") — prefer a fine-grained PAT limited to Contents + Pull requests + Issues (least privilege)")
	} else {
		rep.add("token scope", StatusPass, "fine-grained / least-privilege token")
	}
}
