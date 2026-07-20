package worker

import (
	"context"
	"errors"
	"reflect"
	"sort"
	"testing"
	"time"

	"github.com/samhotchkiss/flowbee/internal/testutil"
)

func sortedAttest(a Allowlist, identity string, claimed []string, arch, os string) []string {
	got := a.attest(identity, claimed, arch, os)
	sort.Strings(got)
	return got
}

func TestAttestedForRevalidatesPersistedClaimsAgainstCurrentPolicyAndExpiry(t *testing.T) {
	ctx := context.Background()
	st := testutil.NewStore(t)
	now := time.Date(2026, 7, 19, 20, 0, 0, 0, time.UTC)
	legacy := NewRegistry(st, 300, 30, OpenAllowlist())
	if _, err := legacy.Register(ctx, Registration{WorkerID: "worker-1", Identity: "worker",
		Arch: "arm64", OS: "darwin", Capabilities: []string{
			"role:eng_worker", "role:code_reviewer", "arch:arm64", "arch:amd64", "os:linux",
		}}, now); err != nil {
		t.Fatal(err)
	}

	strict := NewRegistry(st, 300, 30, Allowlist{Permit: map[string][]string{
		"worker": {"role:eng_worker"},
	}})
	got, err := strict.AttestedFor(ctx, "worker", now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(got)
	want := []string{"arch:arm64", "role:eng_worker"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("restart revalidation attested=%v want=%v", got, want)
	}
	got, err = strict.AttestedFor(ctx, "worker", now.Add(24*time.Hour))
	if err != nil || len(got) != 0 {
		t.Fatalf("expired attestation remained authoritative: caps=%v err=%v", got, err)
	}
}

func TestRegisterRejectsWorkerIDReassignmentAtomically(t *testing.T) {
	ctx := context.Background()
	st := testutil.NewStore(t)
	reg := NewRegistry(st, 300, 30, OpenAllowlist())
	now := time.Unix(1000, 0)
	if _, err := reg.Register(ctx, Registration{WorkerID: "stable", Identity: "capacity-local"}, now); err != nil {
		t.Fatal(err)
	}
	if _, err := reg.Register(ctx, Registration{WorkerID: "stable", Identity: "reviewer-russ",
		Capabilities: []string{"role:code_reviewer"}}, now); !errors.Is(err, ErrWorkerIDReassignment) {
		t.Fatalf("worker id reassignment err=%v want %v", err, ErrWorkerIDReassignment)
	}
	var identity, caps string
	if err := st.DB.QueryRowContext(ctx, `SELECT identity,attested_capabilities FROM workers WHERE worker_id='stable'`).
		Scan(&identity, &caps); err != nil {
		t.Fatal(err)
	}
	if identity != "capacity-local" || caps != "null" {
		t.Fatalf("reassignment mutated owner row: identity=%q caps=%s", identity, caps)
	}
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

// TestAttestPassesThroughModelTag: model:<backend> is a self-declared display tag that
// never gates a lease, so the attestor passes it through as-is (so it shows on the roster
// and in `flowbee status`) — even under a STRICT allowlist that gates role:/model_family:.
func TestAttestPassesThroughModelTag(t *testing.T) {
	a := OpenAllowlist()
	got := sortedAttest(a, "alice", []string{"role:eng_worker", "model:codex", "model_family:sonnet"}, "arm64", "macos")
	want := []string{"model:codex", "model_family:sonnet", "role:eng_worker"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("attested=%v want %v (model: tag must pass through for display)", got, want)
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
