package driver

import (
	"context"
	"errors"
	"fmt"
	"time"
)

type LifecycleProjector interface {
	ProjectLifecycleResult(context.Context, Action, LifecycleReceipt, time.Time) error
}

type LifecycleRuntime struct {
	Port         DriverPort
	Store        SQLActionStore
	Projector    LifecycleProjector
	Gate         LifecycleGate
	Owner        string
	ClaimTTL     time.Duration
	MaximumTries int
}

type LifecycleRuntimeReport struct{ Reclaimed, Verified, Executed, Held, Retried, DeadLettered int }

func (r LifecycleRuntime) Tick(ctx context.Context, now time.Time) (LifecycleRuntimeReport, error) {
	var out LifecycleRuntimeReport
	if r.Port == nil || r.Store.DB == nil || r.Projector == nil || r.Owner == "" {
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
		return r.verify(ctx, action, now, out)
	}
	action, ok, err := r.Store.ClaimNextLifecycleAction(ctx, r.Owner, now, r.ClaimTTL)
	if err != nil || !ok {
		return out, err
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
	receipt, err := r.execute(ctx, action)
	if receipt.LifecycleReceiptID != "" {
		if persistErr := r.Store.PersistLifecycleReceipt(ctx, receipt); persistErr != nil {
			return out, persistErr
		}
	}
	if err != nil {
		if errors.Is(err, ErrUncertain) {
			if markErr := r.Store.MarkActionVerifying(ctx, action.ActionID, r.Owner,
				action.Epoch, err.Error(), now); markErr != nil {
				return out, markErr
			}
			return out, nil
		}
		return r.retryOrDeadLetter(ctx, action, err, now, out)
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

func (r LifecycleRuntime) execute(ctx context.Context, action Action) (LifecycleReceipt, error) {
	target := action.SessionTarget()
	switch action.Kind {
	case "builder_park":
		return r.Port.StopSession(ctx, target, action)
	case "builder_launch", "builder_rework", "conflict_resolution":
		return r.Port.EnsureLifecycleSession(ctx, target, action)
	default:
		return LifecycleReceipt{}, fmt.Errorf("unsupported Driver lifecycle action %s", action.Kind)
	}
}

func (r LifecycleRuntime) verify(ctx context.Context, action Action, now time.Time,
	out LifecycleRuntimeReport) (LifecycleRuntimeReport, error) {
	receipt, ok, err := r.Port.LifecycleReceiptByAction(ctx, action.ActionID,
		action.LifecycleKey, action.TargetEpoch)
	if err != nil {
		_ = r.Store.ReleaseVerifying(ctx, action.ActionID, r.Owner, action.Epoch, err.Error(), now)
		return out, err
	}
	if !ok {
		presence, presenceErr := r.Port.LifecycleTargetPresence(ctx, action.LifecycleKey, action.TargetEpoch)
		if presenceErr != nil {
			_ = r.Store.ReleaseVerifying(ctx, action.ActionID, r.Owner, action.Epoch,
				presenceErr.Error(), now)
			return out, presenceErr
		}
		safe := (action.Kind == "builder_launch" || action.Kind == "builder_rework") && presence.ExactAbsent()
		if action.Kind == "builder_park" {
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
		recovered, executeErr := r.execute(ctx, action)
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
		verified, verifyErr := r.Port.VerifyLifecycleEffect(ctx, receipt.LifecycleReceiptID,
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
