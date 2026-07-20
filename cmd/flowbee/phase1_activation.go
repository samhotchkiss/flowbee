package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/samhotchkiss/flowbee/internal/api"
	"github.com/samhotchkiss/flowbee/internal/driver"
	"github.com/samhotchkiss/flowbee/internal/store"
)

const phase1ActorLifecycleStartupBudget = 32
const phase1WatchdogLeaseFreshness = 2 * time.Minute

type actorLifecycleTick func(context.Context, time.Time) (driver.ActorLifecycleRuntimeReport, error)

// drainStartupActorLifecycle executes all immediately-progressable actor
// lifecycle work before Phase 1 evaluates project readiness. One runtime Tick
// claims at most one action, so a single successful Tick is not evidence that
// both the Interactor and Orchestrator are active. Held, uncertain, retrying, or
// dead-lettered actions deliberately stop the drain and are rejected by the
// subsequent project activation proof.
func drainStartupActorLifecycle(ctx context.Context, now time.Time, tick actorLifecycleTick,
	budget int) (driver.ActorLifecycleRuntimeReport, error) {
	if tick == nil || budget < 1 {
		return driver.ActorLifecycleRuntimeReport{}, fmt.Errorf("startup project actor lifecycle requires a tick and positive budget")
	}
	var total driver.ActorLifecycleRuntimeReport
	for attempt := 0; attempt < budget; attempt++ {
		report, err := tick(ctx, now.Add(time.Duration(attempt)*time.Nanosecond))
		if err != nil {
			return total, err
		}
		addActorLifecycleReport(&total, report)
		immediateProgress := report.Materialized + report.Resumed + report.VerificationReady +
			report.Executed + report.Verified
		if immediateProgress == 0 {
			return total, nil
		}
	}
	return total, fmt.Errorf("startup project actor lifecycle exceeded %d immediately-progressable passes", budget)
}

func addActorLifecycleReport(total *driver.ActorLifecycleRuntimeReport, report driver.ActorLifecycleRuntimeReport) {
	total.Materialized += report.Materialized
	total.Held += report.Held
	total.Resumed += report.Resumed
	total.DeliveryUncertain += report.DeliveryUncertain
	total.VerificationReady += report.VerificationReady
	total.Executed += report.Executed
	total.Verified += report.Verified
	total.Retried += report.Retried
	total.DeadLettered += report.DeadLettered
}

func requirePhase1ProjectLiveReady(ctx context.Context, st *store.Store, projectID string,
	now time.Time) (store.ProjectActivationStatus, error) {
	if st == nil || strings.TrimSpace(projectID) == "" {
		return store.ProjectActivationStatus{}, fmt.Errorf("Phase 1 activation requires an exact project")
	}
	status, err := st.ProjectActivation(ctx, projectID, now, projectActivationCapacityFreshness)
	if err != nil {
		return store.ProjectActivationStatus{}, fmt.Errorf("read Phase 1 project %q activation: %w", projectID, err)
	}
	if !status.LiveReady {
		return status, fmt.Errorf("Phase 1 project %q is not live-ready after startup reconciliation: holds=%s",
			projectID, strings.Join(status.Holds, ","))
	}
	return status, nil
}

func currentPhase1ProjectReadiness(st *store.Store, projectID, watchdogID string,
	now time.Time) api.Phase1ProjectReadiness {
	out := api.Phase1ProjectReadiness{Required: true, ProjectID: projectID, Status: "held"}
	if st == nil || strings.TrimSpace(projectID) == "" || strings.TrimSpace(watchdogID) == "" {
		out.Reason = "exact project and external watchdog identity are required"
		out.Holds = []string{"external_watchdog_identity_missing"}
		return out
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	status, err := st.ProjectActivation(ctx, projectID, now, projectActivationCapacityFreshness)
	if err != nil {
		out.Reason = err.Error()
		out.Holds = []string{"project_activation_unavailable"}
		return out
	}
	out.Holds = append(out.Holds, status.Holds...)
	lease, fresh, err := st.ExternalWatchdogLeaseFresh(ctx, projectID, watchdogID,
		now, phase1WatchdogLeaseFreshness)
	if err != nil {
		out.Reason = err.Error()
		out.Holds = append(out.Holds, "external_watchdog_lease_unavailable")
		return out
	}
	if !fresh {
		out.Holds = append(out.Holds, "external_watchdog_lease_missing_or_stale")
		if lease.WatchdogID != "" && lease.WatchdogID != watchdogID {
			out.Holds = append(out.Holds, "external_watchdog_identity_mismatch")
		}
	}
	if status.LiveReady && fresh {
		out.Available, out.Status, out.Holds = true, "ready", nil
		return out
	}
	out.Reason = strings.Join(out.Holds, ",")
	return out
}
