package driver

import (
	"regexp"
	"testing"
)

func TestDriverGrantUUIDIsCanonicalAndNeverReusedAcrossEpochs(t *testing.T) {
	one := driverGrantUUID("action-1", 1)
	two := driverGrantUUID("action-1", 2)
	if one == two || one != driverGrantUUID("action-1", 1) {
		t.Fatalf("grant IDs are not stable-per-epoch and unique-across-epochs: %q %q", one, two)
	}
	if !regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-5[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`).MatchString(one) {
		t.Fatalf("grant ID is not canonical UUID: %q", one)
	}
}
