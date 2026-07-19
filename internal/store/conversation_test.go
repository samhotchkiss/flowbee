package store_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/samhotchkiss/flowbee/internal/store"
	"github.com/samhotchkiss/flowbee/internal/workintent"
)

func TestConversationThreadStableIdentityAndFocusFencing(t *testing.T) {
	ctx := context.Background()
	st := openConversationStore(t, ":memory:")
	now := time.Date(2026, 7, 19, 17, 0, 0, 0, time.UTC)
	in := store.CreateConversationThreadInput{
		ID: "thread-1", ProjectID: "default", ConversationKey: "primary", Title: "Flowbee",
		InteractorActorID: "interactor:default", InteractorBindingID: "binding-1",
		InteractorIncarnationID: "run-1", IdempotencyKey: "create-1",
	}
	first, err := st.CreateConversationThread(ctx, in, now)
	if err != nil {
		t.Fatal(err)
	}
	if first.ID != "thread-1" || first.FocusKind != store.ConversationFocusProject || first.FocusRef != "default" || first.StateVersion != 1 {
		t.Fatalf("thread=%+v", first)
	}
	// A lost acknowledgement and even a reloaded client with a fresh request key
	// resolve the same stable project conversation key, not a duplicate thread.
	replay, err := st.CreateConversationThread(ctx, in, now.Add(time.Minute))
	if err != nil || replay.ID != first.ID {
		t.Fatalf("exact replay=(%+v,%v)", replay, err)
	}
	in.IdempotencyKey = "create-after-reload"
	reload, err := st.CreateConversationThread(ctx, in, now.Add(2*time.Minute))
	if err != nil || reload.ID != first.ID {
		t.Fatalf("stable-key reload=(%+v,%v)", reload, err)
	}
	in.Title = "changed"
	if _, err := st.CreateConversationThread(ctx, in, now); !errors.Is(err, store.ErrConversationIdempotencyConflict) {
		t.Fatalf("changed retry err=%v", err)
	}

	artifactHash := conversationSHA("plan-v1")
	focused, err := st.UpdateConversationFocus(ctx, store.UpdateConversationFocusInput{
		ProjectID: "default", ThreadID: first.ID, IdempotencyKey: "focus-1", ExpectedStateVersion: 1,
		FocusKind: store.ConversationFocusArtifact, FocusRef: "artifact://plans/1", FocusArtifactSHA256: artifactHash,
	}, "human:sam", now.Add(3*time.Minute))
	if err != nil || focused.StateVersion != 2 || focused.ID != first.ID || focused.FocusArtifactSHA256 != artifactHash {
		t.Fatalf("focused=(%+v,%v)", focused, err)
	}
	// Browser retry is a no-op even though the thread is now version 2.
	if _, err := st.UpdateConversationFocus(ctx, store.UpdateConversationFocusInput{
		ProjectID: "default", ThreadID: first.ID, IdempotencyKey: "focus-1", ExpectedStateVersion: 1,
		FocusKind: store.ConversationFocusArtifact, FocusRef: "artifact://plans/1", FocusArtifactSHA256: artifactHash,
	}, "human:sam", now.Add(4*time.Minute)); err != nil {
		t.Fatalf("focus replay: %v", err)
	}
	if _, err := st.UpdateConversationFocus(ctx, store.UpdateConversationFocusInput{
		ProjectID: "default", ThreadID: first.ID, IdempotencyKey: "focus-stale", ExpectedStateVersion: 1,
		FocusKind: store.ConversationFocusProject, FocusRef: "default",
	}, "human:sam", now); !errors.Is(err, store.ErrConversationStale) {
		t.Fatalf("stale focus err=%v", err)
	}
	var focusEvents int
	if err := st.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM conversation_events WHERE thread_id=? AND kind='focus_changed'`, first.ID).Scan(&focusEvents); err != nil || focusEvents != 1 {
		t.Fatalf("focus events=%d err=%v", focusEvents, err)
	}
}

func TestConversationMessagesAreImmutableOrderedAndIdempotent(t *testing.T) {
	ctx := context.Background()
	st := openConversationStore(t, ":memory:")
	now := time.Date(2026, 7, 19, 18, 0, 0, 0, time.UTC)
	thread := createConversationThread(t, st, now)

	humanIn := store.AppendConversationMessageInput{
		ID: "message-human", ProjectID: "default", ThreadID: thread.ID, Role: "human", ActorID: "human:sam",
		ContentText: "Build the durable dashboard", IdempotencyKey: "message-submit-1",
	}
	human, err := st.AppendConversationMessage(ctx, humanIn, now)
	if err != nil {
		t.Fatal(err)
	}
	if human.ThreadSeq != 1 || human.ContentSHA256 != conversationSHA(humanIn.ContentText) || human.DeliveryState != "pending" {
		t.Fatalf("human message=%+v", human)
	}
	replay, err := st.AppendConversationMessage(ctx, humanIn, now.Add(time.Minute))
	if err != nil || replay.ID != human.ID || replay.ThreadSeq != 1 {
		t.Fatalf("replay=(%+v,%v)", replay, err)
	}
	humanIn.ContentText = "different"
	if _, err := st.AppendConversationMessage(ctx, humanIn, now); !errors.Is(err, store.ErrConversationIdempotencyConflict) {
		t.Fatalf("changed retry err=%v", err)
	}

	response, err := st.AppendConversationMessage(ctx, store.AppendConversationMessageInput{
		ID: "message-interactor", ProjectID: "default", ThreadID: thread.ID, Role: "interactor",
		ActorID: "interactor:default", AgentIncarnationID: "run-1", ReplyToMessageID: human.ID,
		ContentText: "I captured that as a work intent.", IdempotencyKey: "interactor-response-1",
	}, now.Add(2*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if response.ThreadSeq != 2 || response.DeliveryState != "not_required" {
		t.Fatalf("response=%+v", response)
	}
	rows, err := st.ListConversationMessages(ctx, "default", thread.ID, 0, 100)
	if err != nil || len(rows) != 2 || rows[0].ID != human.ID || rows[1].ID != response.ID {
		t.Fatalf("rows=%+v err=%v", rows, err)
	}
	resumed, err := st.ListConversationMessages(ctx, "default", thread.ID, 1, 100)
	if err != nil || len(resumed) != 1 || resumed[0].ID != response.ID {
		t.Fatalf("resumed=%+v err=%v", resumed, err)
	}
	if _, err := st.DB.ExecContext(ctx, `UPDATE conversation_messages SET content_text='tampered' WHERE id=?`, human.ID); err == nil {
		t.Fatal("immutable conversation message update unexpectedly succeeded")
	}
	if _, err := st.DB.ExecContext(ctx, `DELETE FROM conversation_messages WHERE id=?`, human.ID); err == nil {
		t.Fatal("immutable conversation message delete unexpectedly succeeded")
	}
	var count int
	if err := st.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM conversation_messages WHERE thread_id=?`, thread.ID).Scan(&count); err != nil || count != 2 {
		t.Fatalf("message count=%d err=%v", count, err)
	}
}

