package main

import (
	"testing"
	"time"
)

// TestNextRespawnBackoff: the supervisor's respawn delay doubles and caps at 30s, so a
// crash-looping worker backs off instead of hot-spinning the box.
func TestNextRespawnBackoff(t *testing.T) {
	cases := []struct{ in, want time.Duration }{
		{1 * time.Second, 2 * time.Second},
		{2 * time.Second, 4 * time.Second},
		{16 * time.Second, 30 * time.Second}, // 32 -> capped
		{30 * time.Second, 30 * time.Second}, // stays capped
	}
	for _, c := range cases {
		if got := nextRespawnBackoff(c.in); got != c.want {
			t.Fatalf("nextRespawnBackoff(%s)=%s, want %s", c.in, got, c.want)
		}
	}
}
