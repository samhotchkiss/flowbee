package alarm_test

import (
	"context"
	"testing"
	"time"

	"github.com/samhotchkiss/flowbee/internal/alarm"
	"github.com/samhotchkiss/flowbee/internal/clock"
	"github.com/samhotchkiss/flowbee/internal/job"
	"github.com/samhotchkiss/flowbee/internal/store"
	"github.com/samhotchkiss/flowbee/internal/testutil"
)

type capturePub struct{ fired []string }

func (c *capturePub) PublishAlarm(jobID, kind string) { c.fired = append(c.fired, jobID+":"+kind) }

// TestPollerFiresDueAlarm: a Tick past the window fires the no_eligible_worker
// alarm and publishes it. Before the window, nothing fires.
func TestPollerFiresDueAlarm(t *testing.T) {
	st := testutil.NewStore(t)
	st.NoEligibleWorkerDelay = 30 * time.Second
	ctx := context.Background()
	clk := clock.NewFake(time.Unix(1000, 0))

	if _, err := st.SeedJob(ctx, store.SeedParams{
		ID: "j", Kind: job.KindBuild, Flow: "build", Stage: "build", Role: job.RoleEngWorker,
		RequiredCapabilities: []string{"model_family:codex"}, Now: time.Unix(1000, 0),
	}); err != nil {
		t.Fatal(err)
	}

	pub := &capturePub{}
	p := alarm.New(st, clk, time.Hour, pub)

	// not yet due.
	p.Tick(ctx)
	if ok, _ := st.AlarmFired(ctx, "j", store.TimerNoEligibleWorker); ok {
		t.Fatal("alarm fired before the window")
	}

	// past the window.
	clk.Advance(31 * time.Second)
	p.Tick(ctx)
	if ok, _ := st.AlarmFired(ctx, "j", store.TimerNoEligibleWorker); !ok {
		t.Fatal("alarm did not fire after the window")
	}
	if len(pub.fired) != 1 || pub.fired[0] != "j:no_eligible_worker" {
		t.Fatalf("publisher not notified: %v", pub.fired)
	}

	// idempotent: a second tick does not double-fire (timer already marked).
	p.Tick(ctx)
	if len(pub.fired) != 1 {
		t.Fatalf("alarm double-fired: %v", pub.fired)
	}
}
