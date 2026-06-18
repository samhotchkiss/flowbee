package webhook

import (
	"context"
	"testing"
)

// TestReplayPendingDrivesRefetchAndMarks: pending deliveries left by a crash between
// RecordDelivery and MarkDeliveryProcessed are re-driven on boot — a PR event gets its
// targeted refetch, an issues event gets an intake sweep, and every replayed delivery is
// marked processed so it never strands.
func TestReplayPendingDrivesRefetchAndMarks(t *testing.T) {
	spy := &spyRefetcher{}
	var marked []string
	pending := []PendingDelivery{
		{DeliveryID: "d1", Event: "pull_request", PRNumber: 7},
		{DeliveryID: "d2", Event: "issues", PRNumber: 0},
		{DeliveryID: "d3", Event: "pull_request", PRNumber: 9},
	}
	n := ReplayPending(context.Background(), pending, spy, func(id string) error {
		marked = append(marked, id)
		return nil
	})

	if n != 3 {
		t.Fatalf("replayed %d, want 3", n)
	}
	if got := spy.count(); got != 2 {
		t.Fatalf("PR refetches = %d, want 2 (the two pull_request events)", got)
	}
	if spy.sweeps != 1 {
		t.Fatalf("intake sweeps = %d, want 1 (the issues event)", spy.sweeps)
	}
	if len(marked) != 3 {
		t.Fatalf("marked %d processed, want all 3 (none may strand): %v", len(marked), marked)
	}
}

// TestReplayPendingEmptyIsNoop: nothing pending => no refetch, no error.
func TestReplayPendingEmptyIsNoop(t *testing.T) {
	spy := &spyRefetcher{}
	if n := ReplayPending(context.Background(), nil, spy, func(string) error { return nil }); n != 0 {
		t.Fatalf("empty replay = %d, want 0", n)
	}
	if spy.count() != 0 || spy.sweeps != 0 {
		t.Fatal("empty replay must not touch the refetcher")
	}
}
