package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/samhotchkiss/flowbee/internal/store"
	"github.com/samhotchkiss/flowbee/internal/testutil"
)

func TestControlOriginGateRoutesOnlyExactRecipientEndpoint(t *testing.T) {
	ctx := context.Background()
	st := testutil.NewStore(t)
	now := time.Date(2026, 7, 19, 21, 0, 0, 0, time.UTC)

	// A legacy/global green signal must not reopen anything once endpoint-scoped
	// capability has been installed.
	st.EnableDriverControlOrigin = true
	st.DriverControlOriginGate = func() bool { return true }
	readyDomain := "external"
	st.DriverControlOriginEndpointGate = func(hostID, storeID, domainID string) bool {
		switch readyDomain {
		case "external":
			return hostID == "host-external" && storeID == "store-external" && domainID == "default"
		case "managed":
			return hostID == "host-managed" && storeID == "store-managed" && domainID == "managed_dedicated"
		default:
			return false
		}
	}
	if st.HasDriverControlOrigin() {
		t.Fatal("endpoint-scoped deployment must fail closed for endpoint-less/global checks")
	}

	stamp := now.Format(time.RFC3339Nano)
	for _, endpoint := range []struct{ ref, host, storeID string }{
		{"driver-external", "host-external", "store-external"},
		{"driver-managed", "host-managed", "store-managed"},
	} {
		if _, err := st.DB.ExecContext(ctx, `INSERT INTO driver_instances
			(instance_ref,host_id,store_id,producer_boot_id,state,created_at,updated_at)
			VALUES (?,?,?,'boot','live',?,?)`, endpoint.ref, endpoint.host, endpoint.storeID, stamp, stamp); err != nil {
			t.Fatal(err)
		}
		if _, err := st.DB.ExecContext(ctx, `INSERT INTO driver_observation_cursors
			(store_id,instance_ref,cursor,high_store_seq,uncertainty_epoch,active,updated_at)
			VALUES (?,?,?,1,0,1,?)`, endpoint.storeID, endpoint.ref, "cursor-"+endpoint.storeID, stamp); err != nil {
			t.Fatal(err)
		}
	}

	external := store.DriverSessionBinding{ProjectID: "default", WorkerIdentity: "interactor:external",
		Role: store.DriverInteractorRole, HostID: "host-external", StoreID: "store-external",
		TmuxServerDomainID: "default", TmuxServerInstanceID: "server-external",
		LifecycleOwnership: "external_observed", ExternalWatchID: "watch-external",
		LifecycleKey: "external-russ", TargetEpoch: 1, ProfileID: "external-profile",
		SessionID: "session-external", PaneInstanceID: "pane-external", AgentRunID: "run-external"}
	managed := store.DriverSessionBinding{ProjectID: "default", WorkerIdentity: "interactor:managed",
		Role: store.DriverInteractorRole, HostID: "host-managed", StoreID: "store-managed",
		TmuxServerDomainID: "managed_dedicated", TmuxServerInstanceID: "server-managed",
		LifecycleOwnership: "driver_managed", LifecycleKey: "managed-interactor", TargetEpoch: 1,
		ProfileID: "managed-profile", WorkspaceRootID: "workspace-root",
		WorkspaceRelativePath: "project", SessionID: "session-managed",
		PaneInstanceID: "pane-managed", AgentRunID: "run-managed"}
	for _, binding := range []store.DriverSessionBinding{external, managed} {
		if _, err := st.UpsertDriverSessionBinding(ctx, binding, now); err != nil {
			t.Fatal(err)
		}
	}

	messageIDs := make(map[string]string)
	for _, actor := range []string{external.WorkerIdentity, managed.WorkerIdentity} {
		thread, err := st.CreateConversationThread(ctx, store.CreateConversationThreadInput{
			ID: "thread-" + actor, ProjectID: "default", ConversationKey: "key-" + actor,
			Title: actor, InteractorActorID: actor, InteractorBindingID: "binding-" + actor,
			InteractorIncarnationID: "incarnation-" + actor,
			FocusKind:               store.ConversationFocusProject, FocusRef: "default",
			IdempotencyKey: "create-" + actor,
		}, now)
		if err != nil {
			t.Fatal(err)
		}
		message, err := st.AppendConversationMessage(ctx, store.AppendConversationMessageInput{
			ID: "message-" + actor, ProjectID: "default", ThreadID: thread.ID, Role: "human",
			ActorID: "human:sam", ContentText: "route exactly", StreamState: "complete",
			IdempotencyKey: "message-key-" + actor,
		}, now.Add(time.Second))
		if err != nil {
			t.Fatal(err)
		}
		messageIDs[actor] = message.ID
	}

	report, err := st.ReconcileConversationMessageActions(ctx, now.Add(2*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if report.ActionsCreated != 1 || report.RoutesHeld != 1 {
		t.Fatalf("external-only reconcile = %+v, want one action and one exact-endpoint hold", report)
	}
	assertConversationActionCount(t, ctx, st, messageIDs[external.WorkerIdentity], 1)
	assertConversationActionCount(t, ctx, st, messageIDs[managed.WorkerIdentity], 0)

	readyDomain = "managed"
	report, err = st.ReconcileConversationMessageActions(ctx, now.Add(3*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if report.ActionsCreated != 1 || report.RoutesHeld != 0 {
		t.Fatalf("managed-only reconcile = %+v, want held managed route to recover only on its endpoint", report)
	}
	assertConversationActionCount(t, ctx, st, messageIDs[managed.WorkerIdentity], 1)
	assertConversationActionCount(t, ctx, st, messageIDs[external.WorkerIdentity], 1)

	readyDomain = "none"
	if st.HasDriverControlOriginForBinding(external) || st.HasDriverControlOriginForBinding(managed) {
		t.Fatal("global green signal silently bypassed the exact endpoint gate")
	}
}

func assertConversationActionCount(t *testing.T, ctx context.Context, st *store.Store, messageID string, want int) {
	t.Helper()
	var got int
	if err := st.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM conversation_message_actions WHERE message_id=?`,
		messageID).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("message %s action count = %d, want %d", messageID, got, want)
	}
}
