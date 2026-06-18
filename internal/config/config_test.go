package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestDefaultDBPathIsStandardLocation: the default database path must resolve to the
// standard ~/.flowbee/flowbee.db, NOT a cwd-relative "flowbee.db". The relative
// default made `flowbee board`/`doctor` silently open an empty ./flowbee.db (cryptic
// "no such table") instead of the live control-plane DB when run without FLOWBEE_CONFIG.
func TestDefaultDBPathIsStandardLocation(t *testing.T) {
	got := Default().DatabaseURL
	if got == "flowbee.db" {
		t.Fatal("DatabaseURL defaults to cwd-relative \"flowbee.db\" — CLI commands won't find the live DB")
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		want := filepath.Join(home, ".flowbee", "flowbee.db")
		if got != want {
			t.Fatalf("default DatabaseURL=%q, want %q", got, want)
		}
	} else if !strings.HasSuffix(got, "flowbee.db") {
		t.Fatalf("default DatabaseURL=%q, want a flowbee.db path", got)
	}
}

// TestAllowSelfMergeEnv proves FLOWBEE_ALLOW_SELF_MERGE flips the §14 decision (F2):
// default off (Branch A); "true"/"1" turns Branch B on.
func TestAllowSelfMergeEnv(t *testing.T) {
	if Default().AllowSelfMerge {
		t.Fatal("AllowSelfMerge must default to false (Branch A)")
	}
	for _, v := range []string{"true", "1"} {
		t.Setenv("FLOWBEE_ALLOW_SELF_MERGE", v)
		c, err := Load()
		if err != nil {
			t.Fatalf("load: %v", err)
		}
		if !c.AllowSelfMerge {
			t.Fatalf("FLOWBEE_ALLOW_SELF_MERGE=%q must enable self-merge", v)
		}
	}
	// any other value leaves it off.
	t.Setenv("FLOWBEE_ALLOW_SELF_MERGE", "no")
	c, err := Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if c.AllowSelfMerge {
		t.Fatal("a non-true value must leave self-merge off")
	}
}

// TestContentPolicyEnv proves the content-integrity knobs (F2) wire through env into
// the content.Policy projection.
func TestContentPolicyEnv(t *testing.T) {
	// the zero config projects to the zero Policy (shipped defaults).
	zero := Default().ContentPolicy()
	if zero.Limits.MaxDiffBytes != 0 || zero.Limits.MaxChangedFiles != 0 || len(zero.ExtraDenyPrefixes) != 0 {
		t.Fatalf("default ContentPolicy must be the zero Policy, got %+v", zero)
	}

	t.Setenv("FLOWBEE_CONTENT_MAX_DIFF_BYTES", "4096")
	t.Setenv("FLOWBEE_CONTENT_MAX_CHANGED_FILES", "7")
	t.Setenv("FLOWBEE_CONTENT_DENY_EXTRA", "migrations/, deploy/prod , ")
	c, err := Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	pol := c.ContentPolicy()
	if pol.Limits.MaxDiffBytes != 4096 || pol.Limits.MaxChangedFiles != 7 {
		t.Fatalf("content limits not wired: %+v", pol.Limits)
	}
	if len(pol.ExtraDenyPrefixes) != 2 ||
		pol.ExtraDenyPrefixes[0] != "migrations/" || pol.ExtraDenyPrefixes[1] != "deploy/prod" {
		t.Fatalf("content deny-extra not parsed (CSV, trimmed, empties dropped): %v", pol.ExtraDenyPrefixes)
	}
}

func TestCostCeilingEnv(t *testing.T) {
	// zero config => no default ceiling (shipped posture: metered, never capped).
	if m := Default().CostCeilingMicroUSD(); m != 0 {
		t.Fatalf("default cost ceiling must be 0 (off), got %d", m)
	}
	t.Setenv("FLOWBEE_COST_CEILING_USD", "2.50")
	c, err := Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if c.CostCeilingUSD != 2.50 {
		t.Fatalf("cost_ceiling_usd not wired: %v", c.CostCeilingUSD)
	}
	if m := c.CostCeilingMicroUSD(); m != 2_500_000 {
		t.Fatalf("dollars must project to micro-USD ×1e6, got %d want 2_500_000", m)
	}
}

func TestCostCeilingNegativeRejected(t *testing.T) {
	c := Default()
	c.CostCeilingUSD = -1
	if err := c.Validate(); err == nil {
		t.Fatalf("negative cost_ceiling_usd must fail Validate")
	}
}

// TestReposConfig proves the F9 multi-repo registry parses from YAML, including the
// active default (true when unset) and explicit park.
func TestReposConfig(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/flowbee.yaml"
	yaml := `database_url: x.db
heartbeat_interval_s: 30
lease_ttl_s: 300
repos:
  - id: core
    owner: acme
    repo: core
    default_branch: main
  - id: web
    owner: acme
    repo: web
    token_env: WEB_PAT
    active: false
`
	if err := os.WriteFile(path, []byte(yaml), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	t.Setenv("FLOWBEE_CONFIG", path)
	c, err := Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(c.Repos) != 2 {
		t.Fatalf("want 2 repos, got %d", len(c.Repos))
	}
	if c.Repos[0].ID != "core" || c.Repos[0].Owner != "acme" || !c.Repos[0].IsActive() {
		t.Fatalf("core repo mismatch: %+v", c.Repos[0])
	}
	if c.Repos[1].TokenEnv != "WEB_PAT" || c.Repos[1].IsActive() {
		t.Fatalf("web repo should carry token_env and be parked: %+v", c.Repos[1])
	}
}

// TestValidateReposRegistry: a multi-repo registry with a unique handle + GitHub coords
// per repo validates; the typos that otherwise become a SILENT dead repo at runtime
// (duplicate/empty/reserved id, missing owner/repo) are caught at config time.
func TestValidateReposRegistry(t *testing.T) {
	base := Config{DatabaseURL: "f.db", HeartbeatIntervalS: 30, LeaseTTLS: 1200}

	ok := base
	ok.Repos = []RepoConfig{
		{ID: "flowbee", Owner: "o", Repo: "flowbee"},
		{ID: "russ", Owner: "o", Repo: "russ"},
	}
	if err := ok.Validate(); err != nil {
		t.Fatalf("a valid registry must pass: %v", err)
	}

	bad := []struct {
		name  string
		repos []RepoConfig
	}{
		{"duplicate id", []RepoConfig{{ID: "a", Owner: "o", Repo: "x"}, {ID: "a", Owner: "o", Repo: "y"}}},
		{"missing owner", []RepoConfig{{ID: "a", Repo: "x"}}},
		{"missing repo", []RepoConfig{{ID: "a", Owner: "o"}}},
		{"empty id", []RepoConfig{{Owner: "o", Repo: "x"}}},
		{"reserved id", []RepoConfig{{ID: "default", Owner: "o", Repo: "x"}}},
	}
	for _, c := range bad {
		cfg := base
		cfg.Repos = c.repos
		if err := cfg.Validate(); err == nil {
			t.Errorf("%s: expected a validation error, got nil", c.name)
		}
	}
}
