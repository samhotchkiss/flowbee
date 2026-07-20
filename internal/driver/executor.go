package driver

import (
	"context"
	"errors"
	"fmt"
)

// ActionCommitter is Flowbee's durable action transaction. Implementations must
// persist the immutable payload/hash and epoch before any Driver call.
type ActionCommitter interface {
	CommitAction(context.Context, Action) error
	PersistReceipt(context.Context, Action, Receipt) error
}

// StageEvidence is deliberately separate from transport receipts. A Driver
// receipt only proves terminal insertion; workflow completion requires a later
// observation/fact owned by Flowbee.
type StageEvidence interface {
	AwaitStage(context.Context, Action, Receipt) (bool, error)
}

type ExecuteResult struct {
	Receipt       Receipt
	StageComplete bool
	Uncertain     bool
}

// Executor implements the ownership-safe handoff sequence:
// commit action -> ensure exact session -> grant -> send -> persist receipt ->
// await independent stage evidence. It never calls tmux directly.
type Executor struct {
	Port     DriverPort
	Store    ActionCommitter
	Evidence StageEvidence
}

func (e Executor) Execute(ctx context.Context, target SessionTarget, grant Grant, action Action) (ExecuteResult, error) {
	if e.Port == nil || e.Store == nil {
		return ExecuteResult{}, errors.New("driver executor: missing port or store")
	}
	if err := e.Store.CommitAction(ctx, action); err != nil {
		return ExecuteResult{}, err
	}
	return e.executeCommitted(ctx, target, grant, action)
}

// ExecuteClaimed performs an already-durable outbox action. Runtime workers must
// use this form after ClaimNextAction; committing again would compare the newly
// claimed epoch against the pre-claim immutable intent and is therefore incorrect.
func (e Executor) ExecuteClaimed(ctx context.Context, target SessionTarget, grant Grant, action Action) (ExecuteResult, error) {
	if e.Port == nil || e.Store == nil {
		return ExecuteResult{}, errors.New("driver executor: missing port or store")
	}
	return e.executeCommitted(ctx, target, grant, action)
}

func (e Executor) executeCommitted(ctx context.Context, target SessionTarget, grant Grant, action Action) (ExecuteResult, error) {
	if action.ExecutorKind == "driver" {
		if err := validateBoundRoute(action, target, grant); err != nil {
			return ExecuteResult{}, err
		}
	}
	var identity Identity
	var err error
	if action.ExecutorKind == "driver" && target.Identity.Ownership == "external_observed" {
		identity, err = exactExternalLifecycleIdentity(ctx, e.Port, target)
	} else if action.ExecutorKind == "driver" && observedOnlyTarget(target) {
		identity, err = exactObservedIdentity(ctx, e.Port, target.Identity)
	} else {
		identity, err = e.Port.EnsureSession(ctx, target, action)
	}
	if err != nil {
		return ExecuteResult{}, err
	}
	if !identityMatchesTarget(identity, target) {
		return ExecuteResult{}, ErrIdentityMismatch
	}
	if err := e.Port.Grant(ctx, grant); err != nil {
		return ExecuteResult{}, err
	}
	receiptAction := action
	// Legacy low-level fixtures predate durable route fields on Action. Product
	// runtimes always use executor_kind=driver and are validated above; retain the
	// narrow compatibility path by deriving the receipt expectation from the
	// explicit target and grant passed to this call.
	if action.ExecutorKind != "driver" {
		receiptAction.GrantID, receiptAction.GrantEpoch = grant.GrantID, grant.Epoch
		receiptAction.SenderPrincipalID = grant.SenderPrincipalID
		receiptAction.SenderSessionID, receiptAction.SenderAgentRunID = grant.SenderSessionID, grant.SenderAgentRunID
		receiptAction.RecipientSessionID = grant.RecipientSessionID
		receiptAction.RecipientPaneInstanceID = grant.RecipientPaneInstanceID
		if grant.SenderPrincipalID != "" {
			receiptAction.RecipientAgentRunID = grant.ExpectedRecipientAgentRunID
		}
	}
	req := SendRequest{Action: receiptAction, GrantID: grant.GrantID, RecipientSessionID: grant.RecipientSessionID,
		RecipientPaneInstanceID:     grant.RecipientPaneInstanceID,
		ExpectedRecipientAgentRunID: grant.ExpectedRecipientAgentRunID, GrantEpoch: grant.Epoch}
	// A direct control-origin action is authored by the authenticated Flowbee
	// principal and must omit on_behalf_of_session_id entirely. The field remains
	// only for the frozen legacy session-origin compatibility path.
	if grant.SenderPrincipalID == "" {
		req.OnBehalfOfSessionID = grant.SenderSessionID
	}
	receipt, err := e.Port.Send(ctx, req)
	if err != nil {
		// A crash after Driver accepted delivery is uncertain. Reconcile by the
		// durable by-action receipt; never blindly resend the action.
		if r, ok, lookupErr := e.Port.ReceiptByAction(ctx, receiptAction.ExpectedReceipt()); lookupErr == nil && ok {
			receipt = r
		} else {
			return ExecuteResult{Uncertain: true}, errors.Join(ErrUncertain, err)
		}
	}
	if err := receiptAction.ExpectedReceipt().Validate(receipt); err != nil {
		return ExecuteResult{Receipt: receipt}, err
	}
	if err := e.Store.PersistReceipt(ctx, receiptAction, receipt); err != nil {
		return ExecuteResult{Receipt: receipt}, err
	}
	result := ExecuteResult{Receipt: receipt}
	if !receipt.Submitted() {
		if receipt.MutationUncertain() {
			result.Uncertain = true
			return result, fmt.Errorf("Driver delivery ended %s: %w", receipt.Status, ErrUncertain)
		}
		return result, fmt.Errorf("Driver delivery ended %s (%s)", receipt.Status, receipt.DiagnosticCode)
	}
	if e.Evidence != nil {
		result.StageComplete, err = e.Evidence.AwaitStage(ctx, action, receipt)
		if err != nil {
			return result, err
		}
	}
	return result, nil
}