func TestConversationDeliveryAndPersistedCursorSurviveRestart(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "flowbee.db")
	st := openConversationStore(t, path)
	now := time.Date(2026, 7, 19, 19, 0, 0, 0, time.UTC)
	thread := createConversationThread(t, st, now)
	message, err := st.AppendConversationMessage(ctx, store.AppendConversationMessageInput{
		ID: "message-route", ProjectID: "default", ThreadID: thread.ID, Role: "human", ActorID: "human:sam",
		ContentText: "Keep moving", IdempotencyKey: "message-route-create",
	}, now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	before, err := st.ConversationDigestSeq(ctx, "default", thread.ID)
	if err != nil || before == 0 {
		t.Fatalf("before=%d err=%v", before, err)
	}
	routing, err := st.UpdateConversationMessageDelivery(ctx, store.UpdateConversationDeliveryInput{
		ProjectID: "default", ThreadID: thread.ID, MessageID: message.ID, IdempotencyKey: "route-1",
		ExpectedStateVersion: 1, State: "routing", ActionID: "action-1",
	}, "projector:driver", now.Add(2*time.Minute))
	if err != nil || routing.DeliveryState != "routing" || routing.DeliveryStateVersion != 2 {
		t.Fatalf("routing=(%+v,%v)", routing, err)
	}
	// Same command after state advance is still exactly-once.
	if _, err := st.UpdateConversationMessageDelivery(ctx, store.UpdateConversationDeliveryInput{
		ProjectID: "default", ThreadID: thread.ID, MessageID: message.ID, IdempotencyKey: "route-1",
		ExpectedStateVersion: 1, State: "routing", ActionID: "action-1",
	}, "projector:driver", now.Add(3*time.Minute)); err != nil {
		t.Fatalf("delivery replay: %v", err)
	}
	if _, err := st.UpdateConversationMessageDelivery(ctx, store.UpdateConversationDeliveryInput{
		ProjectID: "default", ThreadID: thread.ID, MessageID: message.ID, IdempotencyKey: "route-skip",
		ExpectedStateVersion: 2, State: "acknowledged",
	}, "projector:driver", now); err == nil {
		t.Fatal("routing -> acknowledged skipped submitted evidence")
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	st = openConversationStore(t, path)
	events, err := st.ListConversationEvents(ctx, "default", thread.ID, before, 100)
	if err != nil || len(events) != 1 || events[0].Kind != "delivery_changed" || events[0].Seq <= before {
		t.Fatalf("restart events=%+v err=%v", events, err)
	}
	loaded, err := st.ListConversationMessages(ctx, "default", thread.ID, 0, 100)
	if err != nil || len(loaded) != 1 || loaded[0].DeliveryState != "routing" {
		t.Fatalf("restart messages=%+v err=%v", loaded, err)
	}
}

func TestConversationProseCannotSatisfyTypedDecision(t *testing.T) {
	ctx := context.Background()
	st := openConversationStore(t, ":memory:")
	now := time.Date(2026, 7, 19, 20, 0, 0, 0, time.UTC)
	thread := createConversationThread(t, st, now)
	decision, err := st.CreateDecisionRequest(ctx, store.CreateDecisionRequestInput{
		ID: "decision-stays-open", ProjectID: "default", Kind: workintent.DecisionPlanReview,
		Title: "Approve plan", Prompt: "Approve the exact plan?", Options: json.RawMessage(`[]`),
		ResponseSchema: json.RawMessage(`{}`), ExpectedResponseKinds: []workintent.ResponseKind{workintent.ResponseApprove},
		Priority: 1, RequestedBy: "interactor:default", RouteTo: "human",
		SubjectArtifactRef: "artifact://plan/1", SubjectVersion: 1,
		SubjectSHA256: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.AppendConversationMessage(ctx, store.AppendConversationMessageInput{
		ProjectID: "default", ThreadID: thread.ID, Role: "human", ActorID: "human:sam",
		ContentText: "Looks good, approved!", IdempotencyKey: "prose-is-not-authority",
	}, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	stillOpen, err := st.GetDecisionRequest(ctx, "default", decision.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stillOpen.State != workintent.RequestOpen || stillOpen.CurrentResponseID != "" {
		t.Fatalf("conversation prose mutated typed decision: %+v", stillOpen)
	}
}

func openConversationStore(t *testing.T, dsn string) *store.Store {
	t.Helper()
	st, err := store.Open(context.Background(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.MigrateUp(context.Background(), st.DB); err != nil {
		_ = st.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func createConversationThread(t *testing.T, st *store.Store, now time.Time) store.ConversationThread {
	t.Helper()
	thread, err := st.CreateConversationThread(context.Background(), store.CreateConversationThreadInput{
		ID: "thread-1", ProjectID: "default", ConversationKey: "primary", Title: "Default project",
		InteractorActorID: "interactor:default", InteractorBindingID: "binding-1",
		InteractorIncarnationID: "run-1", IdempotencyKey: "thread-create",
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	return thread
}

func conversationSHA(value string) string {
	sum := sha256.Sum256([]byte(value))
	return "sha256:" + hex.EncodeToString(sum[:])
}
