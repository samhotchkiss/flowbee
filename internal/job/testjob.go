package job

import (
	"sort"
	"strings"
)

// F10: the `test` job type + its DIFF-DERIVED capability constraints (the
// arch-lottery fix). A `test` job must run on a worker that can actually execute
// the build's tests: an arm64-only test routed to an x86 box is the "arch lottery"
// (it passes or fails on which box happened to win the lease, not on the code).
// TestConstraints derives the REQUIRED capability tags a test job carries from
// DETERMINISTIC facts about the change — the declared test matrix plus path-based
// signals in the diff — so the scheduler (which already matches required caps
// against the worker's ATTESTED handshake) routes an arm64 test job ONLY to an
// arm64-capable worker, never x86.
//
// This is a PURE deterministic-core function: it reads no clock, mints no IDs, and
// makes no I/O. Same inputs -> same constraint set, always (archcheck-clean).

// TestMatrix is the build's DECLARED test environment requirements (resolved from
// the spec / a `flowbee.yaml` test stanza and folded onto the job as a value). It
// is the AUTHORITATIVE source of the os/arch/tool a test job needs; the diff is a
// secondary, conservative signal that can only ADD tool constraints, never relax
// the declared matrix. All fields optional — an empty matrix derives constraints
// from the diff alone (and an empty diff yields no extra constraints: any tester
// may run it).
type TestMatrix struct {
	// Arch is the CPU architecture the tests MUST run on (e.g. "arm64", "x86_64").
	// Empty means "no arch requirement" (architecture-agnostic tests).
	Arch string
	// OS is the operating system the tests MUST run on (e.g. "linux", "macos").
	// Empty means "no os requirement".
	OS string
	// Tools are extra tool capabilities the test run needs (e.g. "docker", "cgo").
	// They become tool:<name> required capabilities.
	Tools []string
}

// DeriveTestConstraints is the pure §F10 arch-lottery fix: it returns the set of
// REQUIRED capability tags a `test` job must carry, derived deterministically from
// the build's declared TestMatrix and the touched paths of its diff. The result is
// suitable to pass straight into SeedParams.RequiredCapabilities; the scheduler's
// existing CapabilitiesSatisfy then routes the job ONLY to a worker that ATTESTED
// the matching arch/os/tool against its handshake.
//
// Rules (deterministic, conservative):
//   - role:tester is ALWAYS required (only a tester worker runs a test job).
//   - a declared Arch -> arch:<arch>; a declared OS -> os:<os>.
//   - each declared Tool -> tool:<tool>.
//   - diff-derived tool signals ADD tool:* requirements (e.g. a Dockerfile change
//     needs tool:docker; a cgo/.c/.h change needs tool:cgo) — they never relax the
//     declared matrix, only tighten it. This is the "diff-derived constraints"
//     half: the change itself can demand a capability the matrix did not declare.
//
// The returned slice is sorted + de-duplicated for a stable, replayable order.
func DeriveTestConstraints(m TestMatrix, touchedPaths []string) []string {
	set := map[string]bool{
		"role:tester": true,
	}
	if a := normalizeTag(m.Arch); a != "" {
		set["arch:"+a] = true
	}
	if o := normalizeTag(m.OS); o != "" {
		set["os:"+o] = true
	}
	for _, t := range m.Tools {
		if nt := normalizeTag(t); nt != "" {
			set["tool:"+nt] = true
		}
	}
	for _, tool := range diffDerivedTools(touchedPaths) {
		set["tool:"+tool] = true
	}

	out := make([]string, 0, len(set))
	for tag := range set {
		out = append(out, tag)
	}
	sort.Strings(out)
	return out
}

// diffDerivedTools inspects the touched paths for change classes that DEMAND a
// specific tool capability to test (independent of any declared matrix). Pure +
// deterministic. The mapping is intentionally conservative: only unambiguous
// path/extension signals add a tool requirement.
func diffDerivedTools(touched []string) []string {
	tools := map[string]bool{}
	for _, raw := range touched {
		p := strings.ToLower(normalizeTag(raw))
		if p == "" {
			continue
		}
		base := p
		if i := strings.LastIndexByte(p, '/'); i >= 0 {
			base = p[i+1:]
		}
		switch {
		case base == "dockerfile" || strings.HasPrefix(base, "dockerfile.") ||
			strings.HasSuffix(base, ".dockerfile") || base == "docker-compose.yml" ||
			base == "compose.yml":
			tools["docker"] = true
		case strings.HasSuffix(base, ".c") || strings.HasSuffix(base, ".h") ||
			strings.HasSuffix(base, ".cc") || strings.HasSuffix(base, ".cpp") ||
			strings.HasSuffix(base, ".m"):
			// C/C++/objc sources need a native toolchain (cgo for Go builds).
			tools["cgo"] = true
		}
	}
	out := make([]string, 0, len(tools))
	for t := range tools {
		out = append(out, t)
	}
	sort.Strings(out)
	return out
}

// normalizeTag trims and lowercases a tag VALUE (the part after the colon), so
// "ARM64" and " arm64 " both normalize to "arm64". It does not touch the "arch:"
// prefix (callers add it). An empty/whitespace value normalizes to "".
func normalizeTag(s string) string {
	return strings.ToLower(strings.TrimSpace(s))
}
