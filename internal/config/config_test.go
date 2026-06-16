package config

import "testing"

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
