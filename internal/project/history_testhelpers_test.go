package project

import (
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/samhotchkiss/flowbee/internal/gitops"
)

func mustRun(t *testing.T, dir, name string, args ...string) string {
	t.Helper()
	cmd := exec.Command(name, args...)
	if dir != "" {
		cmd.Dir = dir
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %s: %v\n%s", name, strings.Join(args, " "), err, out)
	}
	return string(out)
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func revParse(t *testing.T, m *gitops.Mirror, rev string) string {
	t.Helper()
	out := mustRun(t, "", "git", "--git-dir", m.Path, "rev-parse", rev)
	return strings.TrimSpace(out)
}

// commitFiles returns the repo-relative paths a commit changed against its parent.
func commitFiles(t *testing.T, m *gitops.Mirror, sha string) []string {
	t.Helper()
	out := mustRun(t, "", "git", "--git-dir", m.Path,
		"diff-tree", "--no-commit-id", "--name-only", "-r", sha)
	var files []string
	for _, l := range strings.Split(strings.TrimSpace(out), "\n") {
		if l = strings.TrimSpace(l); l != "" {
			files = append(files, l)
		}
	}
	return files
}

func commitAuthor(t *testing.T, m *gitops.Mirror, sha string) string {
	t.Helper()
	out := mustRun(t, "", "git", "--git-dir", m.Path, "show", "-s", "--format=%an", sha)
	return strings.TrimSpace(out)
}
