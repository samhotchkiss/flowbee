package store_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/samhotchkiss/flowbee/internal/store"
	"github.com/samhotchkiss/flowbee/internal/testutil"
)

// A healthy Driver/Interactor observation does not make Flowbee-authored
// messaging available. Without a supported control origin, the immutable human
// message stays durable and visibly held; no transport action is fabricated.
func TestConversationMessageHoldsWithoutDriverControlOrigin(t *testing.T) {
	ctx := context.Background()
	st := testutil.NewStore(t)
	now := time.Date(2026, 7, 19, 20, 0, 0, 0, time.UTC)
	thread, err := st.CreateConversationThread(ctx, store.CreateConversationThreadInput{
		ID: "thread-control-gap", ProjectID: "default", ConversationKey: "control-gap",
		Title: "Control gap", InteractorActorID: "interactor:default",
		InteractorBindingID: "interactor-binding", InteractorIncarnationID: "interactor-run",
		FocusKind: store.ConversationFocusProject, FocusRef: "default",
		IdempotencyKey: "create-control-gap",
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	message, err := st.AppendConversationMessage(ctx, store.AppendConversationMessageInput{
		ID: "message-control-gap", ProjectID: "default", ThreadID: thread.ID,
		Role: "human", ActorID: "human:sam", ContentText: "Please continue.",
		StreamState: "complete", IdempotencyKey: "message-control-gap-key",
	}, now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	// Simulate a post-start database mutation that fabricates the old synthetic
	// sender plus a fully live recipient route. Inventory facts must not become
	// authority while the negotiated runtime capability remains false.
	stamp := now.UTC().Format(time.RFC3339Nano)
	if _, err := st.DB.ExecContext(ctx, `INSERT INTO driver_instances
		(instance_ref,host_id,store_id,producer_boot_id,state,created_at,updated_at)
		VALUES ('control-gap-driver','host','store','boot','live',?,?)`, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB.ExecContext(ctx, `INSERT INTO driver_observation_cursors
		(store_id,instance_ref,cursor,high_store_seq,uncertainty_epoch,active,updated_at)
		VALUES ('store','control-gap-driver','cursor',1,0,1,?)`, stamp); err != nil {
		t.Fatal(err)
	}
	for _, binding := range []store.DriverSessionBinding{
		{WorkerIdentity: store.DriverControlIdentity, Role: store.DriverControlRole,
			HostID: "host", StoreID: "store", TmuxServerDomainID: "flowbee", TmuxServerInstanceID: "server", LifecycleOwnership: "driver_managed",
			LifecycleKey: "synthetic-control", TargetEpoch: 1, ProfileID: "synthetic",
			WorkspaceRootID: "root", WorkspaceRelativePath: "flowbee",
			SessionID: "synthetic-session", PaneInstanceID: "synthetic-pane", AgentRunID: "synthetic-run"},
		{WorkerIdentity: "interactor:default", Role: store.DriverInteractorRole,
			HostID: "host", StoreID: "store", TmuxServerDomainID: "flowbee", TmuxServerInstanceID: "server", LifecycleOwnership: "driver_managed",
			LifecycleKey: "interactor", TargetEpoch: 1, ProfileID: "interactor",
			WorkspaceRootID: "root", WorkspaceRelativePath: "project",
			SessionID: "interactor-session", PaneInstanceID: "interactor-pane", AgentRunID: "interactor-run"},
	} {
		if _, err := st.UpsertDriverSessionBinding(ctx, binding, now); err != nil {
			t.Fatal(err)
		}
	}

	rep, err := st.ReconcileConversationMessageActions(ctx, now.Add(2*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if rep.RoutesHeld != 1 || rep.ActionsCreated != 0 {
		t.Fatalf("reconcile=%+v, want one route hold and zero actions", rep)
	}
	var state, lastError string
	if err := st.DB.QueryRowContext(ctx, `SELECT state,last_error FROM conversation_message_deliveries
		WHERE message_id=?`, message.ID).Scan(&state, &lastError); err != nil {
		t.Fatal(err)
	}
	var actions int
	if err := st.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM conversation_message_actions
		WHERE message_id=?`, message.ID).Scan(&actions); err != nil {
		t.Fatal(err)
	}
	if state != "pending" || actions != 0 || !strings.Contains(lastError, "GAP-FD-003") {
		t.Fatalf("delivery state=%q actions=%d error=%q", state, actions, lastError)
	}
	// A later pass is idempotently held as well: no post-start binding can make
	// the disabled capability mutable.
	if rep, err = st.ReconcileConversationMessageActions(ctx, now.Add(3*time.Second)); err != nil ||
		rep.ActionsCreated != 0 || rep.RoutesHeld != 1 {
		t.Fatalf("second reconcile=%+v err=%v", rep, err)
	}
}
