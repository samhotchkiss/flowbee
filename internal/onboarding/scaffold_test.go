package onboarding

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseRemote(t *testing.T) {
	cases := []struct {
		url, owner, repo string
	}{
		{"git@github.com:acme/widgets.git", "acme", "widgets"},
		{"git@github.com:acme/widgets", "acme", "widgets"},
		{"https://github.com/acme/widgets.git", "acme", "widgets"},
		{"https://github.com/acme/widgets", "acme", "widgets"},
		{"ssh://git@github.com/acme/widgets.git", "acme", "widgets"},
		{"https://github.com/acme/Multi-Word_Repo.git", "acme", "Multi-Word_Repo"},
		{"https://gitlab.com/acme/widgets.git", "", ""}, // not github
		{"not a url", "", ""},
	}
	for _, c := range cases {
		o, r := ParseRemote(c.url)
		if o != c.owner || r != c.repo {
			t.Errorf("ParseRemote(%q) = %q/%q, want %q/%q", c.url, o, r, c.owner, c.repo)
		}
	}
}

func TestInit_ScaffoldsRunnableConfig(t *testing.T) {
	root := t.TempDir()
	res, err := Init(root)
	if err != nil {
		t.Fatalf("Init: %v", err)
	}

	// flowbee.yaml + the whole flows tree must exist.
	want := []string{
		"flowbee.yaml",
		"flows/default.yaml",
		"flows/flows.yaml",
		"flows/identities/builder.yaml",
		"flows/identities/issue-reviewer.yaml",
		"flows/identities/reviewer-correctness.yaml",
		"flows/identities/reviewer-tests.yaml",
		"flows/identities/reviewer-security.yaml",
		"flows/lenses/builder.md",
		"flows/lenses/issue-reviewer.md",
	}
	for _, rel := range want {
		if _, err := os.Stat(filepath.Join(root, rel)); err != nil {
			t.Errorf("expected scaffolded %s: %v", rel, err)
		}
	}

	// db must be gitignored.
	gi, err := os.ReadFile(filepath.Join(root, ".gitignore"))
	if err != nil {
		t.Fatalf("read .gitignore: %v", err)
	}
	if !strings.Contains(string(gi), "flowbee.db") {
		t.Errorf(".gitignore does not ignore flowbee.db:\n%s", gi)
	}

	// 3-item checklist.
	cl := res.Checklist()
	if len(cl) != 3 {
		t.Fatalf("checklist has %d items, want 3: %v", len(cl), cl)
	}
}

func TestInit_PrefillsOwnerRepoFromRemote(t *testing.T) {
	root := initGitRepo(t)
	res, err := Init(root)
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	if res.Owner != "acme" || res.Repo != "widgets" {
		t.Fatalf("prefill = %q/%q, want acme/widgets", res.Owner, res.Repo)
	}
	yaml, err := os.ReadFile(filepath.Join(root, "flowbee.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	s := string(yaml)
	if !strings.Contains(s, "github_owner: acme") || !strings.Contains(s, "github_repo: widgets") {
		t.Errorf("flowbee.yaml missing prefilled coords:\n%s", s)
	}
	// Branch B posture is led with.
	if !strings.Contains(s, "allow_self_merge: true") {
		t.Errorf("flowbee.yaml should default allow_self_merge: true:\n%s", s)
	}
}

func TestInit_Idempotent(t *testing.T) {
	root := t.TempDir()
	if _, err := Init(root); err != nil {
		t.Fatalf("first Init: %v", err)
	}
	// hand-edit flowbee.yaml to prove a re-run does NOT clobber it.
	custom := "github_owner: edited\n"
	if err := os.WriteFile(filepath.Join(root, "flowbee.yaml"), []byte(custom), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := Init(root)
	if err != nil {
		t.Fatalf("second Init: %v", err)
	}
	got, _ := os.ReadFile(filepath.Join(root, "flowbee.yaml"))
	if string(got) != custom {
		t.Errorf("re-run clobbered an existing flowbee.yaml; got:\n%s", got)
	}
	if !contains(res.Skipped, "flowbee.yaml") {
		t.Errorf("re-run should report flowbee.yaml as kept; skipped=%v", res.Skipped)
	}
}

func TestInit_ExistingGitignoreDbGlobNotDoubled(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, ".gitignore"), []byte("*.db\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := Init(root)
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	if !res.GitignoreKept {
		t.Errorf("existing *.db glob should be detected as covering the db")
	}
	gi, _ := os.ReadFile(filepath.Join(root, ".gitignore"))
	if strings.Count(string(gi), "flowbee.db") != 0 {
		t.Errorf("should not append flowbee.db when *.db already covers it:\n%s", gi)
	}
}

func contains(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}
