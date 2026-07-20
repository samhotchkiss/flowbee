package bootstrap

import (
	"context"
	"testing"
)

func TestTopologyKeepsOnlyInteractorOnDefaultAndAllMachineryManaged(t *testing.T) {
	inventory := ProjectTopology(bootstrapPlan())
	if len(inventory.External) != 1 || inventory.External[0].Class != "project_interactor" ||
		inventory.External[0].PresentationName != "russ-interactor" {
		t.Fatalf("external inventory = %+v", inventory.External)
	}
	classes := map[string]bool{}
	for _, target := range inventory.Managed {
		if target.TmuxServerDomain != ManagedTmuxServerDomain || target.Class == "project_interactor" {
			t.Fatalf("managed inventory leaked domain/class: %+v", inventory.Managed)
		}
		classes[target.Class] = true
	}
	for _, required := range []string{"control_plane", "project_orchestrator", "dynamic_worker", "driver_console"} {
		if !classes[required] {
			t.Fatalf("managed inventory missing %s: %+v", required, inventory.Managed)
		}
	}
}

func TestTopologyDriverConsoleIsOptionalAndNonBlocking(t *testing.T) {
	plan := bootstrapPlan()
	plan.Groups[0].MemberClasses = []string{"control_plane", "project_orchestrator", "dynamic_worker"}
	fake := NewFakePort("russ")
	fake.AutoLiveActivation = true
	orchestrator := newBootstrap(fake, NewMemoryCheckpointStore())
	if err := validatePlan(plan, orchestrator.Ports); err != nil {
		t.Fatalf("plan without cosmetic Driver console must remain valid: %v", err)
	}
	result, err := orchestrator.Run(context.Background(), plan)
	if err != nil || !result.Complete || result.Hold != "" {
		t.Fatalf("cosmetic Driver console absence blocked bootstrap/attach: result=%+v err=%v", result, err)
	}
	inventory := ProjectTopology(plan)
	classes := map[string]bool{}
	for _, target := range inventory.Managed {
		classes[target.Class] = true
	}
	for _, required := range []string{"control_plane", "project_orchestrator", "dynamic_worker"} {
		if !classes[required] {
			t.Fatalf("managed inventory missing required %s: %+v", required, inventory.Managed)
		}
	}
	if classes[driverConsoleTopologyClass] {
		t.Fatalf("unrequested cosmetic Driver console was projected: %+v", inventory.Managed)
	}
}
