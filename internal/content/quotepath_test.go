package content

import "testing"

// TestQuotePathBypass locks the content-gate quoting bypass shut. With git's
// default core.quotepath=true, a pathname containing a byte >= 0x80 is emitted in
// C-quoted form ("a/.github/workflows/\303\251.yml"). Before the fix the leading
// quote + octal escapes defeated stripDiffPathPrefix and every denylist
// classifier, so a workflow/secret/Dockerfile with one non-ASCII byte in its name
// self-merged to main unreviewed. These are the EXACT bytes `git diff` produces.
func TestQuotePathBypass(t *testing.T) {
	// café.pem — secret_material; .github/workflows/é.yml — ci_workflow.
	// \303\251 is the UTF-8 encoding of 'é'.
	cases := []struct {
		name string
		diff string
	}{
		{"quoted_pem_add", `diff --git "a/deploy/caf\303\251.pem" "b/deploy/caf\303\251.pem"
new file mode 100644
index 0000000..1111111
--- /dev/null
+++ "b/deploy/caf\303\251.pem"
@@ -0,0 +1 @@
+nothing-secret-looking-here
`},
		{"quoted_workflow_add", `diff --git "a/.github/workflows/\303\251.yml" "b/.github/workflows/\303\251.yml"
new file mode 100644
index 0000000..1111111
--- /dev/null
+++ "b/.github/workflows/\303\251.yml"
@@ -0,0 +1,2 @@
+name: x
+on: pull_request
`},
		// Non-ASCII byte in the DIRECTORY, dangerous basename ("Dockerfile") intact —
		// the real bypass vector for a basename-matched class. (Dockerfilé itself is
		// NOT a functional Dockerfile, so that is correctly not a bypass.)
		{"quoted_dockerfile_in_nonascii_dir", `diff --git "a/caf\303\251/Dockerfile" "b/caf\303\251/Dockerfile"
new file mode 100644
index 0000000..1111111
--- /dev/null
+++ "b/caf\303\251/Dockerfile"
@@ -0,0 +1 @@
+FROM scratch
`},
		{"quoted_rename_into_workflows", `diff --git a/x.yml "b/.github/workflows/\303\251.yml"
similarity index 100%
rename from x.yml
rename to ".github/workflows/\303\251.yml"
`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// The autonomous self-merge gate runs the default policy (AllowOwnSource off
			// is irrelevant here: these are universal classes, never carved out).
			r := Check(Patch{Diff: tc.diff}, Limits{})
			if r.DenylistClear {
				t.Fatalf("BYPASS: quoted dangerous path passed the denylist (hits=%v)", r.DenylistHits)
			}
		})
	}
}

// TestUnquoteGitPath unit-checks the decoder directly, including the benign
// passthrough (an unquoted path must be returned verbatim) and a multi-byte
// sequence, so a future refactor can't silently regress the boundary.
func TestUnquoteGitPath(t *testing.T) {
	cases := []struct{ in, want string }{
		{`a/normal/path.go`, `a/normal/path.go`},                                      // not quoted: passthrough
		{`"a/caf\303\251.pem"`, "a/café.pem"},                                          // octal -> UTF-8 bytes
		{`"a/.github/workflows/\303\251.yml"`, "a/.github/workflows/é.yml"},            // workflow
		{`"a/tab\there"`, "a/tab\there"},                                               // \t escape
		{`"a/quote\"x"`, `a/quote"x`},                                                  // escaped quote
		{`"a/back\\slash"`, `a/back\slash`},                                            // escaped backslash
	}
	for _, c := range cases {
		if got := unquoteGitPath(c.in); got != c.want {
			t.Errorf("unquoteGitPath(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestQuotePathDoesNotOverBlock confirms the unquoting doesn't newly trip the gate
// on a benign quoted path (a non-ASCII source file that is NOT in a denylisted
// class must still pass), so the fix isn't a blunt "any quoted path is dangerous".
func TestQuotePathDoesNotOverBlock(t *testing.T) {
	diff := `diff --git "a/docs/caf\303\251.md" "b/docs/caf\303\251.md"
new file mode 100644
--- /dev/null
+++ "b/docs/caf\303\251.md"
@@ -0,0 +1 @@
+hello
`
	r := Check(Patch{Diff: diff}, Limits{})
	if !r.DenylistClear {
		t.Fatalf("over-block: benign quoted docs path was denied (hits=%v)", r.DenylistHits)
	}
}
