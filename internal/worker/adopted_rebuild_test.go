package worker

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/samhotchkiss/flowbee/client"
)

func TestBootstrapAdoptedRebuildMakesResultCumulative(t *testing.T) {
	dir := t.TempDir()
	runGit := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
		return string(out)
	}
	runGit("init", "-q")
	runGit("config", "user.name", "Test")
	runGit("config", "user.email", "test@example.com")
	if err := os.WriteFile(filepath.Join(dir, "feature.txt"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit("add", "feature.txt")
	runGit("commit", "-qm", "base")
	base := strings.TrimSpace(runGit("rev-parse", "HEAD"))

	prior := "diff --git a/feature.txt b/feature.txt\n" +
		"--- a/feature.txt\n+++ b/feature.txt\n@@ -1 +1,2 @@\n base\n+adopted change\n"
	c := &client.LeaseContext{Role: "eng_worker", Rebuild: true, Diff: prior}
	if _, err := writeTaskContext(dir, client.LeaseGrant{JobID: "adopted", Context: c}); err != nil {
		t.Fatal(err)
	}
	if err := bootstrapAdoptedRebuild(dir, c); err != nil {
		t.Fatalf("bootstrap prior patch: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "feature.txt"), []byte("base\nadopted change\nreview correction\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit("add", "feature.txt")
	diff := runGit("diff", "--cached", base)
	if !strings.Contains(diff, "+adopted change") || !strings.Contains(diff, "+review correction") {
		t.Fatalf("rebuild result is not cumulative:\n%s", diff)
	}
}

func TestAdoptedRepairRequiresLeaseAuthoritativeHead(t *testing.T) {
	// adoptedRepair's provisioning predicate is deliberately exact: a branch tip
	// observed after claim may only be used when it is the reconciled GitHub
	// headRefOid carried by the lease. Cover the movement that previously slipped
	// through before the later compare-and-push guard could see it.
	for _, tc := range []struct {
		name     string
		tip      string
		exists   bool
		expected string
		valid    bool
	}{
		{name: "exact authoritative head", tip: "head-a", exists: true, expected: "head-a", valid: true},
		{name: "moved before fetch", tip: "head-b", exists: true, expected: "head-a"},
		{name: "branch disappeared", expected: "head-a"},
		{name: "lease lacks authoritative head", tip: "head-a", exists: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			valid := adoptedRepairHeadMatches(tc.expected, tc.tip, tc.exists)
			if valid != tc.valid {
				t.Fatalf("authoritative tip validity=%v, want %v", valid, tc.valid)
			}
		})
	}
}
