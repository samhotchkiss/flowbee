package driver

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/samhotchkiss/flowbee/internal/store"
)

// ActorLifecycleRuntime executes only the project-actor outbox introduced by
// migration 0052. It deliberately does not reuse epic_actions: project actors
// have independent intent versions, watches, leases, receipts, and recovery
// clocks.
type ActorLifecycleRuntime struct {
	Resolver              *EndpointResolver
	Store                 *store.Store
	Materials             LifecycleLaunchMaterialResolver
	RequireManagedAgentV3 bool
	Owner                 string
	ClaimTTL              time.Duration
	MaxRecovery           int
}

type ActorLifecycleRuntimeReport struct {
	Materialized      int
	Held              int
	Resumed           int
	DeliveryUncertain int
	VerificationReady int
	Executed          int
	Verified          int
	Retried           int
	DeadLettered      int
	PresenceScanned   int
	PresenceHeld      int
	PresenceRecovered int
	PresenceErrors    int
}

func actorDriverAction(action store.ProjectActorLifecycleAction) Action {
	return Action{
		ActionID: action.ID, Epoch: action.ActionEpoch, ProjectID: action.ProjectID,
		Kind: action.Operation, DedupKey: action.DedupKey, Payload: action.Payload, InstanceRef: action.InstanceRef,
		PayloadSHA256: action.PayloadSHA, ExecutorKind: "project_actor_lifecycle",
		TargetRole: action.Role, TargetHostID: action.TargetHostID, TargetStoreID: action.TargetStoreID,
		TargetServerDomainID: action.TargetServerDomainID, TargetServerID: action.TargetServerID,
		LifecycleKey: action.LifecycleKey, TargetEpoch: action.TargetEpoch, ProfileID: action.ProfileID,
		WorkspaceRootID: action.WorkspaceRootID, WorkspaceRelativePath: action.WorkspaceRelativePath,
		LeaseID: action.LeaseID, LeaseEpoch: action.LeaseEpoch, ExternalWatchID: action.ExternalWatchID,
		RecipientSessionID: action.ExpectedSessionID, RecipientPaneInstanceID: action.ExpectedPaneInstanceID,
		RecipientAgentRunID: action.ExpectedAgentRunID,
	}
}

func actorStoreIdentity(identity Identity) store.ProjectActorLifecycleIdentity {
	return store.ProjectActorLifecycleIdentity{
		HostID: identity.HostID, StoreID: identity.StoreID,
		TmuxServerDomainID:   identity.TmuxServerDomainID,
		TmuxServerInstanceID: identity.TmuxServerInstanceID,
		LifecycleOwnership:   identity.Ownership, LifecycleKey: identity.LifecycleKey,
		TargetEpoch: identity.TargetEpoch, SessionID: identity.SessionID,
		PaneInstanceID: identity.PaneInstanceID, AgentRunID: identity.AgentRunID,
		Provider: identity.Provider, ConversationID: identity.ConversationID,
	}
}

func actorStoreReceipt(receipt LifecycleReceipt) store.ProjectActorLifecycleReceipt {
	return store.ProjectActorLifecycleReceipt{
		ID: receipt.LifecycleReceiptID, ActionID: receipt.ActionID, ActionEpoch: receipt.ActionEpoch,
		Operation: receipt.Operation, LifecycleKey: receipt.LifecycleKey, TargetEpoch: receipt.TargetEpoch,
		LeaseID: receipt.LeaseID, LeaseEpoch: receipt.LeaseEpoch,
		TmuxServerDomainID: receipt.TmuxServerDomainID, ExternalWatchID: receipt.ExternalWatchID,
		Status: receipt.Status, IdentityBefore: actorStoreIdentity(receipt.IdentityBefore),
		IdentityAfter: actorStoreIdentity(receipt.IdentityAfter), AbsenceObservedAt: receipt.AbsenceObservedAt,
		DiagnosticCode: receipt.DiagnosticCode,
	}
}

func (r ActorLifecycleRuntime) normalize() (ActorLifecycleRuntime, error) {
	if r.Resolver == nil || r.Store == nil || r.Store.DB == nil || r.Owner == "" {
		return r, errors.New("project actor lifecycle runtime requires endpoint resolver, store, and owner")
	}
	if r.ClaimTTL <= 0 {
		r.ClaimTTL = time.Minute
	}
	if r.MaxRecovery <= 0 {
		r.MaxRecovery = 5
	}
	return r, nil
}

