package main

import "testing"

func TestBaseV2ReconcilerGraceCoversDedicatedWorkerTicks(t *testing.T) {
	grace := baseV2ReconcilerGrace()
	for _, name := range []string{
		"builder_lifecycle",
		"epic_worker_stop",
		"epic_worker_liveness",
		"project_actor_lifecycle",
		"builder_launch",
	} {
		if grace[name] <= 0 {
			t.Fatalf("v2 reconciler %q is missing a positive supervision grace", name)
		}
	}
}
