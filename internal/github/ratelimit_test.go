package github

import (
	"net/http"
	"strconv"
	"testing"
	"time"
)

// TestRateLimitBackoff: all three GitHub rate-limit signals are detected and produce a
// sane (1s..1h) backoff; an unrelated response is not mistaken for a rate-limit.
func TestRateLimitBackoff(t *testing.T) {
	// secondary/abuse limit: Retry-After seconds.
	if d, ok := rateLimitBackoff(http.Header{"Retry-After": {"30"}}); !ok || d != 30*time.Second {
		t.Fatalf("Retry-After=30 -> %v ok=%v, want 30s true", d, ok)
	}
	// primary limit: X-RateLimit-Remaining:0 + Reset (unix). ~2 min out.
	reset := strconv.FormatInt(time.Now().Add(2*time.Minute).Unix(), 10)
	d, ok := rateLimitBackoff(http.Header{"X-Ratelimit-Remaining": {"0"}, "X-Ratelimit-Reset": {reset}})
	if !ok || d < 30*time.Second || d > 3*time.Minute {
		t.Fatalf("X-RateLimit-Reset ~2m -> %v ok=%v, want ~2m true", d, ok)
	}
	// clamp: a bogus far-future reset caps at 1h.
	far := strconv.FormatInt(time.Now().Add(10*time.Hour).Unix(), 10)
	if d, _ := rateLimitBackoff(http.Header{"X-Ratelimit-Remaining": {"0"}, "X-Ratelimit-Reset": {far}}); d != time.Hour {
		t.Fatalf("far-future reset -> %v, want clamp to 1h", d)
	}
	// not rate-limited: remaining > 0, no Retry-After.
	if _, ok := rateLimitBackoff(http.Header{"X-Ratelimit-Remaining": {"4321"}}); ok {
		t.Fatal("remaining>0 must NOT read as a rate-limit")
	}
	if _, ok := rateLimitBackoff(http.Header{}); ok {
		t.Fatal("empty headers must NOT read as a rate-limit")
	}
}

// TestIsGraphQLRateLimited: a GraphQL 200 carrying RATE_LIMITED (by type or message) is
// recognized; an ordinary GraphQL error is not.
func TestIsGraphQLRateLimited(t *testing.T) {
	cases := []struct {
		typ, msg string
		want     bool
	}{
		{"RATE_LIMITED", "", true},
		{"rate_limited", "", true}, // case-insensitive
		{"", "API rate limit exceeded for installation", true},
		{"NOT_FOUND", "Could not resolve to a node", false},
		{"", "Field 'x' doesn't exist", false},
	}
	for _, c := range cases {
		if got := isGraphQLRateLimited(c.typ, c.msg); got != c.want {
			t.Errorf("isGraphQLRateLimited(%q,%q)=%v, want %v", c.typ, c.msg, got, c.want)
		}
	}
}
