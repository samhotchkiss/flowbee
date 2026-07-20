package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/samhotchkiss/flowbee/internal/driver"
	"github.com/samhotchkiss/flowbee/internal/testutil"
)

func TestDrainStartupActorLifecycleRunsEveryImmediatelyProgressableAction(t *testing.T) {
	now := time.Date(2026, 7, 20, 5, 0, 0, 0, time.UTC)
	var calls int
	report, err := drainStartupActorLifecycle(context.Background(), now,
		func(_ context.Context, got time.Time) (driver.ActorLifecycleRuntimeReport, error) {
			calls++
			if got.Before(now) {
				t.Fatalf("startup tick moved backwards: %s", got)
			}
			switch calls {
			case 1:
				return driver.ActorLifecycleRuntimeReport{Materialized: 2, Executed: 1}, nil
			case 2:
				return driver.ActorLifecycleRuntimeReport{Executed: 1}, nil
			default:
				return driver.ActorLifecycleRuntimeReport{}, nil
			}
		}, 8)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 3 || report.Materialized != 2 || report.Executed != 2 {
		t.Fatalf("calls=%d report=%+v", calls, report)
	}
}

func TestDrainStartupActorLifecycleDoesNotSpinOnDurableHold(t *testing.T) {
	var calls int
	report, err := drainStartupActorLifecycle(context.Background(), time.Now(),
		func(context.Context, time.Time) (driver.ActorLifecycleRuntimeReport, error) {
			calls++
			return driver.ActorLifecycleRuntimeReport{Held: 1}, nil
		}, 8)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 || report.Held != 1 {
		t.Fatalf("held lifecycle was spun or lost: calls=%d report=%+v", calls, report)
	}
}

func TestDrainStartupActorLifecycleFailsClosedAtBudget(t *testing.T) {
	_, err := drainStartupActorLifecycle(context.Background(), time.Now(),
		func(context.Context, time.Time) (driver.ActorLifecycleRuntimeReport, error) {
			return driver.ActorLifecycleRuntimeReport{Executed: 1}, nil
		}, 2)
	if err == nil || !strings.Contains(err.Error(), "exceeded 2") {
		t.Fatalf("unbounded startup lifecycle did not fail closed: %v", err)
	}
}

func TestRequirePhase1ProjectLiveReadyRejectsIncompleteExactProject(t *testing.T) {
	st := testutil.NewStore(t)
	status, err := requirePhase1ProjectLiveReady(context.Background(), st, "default", time.Now())
	if err == nil || !strings.Contains(err.Error(), `project "default" is not live-ready`) {
		t.Fatalf("incomplete project readiness err=%v status=%+v", err, status)
	}
	if status.LiveReady || len(status.Holds) == 0 {
		t.Fatalf("incomplete project false-greened: %+v", status)
	}
}
