package mailurgency

import (
	"context"
	"testing"
)

func TestClassifierSourceExclusions(t *testing.T) {
	ctx := context.Background()
	candidate := RegexCandidate{Priority: "p1", ImpactStatement: "urgent pricing risk"}
	comp := goodComprehension("verdict-1", "p1")

	tests := []struct {
		name string
		msg  Message
		want string
	}{
		{
			name: "sent urgent-looking text",
			msg:  Message{TenantID: "t", MessageID: "m1", Status: "SENT", SenderEmail: "vendor@example.com"},
			want: ReasonSentOrDraft,
		},
		{
			name: "draft urgent-looking text",
			msg:  Message{TenantID: "t", MessageID: "m2", Status: "draft", SenderEmail: "vendor@example.com"},
			want: ReasonSentOrDraft,
		},
		{
			name: "drafted urgent-looking text",
			msg:  Message{TenantID: "t", MessageID: "m3", Status: "drafted", SenderEmail: "vendor@example.com"},
			want: ReasonSentOrDraft,
		},
		{
			name: "first party sender",
			msg: Message{
				TenantID:          "t",
				MessageID:         "m4",
				Status:            "received",
				SenderEmail:       "sam@Sub.RussHQ.NET",
				FirstPartyDomains: []string{"russhq.net."},
			},
			want: ReasonFirstPartySender,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			item, decision := ClassifyMailImpact(ctx, tt.msg, comp, candidate, nil)
			if item != nil {
				t.Fatalf("excluded message produced item: %+v", item)
			}
			if decision.Reason != tt.want {
				t.Fatalf("reason=%q want %q", decision.Reason, tt.want)
			}
		})
	}
}

func TestClassifierBulkAndRegexNeedLLMVerdict(t *testing.T) {
	ctx := context.Background()
	bulk := Message{
		TenantID:    "t",
		MessageID:   "bulk-1",
		Status:      "received",
		SenderEmail: "marketing@example.com",
		Headers:     map[string]string{"List-Unsubscribe": "<mailto:u@example.com>"},
	}
	candidate := RegexCandidate{Priority: "p1", ImpactStatement: "urgent invoice renewal pricing"}

	if item, decision := ClassifyMailImpact(ctx, bulk, nil, candidate, nil); item != nil || decision.Reason != ReasonBulkWithoutPersonalAsk {
		t.Fatalf("bulk without comprehension item=%+v decision=%+v", item, decision)
	}

	notPersonal := goodComprehension("verdict-2", "p1")
	notPersonal.PersonalAskConfirmed = false
	if item, decision := ClassifyMailImpact(ctx, bulk, notPersonal, candidate, nil); item != nil || decision.Reason != ReasonBulkWithoutPersonalAsk {
		t.Fatalf("bulk non-personal item=%+v decision=%+v", item, decision)
	}

	personalRejectedUrgency := goodComprehension("verdict-3", "p2")
	if item, decision := ClassifyMailImpact(ctx, bulk, personalRejectedUrgency, candidate, nil); item != nil || decision.Reason != ReasonLLMRejectedUrgency {
		t.Fatalf("bulk personal but rejected urgency item=%+v decision=%+v", item, decision)
	}

	personalAllowed := goodComprehension("verdict-4", "p1")
	item, decision := ClassifyMailImpact(ctx, bulk, personalAllowed, candidate, nil)
	if item == nil || !decision.Eligible {
		t.Fatalf("bulk personal allowed should classify, item=%+v decision=%+v", item, decision)
	}
	if item.ComprehensionID != "verdict-4" || item.ImpactStatement != personalAllowed.ImpactStatement {
		t.Fatalf("item must carry LLM verdict reference and rationale: %+v", item)
	}

	inbound := Message{TenantID: "t", MessageID: "m", Status: "received", SenderEmail: "vendor@example.com"}
	if item, decision := ClassifyMailImpact(ctx, inbound, nil, candidate, nil); item != nil || decision.Reason != ReasonMissingLLMVerdict {
		t.Fatalf("regex-only urgent surfaced item=%+v decision=%+v", item, decision)
	}
}

