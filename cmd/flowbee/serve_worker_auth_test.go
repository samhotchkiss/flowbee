package main

import (
	"context"
	"testing"
	"time"

	"github.com/samhotchkiss/flowbee/internal/config"
	"github.com/samhotchkiss/flowbee/internal/testutil"
	"github.com/samhotchkiss/flowbee/internal/worker"
)

func TestProductionWorkerAllowlistFailsClosedByIdentityRoleFamilyAndTool(t *testing.T) {
	cfg := config.Config{
		EnrolledIdentities: []string{"reviewer-russ:grok", "capacity-local"},
		WorkerAttestations: map[string][]string{
			"reviewer-russ":  {"role:code_reviewer", "tool:git"},
			"capacity-local": {},
		},
	}
	allow := productionWorkerAllowlist(cfg)
	if allow.Open {
		t.Fatal("production attestation allowlist is open")
	}
	st := testutil.NewStore(t)
	registry := worker.NewRegistry(st, 300, 30, allow)
	now := time.Date(2026, 7, 19, 20, 0, 0, 0, time.UTC)
	registered, err := registry.Register(context.Background(), worker.Registration{
		WorkerID: "reviewer-1", Identity: "reviewer-russ", Arch: "arm64", OS: "darwin",
		Capabilities: []string{"role:code_reviewer", "role:eng_worker", "model_family:grok",
			"model_family:claude", "tool:git", "tool:docker"},
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{"role:code_reviewer": true, "model_family:grok": true, "tool:git": true}
	if len(registered.AttestedCapabilities) != len(want) {
		t.Fatalf("attested=%v want exactly %v", registered.AttestedCapabilities, want)
	}
	for _, capability := range registered.AttestedCapabilities {
		if !want[capability] {
			t.Fatalf("unauthorized capability %q attested: %v", capability, registered.AttestedCapabilities)
		}
	}

	collector, err := registry.Register(context.Background(), worker.Registration{
		WorkerID: "collector-1", Identity: "capacity-local",
		Capabilities: []string{"role:code_reviewer", "tool:shell"},
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(collector.AttestedCapabilities) != 0 {
		t.Fatalf("capacity collector gained scheduling authority: %v", collector.AttestedCapabilities)
	}
}