func (r ActorLifecycleRuntime) Tick(ctx context.Context, now time.Time) (ActorLifecycleRuntimeReport, error) {
	r, err := r.normalize()
	if err != nil {
		return ActorLifecycleRuntimeReport{}, err
	}
	presenceReport, err := r.reconcileActivePresence(ctx, now)
	if err != nil {
		return ActorLifecycleRuntimeReport{}, err
	}
	recovery, err := r.Store.ReconcileExpiredProjectActorLifecycleClaims(ctx, now, r.MaxRecovery, 20)
	if err != nil {
		return ActorLifecycleRuntimeReport{}, err
	}
	intentRecovery, err := r.Store.ReconcileProjectActorLifecycleActions(ctx, now, 20)
	if err != nil {
		return ActorLifecycleRuntimeReport{}, err
	}
	report := ActorLifecycleRuntimeReport{Materialized: intentRecovery.Materialized,
		Held: intentRecovery.Held, Resumed: intentRecovery.Resumed, DeliveryUncertain: recovery.DeliveryUncertain,
		VerificationReady: recovery.VerificationReady, DeadLettered: recovery.DeadLettered,
		PresenceScanned: presenceReport.Scanned, PresenceHeld: presenceReport.Held,
		PresenceRecovered: presenceReport.Recovered,
		PresenceErrors:    presenceReport.Errors}
	verification, err := r.Store.ClaimNextProjectActorLifecycleVerification(ctx, r.Owner, now, now.Add(r.ClaimTTL))
	if err == nil {
		return r.verify(ctx, verification, now, report)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return report, err
	}
	action, err := r.Store.ClaimNextProjectActorLifecycleAction(ctx, r.Owner, now, now.Add(r.ClaimTTL))
	if errors.Is(err, sql.ErrNoRows) {
		return report, nil
	}
	if err != nil {
		return report, err
	}
	endpoint, err := r.Resolver.ResolveAction(actorDriverAction(action))
	if err != nil {
		return r.preEffectFailure(ctx, action, err, now, report)
	}
	driverAction := actorDriverAction(action)
	cleanup := func(bool) {}
	if action.Operation == "ensure" && (r.RequireManagedAgentV3 || r.Materials != nil) {
		if r.Materials == nil {
			return r.preEffectFailure(ctx, action, errors.New("project actor Q3 material resolver unavailable"), now, report)
		}
		driverAction, cleanup, err = r.Materials.ResolveLifecycleLaunch(ctx, driverAction, now)
		if err != nil {
			return r.preEffectFailure(ctx, action, err, now, report)
		}
		if r.RequireManagedAgentV3 {
			if err := ValidateFlowbeeManagedAgentLaunch(driverAction.SessionTarget()); err != nil {
				cleanup(false)
				return r.preEffectFailure(ctx, action, err, now, report)
			}
		}
	}
	receipt, executeErr := executeActorLifecycle(ctx, endpoint.Port, driverAction)
	cleanup(executeErr == nil && receipt.Resolved())
	if executeErr != nil {
		// Once the lifecycle method was invoked, Flowbee cannot infer whether the
		// Driver committed before the response was lost. Verification is the only
		// safe continuation; this action is never returned to pending here.
		if markErr := r.Store.MarkProjectActorLifecycleActionVerifying(ctx, action.ID, r.Owner,
			action.ActionEpoch, now, now.Add(r.ClaimTTL), executeErr.Error()); markErr != nil {
			return report, markErr
		}
		return report, nil
	}
	if !receipt.Resolved() {
		if markErr := r.Store.MarkProjectActorLifecycleActionVerifying(ctx, action.ID, r.Owner,
			action.ActionEpoch, now, now.Add(r.ClaimTTL), "Driver lifecycle status "+receipt.Status); markErr != nil {
			return report, markErr
		}
		return report, nil
	}
	if err := r.persistProjectAcknowledge(ctx, action, driverAction, receipt, now); err != nil {
		// Persisted receipts are replayable. If projection/ack failed, preserve the
		// effect as verification work and never call the lifecycle mutation again.
		_ = r.Store.MarkProjectActorLifecycleActionVerifying(ctx, action.ID, r.Owner,
			action.ActionEpoch, now, now.Add(r.ClaimTTL), err.Error())
		return report, err
	}
	report.Executed++
	return report, nil
}

type actorPresenceReport struct{ Scanned, Held, Recovered, Errors int }

