package driver

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

var ErrLifecycleLaunchMaterial = errors.New("lifecycle launch material unavailable")

type LifecycleProjector interface {
	ProjectLifecycleResult(context.Context, Action, LifecycleReceipt, time.Time) error
}

type LifecycleLaunchMaterialResolver interface {
	ResolveLifecycleLaunch(context.Context, Action, time.Time) (Action, func(bool), error)
}

type LifecycleLaunchMaterialFinalizer interface {
	FinalizeLifecycleLaunch(context.Context, Action, LifecycleReceipt, time.Time) error
}

// LifecycleWorkspaceManager is the Flowbee-owned filesystem boundary required
// by Driver managed-session lifecycle. Driver receives only a configured root
// ID plus relative path and deliberately does not create or delete repositories.
// Prepare runs after the immutable outbox action has been claimed and before an
// Ensure call. Finalize runs only after Driver has returned positive absence for
// the exact Stop action and before Store projects the worker as stopped.
type LifecycleWorkspaceManager interface {
	PrepareLifecycleWorkspace(context.Context, Action, time.Time) error
	FinalizeLifecycleWorkspace(context.Context, Action, LifecycleReceipt, time.Time) error
	// FinalizePreEffectLifecycleWorkspace removes an exact locally prepared
	// workspace after Store has certified that the corresponding Ensure never
	// reached Driver.  It must not inspect or mutate a terminal session.
	FinalizePreEffectLifecycleWorkspace(context.Context, Action, time.Time) error
}

type LifecycleRuntime struct {
	Port                  DriverPort
	Resolver              *EndpointResolver
	Store                 SQLActionStore
	Projector             LifecycleProjector
	Gate                  LifecycleGate
	Materials             LifecycleLaunchMaterialResolver
	Workspaces            LifecycleWorkspaceManager
	RequireManagedAgentV3 bool
	Owner                 string
	ClaimTTL              time.Duration
	MaximumTries          int
}

type LifecycleRuntimeReport struct{ Reclaimed, Verified, Executed, Held, Retried, DeadLettered int }

