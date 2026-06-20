package job

import "testing"

// TestNormalizePriority pins the 1..10 lower-is-more-urgent band: 0 (unset) -> the default
// 5, out-of-range values clamp, and in-band values pass through unchanged.
func TestNormalizePriority(t *testing.T) {
	cases := []struct{ in, want int }{
		{0, 5},   // unset -> default
		{1, 1},   // most urgent, in band
		{5, 5},   // default, in band
		{10, 10}, // least urgent, in band
		{3, 3},   // arbitrary in band
		{-4, 1},  // negative clamps to most-urgent
		{11, 10}, // above band clamps to least-urgent
		{99, 10},
	}
	for _, c := range cases {
		if got := NormalizePriority(c.in); got != c.want {
			t.Errorf("NormalizePriority(%d) = %d, want %d", c.in, got, c.want)
		}
	}
}