func (r ActorLifecycleRuntime) reconcileActivePresence(ctx context.Context, now time.Time) (actorPresenceReport, error) {
	var report actorPresenceReport
	targets, err := r.Store.ActiveProjectActorLivenessTargets(ctx)
	if err != nil {
		return report, err
	}
	for _, target := range targets {
		report.Scanned++
		endpoint, err := r.Resolver.ResolveAction(Action{InstanceRef: target.InstanceRef,
			TargetHostID: target.HostID, TargetStoreID: target.StoreID,
			TargetServerDomainID: target.ServerDomainID, TargetServerID: target.ServerID})
		if err != nil {
			report.Errors++
			continue
		}
		if target.LifecycleOwnership == "external_observed" {
			if err := target.ValidateManagedRecovery(); err != nil {
				report.Errors++
				continue
			}
			inventory, err := endpoint.Port.LifecycleProfiles(ctx)
			if err != nil {
				report.Errors++
				continue
			}
			probe := SessionTarget{Identity: Identity{HostID: target.HostID, StoreID: target.StoreID,
				TmuxServerDomainID: target.ServerDomainID, TmuxServerInstanceID: target.ServerID,
				Ownership: "driver_managed", LifecycleKey: target.LifecycleKey, TargetEpoch: target.TargetEpoch + 1},
				LifecycleKey: target.LifecycleKey, TargetEpoch: target.TargetEpoch + 1,
				ProfileID:             target.ManagedRecoveryProfileID,
				WorkspaceRootID:       target.ManagedRecoveryWorkspaceRootID,
				WorkspaceRelativePath: target.ManagedRecoveryWorkspaceRelativePath,
				Bootstrap: &LifecycleBootstrapArtifact{Format: "initial_prompt_utf8/v1", ContentUTF8: "preflight",
					PayloadSHA256: "sha256:0000000000000000000000000000000000000000000000000000000000000000"},
				CredentialEnvelope: &LifecycleCredentialEnvelope{EnvelopeID: "preflight", Format: "flowbee.credential/v1",
					SecretUTF8: "preflight", PayloadSHA256: "sha256:0000000000000000000000000000000000000000000000000000000000000000",
					CredentialEpoch: target.TargetEpoch + 1},
				PresentationName: target.ProjectID + "-interactor"}
			if err := inventory.ValidateLaunch(target.ManagedRecoveryProfileID, target.ServerDomainID, probe); err != nil {
				report.Errors++
				continue
			}
		}
		presence, err := endpoint.Port.LifecycleTargetPresence(ctx, target.LifecycleKey, target.TargetEpoch)
		if err != nil {
			report.Errors++
			continue
		}
		if presence.Presence == "present" && actorLivenessPresenceMatches(presence.Identity, target) {
			continue
		}
		if !presence.ExactAbsent() {
			report.Errors++
			continue
		}
		if target.LifecycleOwnership == "external_observed" {
			_, promoted, err := r.Store.PromoteAdoptedInteractorExactAbsence(ctx, target, now)
			if err != nil {
				report.Errors++
				continue
			}
			if promoted {
				report.Recovered++
			}
			continue
		}
		if err := r.Store.HoldManagedProjectActorExactAbsence(ctx, target, now); err != nil {
			return report, err
		}
		held, err := r.Store.CurrentProjectActorLifecycle(ctx, target.ProjectID, target.Role)
		if err != nil {
			return report, err
		}
		_, _, err = r.Store.CommitProjectActorLifecycleIntent(ctx, store.ProjectActorLifecycleCommand{
			ProjectID: target.ProjectID, Role: target.Role, ActorID: target.ActorID,
			ExpectedRouteStateVersion:     target.RouteStateVersion,
			ExpectedLifecycleStateVersion: held.StateVersion, Operation: "ensure",
			IdempotencyKey: "actor-presence-recover:" + target.ProjectID + ":" + target.Role + ":" +
				target.ActorID + ":" + fmt.Sprint(target.TargetEpoch+1),
			InstanceRef: target.InstanceRef, TargetHostID: target.HostID, TargetStoreID: target.StoreID,
			TargetServerDomainID: target.ServerDomainID, TargetServerID: target.ServerID,
			LifecycleOwnership: "driver_managed", LifecycleKey: target.LifecycleKey,
			TargetEpoch: target.TargetEpoch + 1, ProfileID: target.ProfileID,
			WorkspaceRootID: target.WorkspaceRootID, WorkspaceRelativePath: target.WorkspaceRelativePath,
		}, now)
		if err != nil {
			report.Held++
			continue
		}
		report.Recovered++
	}
	return report, nil
}

