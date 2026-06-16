package job

import (
	"reflect"
	"testing"
)

// TestDeriveTestConstraints proves the arch-lottery fix at the pure level: a test
// matrix declaring arm64 derives an arch:arm64 requirement (and role:tester
// always), so the scheduler routes the test job ONLY to an arm64-capable worker.
func TestDeriveTestConstraints(t *testing.T) {
	cases := []struct {
		name    string
		matrix  TestMatrix
		touched []string
		want    []string
	}{
		{
			name:   "arm64 matrix derives arch:arm64 + role:tester",
			matrix: TestMatrix{Arch: "arm64"},
			want:   []string{"arch:arm64", "role:tester"},
		},
		{
			name:   "x86_64 matrix derives arch:x86_64",
			matrix: TestMatrix{Arch: "x86_64", OS: "linux"},
			want:   []string{"arch:x86_64", "os:linux", "role:tester"},
		},
		{
			name:    "case + whitespace normalized",
			matrix:  TestMatrix{Arch: " ARM64 "},
			want:    []string{"arch:arm64", "role:tester"},
			touched: nil,
		},
		{
			name:    "diff-derived docker tool tightens the matrix",
			matrix:  TestMatrix{Arch: "arm64"},
			touched: []string{"Dockerfile", "internal/x/y.go"},
			want:    []string{"arch:arm64", "role:tester", "tool:docker"},
		},
		{
			name:    "diff-derived cgo from a C source",
			matrix:  TestMatrix{OS: "linux"},
			touched: []string{"pkg/native/bridge.c"},
			want:    []string{"os:linux", "role:tester", "tool:cgo"},
		},
		{
			name:   "declared tools become tool:* requirements",
			matrix: TestMatrix{Tools: []string{"docker", "qemu"}},
			want:   []string{"role:tester", "tool:docker", "tool:qemu"},
		},
		{
			name: "empty matrix + empty diff -> just role:tester (any tester runs it)",
			want: []string{"role:tester"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := DeriveTestConstraints(c.matrix, c.touched)
			if !reflect.DeepEqual(got, c.want) {
				t.Fatalf("DeriveTestConstraints(%+v, %v) = %v, want %v", c.matrix, c.touched, got, c.want)
			}
		})
	}
}

// TestDeriveTestConstraintsArchLottery is the focused arch-lottery assertion: an
// arm64 test job's constraints are satisfied by an arm64 worker's attested caps and
// NOT by an x86 worker's — using the same CapabilitiesSatisfy the scheduler runs.
func TestDeriveTestConstraintsArchLottery(t *testing.T) {
	req := DeriveTestConstraints(TestMatrix{Arch: "arm64"}, nil)

	arm64Worker := []string{"role:tester", "arch:arm64", "os:linux"}
	x86Worker := []string{"role:tester", "arch:x86_64", "os:linux"}

	if !CapabilitiesSatisfy(arm64Worker, req) {
		t.Fatalf("arm64 worker %v must satisfy arm64 test req %v", arm64Worker, req)
	}
	if CapabilitiesSatisfy(x86Worker, req) {
		t.Fatalf("x86 worker %v must NOT satisfy arm64 test req %v (arch lottery)", x86Worker, req)
	}
}

// TestCIGreenAtHead proves the pluggable-CI fold: ci_green@head holds from EITHER
// reconciled Actions OR a Flowbee test-job fact bound to the same head, and a
// test-job fact bound to a stale head does not count.
func TestCIGreenAtHead(t *testing.T) {
	testGreen := []CIFact{{HeadSHA: "h1", Green: true, Provenance: CIProvFlowbeeTest}}

	// reconciled green satisfies regardless of test facts.
	if !CIGreenAtHead(true, "h1", nil) {
		t.Fatal("reconciled green must satisfy")
	}
	// reconciled red BUT a green Flowbee test fact at the same head satisfies.
	if !CIGreenAtHead(false, "h1", testGreen) {
		t.Fatal("flowbee test green at head must satisfy when Actions is red/absent")
	}
	// a green test fact bound to a DIFFERENT (stale) head does NOT satisfy a moved head.
	if CIGreenAtHead(false, "h2", testGreen) {
		t.Fatal("stale-head test green must NOT satisfy a moved head (SHA binding)")
	}
	// no head reconciled yet -> a test fact cannot bind; only reconciled bool counts.
	if CIGreenAtHead(false, "", testGreen) {
		t.Fatal("test fact must not satisfy when no head is reconciled")
	}
	// a RED test fact does not satisfy.
	red := []CIFact{{HeadSHA: "h1", Green: false, Provenance: CIProvFlowbeeTest}}
	if CIGreenAtHead(false, "h1", red) {
		t.Fatal("a red test fact must not satisfy")
	}
	// a non-flowbee-test provenance is not folded in here (it would arrive via the
	// reconciled bool, not the test-fact list).
	other := []CIFact{{HeadSHA: "h1", Green: true, Provenance: CIProvReconciledActions}}
	if CIGreenAtHead(false, "h1", other) {
		t.Fatal("a reconciled-provenance fact in the test list must not satisfy the fold")
	}
}
