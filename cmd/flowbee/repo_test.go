package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/samhotchkiss/flowbee/internal/config"
)

// TestRepoAddAppendsPreservingFile: adding a repo appends a repos[] entry, keeps the existing
// settings AND comments intact, and the result loads as a valid config with the new repo.
func TestRepoAddAppendsPreservingFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "flowbee.yaml")
	original := "# my control plane\nlog_level: info\nrequired_reviewers: 1\nrepos:\n  - id: flowbee\n    owner: samhotchkiss\n    repo: flowbee\n"
	if err := os.WriteFile(path, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := appendRepoEntry(path, repoEntry{ID: "myapp", Owner: "samhotchkiss", Repo: "myapp", DefaultBranch: "main", AllowOwnSourceMerge: true, RequiredReviewers: 1}); err != nil {
		t.Fatalf("append: %v", err)
	}

	out, _ := os.ReadFile(path)
	if !strings.Contains(string(out), "# my control plane") {
		t.Errorf("the file's comment must be preserved:\n%s", out)
	}
	// loads as a valid config with BOTH repos.
	t.Setenv("FLOWBEE_CONFIG", path)
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("the written config must load: %v\n%s", err, out)
	}
	ids := map[string]config.RepoConfig{}
	for _, r := range cfg.Repos {
		ids[r.ID] = r
	}
	if _, ok := ids["flowbee"]; !ok {
		t.Errorf("the pre-existing repo must survive; got repos %v", ids)
	}
	added, ok := ids["myapp"]
	if !ok {
		t.Fatalf("the new repo must be present; got %v", ids)
	}
	if added.Owner != "samhotchkiss" || added.Repo != "myapp" || !added.AllowOwnSourceMerge {
		t.Errorf("new repo fields wrong: %+v", added)
	}
}

// TestRepoAddRejectsDuplicate: a duplicate id OR owner/repo is refused (no silent overwrite).
func TestRepoAddRejectsDuplicate(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "flowbee.yaml")
	if err := os.WriteFile(path, []byte("repos:\n  - id: app\n    owner: o\n    repo: app\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := appendRepoEntry(path, repoEntry{ID: "app", Owner: "o", Repo: "other"}); err == nil {
		t.Error("a duplicate id must be refused")
	}
	if err := appendRepoEntry(path, repoEntry{ID: "different", Owner: "o", Repo: "app"}); err == nil {
		t.Error("a duplicate owner/repo must be refused")
	}
}

// TestRepoAddCreatesReposAndFile: with no repos: key (or no file at all), the command creates
// the list and a loadable config.
func TestRepoAddCreatesReposAndFile(t *testing.T) {
	dir := t.TempDir()
	// no file at all
	path := filepath.Join(dir, "sub", "flowbee.yaml")
	if err := appendRepoEntry(path, repoEntry{ID: "fresh", Owner: "o", Repo: "fresh", DefaultBranch: "main"}); err != nil {
		t.Fatalf("append to new file: %v", err)
	}
	t.Setenv("FLOWBEE_CONFIG", path)
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(cfg.Repos) != 1 || cfg.Repos[0].ID != "fresh" {
		t.Fatalf("expected one repo 'fresh', got %+v", cfg.Repos)
	}
}
