package store_test

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/samhotchkiss/flowbee/internal/store"
	"github.com/samhotchkiss/flowbee/internal/testutil"
)

func actorLifecycleFixture(t *testing.T, actorID string) (*store.Store, context.Context, time.Time, store.ProjectActorRoute) {
	return actorLifecycleFixtureRole(t, store.DriverOrchestratorRole, actorID)
}

func actorLifecycleFixtureRole(t *testing.T, role, actorID string) (*store.Store, context.Context, time.Time, store.ProjectActorRoute) {
	t.Helper()
	st := testutil.NewStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 19, 20, 0, 0, 0, time.UTC)
	if _, err := st.CreatePortfolioProject(ctx, store.PortfolioProject{
		ID: "russ", Name: "Russ", Priority: 10, SchedulerWeight: 1, ConcurrencyCap: 2,
	}, now); err != nil {
		t.Fatal(err)
	}
	route, err := st.RegisterProjectActor(ctx, store.ProjectActorRoute{
		ProjectID: "russ", Role: role, ActorID: actorID,
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	return st, ctx, now, route
}

func managedEnsureCommand(route store.ProjectActorRoute, key string) store.ProjectActorLifecycleCommand {
	return store.ProjectActorLifecycleCommand{
		ProjectID: route.ProjectID, Role: route.Role, ActorID: route.ActorID,
		ExpectedRouteStateVersion: int64(route.StateVersion), Operation: "ensure", IdempotencyKey: key,
		InstanceRef: "managed-driver", TargetHostID: "local", TargetStoreID: "managed-store",
		TargetServerDomainID: "managed_dedicated", TargetServerID: "server-managed",
		LifecycleOwnership: "driver_managed", LifecycleKey: "orchestrator-russ", TargetEpoch: 1,
		ProfileID: "codex-orchestrator", WorkspaceRootID: "russ-root", WorkspaceRelativePath: "russ",
	}
}

func externalAdoptCommand(route store.ProjectActorRoute, key string) store.ProjectActorLifecycleCommand {
	return store.ProjectActorLifecycleCommand{
		ProjectID: route.ProjectID, Role: route.Role, ActorID: route.ActorID,
		ExpectedRouteStateVersion: int64(route.StateVersion), Operation: "adopt", IdempotencyKey: key,
		InstanceRef: "external-driver", TargetHostID: "local", TargetStoreID: "external-store",
		TargetServerDomainID: "external_default", TargetServerID: "server-external",
		LifecycleOwnership: "external_observed", LifecycleKey: "external-russ-claude", TargetEpoch: 1,
		ProfileID: "claude-interactor", ExternalWatchID: "watch-russ-claude",
		ExpectedSessionID: "session-russ-claude", ExpectedPaneInstanceID: "pane-russ-claude",
		ExpectedAgentRunID: "run-russ-claude-1",
	}
}

func terminalReceipt(action store.ProjectActorLifecycleAction, status string) store.ProjectActorLifecycleReceipt {
	return store.ProjectActorLifecycleReceipt{
		ActionID: action.ID, Operation: action.Operation, LifecycleKey: action.LifecycleKey,
		Status: status, ActionEpoch: action.ActionEpoch, TargetEpoch: action.TargetEpoch,
		LeaseID: action.LeaseID, LeaseEpoch: action.LeaseEpoch,
		TmuxServerDomainID: action.TargetServerDomainID, ExternalWatchID: action.ExternalWatchID,
	}
}

func actionExpectedIdentity(action store.ProjectActorLifecycleAction) store.ProjectActorLifecycleIdentity {
	return store.ProjectActorLifecycleIdentity{
		HostID: action.TargetHostID, StoreID: action.TargetStoreID,
		TmuxServerDomainID: action.TargetServerDomainID, TmuxServerInstanceID: action.TargetServerID,
		LifecycleOwnership: action.LifecycleOwnership, LifecycleKey: action.LifecycleKey,
		TargetEpoch: action.TargetEpoch, SessionID: action.ExpectedSessionID,
		PaneInstanceID: action.ExpectedPaneInstanceID, AgentRunID: action.ExpectedAgentRunID,
		Provider: "claude", ConversationID: "conversation-russ-claude",
	}
}

func projectAndAckActorReceipt(t *testing.T, st *store.Store, ctx context.Context,
	action store.ProjectActorLifecycleAction, owner string, receipt store.ProjectActorLifecycleReceipt, now time.Time) {
	t.Helper()
	if _, err := st.PersistProjectActorLifecycleReceipt(ctx, receipt, now); err != nil {
		t.Fatal(err)
	}
	if err := st.ProjectPersistedProjectActorLifecycleReceipt(ctx, action.ID, now); err != nil {
		t.Fatal(err)
	}
	if err := st.AcknowledgeProjectActorLifecycleAction(ctx, action.ID, owner, action.ActionEpoch, now); err != nil {
		t.Fatal(err)
	}
}

func TestProjectActorLifecycleCommitIsAtomicImmutableAndIdempotent(t *testing.T) {
	st, ctx, now, route := actorLifecycleFixture(t, "russ-codex")
	command := managedEnsureCommand(route, "ensure-russ-orchestrator-v1")
	lifecycle, action, err := st.CommitProjectActorLifecycleIntent(ctx, command, now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if lifecycle.State != "awaiting_ensure" || lifecycle.CurrentActionID != action.ID || action.State != "pending" {
		t.Fatalf("intent/action were not atomically visible: lifecycle=%+v action=%+v", lifecycle, action)
	}
	var bindings int
	if err := st.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM driver_session_bindings WHERE project_id='russ'`).Scan(&bindings); err != nil {
		t.Fatal(err)
	}
	if bindings != 0 {
		t.Fatalf("commit-before-effect violated: got %d session bindings", bindings)
	}

	// A lost response retries with the caller's original expected version. The
	// payload-bound key must return the same durable action, not conflict.
	replayedLifecycle, replayedAction, err := st.CommitProjectActorLifecycleIntent(ctx, command, now.Add(2*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if replayedLifecycle.StateVersion != lifecycle.StateVersion || replayedAction.ID != action.ID {
		t.Fatalf("lost-response replay changed intent/action: lifecycle=%+v action=%+v", replayedLifecycle, replayedAction)
	}
	changed := command
	changed.ProfileID = "different-profile"
	if _, _, err := st.CommitProjectActorLifecycleIntent(ctx, changed, now.Add(3*time.Minute)); !errors.Is(err, store.ErrProjectActorLifecycleConflict) {
		t.Fatalf("changed body under idempotency key: got %v", err)
	}
	if _, err := st.DB.ExecContext(ctx, `UPDATE project_actor_lifecycle_actions SET target_store_id='forged' WHERE id=?`, action.ID); err == nil ||
		!strings.Contains(err.Error(), "immutable") {
		t.Fatalf("immutable action accepted mutation: %v", err)
	}
}

func TestProjectActorLifecycleSchemaHasOnlyStablePaneAuthority(t *testing.T) {
	st, ctx, _, _ := actorLifecycleFixture(t, "russ-codex")
	rows, err := st.DB.QueryContext(ctx, `PRAGMA table_info(project_actor_lifecycle_actions)`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	columns := map[string]bool{}
	for rows.Next() {
		var cid, notNull, primaryKey int
		var name, kind string
		var defaultValue sql.NullString
		if err := rows.Scan(&cid, &name, &kind, &notNull, &defaultValue, &primaryKey); err != nil {
			t.Fatal(err)
		}
		columns[name] = true
	}
	if !columns["expected_pane_instance_id"] {
		t.Fatal("stable pane_instance_id fence is missing")
	}
	for _, forbidden := range []string{"pane_id", "pane_selector", "pane_name", "tmux_session_name", "cwd", "pid", "socket_path"} {
		if columns[forbidden] {
			t.Fatalf("raw process/tmux authority leaked into durable action schema: %s", forbidden)
		}
	}
}

func TestProjectActorLifecycleCrashAfterProjectionReplaysWithoutDuplicateEffect(t *testing.T) {
	st, ctx, now, route := actorLifecycleFixture(t, "russ-codex")
	_, pending, err := st.CommitProjectActorLifecycleIntent(ctx,
		managedEnsureCommand(route, "ensure-russ-orchestrator-v1"), now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := st.ClaimNextProjectActorLifecycleAction(ctx, "executor-a", now.Add(2*time.Minute), now.Add(7*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if claimed.ID != pending.ID || claimed.ActionEpoch != 1 || claimed.State != "delivering" {
		t.Fatalf("unexpected claim: %+v", claimed)
	}
	receipt := store.ProjectActorLifecycleReceipt{
		ActionID: claimed.ID, Operation: claimed.Operation, LifecycleKey: claimed.LifecycleKey,
		Status: "ensured", ActionEpoch: claimed.ActionEpoch, TargetEpoch: claimed.TargetEpoch,
		LeaseID: claimed.LeaseID, LeaseEpoch: claimed.LeaseEpoch, TmuxServerDomainID: claimed.TargetServerDomainID,
		IdentityAfter: store.ProjectActorLifecycleIdentity{
			HostID: claimed.TargetHostID, StoreID: claimed.TargetStoreID,
			TmuxServerDomainID: claimed.TargetServerDomainID, TmuxServerInstanceID: claimed.TargetServerID,
			LifecycleOwnership: claimed.LifecycleOwnership, LifecycleKey: claimed.LifecycleKey,
			TargetEpoch: claimed.TargetEpoch, SessionID: "session-russ-orch",
			PaneInstanceID: "pane-instance-russ-orch", AgentRunID: "agent-run-russ-orch",
			Provider: "codex", ConversationID: "conversation-russ-orch",
		},
	}
	stale := receipt
	stale.TmuxServerDomainID = "external_default"
	if _, err := st.PersistProjectActorLifecycleReceipt(ctx, stale, now.Add(3*time.Minute)); !errors.Is(err, store.ErrProjectActorActionStale) {
		t.Fatalf("stale domain receipt was accepted: %v", err)
	}
	persisted, err := st.PersistProjectActorLifecycleReceipt(ctx, receipt, now.Add(3*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if persisted.ID == "" {
		t.Fatal("receipt was not durably identified")
	}
	if _, err := st.PersistProjectActorLifecycleReceipt(ctx, receipt, now.Add(3*time.Minute)); err != nil {
		t.Fatalf("exact receipt replay failed: %v", err)
	}
	changedReceipt := receipt
	changedReceipt.Status = "different"
	if _, err := st.PersistProjectActorLifecycleReceipt(ctx, changedReceipt, now.Add(3*time.Minute)); !errors.Is(err, store.ErrProjectActorActionStale) {
		t.Fatalf("changed receipt replay was accepted: %v", err)
	}
	if err := st.ProjectPersistedProjectActorLifecycleReceipt(ctx, receipt.ActionID, now.Add(3*time.Minute)); err != nil {
		t.Fatal(err)
	}
	// Simulate a crash after projection but before the executor acknowledges its
	// outbox row. Re-folding the same receipt must be a no-op.
	if err := st.ProjectPersistedProjectActorLifecycleReceipt(ctx, receipt.ActionID, now.Add(4*time.Minute)); err != nil {
		t.Fatal(err)
	}
	var bindings, projectedEvents int
	if err := st.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM driver_session_bindings
		WHERE project_id='russ' AND worker_identity='russ-codex' AND role='orchestrator'`).Scan(&bindings); err != nil {
		t.Fatal(err)
	}
	if err := st.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM control_events
		WHERE project_id='russ' AND kind='project_actor_lifecycle_projected'`).Scan(&projectedEvents); err != nil {
		t.Fatal(err)
	}
	if bindings != 1 || projectedEvents != 1 {
		t.Fatalf("projection replay duplicated effect: bindings=%d events=%d", bindings, projectedEvents)
	}
	if err := st.AcknowledgeProjectActorLifecycleAction(ctx, claimed.ID, "executor-a", claimed.ActionEpoch,
		now.Add(5*time.Minute)); err != nil {
		t.Fatal(err)
	}
	lifecycle, err := st.CurrentProjectActorLifecycle(ctx, "russ", store.DriverOrchestratorRole)
	if err != nil {
		t.Fatal(err)
	}
	action, err := st.GetProjectActorLifecycleAction(ctx, claimed.ID)
	if err != nil {
		t.Fatal(err)
	}
	if lifecycle.State != "active" || lifecycle.CurrentActionID != "" || lifecycle.ActiveBindingID == "" || action.State != "acknowledged" {
		t.Fatalf("projection/ack did not converge: lifecycle=%+v action=%+v", lifecycle, action)
	}
	if _, err := st.PersistProjectActorLifecycleReceipt(ctx, receipt, now.Add(6*time.Minute)); err != nil {
		t.Fatalf("receipt replay after acknowledgement failed: %v", err)
	}
	if err := st.ProjectPersistedProjectActorLifecycleReceipt(ctx, receipt.ActionID, now.Add(6*time.Minute)); err != nil {
		t.Fatalf("projection replay after acknowledgement failed: %v", err)
	}
}

func TestProjectActorLifecycleExpiredClaimBecomesVerificationThenDeadLetter(t *testing.T) {
	st, ctx, now, route := actorLifecycleFixture(t, "russ-codex")
	_, pending, err := st.CommitProjectActorLifecycleIntent(ctx,
		managedEnsureCommand(route, "ensure-russ-orchestrator-v1"), now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := st.ClaimNextProjectActorLifecycleAction(ctx, "executor-a", now.Add(2*time.Minute), now.Add(3*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	report, err := st.ReconcileExpiredProjectActorLifecycleClaims(ctx, now.Add(4*time.Minute), 1, 10)
	if err != nil {
		t.Fatal(err)
	}
	if report.DeliveryUncertain != 1 || report.DeadLettered != 0 {
		t.Fatalf("delivery expiry was not made uncertain: %+v", report)
	}
	verification, err := st.ClaimNextProjectActorLifecycleVerification(ctx, "verifier-b",
		now.Add(5*time.Minute), now.Add(6*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if verification.ID != pending.ID || verification.ActionEpoch != claimed.ActionEpoch || verification.State != "verifying" {
		t.Fatalf("verification changed effect authority: claimed=%+v verification=%+v", claimed, verification)
	}
	report, err = st.ReconcileExpiredProjectActorLifecycleClaims(ctx, now.Add(7*time.Minute), 1, 10)
	if err != nil {
		t.Fatal(err)
	}
	if report.DeadLettered != 1 {
		t.Fatalf("bounded verification did not dead-letter: %+v", report)
	}
	action, err := st.GetProjectActorLifecycleAction(ctx, pending.ID)
	if err != nil {
		t.Fatal(err)
	}
	lifecycle, err := st.CurrentProjectActorLifecycle(ctx, "russ", store.DriverOrchestratorRole)
	if err != nil {
		t.Fatal(err)
	}
	var alerts int
	if err := st.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM control_alerts
		WHERE project_id='russ' AND kind='project_actor_lifecycle_stalled'`).Scan(&alerts); err != nil {
		t.Fatal(err)
	}
	if action.State != "dead_letter" || lifecycle.State != "failed" || !lifecycle.AlertPending || alerts != 1 {
		t.Fatalf("dead-letter was not visible/durable: action=%+v lifecycle=%+v alerts=%d", action, lifecycle, alerts)
	}
	if _, err := st.ClaimNextProjectActorLifecycleAction(ctx, "executor-c", now.Add(8*time.Minute), now.Add(9*time.Minute)); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("dead-letter became resendable: %v", err)
	}
}