func (r LifecycleRuntime) Tick(ctx context.Context, now time.Time) (LifecycleRuntimeReport, error) {
	var out LifecycleRuntimeReport
	if (r.Resolver == nil && nilDriverPort(r.Port)) || r.Store.DB == nil || r.Projector == nil || r.Owner == "" {
		return out, errors.New("driver lifecycle runtime requires port, store, projector, and owner")
	}
	if r.ClaimTTL <= 0 {
		r.ClaimTTL = time.Minute
	}
	if r.MaximumTries <= 0 {
		r.MaximumTries = 5
	}
	n, err := r.Store.ReclaimExpiredActions(ctx, now)
	if err != nil {
		return out, err
	}
	out.Reclaimed = int(n)
	if action, ok, err := r.Store.ClaimNextLifecycleVerifying(ctx, r.Owner, now, r.ClaimTTL); err != nil {
		return out, err
	} else if ok {
		// A local pre-effect workspace cleanup has no Driver receipt by design.
		// If the process died after filesystem cleanup but before Store
		// projection, replay the idempotent local finalizer rather than trying to
		// discover/verify a nonexistent Driver effect.
		if action.Kind == "worker_workspace_cleanup" {
			return r.finalizePreEffectWorkspace(ctx, action, now, out, true)
		}
		return r.verify(ctx, action, now, out)
	}
	action, ok, err := r.Store.ClaimNextLifecycleAction(ctx, r.Owner, now, r.ClaimTTL)
	if err != nil || !ok {
		return out, err
	}
	if action.Kind == "worker_workspace_cleanup" {
		return r.finalizePreEffectWorkspace(ctx, action, now, out, false)
	}
	if action.Kind == "builder_launch" || action.Kind == "builder_rework" {
		gate := r.Gate
		if gate == nil {
			gate, _ = r.Projector.(LifecycleGate)
		}
		if gate == nil {
			return r.retryOrDeadLetter(ctx, action,
				errors.New("builder relaunch has no fail-closed capacity gate"), now, out)
		}
		decision, gateErr := gate.PrepareLifecycleAction(ctx, action, now)
		if gateErr != nil {
			return r.retryOrDeadLetter(ctx, action, gateErr, now, out)
		}
		if !decision.Allowed {
			detail := decision.Detail
			if detail == "" {
				detail = "builder relaunch capacity unavailable"
			}
			if err := r.Store.RetryAction(ctx, action.ActionID, r.Owner, action.Epoch,
				detail, now.Add(time.Minute), now); err != nil {
				return out, err
			}
			out.Held++
			return out, nil
		}
	}
	port, err := resolveRuntimePort(r.Resolver, r.Port, action)
	if err != nil {
		return r.retryOrDeadLetter(ctx, action, err, now, out)
	}
	receipt, err := r.executeWithMaterials(ctx, port, action, now)
	if receipt.LifecycleReceiptID != "" {
		if persistErr := r.Store.PersistLifecycleReceipt(ctx, receipt); persistErr != nil {
			return out, persistErr
		}
	}
	if err != nil {
		if errors.Is(err, ErrLifecycleLaunchMaterial) {
			return r.preEffectFailure(ctx, action, err, now, out)
		}
		// Once a lifecycle request reaches Driver, a transport error cannot prove
		// the terminal was not mutated. Persist uncertainty for every response-loss
		// shape; recovery must use by-action receipt lookup and exact presence before
		// the same immutable effect may be resumed. Validation, resolution, and
		// capacity failures above this call remain ordinary retryable pre-send holds.
		if markErr := r.Store.MarkActionVerifying(ctx, action.ActionID, r.Owner,
			action.Epoch, err.Error(), now); markErr != nil {
			return out, markErr
		}
		return out, nil
	}
	if !receipt.Resolved() {
		// A terminal Driver receipt proves this exact action has completed. Its
		// immutable action ID/body cannot be retried with a later action epoch;
		// surface the hold and require an explicitly re-armed/new recovery effect.
		if err := r.Store.DeadLetterAction(ctx, action.ActionID, r.Owner, action.Epoch,
			"Driver lifecycle terminal status "+receipt.Status, now); err != nil {
			return out, err
		}
		out.DeadLettered++
		return out, nil
	}
	if err := r.finalizeMaterials(ctx, action, receipt, now); err != nil {
		if markErr := r.Store.MarkActionVerifying(ctx, action.ActionID, r.Owner, action.Epoch,
			err.Error(), now); markErr != nil {
			return out, markErr
		}
		return out, err
	}
	if err := r.Projector.ProjectLifecycleResult(ctx, action, receipt, now); err != nil {
		// The Driver effect is already durable. Never repeat it because Flowbee's
		// projection transaction failed; recover from the same receipt.
		if markErr := r.Store.MarkActionVerifying(ctx, action.ActionID, r.Owner,
			action.Epoch, err.Error(), now); markErr != nil {
			return out, markErr
		}
		return out, err
	}
	if err := r.Store.AcknowledgeAction(ctx, action.ActionID, r.Owner, action.Epoch, now); err != nil {
		return out, err
	}
	out.Executed++
	return out, nil
}

func (r LifecycleRuntime) preEffectFailure(ctx context.Context, action Action, cause error,
	now time.Time, out LifecycleRuntimeReport) (LifecycleRuntimeReport, error) {
	kind := "materials_resolve"
	if strings.Contains(cause.Error(), "workspace:") {
		kind = "workspace_prepare"
	} else if strings.Contains(cause.Error(), "Flowbee managed agent launch") {
		kind = "launch_validate"
	}
	dead, err := r.Store.RecordLifecyclePreEffectFailure(ctx, action.ActionID, r.Owner, action.Epoch,
		kind, cause.Error(), r.MaximumTries, now)
	if err != nil {
		return out, err
	}
	if dead {
		out.DeadLettered++
	} else {
		out.Retried++
	}
	return out, nil
}