func TestDeriverEventTimingAndBackfillOutage(t *testing.T) {
	ctx := context.Background()
	msg := Message{TenantID: "t", UserID: "u", MessageID: "m", ThreadID: "th", Status: "received", SenderEmail: "vendor@example.com"}
	obs := NewMemoryObserver()

	if need, decision := DeriveEmailNeed(ctx, EventMessageReceived, msg, nil, "p1", obs); need != nil || decision.Reason != ReasonMissingLLMVerdict {
		t.Fatalf("message.received derived need=%+v decision=%+v", need, decision)
	}
	if obs.Deferred["deriver"] != 1 {
		t.Fatalf("message.received should count deferred, got %+v", obs.Deferred)
	}

	if need, decision := DeriveEmailNeed(ctx, EventStage3Completed, msg, nil, "p1", obs); need != nil || decision.Reason != ReasonMissingLLMVerdict {
		t.Fatalf("comprehension outage derived need=%+v decision=%+v", need, decision)
	}

	comp := goodComprehension("verdict-need", "p1")
	need, decision := DeriveEmailNeed(ctx, EventStage3Completed, msg, comp, "p1", obs)
	if need == nil || !decision.Eligible {
		t.Fatalf("stage3.completed should derive completed personal ask, need=%+v decision=%+v", need, decision)
	}
	if need.ComprehensionID != "verdict-need" {
		t.Fatalf("need missing verdict ref: %+v", need)
	}
}

func TestDeriverBulkNewsletterRejectedBeforeAndAfterComprehension(t *testing.T) {
	ctx := context.Background()
	msg := Message{
		TenantID:    "t",
		MessageID:   "newsletter",
		Status:      "received",
		SenderEmail: "news@example.com",
		Headers:     map[string]string{"List-Unsubscribe": "<mailto:u@example.com>"},
	}

	if need, decision := DeriveEmailNeed(ctx, EventMessageReceived, msg, nil, "p1", nil); need != nil || decision.Reason != ReasonMissingLLMVerdict {
		t.Fatalf("message.received newsletter need=%+v decision=%+v", need, decision)
	}

	comp := goodComprehension("verdict-news", "p1")
	comp.PersonalAskConfirmed = false
	if need, decision := DeriveEmailNeed(ctx, EventStage3Completed, msg, comp, "p1", nil); need != nil || decision.Reason != ReasonBulkWithoutPersonalAsk {
		t.Fatalf("non-personal bulk newsletter need=%+v decision=%+v", need, decision)
	}
}

