// Package onboarding implements `flowbee init` (scaffold runnable config into a
// repo) and `flowbee doctor` (validate that config + GitHub reachability + flow
// identities are sound). It is the code half of the F13 onboarding item: the
// thing the AGENTS.md runbook drives an install agent to run, so a fresh repo
// goes from "nothing" to "runnable Flowbee config" in one command, then proves
// green with doctor.
//
// This package is NOT part of the deterministic core (it shells out to git and
// touches GitHub), so archcheck does not constrain it.
package onboarding

import (
	"embed"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

// scaffoldFS carries the seeded flow assets (the same flows/ tree the engine
// ships, sourced from the `hire` corpus via tools/seedidentities) so `init` is
// self-contained: a freshly-cloned binary scaffolds runnable identities + lenses
// without needing the hire repo on disk.
//
//go:embed assets/flows
var scaffoldFS embed.FS

// flowbeeYAMLTemplate is the flowbee.yaml `init` writes. owner/repo are prefilled
// from the git remote when available; everything else is the shipped default
// posture, led with Mode A + Branch B (the resolved §14 production posture).
const flowbeeYAMLTemplate = `# Flowbee config — scaffolded by ` + "`flowbee init`" + `.
# Config (this file + flows/) lives IN the repo, versioned. Runtime state lives
# in flowbee.db (gitignored + litestream'd). Environment variables (FLOWBEE_*)
# override every key below.

# ── GitHub coordinates ──────────────────────────────────────────────────────
# Prefilled from your git remote. A fine-grained, repo-scoped PAT in
# FLOWBEE_GITHUB_TOKEN is enough for a single operator (reconcile-first makes the
# 5k/hr budget irrelevant; Flowbee owns actor identity). Set the PAT in your
# environment, never in this file.
github_owner: %s
github_repo: %s

# ── Store / listeners ───────────────────────────────────────────────────────
database_url: flowbee.db     # SQLite file (WAL); no database server required
private_addr: ":7070"        # worker API (loopback / Tailscale only)
health_addr: ":7001"         # /healthz
webhook_addr: ":8443"        # GitHub webhooks
lease_ttl_s: 1200            # the un-gameable absolute cap on a lease — set it ABOVE your
                             # slowest agent run (a real claude/codex build is 4-8 min),
                             # or a long build is revoked mid-run and its result fenced.
                             # Must also be >= 3 * heartbeat_interval_s (DESIGN §6.3.3).
heartbeat_interval_s: 30
long_poll_wait_s: 30
river_max_workers: 10
log_level: info

# ── Autonomous-merge posture (§14, resolved: Branch B) ──────────────────────
# true = Flowbee merges an approved + content-clean + CI-green-at-head job ITSELF,
# no human gate. The safety net is deterministic: content-integrity gate +
# CI-green-at-head + the reconciled, SHA-bound verdict. This is the production
# posture. Set false to keep a human in the loop (Branch A). Overridable via
# FLOWBEE_ALLOW_SELF_MERGE.
allow_self_merge: true
`

// gitignoreEntries are the runtime-state paths `init` ensures are ignored: the
// SQLite db (gitignored + litestream'd) and its WAL/SHM sidecars.
var gitignoreEntries = []string{
	"flowbee.db",
	"flowbee.db-wal",
	"flowbee.db-shm",
}

// gitignoreBlockHeader marks the block `init` appends so a re-run is idempotent.
const gitignoreBlockHeader = "# flowbee runtime state (scaffolded by `flowbee init`)"

// InitResult is what `init` did, so the CLI can print the 3-item checklist and a
// test can assert on it without re-reading the filesystem.
type InitResult struct {
	Root          string   // the repo root scaffolded into
	Owner         string   // owner prefilled into flowbee.yaml ("" if undetected)
	Repo          string   // repo prefilled into flowbee.yaml ("" if undetected)
	Created       []string // repo-relative paths newly written
	Skipped       []string // repo-relative paths that already existed (left intact)
	GitignoreKept bool     // true if .gitignore already covered the db (no append)
}

// Init scaffolds Flowbee config into root: flowbee.yaml + flows/{default.yaml,
// flows.yaml, identities/*, lenses/*}, prefilling github_owner/github_repo from
// the git remote, and ensuring flowbee.db is gitignored. Existing files are never
// clobbered — they are reported in Skipped and left intact (idempotent re-runs).
func Init(root string) (InitResult, error) {
	res := InitResult{Root: root}

	owner, repo := DetectRemote(root)
	res.Owner, res.Repo = owner, repo

	// flowbee.yaml (prefilled coords; leaves placeholders empty if undetected).
	yaml := fmt.Sprintf(flowbeeYAMLTemplate, yamlScalar(owner), yamlScalar(repo))
	if err := writeIfAbsent(root, "flowbee.yaml", []byte(yaml), &res); err != nil {
		return res, err
	}

	// flows/ tree, copied from the embedded seeded assets.
	if err := copyEmbeddedFlows(root, &res); err != nil {
		return res, err
	}

	// gitignore the db.
	if err := ensureGitignore(root, &res); err != nil {
		return res, err
	}

	return res, nil
}

// Checklist is the 3-item, do-this-next list `init` prints. It is data (not just
// printed prose) so a test can assert the contract and the CLI renders it.
func (r InitResult) Checklist() []string {
	coord := "set github_owner/github_repo in flowbee.yaml"
	if r.Owner != "" && r.Repo != "" {
		coord = fmt.Sprintf("review the prefilled repo coords (%s/%s) in flowbee.yaml", r.Owner, r.Repo)
	}
	return []string{
		"export FLOWBEE_GITHUB_TOKEN=<a fine-grained, repo-scoped PAT> (" + coord + ")",
		"review flows/default.yaml + flows/identities/* (models per stage) and adjust to taste",
		"run `flowbee doctor` to confirm green, then `flowbee migrate up` and `flowbee serve`",
	}
}

// DetectRemote parses `git -C root remote get-url origin` into owner/repo,
// handling both SSH (git@github.com:owner/repo.git) and HTTPS
// (https://github.com/owner/repo[.git]) forms. Returns ("","") when there is no
// usable remote — init still scaffolds, leaving the coords blank for the user.
func DetectRemote(root string) (owner, repo string) {
	out, err := exec.Command("git", "-C", root, "remote", "get-url", "origin").Output()
	if err != nil {
		return "", ""
	}
	return ParseRemote(strings.TrimSpace(string(out)))
}

var remoteRE = regexp.MustCompile(`(?:github\.com[:/])([^/]+)/(.+?)(?:\.git)?$`)

// ParseRemote extracts owner/repo from a GitHub remote URL (SSH or HTTPS). It is
// split out so it is unit-testable without a git repo on disk.
func ParseRemote(url string) (owner, repo string) {
	m := remoteRE.FindStringSubmatch(url)
	if m == nil {
		return "", ""
	}
	return m[1], strings.TrimSuffix(m[2], "/")
}

// --- helpers ---

func copyEmbeddedFlows(root string, res *InitResult) error {
	return fs.WalkDir(scaffoldFS, "assets/flows", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		// assets/flows/<rest> -> flows/<rest>
		rel := "flows/" + strings.TrimPrefix(p, "assets/flows/")
		b, err := scaffoldFS.ReadFile(p)
		if err != nil {
			return err
		}
		return writeIfAbsent(root, rel, b, res)
	})
}

