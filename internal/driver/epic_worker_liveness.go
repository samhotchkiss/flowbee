package driver

import (
	"context"
	"errors"
	"time"

	"github.com/samhotchkiss/flowbee/internal/store"
)

// EpicWorkerLivenessRuntime continuously verifies workers after Ensure has
// already been acknowledged. LifecycleRuntime's verifying path is not enough:
// without this loop a later pane/agent death would leave a durable active row
// forever and silently stall the epic.
type EpicWorkerLivenessRuntime struct {
	Port     DriverPort
	Resolver *EndpointResolver
	Store    *store.Store
}

type EpicWorkerLivenessReport struct {
	Scanned, Present, Recovered, Stopped, Held, ProbeErrors int
}

func (r EpicWorkerLivenessRuntime) Tick(ctx context.Context, now time.Time) (EpicWorkerLivenessReport, error) {
	var out EpicWorkerLivenessReport
	if r.Store == nil || r.Store.DB == nil || (r.Resolver == nil && nilDriverPort(r.Port)) {
		return out, errors.New("epic worker liveness requires store and exact Driver endpoint")
	}
	targets, err := r.Store.ActiveEpicWorkerLivenessTargets(ctx)
	if err != nil {
		return out, err
	}
	for _, target := range targets {
		out.Scanned++
		action := Action{TargetHostID: target.HostID, TargetStoreID: target.StoreID,
			TargetServerDomainID: target.ServerDomainID, TargetServerID: target.ServerID}
		port, resolveErr := resolveRuntimePort(r.Resolver, r.Port, action)
		if resolveErr != nil {
			out.ProbeErrors++
			if err := r.Store.RecordEpicWorkerPresenceUncertain(ctx, target,
				"exact Driver endpoint unavailable: "+resolveErr.Error(), now); err != nil {
				return out, err
			}
			continue
		}
		presence, probeErr := port.LifecycleTargetPresence(ctx, target.LifecycleKey, target.TargetEpoch)
		if probeErr != nil {
			out.ProbeErrors++
			if err := r.Store.RecordEpicWorkerPresenceUncertain(ctx, target,
				"exact Driver presence probe failed: "+probeErr.Error(), now); err != nil {
				return out, err
			}
			continue
		}
		if presence.Presence == "present" && lifecyclePresenceMatchesWorker(presence.Identity, target) {
			out.Present++
			continue
		}
		if !presence.ExactAbsent() {
			out.Held++
			if err := r.Store.RecordEpicWorkerPresenceUncertain(ctx, target,
				"Driver presence did not prove the exact active incarnation absent", now); err != nil {
				return out, err
			}
			continue
		}
		result, err := r.Store.RecoverEpicWorkerExactAbsence(ctx, target, now)
		if err != nil {
			return out, err
		}
		if result.ReplacementCreated {
			out.Recovered++
		}
		if result.Stopped {
			out.Stopped++
		}
		if result.Held {
			out.Held++
		}
	}
	return out, nil
}

func lifecyclePresenceMatchesWorker(identity Identity, target store.EpicWorkerLivenessTarget) bool {
	return identity.HostID == target.HostID && identity.StoreID == target.StoreID &&
		identity.TmuxServerDomainID == target.ServerDomainID &&
		identity.TmuxServerInstanceID == target.ServerID && identity.Ownership == "driver_managed" &&
		identity.LifecycleKey == target.LifecycleKey && identity.TargetEpoch == target.TargetEpoch &&
		identity.SessionID == target.SessionID && identity.PaneInstanceID == target.PaneInstanceID &&
		identity.AgentRunID == target.AgentRunID
}
