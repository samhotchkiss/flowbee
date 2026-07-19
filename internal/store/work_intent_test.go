package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/samhotchkiss/flowbee/internal/store"
	"github.com/samhotchkiss/flowbee/internal/testutil"
	"github.com/samhotchkiss/flowbee/internal/workintent"
)

const workIntentSHA = "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"

func bindWorkIntentDriverRoute(t *testing.T, st *store.Store, orchestrator string, now time.Time) {
	t.Helper()
	st.EnableDriverControlOrigin = true // future-capability fake route
	ctx := context.Background()
	storeID := "intent-driver-store-" + orchestrator
	instanceRef := "intent-driver-instance-" + orchestrator
	if _, err := st.DB.ExecContext(ctx, `INSERT INTO driver_instances
		(instance_ref,host_id,store_id,producer_boot_id,state,created_at,updated_at)
		VALUES (?,?,?,'boot','live',?,?)`, instanceRef, "intent-host", storeID,
		now.UTC().Format(time.RFC3339Nano), now.UTC().Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB.ExecContext(ctx, `INSERT INTO driver_observation_cursors
		(store_id,instance_ref,cursor,high_store_seq,uncertainty_epoch,active,updated_at)
		VALUES (?,?,'cursor-10',10,0,1,?)`, storeID, instanceRef,
		now.UTC().Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	bindings := []store.DriverSessionBinding{
		{WorkerIdentity: orchestrator, Role: store.DriverOrchestratorRole,
			HostID: "intent-host", StoreID: storeID, TmuxServerInstanceID: "intent-server",
			LifecycleKey: "orchestrator-" + orchestrator, TargetEpoch: 1, ProfileID: "orchestrator",
			WorkspaceRootID: "workspace-root", WorkspaceRelativePath: "repo",
			SessionID:      "orchestrator-session-" + orchestrator,
			PaneInstanceID: "orchestrator-pane-" + orchestrator,
			AgentRunID:     "orchestrator-agent-" + orchestrator},
	}
	for _, binding := range bindings {
		if _, err := st.UpsertDriverSessionBinding(ctx, binding, now); err != nil {
			t.Fatal(err)
		}
	}
}

func TestWorkIntentAutomaticallyCreatesOneOrchestratorDeliveryWithoutHumanSend(t *testing.T) {
	ctx := context.Background()
	st := testutil.NewStore(t)
	now := time.Date(2026, 7, 19, 18, 0, 0, 0, time.UTC)
	bindWorkIntentDriverRoute(t, st, "orchestrator-run-1", now)
	input := store.CreateWorkIntentInput{
		ProjectID: "default", SourceConversationID: "thread-1", SourceMessageID: "message-1",
		SourceMessageVersion: 1, InteractorIncarnationID: "interactor-run-1",
		Title: "Build the dashboard", Summary: "Typed product intent", ArtifactRef: "artifact://intent/1",
		ArtifactSHA256: workIntentSHA, IntentVersion: 1, DefinitionComplete: true,
		OwnerActorID: "interactor", OrchestratorRegistration: "orchestrator-run-1",
	}
	created, err := st.CreateWorkIntent(ctx, input, now)
	if err != nil {
		t.Fatal(err)
	}
	// Lost acknowledgement retry may regenerate the opaque ID. Stable source
	// message/version plus identical immutable artifact returns the original row.
	replayed, err := st.CreateWorkIntent(ctx, input, now.Add(time.Second))
	if err != nil || replayed.ID != created.ID {
		t.Fatalf("capture replay=%+v err=%v original=%s", replayed, err, created.ID)
	}
	changed := input
	changed.Priority = 1
	if _, err := st.CreateWorkIntent(ctx, changed, now.Add(2*time.Second)); err == nil {
		t.Fatal("changed lost-ack replay metadata was accepted")
	}

	first, err := st.ReconcileWorkIntents(ctx, now.Add(time.Minute), 10*time.Minute)
	if err != nil || first.Advanced != 1 || first.ActionsCreated != 0 {
		t.Fatalf("captured pass=%+v err=%v", first, err)
	}
	second, err := st.ReconcileWorkIntents(ctx, now.Add(2*time.Minute), 10*time.Minute)
	if err != nil || second.Advanced != 1 || second.ActionsCreated != 1 {
		t.Fatalf("automatic promotion pass=%+v err=%v", second, err)
	}
	intent, err := st.GetWorkIntent(ctx, "default", created.ID)
	if err != nil || intent.State != workintent.StateReadyForOrchestrator || intent.DeliveryActionID == "" {
		t.Fatalf("promoted intent=%+v err=%v", intent, err)
	}
	third, err := st.ReconcileWorkIntents(ctx, now.Add(3*time.Minute), 10*time.Minute)
	if err != nil || third.ActionsCreated != 0 {
		t.Fatalf("idempotent promotion pass=%+v err=%v", third, err)
	}
	var actions int
	if err := st.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM work_intent_actions
		WHERE work_intent_id=? AND kind='deliver_to_orchestrator'`, created.ID).Scan(&actions); err != nil || actions != 1 {
		t.Fatalf("delivery actions=%d err=%v", actions, err)
	}
	var principal, senderBinding string
	if err := st.DB.QueryRowContext(ctx, `SELECT sender_principal_id,sender_binding_id
		FROM work_intent_actions WHERE work_intent_id=?`, created.ID).
		Scan(&principal, &senderBinding); err != nil {
		t.Fatal(err)
	}
	if principal != store.DriverControlIdentity || senderBinding != "" {
		t.Fatalf("origin principal=%q sender_binding=%q", principal, senderBinding)
	}
}

func TestWorkIntentWaitsForExactTypedDecisionThenPromotes(t *testing.T) {
	ctx := context.Background()
	st := testutil.NewStore(t)
	now := time.Date(2026, 7, 19, 19, 0, 0, 0, time.UTC)
	bindWorkIntentDriverRoute(t, st, "orchestrator-run-2", now)
	decision, err := st.CreateDecisionRequest(ctx, store.CreateDecisionRequestInput{
		ID: "intent-decision", ProjectID: "default", Kind: workintent.DecisionDesignReview,
		Title: "Design approval", Prompt: "Approve exact intent design.",
		ExpectedResponseKinds: []workintent.ResponseKind{workintent.ResponseApprove},
		RequestedBy:           "interactor", RouteTo: "human", SubjectArtifactRef: "artifact://intent/2",
		SubjectVersion: 2, SubjectSHA256: workIntentSHA,
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	intent, err := st.CreateWorkIntent(ctx, store.CreateWorkIntentInput{
		ProjectID: "default", SourceMessageID: "message-2", SourceMessageVersion: 1,
		InteractorIncarnationID: "interactor-run-2", Title: "Intent with gate",
		ArtifactRef: "artifact://intent/2", ArtifactSHA256: workIntentSHA, IntentVersion: 2,
		DefinitionComplete: true, OwnerActorID: "interactor",
		OrchestratorRegistration: "orchestrator-run-2", RequiredDecisionIDs: []string{decision.ID},
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.ReconcileWorkIntents(ctx, now.Add(time.Minute), 10*time.Minute); err != nil {
		t.Fatal(err)
	}
	if _, err := st.ReconcileWorkIntents(ctx, now.Add(2*time.Minute), 10*time.Minute); err != nil {
		t.Fatal(err)
	}
	got, _ := st.GetWorkIntent(ctx, "default", intent.ID)
	if got.State != workintent.StateAwaitingDecision || got.DeliveryActionID != "" {
		t.Fatalf("unapproved intent advanced: %+v", got)
	}
	if _, err := st.RespondDecision(ctx, "default", store.DecisionResponseInput{
		RequestID: decision.ID, RequestVersion: 1, SubjectVersion: 2, SubjectSHA256: workIntentSHA,
		Kind: workintent.ResponseApprove, ActorID: "sam", IdempotencyKey: "approve-intent",
	}, now.Add(3*time.Minute)); err != nil {
		t.Fatal(err)
	}
	result, err := st.ReconcileWorkIntents(ctx, now.Add(4*time.Minute), 10*time.Minute)
	if err != nil || result.ActionsCreated != 1 {
		t.Fatalf("approved reconcile=%+v err=%v", result, err)
	}
	got, _ = st.GetWorkIntent(ctx, "default", intent.ID)
	if got.State != workintent.StateReadyForOrchestrator || got.DeliveryActionID == "" {
		t.Fatalf("approved intent did not promote: %+v", got)
	}
}

func TestWorkIntentMissingOrchestratorIsDurablyVisibleAndRearmed(t *testing.T) {
	ctx := context.Background()
	st := testutil.NewStore(t)
	now := time.Date(2026, 7, 19, 20, 0, 0, 0, time.UTC)
	intent, err := st.CreateWorkIntent(ctx, store.CreateWorkIntentInput{
		ProjectID: "default", SourceMessageID: "message-3", SourceMessageVersion: 1,
		InteractorIncarnationID: "interactor-run-3", Title: "Missing route",
		ArtifactRef: "artifact://intent/3", ArtifactSHA256: workIntentSHA, IntentVersion: 1,
		DefinitionComplete: true, OwnerActorID: "interactor",
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = st.ReconcileWorkIntents(ctx, now.Add(time.Minute), 10*time.Minute)
	result, err := st.ReconcileWorkIntents(ctx, now.Add(2*time.Minute), 10*time.Minute)
	if err != nil || result.Held != 1 {
		t.Fatalf("missing route result=%+v err=%v", result, err)
	}
	held, _ := st.GetWorkIntent(ctx, "default", intent.ID)
	if held.State != workintent.StateReadyForOrchestrator || held.HoldKind != "orchestrator_route_missing" {
		t.Fatalf("held intent=%+v", held)
	}
	var attention, alert int
	_ = st.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM attention_items WHERE kind='work_intent_promotion_stalled'
		AND dedup_key=? AND state='open'`, "work_intent_promotion_stalled:"+intent.ID).Scan(&attention)
	_ = st.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM control_alerts WHERE kind='work_intent_promotion_stalled'
		AND epic_id IS NULL`).Scan(&alert)
	if attention != 1 || alert != 1 {
		t.Fatalf("attention=%d alert=%d", attention, alert)
	}
	if err := st.RegisterWorkIntentOrchestrator(ctx, "default", intent.ID, held.StateVersion,
		"orchestrator-run-3", "operator", now.Add(3*time.Minute)); err != nil {
		t.Fatal(err)
	}
	bindWorkIntentDriverRoute(t, st, "orchestrator-run-3", now.Add(3*time.Minute))
	result, err = st.ReconcileWorkIntents(ctx, now.Add(4*time.Minute), 10*time.Minute)
	if err != nil || result.ActionsCreated != 1 {
		t.Fatalf("route rearm reconcile=%+v err=%v", result, err)
	}
}

func TestWorkIntentDefinitionFenceRejectsStaleArtifact(t *testing.T) {
	st := testutil.NewStore(t)
	now := time.Now().UTC()
	intent, err := st.CreateWorkIntent(context.Background(), store.CreateWorkIntentInput{
		ProjectID: "default", SourceMessageID: "message-4", SourceMessageVersion: 1,
		InteractorIncarnationID: "interactor-run-4", Title: "Fence",
		ArtifactRef: "artifact://intent/4", ArtifactSHA256: workIntentSHA, IntentVersion: 1,
		OwnerActorID: "interactor",
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	err = st.SetWorkIntentDefinition(context.Background(), "default", intent.ID, intent.StateVersion,
		2, workIntentSHA, true, nil, "interactor", now.Add(time.Minute))
	if !errors.Is(err, store.ErrWorkIntentFenced) {
		t.Fatalf("stale intent version err=%v", err)
	}
}