func TestProjectActorLifecycleUncertainDeliveryDoesNotReturnToPending(t *testing.T) {
	st, ctx, now, route := actorLifecycleFixture(t, "russ-codex")
	_, pending, err := st.CommitProjectActorLifecycleIntent(ctx,
		managedEnsureCommand(route, "ensure-russ-orchestrator-v1"), now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := st.ClaimNextProjectActorLifecycleAction(ctx, "executor-a", now.Add(2*time.Minute), now.Add(7*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if err := st.MarkProjectActorLifecycleActionVerifying(ctx, pending.ID, "executor-a", claimed.ActionEpoch,
		now.Add(3*time.Minute), now.Add(13*time.Minute), "driver delivery uncertain"); err != nil {
		t.Fatal(err)
	}
	action, err := st.GetProjectActorLifecycleAction(ctx, pending.ID)
	if err != nil {
		t.Fatal(err)
	}
	lifecycle, err := st.CurrentProjectActorLifecycle(ctx, "russ", store.DriverOrchestratorRole)
	if err != nil {
		t.Fatal(err)
	}
	if action.State != "verifying" || lifecycle.State != "verifying_ensure" || lifecycle.StateDueAt.IsZero() {
		t.Fatalf("uncertain delivery was not made durable/visible: action=%+v lifecycle=%+v", action, lifecycle)
	}
	if _, err := st.ClaimNextProjectActorLifecycleAction(ctx, "executor-b", now.Add(20*time.Minute), now.Add(25*time.Minute)); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("uncertain action became resendable: %v", err)
	}
}

func TestProjectActorLifecycleRetriesOnlyCertifiedPreEffectFailure(t *testing.T) {
	st, ctx, now, route := actorLifecycleFixture(t, "russ-codex")
	_, pending, err := st.CommitProjectActorLifecycleIntent(ctx,
		managedEnsureCommand(route, "ensure-russ-orchestrator-v1"), now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := st.ClaimNextProjectActorLifecycleAction(ctx, "executor-a", now.Add(2*time.Minute), now.Add(7*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if err := st.RecordProjectActorLifecyclePreEffectFailure(ctx, pending.ID, "executor-a", claimed.ActionEpoch,
		"route denied before submission", now.Add(3*time.Minute), now.Add(4*time.Minute), 3); err != nil {
		t.Fatal(err)
	}
	action, err := st.GetProjectActorLifecycleAction(ctx, pending.ID)
	if err != nil {
		t.Fatal(err)
	}
	lifecycle, err := st.CurrentProjectActorLifecycle(ctx, "russ", store.DriverOrchestratorRole)
	if err != nil {
		t.Fatal(err)
	}
	if action.State != "pending" || action.RecoveryCount != 1 || action.ClaimOwner != "" ||
		lifecycle.State != "awaiting_ensure" || lifecycle.StateDueAt.IsZero() {
		t.Fatalf("pre-effect retry was not durably scheduled: action=%+v lifecycle=%+v", action, lifecycle)
	}
}

func TestProjectActorLifecycleReconcilerRepairsMissingMaterialization(t *testing.T) {
	st, ctx, now, route := actorLifecycleFixture(t, "russ-codex")
	lifecycle, action, err := st.CommitProjectActorLifecycleIntent(ctx,
		managedEnsureCommand(route, "ensure-russ-orchestrator-v1"), now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	// Model a cancelled pre-effect action whose durable intent survived. The
	// reconciler must derive a new generation from that intent, not from memory.
	stamp := now.Add(2 * time.Minute).Format(time.RFC3339Nano)
	if _, err := st.DB.ExecContext(ctx, `UPDATE project_actor_lifecycle_actions
		SET state='cancelled_superseded',updated_at=? WHERE id=?`, stamp, action.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB.ExecContext(ctx, `UPDATE project_actor_lifecycles SET current_action_id='',
		state_due_at=?,updated_at=? WHERE project_id=? AND role=? AND actor_id=?`, stamp, stamp,
		lifecycle.ProjectID, lifecycle.Role, lifecycle.ActorID); err != nil {
		t.Fatal(err)
	}
	report, err := st.ReconcileProjectActorLifecycleActions(ctx, now.Add(3*time.Minute), 10)
	if err != nil {
		t.Fatal(err)
	}
	if report.Materialized != 1 {
		t.Fatalf("expected one recovered action, got %+v", report)
	}
	current, err := st.CurrentProjectActorLifecycle(ctx, "russ", store.DriverOrchestratorRole)
	if err != nil {
		t.Fatal(err)
	}
	recovered, err := st.GetProjectActorLifecycleAction(ctx, current.CurrentActionID)
	if err != nil {
		t.Fatal(err)
	}
	if recovered.ID == action.ID || recovered.ActionGeneration != action.ActionGeneration+1 || recovered.PayloadSHA != action.PayloadSHA {
		t.Fatalf("reconciler did not derive the next immutable action: old=%+v new=%+v", action, recovered)
	}
}

func TestProjectActorLifecycleExternalAdoptThenRelease(t *testing.T) {
	st, ctx, now, route := actorLifecycleFixtureRole(t, store.DriverInteractorRole, "russ-claude")
	_, _, err := st.CommitProjectActorLifecycleIntent(ctx,
		externalAdoptCommand(route, "adopt-russ-claude-v1"), now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	adopt, err := st.ClaimNextProjectActorLifecycleAction(ctx, "executor-a", now.Add(2*time.Minute), now.Add(7*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	adoptReceipt := terminalReceipt(adopt, "adopted")
	adoptReceipt.IdentityAfter = actionExpectedIdentity(adopt)
	projectAndAckActorReceipt(t, st, ctx, adopt, "executor-a", adoptReceipt, now.Add(3*time.Minute))
	active, err := st.CurrentProjectActorLifecycle(ctx, "russ", store.DriverInteractorRole)
	if err != nil {
		t.Fatal(err)
	}
	if active.State != "active" || active.ExternalWatchID != "watch-russ-claude" || active.ActiveBindingID == "" {
		t.Fatalf("external adopt did not activate exact watch binding: %+v", active)
	}

	releaseCommand := store.ProjectActorLifecycleCommand{
		ProjectID: "russ", Role: store.DriverInteractorRole, ActorID: "russ-claude",
		ExpectedRouteStateVersion: int64(route.StateVersion), ExpectedLifecycleStateVersion: active.StateVersion,
		Operation: "release", IdempotencyKey: "release-russ-claude-v1", InstanceRef: "external-driver",
	}
	if _, _, err := st.CommitProjectActorLifecycleIntent(ctx, releaseCommand, now.Add(4*time.Minute)); err != nil {
		t.Fatal(err)
	}
	release, err := st.ClaimNextProjectActorLifecycleAction(ctx, "executor-b", now.Add(5*time.Minute), now.Add(10*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	releaseReceipt := terminalReceipt(release, "released")
	releaseReceipt.IdentityBefore = actionExpectedIdentity(release)
	projectAndAckActorReceipt(t, st, ctx, release, "executor-b", releaseReceipt, now.Add(6*time.Minute))
	retired, err := st.CurrentProjectActorLifecycle(ctx, "russ", store.DriverInteractorRole)
	if err != nil {
		t.Fatal(err)
	}
	if retired.State != "released" || retired.ActiveBindingID != "" {
		t.Fatalf("external release did not retire binding: %+v", retired)
	}
	var activeBindings int
	if err := st.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM driver_session_bindings
		WHERE project_id='russ' AND worker_identity='russ-claude' AND role='interactor' AND state='active'`).Scan(&activeBindings); err != nil {
		t.Fatal(err)
	}
	if activeBindings != 0 {
		t.Fatalf("release left %d active external bindings", activeBindings)
	}
}

func TestProjectActorLifecycleExternalReattachFencesPriorRun(t *testing.T) {
	st, ctx, now, route := actorLifecycleFixtureRole(t, store.DriverInteractorRole, "russ-claude")
	_, _, err := st.CommitProjectActorLifecycleIntent(ctx,
		externalAdoptCommand(route, "adopt-russ-claude-v1"), now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	adopt, err := st.ClaimNextProjectActorLifecycleAction(ctx, "executor-a", now.Add(2*time.Minute), now.Add(7*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	adoptReceipt := terminalReceipt(adopt, "adopted")
	adoptReceipt.IdentityAfter = actionExpectedIdentity(adopt)
	projectAndAckActorReceipt(t, st, ctx, adopt, "executor-a", adoptReceipt, now.Add(3*time.Minute))
	active, err := st.CurrentProjectActorLifecycle(ctx, "russ", store.DriverInteractorRole)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.CommitProjectActorLifecycleIntent(ctx, store.ProjectActorLifecycleCommand{
		ProjectID: "russ", Role: store.DriverInteractorRole, ActorID: "russ-claude",
		ExpectedRouteStateVersion: int64(route.StateVersion), ExpectedLifecycleStateVersion: active.StateVersion,
		Operation: "reattach", IdempotencyKey: "reattach-russ-claude-v2", InstanceRef: "external-driver",
	}, now.Add(4*time.Minute)); err != nil {
		t.Fatal(err)
	}
	reattach, err := st.ClaimNextProjectActorLifecycleAction(ctx, "executor-b", now.Add(5*time.Minute), now.Add(10*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	receipt := terminalReceipt(reattach, "reattached")
	receipt.IdentityBefore = actionExpectedIdentity(reattach)
	receipt.IdentityAfter = actionExpectedIdentity(reattach)
	receipt.IdentityAfter.SessionID = "session-russ-claude-v2"
	receipt.IdentityAfter.PaneInstanceID = "pane-russ-claude-v2"
	receipt.IdentityAfter.AgentRunID = "run-russ-claude-2"
	staleProjection := store.ProjectActorLifecycleReceiptProjection{
		ActionID: receipt.ActionID, Operation: receipt.Operation, LifecycleKey: receipt.LifecycleKey,
		Status: receipt.Status, ActionEpoch: receipt.ActionEpoch, TargetEpoch: receipt.TargetEpoch,
		LeaseID: receipt.LeaseID, LeaseEpoch: receipt.LeaseEpoch,
		TmuxServerDomainID: receipt.TmuxServerDomainID, ExternalWatchID: receipt.ExternalWatchID,
		IdentityBefore: receipt.IdentityBefore, IdentityAfter: receipt.IdentityAfter,
	}
	staleProjection.IdentityBefore.AgentRunID = "run-russ-claude-stale"
	if err := st.ProjectProjectActorLifecycleResult(ctx, staleProjection, now.Add(6*time.Minute)); !errors.Is(err, store.ErrProjectActorActionStale) {
		t.Fatalf("reattach accepted stale prior agent run: %v", err)
	}
	projectAndAckActorReceipt(t, st, ctx, reattach, "executor-b", receipt, now.Add(7*time.Minute))
	binding, err := st.ActiveDriverSessionBinding(ctx, "russ", "russ-claude", store.DriverInteractorRole)
	if err != nil {
		t.Fatal(err)
	}
	if binding.AgentRunID != "run-russ-claude-2" || binding.ExternalWatchID != "watch-russ-claude" || binding.BindingEpoch != 2 {
		t.Fatalf("reattach did not fence old run/watch authority: %+v", binding)
	}
}

func TestProjectActorLifecycleReplacementHeldUntilPriorRelease(t *testing.T) {
	st, ctx, now, route := actorLifecycleFixtureRole(t, store.DriverInteractorRole, "russ-claude")
	_, _, err := st.CommitProjectActorLifecycleIntent(ctx,
		externalAdoptCommand(route, "adopt-russ-claude-v1"), now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	adopt, err := st.ClaimNextProjectActorLifecycleAction(ctx, "executor-a", now.Add(2*time.Minute), now.Add(7*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	adoptReceipt := terminalReceipt(adopt, "adopted")
	adoptReceipt.IdentityAfter = actionExpectedIdentity(adopt)
	projectAndAckActorReceipt(t, st, ctx, adopt, "executor-a", adoptReceipt, now.Add(3*time.Minute))
	prior, err := st.GetProjectActorLifecycle(ctx, "russ", store.DriverInteractorRole, "russ-claude")
	if err != nil {
		t.Fatal(err)
	}

	nextRoute, err := st.RegisterProjectActor(ctx, store.ProjectActorRoute{
		ProjectID: "russ", Role: store.DriverInteractorRole, ActorID: "russ-claude-v2",
	}, now.Add(4*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	nextCommand := externalAdoptCommand(nextRoute, "adopt-russ-claude-v2")
	nextCommand.LifecycleKey = "external-russ-claude-v2"
	nextCommand.ExternalWatchID = "watch-russ-claude-v2"
	nextCommand.ExpectedSessionID = "session-russ-claude-v2"
	nextCommand.ExpectedPaneInstanceID = "pane-russ-claude-v2"
	nextCommand.ExpectedAgentRunID = "run-russ-claude-v2"
	held, action, err := st.CommitProjectActorLifecycleIntent(ctx, nextCommand, now.Add(5*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if held.State != "held" || held.HoldKind != "prior_actor_retirement" || action.ID != "" {
		t.Fatalf("replacement was not held before prior retirement: lifecycle=%+v action=%+v", held, action)
	}

	if _, _, err := st.CommitProjectActorLifecycleIntent(ctx, store.ProjectActorLifecycleCommand{
		ProjectID: "russ", Role: store.DriverInteractorRole, ActorID: "russ-claude",
		ExpectedRouteStateVersion: int64(nextRoute.StateVersion), ExpectedLifecycleStateVersion: prior.StateVersion,
		Operation: "release", IdempotencyKey: "release-prior-russ-claude", InstanceRef: "external-driver",
	}, now.Add(6*time.Minute)); err != nil {
		t.Fatal(err)
	}
	release, err := st.ClaimNextProjectActorLifecycleAction(ctx, "executor-b", now.Add(7*time.Minute), now.Add(12*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	releaseReceipt := terminalReceipt(release, "released")
	releaseReceipt.IdentityBefore = actionExpectedIdentity(release)
	projectAndAckActorReceipt(t, st, ctx, release, "executor-b", releaseReceipt, now.Add(8*time.Minute))
	report, err := st.ReconcileProjectActorLifecycleActions(ctx, now.Add(10*time.Minute), 10)
	if err != nil {
		t.Fatal(err)
	}
	if report.Resumed != 1 || report.Materialized != 1 {
		t.Fatalf("replacement did not resume after prior release: %+v", report)
	}
	current, err := st.CurrentProjectActorLifecycle(ctx, "russ", store.DriverInteractorRole)
	if err != nil {
		t.Fatal(err)
	}
	if current.ActorID != "russ-claude-v2" || current.State != "awaiting_adopt" || current.CurrentActionID == "" {
		t.Fatalf("replacement materialization is not current/visible: %+v", current)
	}
}
