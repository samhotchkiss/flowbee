package main

import (
	"bytes"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/samhotchkiss/flowbee/internal/config"
	"gopkg.in/yaml.v3"
)

// repoEntry is the omitempty projection of config.RepoConfig used to EMIT a new repos[] entry:
// it keeps the written YAML minimal (id/owner/repo + the knobs the operator actually set) while
// using the exact keys config.RepoConfig unmarshals. allow_own_source_merge is always written
// (it is the merge-posture decision, worth being explicit about).
type repoEntry struct {
	ID                  string `yaml:"id"`
	Owner               string `yaml:"owner"`
	Repo                string `yaml:"repo"`
	DefaultBranch       string `yaml:"default_branch,omitempty"`
	AllowOwnSourceMerge bool   `yaml:"allow_own_source_merge"`
	ArchiveHistory      bool   `yaml:"archive_history,omitempty"`
	RequiredReviewers   int    `yaml:"required_reviewers,omitempty"`
	TokenEnv            string `yaml:"token_env,omitempty"`
}

// runRepo implements `flowbee repo add <owner/repo>` — the one-command repo onboarding that
// replaces hand-editing flowbee.yaml. It validates + appends a repos[] entry (preserving the
// file's comments/formatting), refusing a duplicate id or owner/repo.
func runRepo(args []string) error {
	if len(args) == 0 || args[0] != "add" {
		return fmt.Errorf("usage: flowbee repo add <owner/repo> [--id X] [--branch main] [--allow-own-source-merge] [--reviewers N] [--archive] [--token-env VAR] [--config PATH]")
	}
	fs := flag.NewFlagSet("repo add", flag.ContinueOnError)
	id := fs.String("id", "", "short stable handle that scopes jobs (default: the repo name)")
	branch := fs.String("branch", "main", "integration/default branch — the PR base")
	ownMerge := fs.Bool("allow-own-source-merge", false, "let Flowbee auto-merge THIS repo's own internal//cmd/ code "+
		"(set for a managed repo that is NOT the Flowbee control plane; only takes effect when global self-merge is on)")
	reviewers := fs.Int("reviewers", 0, "required distinct reviewers, F5 panel (0 = inherit the global setting)")
	archive := fs.Bool("archive", false, "land docs/history/<id>.md provenance on the repo's main on each merge")
	tokenEnv := fs.String("token-env", "", "name of a per-repo PAT env var (empty = shared FLOWBEE_GITHUB_TOKEN)")
	cfgPath := fs.String("config", "", "flowbee.yaml path (default: $FLOWBEE_CONFIG, else ~/.flowbee/flowbee.yaml)")
	// Allow the <owner/repo> positional on EITHER side of the flags (Go's flag package stops
	// at the first non-flag): re-parse the tail after extracting the single positional.
	var positional string
	rest := args[1:]
	for len(rest) > 0 {
		if err := fs.Parse(rest); err != nil {
			return err
		}
		if fs.NArg() == 0 {
			break
		}
		if positional != "" {
			return fmt.Errorf("unexpected extra argument %q (usage: flowbee repo add <owner/repo> [flags])", fs.Arg(0))
		}
		positional = fs.Arg(0)
		rest = fs.Args()[1:]
	}
	if positional == "" {
		return fmt.Errorf("usage: flowbee repo add <owner/repo> [flags]")
	}
	owner, repo, ok := strings.Cut(positional, "/")
	owner, repo = strings.TrimSpace(owner), strings.TrimSpace(repo)
	if !ok || owner == "" || repo == "" {
		return fmt.Errorf("expected <owner/repo>, got %q", fs.Arg(0))
	}
	repoID := strings.TrimSpace(*id)
	if repoID == "" {
		repoID = repo
	}

	path := resolveConfigPath(*cfgPath)
	entry := repoEntry{
		ID: repoID, Owner: owner, Repo: repo, DefaultBranch: *branch,
		AllowOwnSourceMerge: *ownMerge, ArchiveHistory: *archive,
		RequiredReviewers: *reviewers, TokenEnv: *tokenEnv,
	}
	if err := appendRepoEntry(path, entry); err != nil {
		return err
	}

	fmt.Printf("✓ registered repo %q (%s/%s) in %s\n", repoID, owner, repo, path)
	fmt.Println("  Next, to make it live:")
	fmt.Println("   1. restart the control plane — it reads config at startup (graceful kill -TERM the running `flowbee serve`, then relaunch).")
	fmt.Printf("   2. on the repo: create the `flowbee:build` label + a CI workflow that runs on pull_request.\n")
	fmt.Printf("  Then queue work: label an issue `flowbee:build`, or `flowbee spec \"<task>\" --repo %s`.\n", repoID)
	return nil
}

