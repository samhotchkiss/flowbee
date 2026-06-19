package main

import "testing"

// TestIsLoopbackAddrCoverage documents the refuse-to-start guard's classification
// (item 4 of the §7.6 audit). The guard is CONSERVATIVE in the dangerous direction:
// every non-loopback bind it can mis-classify, it mis-classifies as NON-loopback
// (which REFUSES to start / warns), never as loopback. The one real gap is a
// 127.0.0.0/8 address other than 127.0.0.1 (e.g. 127.0.0.2): it is genuinely a
// loopback bind, yet the guard treats it as non-loopback and so DEMANDS auth /
// FLOWBEE_INSECURE. That is a usability papercut, NOT a bypass — it errs safe.
func TestIsLoopbackAddrCoverage(t *testing.T) {
	cases := []struct {
		addr string
		want bool
	}{
		// genuinely loopback, correctly recognized.
		{"127.0.0.1:7070", true},
		{"localhost:7070", true},
		{"::1:7070", true}, // note: LastIndex(":") strips "7070"; host="::1" matches.

		// genuinely NON-loopback, correctly rejected (these MUST be false or the CP
		// would silently expose an open API).
		{":7070", false},        // all interfaces — the default bind
		{"0.0.0.0:7070", false}, // all interfaces, explicit
		{"100.64.0.2:7070", false},
		{"10.0.0.5:7070", false},
		{"[::]:7070", false}, // IPv6 all interfaces

		// the SAFE-direction gaps: real loopback addrs the guard treats as NON-loopback.
		// Erring this way only forces auth/INSECURE; it never opens the API.
		{"127.0.0.2:7070", false}, // 127.0.0.0/8 is all loopback, but only .1 is whitelisted
		{"[::1]:7070", false},     // bracketed IPv6 loopback: host parses as "[::1]"
	}
	for _, c := range cases {
		if got := isLoopbackAddr(c.addr); got != c.want {
			t.Errorf("isLoopbackAddr(%q)=%v want %v", c.addr, got, c.want)
		}
	}
	// The load-bearing assertion for the bypass: NO non-loopback string is ever
	// classified loopback. If any of the explicitly-non-loopback cases above flipped
	// to true, the CP would boot an OPEN, no-auth worker API on a network-reachable
	// bind — the exact failure the guard exists to prevent.
}