// exactExternalLifecycleIdentity proves that an already-adopted external actor
// is still the same Driver lifecycle target. It is intentionally read-only:
// routed delivery must never convert observation ownership into Ensure/launch
// authority or mutate the operator's pre-existing tmux session.
func exactExternalLifecycleIdentity(ctx context.Context, port DriverPort, target SessionTarget) (Identity, error) {
	want := target.Identity
	if target.ExternalWatchID == "" || target.LifecycleKey == "" || target.TargetEpoch < 1 ||
		want.Ownership != "external_observed" || want.HostID == "" || want.StoreID == "" ||
		want.TmuxServerDomainID == "" || want.TmuxServerInstanceID == "" || want.SessionID == "" ||
		want.PaneInstanceID == "" || want.AgentRunID == "" {
		return Identity{}, ErrIdentityMismatch
	}
	presence, err := port.LifecycleTargetPresence(ctx, target.LifecycleKey, target.TargetEpoch)
	if err != nil {
		return Identity{}, err
	}
	if presence.Presence != "present" || !identityMatchesTarget(presence.Identity, target) ||
		presence.Identity.Ownership != "external_observed" {
		return Identity{}, ErrIdentityMismatch
	}
	return presence.Identity, nil
}

func observedOnlyTarget(target SessionTarget) bool {
	return target.LifecycleKey == "" && target.TargetEpoch == 0 && target.ProfileID == "" &&
		target.WorkspaceRootID == "" && target.WorkspaceRelativePath == ""
}

// exactObservedIdentity is the non-mutating route for an operator-adopted actor.
// It proves the full stable incarnation against Driver's current snapshot and
// never falls back to a tmux name, raw pane id, CWD, PID, provider, or proximity.
func exactObservedIdentity(ctx context.Context, port DriverPort, want Identity) (Identity, error) {
	if want.HostID == "" || want.StoreID == "" || want.TmuxServerDomainID == "" ||
		want.TmuxServerInstanceID == "" || want.Ownership != "" ||
		want.SessionID == "" || want.PaneInstanceID == "" || want.AgentRunID == "" {
		return Identity{}, ErrIdentityMismatch
	}
	snapshot, err := port.SnapshotSessions(ctx)
	if err != nil {
		return Identity{}, err
	}
	if snapshot.HostID != want.HostID || snapshot.StoreID != want.StoreID {
		return Identity{}, ErrIdentityMismatch
	}
	for _, session := range snapshot.Sessions {
		got := session.Identity
		if got.SessionID != want.SessionID {
			continue
		}
		if session.Lifecycle == "ended" || got.HostID != want.HostID || got.StoreID != want.StoreID ||
			got.TmuxServerDomainID != want.TmuxServerDomainID || got.Ownership != "" ||
			got.TmuxServerInstanceID != want.TmuxServerInstanceID || got.PaneInstanceID != want.PaneInstanceID ||
			got.AgentRunID != want.AgentRunID {
			return Identity{}, ErrIdentityMismatch
		}
		return got, nil
	}
	return Identity{}, ErrIdentityMismatch
}

