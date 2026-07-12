package epicspec

import "testing"

func TestMatchGlob(t *testing.T) {
	cases := []struct {
		glob, path string
		want       bool
	}{
		{"internal/foo/**", "internal/foo/bar.go", true},
		{"internal/foo/**", "internal/foo/sub/bar.go", true},
		{"internal/foo/**", "internal/foobar/bar.go", false}, // no directory boundary blur
		{"internal/foo/**", "internal/foo", false},           // ** requires the trailing slash+content
		{"cmd/bar/**.go", "cmd/bar/main.go", true},
		{"cmd/bar/**.go", "cmd/bar/sub/main.go", true},
		{"cmd/bar/**.go", "cmd/bar/main.py", false},
		{"**.md", "epics/2026-07-03-foo.md", true},
		{"**.md", "epics/2026-07-03-foo.txt", false},
		{"cmd/flowbee/main.go", "cmd/flowbee/main.go", true}, // exact literal path
		{"cmd/flowbee/main.go", "cmd/flowbee/other.go", false},
		{"internal/*.go", "internal/foo.go", true},
		{"internal/*.go", "internal/sub/foo.go", false}, // single "*" stays within a segment
		{"*", "anything", true},
		{"*", "a/b", false}, // bare "*" is single-segment only, unlike ScopeOverlap's bare-star rule
		{"**", "a/b/c", true},
	}
	for _, tc := range cases {
		if got := MatchGlob(tc.glob, tc.path); got != tc.want {
			t.Errorf("MatchGlob(%q, %q) = %v, want %v", tc.glob, tc.path, got, tc.want)
		}
	}
}

func TestMatchGlobEmptyInputs(t *testing.T) {
	if MatchGlob("", "foo") {
		t.Error("empty glob must match nothing")
	}
	if MatchGlob("*", "") {
		t.Error("empty path must match nothing")
	}
}