func actorLivenessPresenceMatches(id Identity, target store.ProjectActorLivenessTarget) bool {
	expectedOwnership := target.LifecycleOwnership
	return id.HostID == target.HostID && id.StoreID == target.StoreID &&
		id.TmuxServerDomainID == target.ServerDomainID && id.TmuxServerInstanceID == target.ServerID &&
		id.Ownership == expectedOwnership && id.LifecycleKey == target.LifecycleKey &&
		id.TargetEpoch == target.TargetEpoch && id.SessionID == target.SessionID &&
		id.PaneInstanceID == target.PaneInstanceID && id.AgentRunID == target.AgentRunID
}

func executeActorLifecycle(ctx context.Context, port DriverPort, action Action) (LifecycleReceipt, error) {
	target := actorSessionTarget(action)
	switch action.Kind {
	case "ensure":
		return port.EnsureLifecycleSession(ctx, target, action)
	case "adopt":
		return port.AdoptSession(ctx, target, action)
	case "reattach":
		return port.ReattachSession(ctx, target, action)
	case "stop":
		return port.StopSession(ctx, target, action)
	case "release":
		return port.ReleaseSession(ctx, target, action)
	default:
		return LifecycleReceipt{}, fmt.Errorf("unsupported project actor lifecycle operation %q", action.Kind)
	}
}

func actorSessionTarget(action Action) SessionTarget {
	target := action.SessionTarget()
	switch action.Kind {
	case "stop":
		target.Identity.Ownership = "driver_managed"
	case "release":
		target.Identity.Ownership = "external_observed"
	case "reattach":
		target.Identity.Ownership = "driver_managed"
		if action.ExternalWatchID != "" {
			target.Identity.Ownership = "external_observed"
		}
	}
	return target
}

func (r ActorLifecycleRuntime) preEffectFailure(ctx context.Context, action store.ProjectActorLifecycleAction,
	cause error, now time.Time, report ActorLifecycleRuntimeReport) (ActorLifecycleRuntimeReport, error) {
	backoff := time.Minute << min(action.RecoveryCount, 3)
	err := r.Store.RecordProjectActorLifecyclePreEffectFailure(ctx, action.ID, r.Owner, action.ActionEpoch,
		cause.Error(), now, now.Add(backoff), r.MaxRecovery)
	if err != nil {
		return report, err
	}
	if action.RecoveryCount+1 >= r.MaxRecovery {
		report.DeadLettered++
	} else {
		report.Retried++
	}
	return report, nil
}

func (r ActorLifecycleRuntime) persistProjectAcknowledge(ctx context.Context,
	action store.ProjectActorLifecycleAction, driverAction Action, receipt LifecycleReceipt, now time.Time) error {
	if _, err := r.Store.PersistProjectActorLifecycleReceipt(ctx, actorStoreReceipt(receipt), now); err != nil {
		return err
	}
	// Receipt durability precedes destruction of the one-shot credential source.
	// A crash after this point is recoverable by action lookup; a crash before it
	// leaves the exact committed envelope available for the same idempotent action.
	if finalizer, ok := r.Materials.(LifecycleLaunchMaterialFinalizer); ok {
		if err := finalizer.FinalizeLifecycleLaunch(ctx, driverAction, receipt, now); err != nil {
			return err
		}
	}
	if err := r.Store.ProjectPersistedProjectActorLifecycleReceipt(ctx, action.ID, now); err != nil {
		return err
	}
	return r.Store.AcknowledgeProjectActorLifecycleAction(ctx, action.ID, r.Owner, action.ActionEpoch, now)
}