// resolveConfigPath mirrors config.Load's precedence for the WRITE target, but defaults to the
// standard ~/.flowbee/flowbee.yaml (the CP's config) rather than a cwd-relative file.
func resolveConfigPath(flagVal string) string {
	if flagVal != "" {
		return flagVal
	}
	if p := os.Getenv("FLOWBEE_CONFIG"); p != "" {
		return p
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		return filepath.Join(home, ".flowbee", "flowbee.yaml")
	}
	return "flowbee.yaml"
}

// appendRepoEntry adds entry to the repos: sequence of the YAML file at path, preserving the
// rest of the file (comments + key order) via yaml.Node. Creates the file / repos list if
// absent. Refuses a duplicate id or owner/repo, and verifies the result still parses as a valid
// config before writing (atomic temp+rename), so a bad edit never corrupts the live config.
func appendRepoEntry(path string, entry repoEntry) error {
	var doc yaml.Node
	b, err := os.ReadFile(path)
	switch {
	case err == nil:
		if e := yaml.Unmarshal(b, &doc); e != nil {
			return fmt.Errorf("parse %s: %w", path, e)
		}
	case os.IsNotExist(err):
		// start a fresh document; documentRoot builds the structure.
	default:
		return fmt.Errorf("read %s: %w", path, err)
	}

	root := documentRoot(&doc)
	repos := findOrCreateSeq(root, "repos")

	for _, item := range repos.Content {
		var existing repoEntry
		if item.Decode(&existing) != nil {
			continue
		}
		if existing.ID == entry.ID {
			return fmt.Errorf("repo id %q is already registered in %s", entry.ID, path)
		}
		if existing.Owner == entry.Owner && existing.Repo == entry.Repo {
			return fmt.Errorf("repo %s/%s is already registered (id %q) in %s", entry.Owner, entry.Repo, existing.ID, path)
		}
	}

	var entryNode yaml.Node
	if err := entryNode.Encode(entry); err != nil {
		return err
	}
	repos.Content = append(repos.Content, &entryNode)

	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(&doc); err != nil {
		return err
	}
	_ = enc.Close()

	// re-parse as a full config: never write something the control plane can't load.
	var check config.Config
	if err := yaml.Unmarshal(buf.Bytes(), &check); err != nil {
		return fmt.Errorf("internal error: produced unloadable config: %w", err)
	}
	return writeFileAtomic(path, buf.Bytes())
}

// documentRoot returns the document's root MappingNode, building the structure for an empty
// (brand-new) document.
func documentRoot(doc *yaml.Node) *yaml.Node {
	if doc.Kind == 0 {
		doc.Kind = yaml.DocumentNode
	}
	if len(doc.Content) == 0 {
		doc.Content = []*yaml.Node{{Kind: yaml.MappingNode, Tag: "!!map"}}
	}
	return doc.Content[0]
}

// findOrCreateSeq returns the SequenceNode value for key in mapping, appending the key + an
// empty sequence if absent.
func findOrCreateSeq(mapping *yaml.Node, key string) *yaml.Node {
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		if mapping.Content[i].Value == key {
			val := mapping.Content[i+1]
			if val.Kind != yaml.SequenceNode {
				val.Kind = yaml.SequenceNode
				val.Tag = "!!seq"
				val.Content = nil
			}
			return val
		}
	}
	keyNode := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}
	seqNode := &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
	mapping.Content = append(mapping.Content, keyNode, seqNode)
	return seqNode
}

func writeFileAtomic(path string, data []byte) error {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
