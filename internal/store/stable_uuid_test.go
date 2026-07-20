package store

import (
	"regexp"
	"testing"
)

func TestStableUUIDIsCanonicalDeterministicAndDomainSeparated(t *testing.T) {
	a := stableUUID("driver-grant/v1", "same-action")
	if a != stableUUID("driver-grant/v1", "same-action") {
		t.Fatal("stable UUID changed for the same immutable key")
	}
	if a == stableUUID("another-domain/v1", "same-action") {
		t.Fatal("stable UUID was not domain-separated")
	}
	canonical := regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-5[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	if !canonical.MatchString(a) {
		t.Fatalf("not a canonical name-derived UUID: %q", a)
	}
}