// validateBoundRoute prevents an executor caller from swapping the immutable
// outbox target or widening A→B into lateral traffic. It runs before Ensure,
// grant projection, or send, so a mismatch produces zero Driver mutation.
func validateBoundRoute(a Action, target SessionTarget, grant Grant) error {
	want := a.SessionTarget()
	if target.Identity.HostID != want.Identity.HostID ||
		target.Identity.StoreID != want.Identity.StoreID ||
		target.Identity.TmuxServerDomainID != want.Identity.TmuxServerDomainID ||
		target.Identity.TmuxServerInstanceID != want.Identity.TmuxServerInstanceID ||
		target.Identity.Ownership != want.Identity.Ownership ||
		target.Identity.SessionID != want.Identity.SessionID ||
		target.Identity.PaneInstanceID != want.Identity.PaneInstanceID ||
		target.Identity.AgentRunID != want.Identity.AgentRunID ||
		target.LifecycleKey != want.LifecycleKey || target.TargetEpoch != want.TargetEpoch ||
		target.ProfileID != want.ProfileID || target.WorkspaceRootID != want.WorkspaceRootID ||
		target.WorkspaceRelativePath != want.WorkspaceRelativePath ||
		target.LeaseID != want.LeaseID || target.LeaseEpoch != want.LeaseEpoch ||
		target.ExternalWatchID != want.ExternalWatchID {
		return fmt.Errorf("Driver target differs from immutable action: %w", ErrIdentityMismatch)
	}
	if grant.GrantID != a.GrantID || grant.Epoch != a.Epoch ||
		(a.GrantEpoch != 0 && grant.Epoch != a.GrantEpoch) ||
		grant.SenderPrincipalID != a.SenderPrincipalID ||
		grant.SenderSessionID != a.SenderSessionID ||
		grant.SenderAgentRunID != a.SenderAgentRunID ||
		grant.RecipientSessionID != a.RecipientSessionID ||
		grant.RecipientPaneInstanceID != a.RecipientPaneInstanceID ||
		grant.ExpectedRecipientAgentRunID != a.RouteGrant().ExpectedRecipientAgentRunID {
		return fmt.Errorf("Driver grant differs from immutable action: %w", ErrGrantDenied)
	}
	controlOrigin := grant.SenderPrincipalID != "" && grant.SenderSessionID == "" && grant.SenderAgentRunID == ""
	sessionOrigin := grant.SenderPrincipalID == "" && grant.SenderSessionID != "" && grant.SenderAgentRunID != "" && grant.SenderSessionID != grant.RecipientSessionID
	if grant.RecipientSessionID == "" || (!controlOrigin && !sessionOrigin) {
		return fmt.Errorf("Driver grant is not an explicit directional route: %w", ErrGrantDenied)
	}
	return nil
}

func identityMatchesTarget(got Identity, target SessionTarget) bool {
	want := target.Identity
	if target.LifecycleKey == "" && want.LifecycleKey == "" && want.TmuxServerInstanceID == "" {
		return got == want // deterministic fake/legacy contract fixtures.
	}
	if got.HostID != want.HostID || got.StoreID != want.StoreID ||
		got.TmuxServerDomainID != want.TmuxServerDomainID ||
		(want.Ownership != "" && got.Ownership != want.Ownership) ||
		got.TmuxServerInstanceID != want.TmuxServerInstanceID {
		return false
	}
	key := target.LifecycleKey
	if key == "" {
		key = want.LifecycleKey
	}
	epoch := target.TargetEpoch
	if epoch == 0 {
		epoch = want.TargetEpoch
	}
	if key != "" && got.LifecycleKey != key || epoch != 0 && got.TargetEpoch != epoch {
		return false
	}
	for _, pair := range [][2]string{{want.SessionID, got.SessionID}, {want.PaneInstanceID, got.PaneInstanceID}, {want.AgentRunID, got.AgentRunID}} {
		if pair[0] != "" && pair[0] != pair[1] {
			return false
		}
	}
	return got.SessionID != "" && got.PaneInstanceID != "" && got.AgentRunID != ""
}