func (r ActorLifecycleRuntime) verify(ctx context.Context, action store.ProjectActorLifecycleAction, now time.Time,
	report ActorLifecycleRuntimeReport) (ActorLifecycleRuntimeReport, error) {
	driverAction := actorDriverAction(action)
	endpoint, err := r.Resolver.ResolveAction(driverAction)
	if err != nil {
		// Resolution failure is pre-effect only for a fresh action. This action is
		// already uncertain, so retain verification authority until the endpoint
		// returns or bounded reconciliation dead-letters it.
		return report, nil
	}
	receipt, found, err := endpoint.Port.LifecycleReceiptByAction(ctx, action.ID, action.LifecycleKey, action.TargetEpoch)
	if err != nil {
		return report, nil
	}
	if found && receipt.Uncertain() {
		action, err = r.Store.AdvanceProjectActorLifecycleVerificationEpoch(ctx, action.ID, r.Owner,
			action.ActionEpoch, now, now.Add(r.ClaimTTL))
		if err != nil {
			return report, err
		}
		driverAction = actorDriverAction(action)
		verified, verifyErr := endpoint.Port.VerifyLifecycleEffect(ctx, receipt.LifecycleReceiptID,
			actorSessionTarget(driverAction), driverAction)
		if verifyErr == nil {
			receipt = verified
		}
	}
	if !found || !receipt.Resolved() {
		presence, presenceErr := endpoint.Port.LifecycleTargetPresence(ctx, action.LifecycleKey, action.TargetEpoch)
		if presenceErr != nil {
			return report, nil
		}
		receipt, found = actorReceiptFromPresence(action, presence)
	}
	if !found || !receipt.Resolved() {
		return report, nil
	}
	if err := r.persistProjectAcknowledge(ctx, action, driverAction, receipt, now); err != nil {
		return report, err
	}
	report.Verified++
	return report, nil
}

func actorReceiptFromPresence(action store.ProjectActorLifecycleAction, presence LifecyclePresence) (LifecycleReceipt, bool) {
	expected := Identity{HostID: action.TargetHostID, StoreID: action.TargetStoreID,
		TmuxServerDomainID: action.TargetServerDomainID, TmuxServerInstanceID: action.TargetServerID,
		LifecycleKey: action.LifecycleKey, TargetEpoch: action.TargetEpoch,
		SessionID: action.ExpectedSessionID, PaneInstanceID: action.ExpectedPaneInstanceID,
		AgentRunID: action.ExpectedAgentRunID}
	receipt := LifecycleReceipt{LifecycleReceiptID: "presence:" + action.ID, Operation: action.Operation,
		ActionID: action.ID, ActionEpoch: action.ActionEpoch, LeaseID: action.LeaseID,
		LeaseEpoch: action.LeaseEpoch, LifecycleKey: action.LifecycleKey,
		TmuxServerDomainID: action.TargetServerDomainID, ExternalWatchID: action.ExternalWatchID,
		TargetEpoch: action.TargetEpoch, DiagnosticCode: "exact_lifecycle_presence"}
	switch action.Operation {
	case "stop":
		if !presence.ExactAbsent() {
			return LifecycleReceipt{}, false
		}
		receipt.Status, receipt.AbsenceObservedAt, receipt.IdentityBefore = "target_absent", presence.ObservedAt, expected
	case "release":
		if !presence.ExactAbsent() {
			return LifecycleReceipt{}, false
		}
		receipt.Status, receipt.IdentityBefore = "released", expected
	case "ensure":
		if presence.Presence != "present" || !actorPresenceMatchesTarget(presence.Identity, action, false) ||
			presence.Identity.Ownership != "driver_managed" {
			return LifecycleReceipt{}, false
		}
		receipt.Status, receipt.IdentityAfter = "ensured", presence.Identity
	case "adopt":
		if presence.Presence != "present" || !actorPresenceMatchesTarget(presence.Identity, action, true) ||
			presence.Identity.Ownership != "external_observed" {
			return LifecycleReceipt{}, false
		}
		receipt.Status, receipt.IdentityAfter = "adopted", presence.Identity
	case "reattach":
		if presence.Presence != "present" || !actorPresenceMatchesTarget(presence.Identity, action, false) ||
			(presence.Identity.Ownership != "driver_managed" && presence.Identity.Ownership != "external_observed") {
			return LifecycleReceipt{}, false
		}
		receipt.Status, receipt.IdentityBefore, receipt.IdentityAfter = "reattached", expected, presence.Identity
	default:
		return LifecycleReceipt{}, false
	}
	return receipt, true
}

func actorPresenceMatchesTarget(id Identity, action store.ProjectActorLifecycleAction, requireExpectedIncarnation bool) bool {
	if id.HostID != action.TargetHostID || id.StoreID != action.TargetStoreID ||
		id.TmuxServerDomainID != action.TargetServerDomainID || id.TmuxServerInstanceID != action.TargetServerID ||
		id.LifecycleKey != action.LifecycleKey || id.TargetEpoch != action.TargetEpoch ||
		id.SessionID == "" || id.PaneInstanceID == "" || id.AgentRunID == "" {
		return false
	}
	return !requireExpectedIncarnation || id.SessionID == action.ExpectedSessionID &&
		id.PaneInstanceID == action.ExpectedPaneInstanceID && id.AgentRunID == action.ExpectedAgentRunID
}
