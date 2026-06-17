package main

import (
	"strings"
	"testing"
)

// TestUpRolesMatchFleetGuarantees pins that `flowbee up` is NOT a degraded shadow of
// `flowbee fleet`: it runs a genuinely diverse-model fleet with a conflict resolver
// and the cost-reporting harness — the three things the old hand-rolled single
// agent-cmd silently dropped.
func TestUpRolesMatchFleetGuarantees(t *testing.T) {
	roles := upRoles("", "") // per-role defaults (no operator override)

	by := map[string]upRole{}
	for _, r := range roles {
		by[r.role] = r
	}

	// (1) conflict_resolver MUST be present — else every real merge conflict routes to
	// a resolving_conflict job no worker can claim and escalates to needs_human.
	if _, ok := by["conflict_resolver"]; !ok {
		t.Fatalf("flowbee up must spawn a conflict_resolver; roles=%v", rolesList(roles))
	}
	// every pipeline stage covered.
	for _, want := range []string{"spec_author", "spec_reviewer", "eng_worker", "code_reviewer", "conflict_resolver"} {
		if _, ok := by[want]; !ok {
			t.Fatalf("flowbee up missing role %q; roles=%v", want, rolesList(roles))
		}
	}

	// (2) genuine model diversity (§5.5): the code reviewer's model differs from the
	// builder's — a REAL --model alias difference, not just a family tag.
	builder, reviewer := by["eng_worker"], by["code_reviewer"]
	if builder.family == reviewer.family {
		t.Fatalf("anti-affinity: builder family %q == reviewer family %q", builder.family, reviewer.family)
	}
	if !strings.Contains(builder.cmd, "--model "+builder.family) {
		t.Fatalf("builder cmd must pin --model %s, got: %s", builder.family, builder.cmd)
	}
	if !strings.Contains(reviewer.cmd, "--model "+reviewer.family) {
		t.Fatalf("reviewer cmd must pin --model %s, got: %s", reviewer.family, reviewer.cmd)
	}
	// spec author vs spec reviewer diversity too.
	if by["spec_author"].family == by["spec_reviewer"].family {
		t.Fatalf("spec author/reviewer must differ in model family")
	}

	// (3) cost-metering harness: every role's agent reports tokens/cost via JSON, or
	// the cost meter + per-job ceiling are dead.
	for _, r := range roles {
		if !strings.Contains(r.cmd, "--output-format json") {
			t.Fatalf("role %q agent cmd must emit --output-format json (cost metering), got: %s", r.role, r.cmd)
		}
	}

	// (4) builders + the resolver get the file-writing build template (they mutate the
	// worktree); the author + reviewers get the verdict/spec template.
	for _, r := range []string{"eng_worker", "conflict_resolver"} {
		if !strings.Contains(by[r].cmd, "Create the file(s) on disk now") {
			t.Fatalf("role %q must use the file-writing build template, got: %s", r, by[r].cmd)
		}
	}
	if strings.Contains(by["code_reviewer"].cmd, "Create the file(s) on disk now") {
		t.Fatalf("code_reviewer must NOT use the file-writing template (it authors a verdict, not files)")
	}
}

// TestUpRolesHonorOverride: an operator override (--agent-cmd / --build-agent-cmd)
// replaces the per-role defaults, same as fleet.
func TestUpRolesHonorOverride(t *testing.T) {
	roles := upRoles("REVIEW-OVERRIDE", "BUILD-OVERRIDE")
	for _, r := range roles {
		switch r.role {
		case "eng_worker", "conflict_resolver":
			if r.cmd != "BUILD-OVERRIDE" {
				t.Fatalf("role %q should take the build override, got %q", r.role, r.cmd)
			}
		default:
			if r.cmd != "REVIEW-OVERRIDE" {
				t.Fatalf("role %q should take the review override, got %q", r.role, r.cmd)
			}
		}
	}
}

func rolesList(rs []upRole) []string {
	out := make([]string, len(rs))
	for i, r := range rs {
		out[i] = r.role
	}
	return out
}