// finalizePreEffectWorkspace executes a Flowbee-local cleanup effect.  It is
// intentionally outside Driver endpoint resolution and creates no lifecycle
// receipt: Store only materializes this action after it has re-proven zero
// Driver receipts plus an explicit pre-effect certificate for the original
// Ensure.  A local filesystem failure remains a claimed effect and is retried;
// it is never projected as stopped until the exact marker/worktree is gone.
func (r LifecycleRuntime) finalizePreEffectWorkspace(ctx context.Context, action Action, now time.Time,
	out LifecycleRuntimeReport, verifying bool) (LifecycleRuntimeReport, error) {
	if r.Workspaces == nil {
		if verifying {
			_ = r.Store.ReleaseVerifying(ctx, action.ActionID, r.Owner, action.Epoch,
				"pre-effect workspace cleanup manager unavailable", now)
			return out, errors.New("pre-effect workspace cleanup manager unavailable")
		}
		return r.retryOrDeadLetter(ctx, action, errors.New("pre-effect workspace cleanup manager unavailable"), now, out)
	}
	if err := r.Workspaces.FinalizePreEffectLifecycleWorkspace(ctx, action, now); err != nil {
		if verifying {
			if releaseErr := r.Store.ReleaseVerifying(ctx, action.ActionID, r.Owner, action.Epoch, err.Error(), now); releaseErr != nil {
				return out, releaseErr
			}
			return out, err
		}
		return r.retryOrDeadLetter(ctx, action, err, now, out)
	}
	receipt := LifecycleReceipt{ActionID: action.ActionID, ActionEpoch: action.Epoch,
		LifecycleKey: action.LifecycleKey, TargetEpoch: action.TargetEpoch,
		Operation: "workspace_cleanup", Status: "cleaned"}
	if err := r.Projector.ProjectLifecycleResult(ctx, action, receipt, now); err != nil {
		if verifying {
			if releaseErr := r.Store.ReleaseVerifying(ctx, action.ActionID, r.Owner, action.Epoch,
				err.Error(), now); releaseErr != nil {
				return out, releaseErr
			}
			return out, err
		}
		if markErr := r.Store.MarkActionVerifying(ctx, action.ActionID, r.Owner, action.Epoch,
			err.Error(), now); markErr != nil {
			return out, markErr
		}
		return out, err
	}
	var ackErr error
	if verifying {
		ackErr = r.Store.AcknowledgeVerifying(ctx, action.ActionID, r.Owner, action.Epoch, now)
	} else {
		ackErr = r.Store.AcknowledgeAction(ctx, action.ActionID, r.Owner, action.Epoch, now)
	}
	if ackErr != nil {
		return out, ackErr
	}
	out.Executed++
	return out, nil
}

func (r LifecycleRuntime) execute(ctx context.Context, port DriverPort, action Action) (LifecycleReceipt, error) {
	target := action.SessionTarget()
	switch action.Kind {
	case "builder_park", "worker_stop":
		return port.StopSession(ctx, target, action)
	case "builder_launch", "builder_rework", "conflict_resolution", "reviewer_launch", "worker_recover":
		return port.EnsureLifecycleSession(ctx, target, action)
	default:
		return LifecycleReceipt{}, fmt.Errorf("unsupported Driver lifecycle action %s", action.Kind)
	}
}

func (r LifecycleRuntime) executeWithMaterials(ctx context.Context, port DriverPort, action Action,
	now time.Time) (LifecycleReceipt, error) {
	ensure := action.Kind == "builder_launch" || action.Kind == "builder_rework" ||
		action.Kind == "conflict_resolution" || action.Kind == "reviewer_launch" ||
		action.Kind == "worker_recover"
	cleanup := func(bool) {}
	if ensure && r.Workspaces != nil {
		if err := r.Workspaces.PrepareLifecycleWorkspace(ctx, action, now); err != nil {
			return LifecycleReceipt{}, fmt.Errorf("%w: workspace: %v", ErrLifecycleLaunchMaterial, err)
		}
	} else if ensure && r.RequireManagedAgentV3 {
		return LifecycleReceipt{}, fmt.Errorf("%w: workspace preparer unavailable", ErrLifecycleLaunchMaterial)
	}
	if ensure && r.Materials != nil {
		var err error
		action, cleanup, err = r.Materials.ResolveLifecycleLaunch(ctx, action, now)
		if err != nil {
			return LifecycleReceipt{}, fmt.Errorf("%w: %v", ErrLifecycleLaunchMaterial, err)
		}
	} else if ensure && r.RequireManagedAgentV3 {
		return LifecycleReceipt{}, fmt.Errorf("%w: resolver unavailable", ErrLifecycleLaunchMaterial)
	}
	if ensure && r.RequireManagedAgentV3 {
		if err := ValidateFlowbeeManagedAgentLaunch(action.SessionTarget()); err != nil {
			cleanup(false)
			return LifecycleReceipt{}, fmt.Errorf("%w: %v", ErrLifecycleLaunchMaterial, err)
		}
	}
	receipt, err := r.execute(ctx, port, action)
	cleanup(err == nil && receipt.Resolved())
	return receipt, err
}

