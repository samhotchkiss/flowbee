package main

import (
	"testing"

	"github.com/samhotchkiss/flowbee/internal/api"
)

func TestControlPlaneConfigPostureFingerprintIgnoresProcessAndProvenance(t *testing.T) {
	base := api.RunningConfig{Version: "v1", PID: 100, SourceCommit: "one",
		TreeDirty: true, TreeDirtyKnown: true, OriginMainSHA: "origin-one",
		BehindOriginMainBy: 7, BehindOriginMainKnown: true, SourceWarning: "dirty",
		DatabaseURL: "flowbee.db", PrivateAddr: "127.0.0.1:7070",
		HealthAddr: "127.0.0.1:7071", RequiredReviewers: 2,
		DriverControl: api.DriverControlReadiness{Required: true, Available: true, Status: "ready"}}
	one, err := controlPlaneConfigPostureFingerprint(base)
	if err != nil {
		t.Fatal(err)
	}
	rebuilt := base
	rebuilt.Version, rebuilt.PID, rebuilt.SourceCommit = "v2", 200, "two"
	rebuilt.TreeDirty, rebuilt.OriginMainSHA, rebuilt.BehindOriginMainBy = false, "origin-two", 0
	rebuilt.SourceWarning = ""
	rebuilt.DriverControl = api.DriverControlReadiness{Required: true, Status: "held"}
	two, err := controlPlaneConfigPostureFingerprint(rebuilt)
	if err != nil {
		t.Fatal(err)
	}
	if one != two {
		t.Fatalf("process/provenance changed config posture: %s != %s", one, two)
	}
	changed := base
	changed.RequiredReviewers = 3
	three, err := controlPlaneConfigPostureFingerprint(changed)
	if err != nil {
		t.Fatal(err)
	}
	if three == one {
		t.Fatal("effective config change did not change posture fingerprint")
	}
}
