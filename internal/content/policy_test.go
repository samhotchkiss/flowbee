package content

import "testing"

// a minimal real unified diff touching exactly one path.
func diffTouching(path string) string {
	return "diff --git a/" + path + " b/" + path + "\n" +
		"--- a/" + path + "\n" +
		"+++ b/" + path + "\n" +
		"@@ -1 +1,2 @@\n first\n+second\n"
}

// TestExtraDenyPrefixForcesHit proves the F2 operator EXTRA denylist augments the
// shipped set: a path under a configured prefix is denied (forces the human gate),
// while a path outside every prefix stays clear.
func TestExtraDenyPrefixForcesHit(t *testing.T) {
	pol := Policy{ExtraDenyPrefixes: []string{"migrations/", "deploy/prod"}}

	// under the configured "migrations/" prefix -> denied.
	r := CheckWithPolicy(Patch{Diff: diffTouching("migrations/001_init.sql")}, pol)
	if r.DenylistClear {
		t.Fatalf("a configured-denylist path must NOT be clear: %+v", r)
	}
	if len(r.DenylistHits) == 0 || r.DenylistHits[0] != "configured:migrations/001_init.sql" {
		t.Fatalf("expected a configured hit, got %v", r.DenylistHits)
	}

	// equal to a configured (file) prefix -> denied.
	r = CheckWithPolicy(Patch{Diff: diffTouching("deploy/prod")}, pol)
	if r.DenylistClear {
		t.Fatalf("an exact configured path must be denied: %+v", r)
	}

	// outside every configured prefix AND the shipped set -> clear.
	r = CheckWithPolicy(Patch{Diff: diffTouching("pkg/app/handler.go"),
		Declared: BlastRadius{Paths: []string{"pkg/app/handler.go"}}}, pol)
	if !r.DenylistClear {
		t.Fatalf("an unprotected path must be denylist-clear: %v", r.DenylistHits)
	}
}

// TestExtraDenyNeverRemovesShipped proves the EXTRA denylist can only ADD: even an
// empty/odd extra config never weakens the shipped protected set (CI workflows).
func TestExtraDenyNeverRemovesShipped(t *testing.T) {
	for _, extra := range [][]string{nil, {}, {""}, {"unrelated/"}} {
		r := CheckWithPolicy(Patch{Diff: diffTouching(".github/workflows/ci.yml")},
			Policy{ExtraDenyPrefixes: extra})
		if r.DenylistClear {
			t.Fatalf("the shipped CI denylist must always fire (extra=%v)", extra)
		}
	}
}

// TestConfigurableLimits proves the F2 size ceilings are honored: a diff under a
// tightened MaxChangedFiles passes, over it fails static checks.
func TestConfigurableLimits(t *testing.T) {
	twoFiles := diffTouching("a.txt") + diffTouching("b.txt")
	decl := BlastRadius{Paths: []string{"a.txt", "b.txt"}}

	// default limits: two files is fine.
	if r := Check(Patch{Diff: twoFiles, Declared: decl}, Limits{}); !r.StaticChecksPass {
		t.Fatalf("two files must pass under default limits: %v", r.StaticFailures)
	}

	// tightened ceiling of 1 changed file: two files fails static checks.
	tight := Policy{Limits: Limits{MaxChangedFiles: 1}}
	r := CheckWithPolicy(Patch{Diff: twoFiles, Declared: decl}, tight)
	if r.StaticChecksPass {
		t.Fatalf("two files must FAIL under MaxChangedFiles=1: %+v", r)
	}
	if r.Eligible() {
		t.Fatalf("an over-ceiling diff must not be self_merge-eligible")
	}

	// a tightened byte ceiling.
	rb := CheckWithPolicy(Patch{Diff: twoFiles, Declared: decl},
		Policy{Limits: Limits{MaxDiffBytes: 4}})
	if rb.StaticChecksPass {
		t.Fatalf("a diff over MaxDiffBytes must fail static checks")
	}
}

// TestZeroPolicyEqualsCheck proves a zero Policy is exactly the shipped-defaults
// Check (backward compatibility).
func TestZeroPolicyEqualsCheck(t *testing.T) {
	p := Patch{Diff: diffTouching("x.go"), Declared: BlastRadius{Paths: []string{"x.go"}}}
	a := Check(p, Limits{})
	b := CheckWithPolicy(p, Policy{})
	if a.Eligible() != b.Eligible() || a.DenylistClear != b.DenylistClear ||
		len(a.DenylistHits) != len(b.DenylistHits) {
		t.Fatalf("zero Policy must equal Check: %+v vs %+v", a, b)
	}
}
