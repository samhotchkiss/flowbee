package worker

import (
	"reflect"
	"sort"
	"testing"
)

func sortedAttest(a Allowlist, identity string, claimed []string, arch, os string) []string {
	got := a.attest(identity, claimed, arch, os)
	sort.Strings(got)
	return got
}

func TestAttestOpenAllowlistGatesArchOSByHandshake(t *testing.T) {
	a := OpenAllowlist()
	// arch:* / os:* are attested ONLY against the handshake; a claim that
	// disagrees with the handshake is dropped (the arch-lottery fix).
	claimed := []string{
		"role:eng_worker", "model_family:codex", "tool:docker",
		"arch:arm64", "os:macos", "arch:x86_64", // x86_64 is a FALSE claim
	}
	got := sortedAttest(a, "alice", claimed, "arm64", "macos")
	want := []string{"arch:arm64", "model_family:codex", "os:macos", "role:eng_worker", "tool:docker"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("attested=%v want %v (false arch:x86_64 must be dropped)", got, want)
	}
}

func TestAttestStrictAllowlistGatesRoleFamilyTool(t *testing.T) {
	// strict: only enrolled identities may attest, and only listed caps.
	a := Allowlist{Permit: map[string][]string{
		"reviewer-bob": {"role:code_reviewer", "model_family:opus"},
	}}
	// bob may attest exactly his enrolled caps; an UNENROLLED role claim is dropped.
	got := sortedAttest(a, "reviewer-bob",
		[]string{"role:code_reviewer", "role:eng_worker", "model_family:opus", "tool:docker"},
		"arm64", "macos")
	want := []string{"model_family:opus", "role:code_reviewer"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("attested=%v want %v (unenrolled role:eng_worker + tool:docker dropped)", got, want)
	}

	// an identity NOT in the allowlist attests no role/family/tool at all — its
	// claims never gate matching (§9.4.1: stops rubber-stamping).
	none := sortedAttest(a, "intruder",
		[]string{"role:code_reviewer", "model_family:opus"}, "arm64", "macos")
	if len(none) != 0 {
		t.Fatalf("unenrolled identity attested %v want none", none)
	}
}