func (r LifecycleRuntime) finalizeMaterials(ctx context.Context, action Action, receipt LifecycleReceipt,
	now time.Time) error {
	if !receipt.Resolved() {
		return nil
	}
	if action.Kind == "worker_stop" && r.Workspaces != nil {
		if err := r.Workspaces.FinalizeLifecycleWorkspace(ctx, action, receipt, now); err != nil {
			return fmt.Errorf("finalize lifecycle workspace: %w", err)
		}
	} else if action.Kind == "worker_stop" && r.RequireManagedAgentV3 {
		return errors.New("finalize lifecycle workspace: manager unavailable")
	}
	if finalizer, ok := r.Materials.(LifecycleLaunchMaterialFinalizer); ok {
		return finalizer.FinalizeLifecycleLaunch(ctx, action, receipt, now)
	}
	return nil
}

func (r LifecycleRuntime) verify(ctx context.Context, action Action, now time.Time,
	out LifecycleRuntimeReport) (LifecycleRuntimeReport, error) {
	port, err := resolveRuntimePort(r.Resolver, r.Port, action)
	if err != nil {
		_ = r.Store.ReleaseVerifying(ctx, action.ActionID, r.Owner, action.Epoch, err.Error(), now)
		return out, err
	}
	receipt, ok, err := port.LifecycleReceiptByAction(ctx, action.ActionID,
		action.LifecycleKey, action.TargetEpoch)
	if err != nil {
		_ = r.Store.ReleaseVerifying(ctx, action.ActionID, r.Owner, action.Epoch, err.Error(), now)
		return out, err
	}
	if !ok {
		presence, presenceErr := port.LifecycleTargetPresence(ctx, action.LifecycleKey, action.TargetEpoch)
		if presenceErr != nil {
			_ = r.Store.ReleaseVerifying(ctx, action.ActionID, r.Owner, action.Epoch,
				presenceErr.Error(), now)
			return out, presenceErr
		}
		safe := (action.Kind == "builder_launch" || action.Kind == "builder_rework" ||
			action.Kind == "reviewer_launch" || action.Kind == "worker_recover") && presence.ExactAbsent()
		if action.Kind == "builder_park" || action.Kind == "worker_stop" {
			safe = presence.ExactAbsent() || presence.Presence == "present" &&
				presence.Identity.SessionID == action.RecipientSessionID &&
				presence.Identity.PaneInstanceID == action.RecipientPaneInstanceID &&
				presence.Identity.AgentRunID == action.RecipientAgentRunID
		}
		if !safe {
			detail := "no Driver receipt and lifecycle presence is " + presence.Presence
			if err := r.Store.ReleaseVerifying(ctx, action.ActionID, r.Owner, action.Epoch,
				detail, now); err != nil {
				return out, err
			}
			return out, nil
		}
		if err := r.Store.ResumeLifecycleAfterAbsentProof(ctx, action, r.Owner, now, r.ClaimTTL); err != nil {
			return out, err
		}
		recovered, executeErr := r.executeWithMaterials(ctx, port, action, now)
		if recovered.LifecycleReceiptID != "" {
			if err := r.Store.PersistLifecycleReceipt(ctx, recovered); err != nil {
				return out, err
			}
		}
		if executeErr != nil {
			_ = r.Store.MarkActionVerifying(ctx, action.ActionID, r.Owner, action.Epoch,
				executeErr.Error(), now)
			if errors.Is(executeErr, ErrUncertain) {
				return out, nil
			}
			return out, executeErr
		}
		if !recovered.Resolved() {
			if err := r.Store.DeadLetterAction(ctx, action.ActionID, r.Owner, action.Epoch,
				"Driver lifecycle recovery resolved "+recovered.Status, now); err != nil {
				return out, err
			}
			out.DeadLettered++
			return out, nil
		}
		if err := r.finalizeMaterials(ctx, action, recovered, now); err != nil {
			_ = r.Store.MarkActionVerifying(ctx, action.ActionID, r.Owner, action.Epoch, err.Error(), now)
			return out, err
		}
		if err := r.Projector.ProjectLifecycleResult(ctx, action, recovered, now); err != nil {
			_ = r.Store.MarkActionVerifying(ctx, action.ActionID, r.Owner, action.Epoch, err.Error(), now)
			return out, err
		}
		if err := r.Store.AcknowledgeAction(ctx, action.ActionID, r.Owner, action.Epoch, now); err != nil {
			return out, err
		}
		out.Executed++
		return out, nil
	}
	if err := r.Store.PersistLifecycleReceipt(ctx, receipt); err != nil {
		return out, err
	}
	if receipt.Uncertain() {
		action, err = r.Store.AdvanceLifecycleVerificationEpoch(ctx, action, r.Owner, now)
		if err != nil {
			return out, err
		}
		verified, verifyErr := port.VerifyLifecycleEffect(ctx, receipt.LifecycleReceiptID,
			action.SessionTarget(), action)
		if verified.LifecycleReceiptID != "" {
			if err := r.Store.PersistLifecycleReceipt(ctx, verified); err != nil {
				return out, err
			}
			receipt = verified
		}
		if verifyErr != nil && !errors.Is(verifyErr, ErrUncertain) {
			_ = r.Store.ReleaseVerifying(ctx, action.ActionID, r.Owner, action.Epoch, verifyErr.Error(), now)
			return out, verifyErr
		}
		if receipt.Uncertain() {
			if err := r.Store.ReleaseVerifying(ctx, action.ActionID, r.Owner, action.Epoch,
				"Driver lifecycle receipt remains uncertain", now); err != nil {
				return out, err
			}
			return out, nil
		}
	}
	if !receipt.Resolved() {
		if err := r.Store.DeadLetterVerifying(ctx, action.ActionID, r.Owner, action.Epoch,
			"Driver lifecycle verification resolved "+receipt.Status, now); err != nil {
			return out, err
		}
		out.DeadLettered++
		return out, nil
	}
	if err := r.finalizeMaterials(ctx, action, receipt, now); err != nil {
		_ = r.Store.MarkActionVerifying(ctx, action.ActionID, r.Owner, action.Epoch, err.Error(), now)
		return out, err
	}
	if err := r.Projector.ProjectLifecycleResult(ctx, action, receipt, now); err != nil {
		_ = r.Store.ReleaseVerifying(ctx, action.ActionID, r.Owner, action.Epoch, err.Error(), now)
		return out, err
	}
	if err := r.Store.AcknowledgeVerifying(ctx, action.ActionID, r.Owner, action.Epoch, now); err != nil {
		return out, err
	}
	out.Verified++
	return out, nil
}

func (r LifecycleRuntime) retryOrDeadLetter(ctx context.Context, action Action, cause error,
	now time.Time, out LifecycleRuntimeReport) (LifecycleRuntimeReport, error) {
	var attempts int
	if err := r.Store.DB.QueryRowContext(ctx, `SELECT attempts FROM epic_actions WHERE id=?`,
		action.ActionID).Scan(&attempts); err != nil {
		return out, err
	}
	if attempts >= r.MaximumTries {
		if err := r.Store.DeadLetterAction(ctx, action.ActionID, r.Owner, action.Epoch,
			cause.Error(), now); err != nil {
			return out, err
		}
		out.DeadLettered++
		return out, nil
	}
	backoff := time.Minute << min(attempts-1, 3)
	if err := r.Store.RetryAction(ctx, action.ActionID, r.Owner, action.Epoch,
		cause.Error(), now.Add(backoff), now); err != nil {
		return out, err
	}
	out.Retried++
	return out, nil
}
