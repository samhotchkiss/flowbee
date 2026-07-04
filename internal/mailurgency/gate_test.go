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
