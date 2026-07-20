package epicspec

import "testing"

func TestSlugFromBranch(t *testing.T) {
	cases := []struct {
		branch   string
		wantSlug string
		wantOK   bool
	}{
		{"epic/2026-07-03-review-gate", "2026-07-03-review-gate", true},
		{"epic/foo", "foo", true},
		// near-misses: must NOT match.
		{"epic", "", false},
		{"epic/", "", false},
		{"epicfoo", "", false},
		{"epic-foo", "", false},
		{"myepic/foo", "", false},
		{"epic/foo/bar", "", false}, // a slug can't itself carry a "/"
		{"feature/epic/foo", "", false},
		{"", "", false},
		{"flowbee/issue-42", "", false},
	}
	for _, tc := range cases {
		slug, ok := SlugFromBranch(tc.branch)
		if slug != tc.wantSlug || ok != tc.wantOK {
			t.Errorf("SlugFromBranch(%q) = (%q, %v), want (%q, %v)", tc.branch, slug, ok, tc.wantSlug, tc.wantOK)
		}
	}
}