func TestClassifierPersistenceBoundaryRequiresGate(t *testing.T) {
	ctx := context.Background()
	store := &memoryAttentionStore{}
	msg := Message{TenantID: "t", UserID: "u", MessageID: "m", Status: "received", SenderEmail: "vendor@example.com"}
	candidate := RegexCandidate{Priority: "p1", ImpactStatement: "urgent invoice renewal pricing"}

	decision, err := ClassifyAndPersistMailImpact(ctx, store, msg, nil, candidate, nil)
	if err != nil {
		t.Fatal(err)
	}
	if decision.Reason != ReasonMissingLLMVerdict {
		t.Fatalf("regex-only urgent decision=%+v", decision)
	}
	if len(store.items) != 0 {
		t.Fatalf("regex-only classifier wrote user-visible attention: %+v", store.items)
	}

	decision, err = ClassifyAndPersistMailImpact(ctx, store, msg, goodComprehension("verdict-persist", "p1"), candidate, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !decision.Eligible || len(store.items) != 1 {
		t.Fatalf("LLM-confirmed classifier did not write exactly one row: decision=%+v items=%+v", decision, store.items)
	}
	if got := store.items[0].ComprehensionID; got != "verdict-persist" {
		t.Fatalf("persisted attention row missing verdict reference %q", got)
	}
}

func TestNeedDerivationPublishesOnlyAfterStage3Gate(t *testing.T) {
	ctx := context.Background()
	publisher := &memoryNeedPublisher{}
	msg := Message{TenantID: "t", UserID: "u", MessageID: "m", Status: "received", SenderEmail: "vendor@example.com"}

	decision, err := PublishEmailNeedDerivation(ctx, publisher, Event{Type: EventMessageReceived}, msg, nil, "p1", nil)
	if err != nil {
		t.Fatal(err)
	}
	if decision.Reason != ReasonMissingLLMVerdict || len(publisher.events) != 0 {
		t.Fatalf("message.received published need derivation: decision=%+v events=%+v", decision, publisher.events)
	}

	decision, err = PublishEmailNeedDerivation(ctx, publisher, Event{Type: EventStage3Completed}, msg, goodComprehension("verdict-event", "p1"), "p1", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !decision.Eligible || len(publisher.events) != 1 {
		t.Fatalf("stage3.completed did not publish: decision=%+v events=%+v", decision, publisher.events)
	}
	if got := publisher.events[0].ComprehensionID; got != "verdict-event" {
		t.Fatalf("published event missing verdict reference %q", got)
	}
}

func TestDeriverHandleEventLoadsPersistedComprehensionBeforeWritingNeed(t *testing.T) {
	ctx := context.Background()
	store := &memoryNeedStore{}
	msg := Message{TenantID: "t", UserID: "u", MessageID: "m", ThreadID: "th", Status: "received", SenderEmail: "vendor@example.com"}
	deriver := Deriver{
		LoadMessage: func(context.Context, Event) (Message, error) {
			return msg, nil
		},
		LoadComprehension: func(context.Context, Event) (*Comprehension, error) {
			return goodComprehension("verdict-handle", "p1"), nil
		},
		Store: store,
	}

	decision, err := deriver.HandleEvent(ctx, Event{Type: EventMessageReceived, TenantID: "t", UserID: "u", MessageID: "m", Priority: "p1"})
	if err != nil {
		t.Fatal(err)
	}
	if decision.Reason != ReasonMissingLLMVerdict || len(store.items) != 0 {
		t.Fatalf("message.received handler wrote need: decision=%+v items=%+v", decision, store.items)
	}

	decision, err = deriver.HandleEvent(ctx, Event{Type: EventStage3Completed, TenantID: "t", UserID: "u", MessageID: "m", Priority: "p1"})
	if err != nil {
		t.Fatal(err)
	}
	if !decision.Eligible || len(store.items) != 1 {
		t.Fatalf("stage3 handler did not write need: decision=%+v items=%+v", decision, store.items)
	}
	if got := store.items[0].ComprehensionID; got != "verdict-handle" {
		t.Fatalf("need missing verdict reference %q", got)
	}
}

func TestEmailNeedDeriverSubscribesOnlyToStage3Completed(t *testing.T) {
	events := (Deriver{}).SubscribedEvents()
	if len(events) != 1 || events[0] != EventStage3Completed {
		t.Fatalf("email need deriver events=%v, want only %s", events, EventStage3Completed)
	}
	for _, event := range events {
		if event == EventMessageReceived {
			t.Fatalf("email need deriver must not subscribe to %s", EventMessageReceived)
		}
	}
}

func TestBackfillOnlyQueuesComprehension(t *testing.T) {
	ctx := context.Background()
	queue := &memoryComprehensionQueue{}
	msg := Message{TenantID: "t", MessageID: "backfill", Status: "received", SenderEmail: "news@example.com"}

	if err := IngestBackfillMessage(ctx, queue, msg); err != nil {
		t.Fatal(err)
	}
	if len(queue.messages) != 1 || queue.messages[0].MessageID != "backfill" {
		t.Fatalf("backfill did not queue comprehension: %+v", queue.messages)
	}
}

func goodComprehension(id string, allowed ...string) *Comprehension {
	return &Comprehension{
		ID:                     id,
		Status:                 StatusCompleted,
		VerdictRecorded:        true,
		PersonalAskConfirmed:   true,
		AllowedUrgencies:       allowed,
		ImpactStatement:        "LLM-confirmed impact",
		ImpactStatementAllowed: true,
	}
}

type memoryAttentionStore struct {
	items []AttentionItem
}

func (s *memoryAttentionStore) InsertMailAttention(_ context.Context, item AttentionItem) error {
	s.items = append(s.items, item)
	return nil
}

type memoryNeedStore struct {
	items []NeedItem
}

func (s *memoryNeedStore) InsertEmailNeed(_ context.Context, item NeedItem) error {
	s.items = append(s.items, item)
	return nil
}

type memoryNeedPublisher struct {
	events []Event
}

func (p *memoryNeedPublisher) PublishEmailNeedDerivation(_ context.Context, event Event) error {
	p.events = append(p.events, event)
	return nil
}

type memoryComprehensionQueue struct {
	messages []Message
}

func (q *memoryComprehensionQueue) EnqueueComprehension(_ context.Context, msg Message) error {
	q.messages = append(q.messages, msg)
	return nil
}
