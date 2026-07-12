package epicspec

import "testing"

func TestScopeOverlap(t *testing.T) {
	cases := []struct {
		name     string
		a, b     []string
		overlaps bool
	}{
		{"identical globs", []string{"internal/foo/**"}, []string{"internal/foo/**"}, true},
		{"disjoint trees", []string{"internal/foo/**"}, []string{"internal/bar/**"}, false},
		{"nested prefix", []string{"internal/**"}, []string{"internal/foo/**"}, true},
		{"nested prefix reversed", []string{"internal/foo/**"}, []string{"internal/**"}, true},
		{"exact file no wildcard", []string{"cmd/flowbee/main.go"}, []string{"cmd/flowbee/main.go"}, true},
		{"exact file vs disjoint dir", []string{"cmd/flowbee/main.go"}, []string{"internal/foo/**"}, false},
		{"bare star overlaps everything", []string{"*"}, []string{"internal/anything/**"}, true},
		{"double-star bare overlaps everything", []string{"**"}, []string{"anything"}, true},
		{"multi-glob one pair overlaps", []string{"internal/a/**", "internal/b/**"}, []string{"internal/b/**", "internal/c/**"}, true},
		{"multi-glob no pair overlaps", []string{"internal/a/**", "internal/b/**"}, []string{"internal/c/**", "internal/d/**"}, false},
		{"empty lists never overlap", []string{}, []string{"internal/foo/**"}, false},
		{"both empty", []string{}, []string{}, false},
		{"string-prefix false-positive is conservative by design", []string{"internal/foo*"}, []string{"internal/foobar/**"}, true},
		{"suffix-anchored glob still uses literal prefix", []string{"cmd/bar/**.go"}, []string{"cmd/bar/**.py"}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, _, _ := ScopeOverlap(c.a, c.b)
			if got != c.overlaps {
				t.Errorf("ScopeOverlap(%v, %v) = %v, want %v", c.a, c.b, got, c.overlaps)
			}
			// symmetry: overlap must not depend on argument order.
			got2, _, _ := ScopeOverlap(c.b, c.a)
			if got2 != c.overlaps {
				t.Errorf("ScopeOverlap(%v, %v) [reversed] = %v, want %v", c.b, c.a, got2, c.overlaps)
			}
		})
	}
}

func TestGlobPrefix(t *testing.T) {
	cases := map[string]string{
		"internal/foo/**":     "internal/foo/",
		"cmd/bar/**.go":       "cmd/bar/",
		"cmd/flowbee/main.go": "cmd/flowbee/main.go",
		"*":                   "",
		"**":                  "",
		"a/b/c*.txt":          "a/b/c",
	}
	for in, want := range cases {
		if got := globPrefix(in); got != want {
			t.Errorf("globPrefix(%q) = %q, want %q", in, got, want)
		}
	}
}