func writeIfAbsent(root, rel string, body []byte, res *InitResult) error {
	dst := filepath.Join(root, rel)
	if _, err := os.Stat(dst); err == nil {
		res.Skipped = append(res.Skipped, rel)
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return fmt.Errorf("mkdir for %s: %w", rel, err)
	}
	if err := os.WriteFile(dst, body, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", rel, err)
	}
	res.Created = append(res.Created, rel)
	return nil
}

func ensureGitignore(root string, res *InitResult) error {
	path := filepath.Join(root, ".gitignore")
	existing := ""
	if f, err := os.Open(path); err == nil {
		b, rerr := io.ReadAll(f)
		f.Close()
		if rerr != nil {
			return fmt.Errorf("read .gitignore: %w", rerr)
		}
		existing = string(b)
	}

	// Already covered? (any entry present is enough — don't double-write.)
	if gitignoreCovers(existing, gitignoreEntries[0]) {
		res.GitignoreKept = true
		return nil
	}

	var b strings.Builder
	b.WriteString(existing)
	if existing != "" && !strings.HasSuffix(existing, "\n") {
		b.WriteString("\n")
	}
	if existing != "" {
		b.WriteString("\n")
	}
	b.WriteString(gitignoreBlockHeader + "\n")
	for _, e := range gitignoreEntries {
		b.WriteString(e + "\n")
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		return fmt.Errorf("write .gitignore: %w", err)
	}
	if _, err := os.Stat(path); err == nil {
		// record .gitignore as touched (created or amended).
		rel := ".gitignore"
		if existing == "" {
			res.Created = append(res.Created, rel)
		} else {
			res.Skipped = append(res.Skipped, rel+" (appended db ignore block)")
		}
	}
	return nil
}

// gitignoreCovers reports whether an existing .gitignore already ignores the db,
// either via an exact line or a glob like `*.db`.
func gitignoreCovers(existing, dbEntry string) bool {
	for _, line := range strings.Split(existing, "\n") {
		l := strings.TrimSpace(line)
		if l == "" || strings.HasPrefix(l, "#") {
			continue
		}
		if l == dbEntry || l == "*.db" || l == "flowbee.db" {
			return true
		}
	}
	return false
}

// yamlScalar renders a coord for the flowbee.yaml template: an empty value stays
// empty (the key is present but blank for the user to fill), a present value is
// emitted bare (owner/repo are always simple identifiers).
func yamlScalar(s string) string { return s }
