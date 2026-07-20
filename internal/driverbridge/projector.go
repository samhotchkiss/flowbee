// Package driverbridge is the one-way adapter between Driver transport types and
// Flowbee's store projection types. Keeping it outside both packages prevents
// transport tests from acquiring a store↔driver import cycle.
package driverbridge

import (
	"context"
	"errors"
	"time"

	"github.com/samhotchkiss/flowbee/internal/driver"
	"github.com/samhotchkiss/flowbee/internal/store"
)

type Projector struct {
	Store            *store.Store
	CapacityFreshFor time.Duration
}

func builderAction(a driver.Action) store.BuilderLifecycleActionProjection {
	return store.BuilderLifecycleActionProjection{
		ActionID: a.ActionID, Epoch: a.Epoch, ProjectID: a.ProjectID, EpicID: a.EpicID,
		Kind: a.Kind, DedupKey: a.DedupKey, Payload: a.Payload,
		PayloadSHA256: a.PayloadSHA256, HeadSHA: a.HeadSHA, BaseSHA: a.BaseSHA,
		TargetHostID: a.TargetHostID, TargetStoreID: a.TargetStoreID,
		TargetServerDomainID: a.TargetServerDomainID,
		TargetServerID:       a.TargetServerID, LifecycleKey: a.LifecycleKey,
		TargetEpoch: a.TargetEpoch, ProfileID: a.ProfileID,
		WorkspaceRootID: a.WorkspaceRootID, WorkspaceRelativePath: a.WorkspaceRelativePath,
		LeaseID: a.LeaseID, LeaseEpoch: a.LeaseEpoch,
		RecipientSessionID:      a.RecipientSessionID,
		RecipientPaneInstanceID: a.RecipientPaneInstanceID,
		RecipientAgentRunID:     a.RecipientAgentRunID,
	}
}

func (p Projector) PrepareLifecycleAction(ctx context.Context, a driver.Action,
	now time.Time) (driver.LifecycleGateResult, error) {
	if p.Store == nil {
		return driver.LifecycleGateResult{}, errors.New("Driver lifecycle gate has no store")
	}
	var result store.BuilderRelaunchCapacityResult
	var err error
	switch a.Kind {
	case "builder_launch":
		result, err = p.Store.PrepareBuilderLaunch(ctx, builderAction(a), now, p.CapacityFreshFor)
	case "builder_rework":
		result, err = p.Store.PrepareBuilderRelaunch(ctx, builderAction(a), now, p.CapacityFreshFor)
	default:
		return driver.LifecycleGateResult{Allowed: true}, nil
	}
	return driver.LifecycleGateResult{Allowed: result.Allowed, Detail: result.Detail}, err
}

func (p Projector) ProjectLifecycleResult(ctx context.Context, a driver.Action,
	r driver.LifecycleReceipt, now time.Time) error {
	if p.Store == nil {
		return errors.New("Driver lifecycle projector has no store")
	}
	return p.Store.ProjectBuilderLifecycleResult(ctx, builderAction(a), store.BuilderLifecycleReceiptProjection{
		ActionID: r.ActionID, ActionEpoch: r.ActionEpoch, Operation: r.Operation,
		LifecycleKey: r.LifecycleKey, TargetEpoch: r.TargetEpoch, Status: r.Status,
		AbsenceObservedAt: r.AbsenceObservedAt,
		IdentityAfter: store.BuilderLifecycleIdentity{
			HostID: r.IdentityAfter.HostID, StoreID: r.IdentityAfter.StoreID,
			TmuxServerDomainID:   r.IdentityAfter.TmuxServerDomainID,
			TmuxServerInstanceID: r.IdentityAfter.TmuxServerInstanceID,
			LifecycleOwnership:   r.IdentityAfter.Ownership,
			LifecycleKey:         r.IdentityAfter.LifecycleKey, TargetEpoch: r.IdentityAfter.TargetEpoch,
			SessionID: r.IdentityAfter.SessionID, PaneInstanceID: r.IdentityAfter.PaneInstanceID,
			AgentRunID: r.IdentityAfter.AgentRunID, Provider: r.IdentityAfter.Provider,
			ConversationID: r.IdentityAfter.ConversationID,
		},
	}, now)
}
