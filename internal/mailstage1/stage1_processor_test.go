package mailstage1

import "testing"

func TestVIPInvestmentAskSurfaces(t *testing.T) {
	got := ScoreStage1(Message{
		SenderEmail: "drew@nmangels.com",
		Subject:     "Special Invitation: Investing in NM",
		Body:        "Please join us to discuss investing in New Mexico founders and the next diligence steps for this round.",
		Sender:      SenderIntel{VIP: true, KnownInvestor: true},
	})

	if got.Composite < ImportantThreshold {
		t.Fatalf("composite=%0.2f want >= %0.2f (%+v)", got.Composite, ImportantThreshold, got)
	}
	if got.Label != LabelActionRequired {
		t.Fatalf("label=%q want %q", got.Label, LabelActionRequired)
	}
	if !got.ContentHighStakes || !got.Substantive {
		t.Fatalf("content should be high-stakes substantive: %+v", got)
	}
	if !hasSignal(got.SubstanceSignals, "investment_ask") {
		t.Fatalf("expected investment_ask signal, got %v", got.SubstanceSignals)
	}
}

func TestProcessorEntryPointUsesStage1ScoringPolicy(t *testing.T) {
	got := NewProcessor().Score(Message{
		SenderEmail: "drew@nmangels.com",
		Subject:     "Fwd: Special Invitation: Investing in NM",
		Body:        "Please review this investment opportunity and decide whether to join the diligence meeting.",
		Sender:      SenderIntel{KnownInvestor: true},
	})

	if got.Composite < ImportantThreshold {
		t.Fatalf("processor entry point buried high-stakes VIP mail: %+v", got)
	}
	if !got.VIPSubstantiveBoost || !hasSignal(got.SubstanceSignals, "investment_ask") {
		t.Fatalf("processor entry point did not use substance-gated VIP policy: %+v", got)
	}
}

func TestProductionProcessorContractUsesResolvedSenderIntelligence(t *testing.T) {
	var processor Stage1Processor = NewProcessor()

	got := processor.Score(Message{
		SenderEmail: "drew@nmangels.com",
		Subject:     "Fwd: Special Invitation: Investing in NM",
		Body:        "Sam, please review this investment opportunity before the diligence meeting.",
		Sender:      SenderIntel{KnownInvestor: true},
	})

	if got.Composite < ImportantThreshold {
		t.Fatalf("production processor contract buried high-stakes VIP mail: %+v", got)
	}
	if got.Label != LabelActionRequired {
		t.Fatalf("label=%q want %q", got.Label, LabelActionRequired)
	}
	if !got.SenderHighStakes || !got.VIPSubstantiveBoost {
		t.Fatalf("resolved sender intelligence did not drive the VIP substantive floor: %+v", got)
	}
	if got.PreBoostComposite >= ImportantThreshold {
		t.Fatalf("fixture should prove late floor behavior, pre-boost=%0.2f", got.PreBoostComposite)
	}
}

func TestVIPForwardedHighStakesAskSurfacesDespiteForwardShape(t *testing.T) {
	got := ScoreStage1(Message{
		SenderEmail: "drew@nmangels.com",
		Subject:     "Fwd: Special Invitation: Investing in NM",
		Body: `---------- Forwarded message ---------
From: NM Angels

Sam, please review this investment opportunity. We are raising a seed round and
would like your decision on whether to join the diligence meeting next week.`,
		Sender: SenderIntel{KnownInvestor: true},
	})

	if got.Composite < ImportantThreshold {
		t.Fatalf("forwarded high-stakes VIP mail was buried: %+v", got)
	}
	if got.ContentImportance != ImportanceHighStakes {
		t.Fatalf("importance=%0.2f want %0.2f", got.ContentImportance, ImportanceHighStakes)
	}
	if !hasSignal(got.SubstanceSignals, "investment_ask") || !hasSignal(got.SubstanceSignals, "decision_required") {
		t.Fatalf("missing high-stakes forwarded-body signals: %v", got.SubstanceSignals)
	}
}

func TestVIPSubstantiveFloorAppliesLateWhenCompositeWouldBeDiluted(t *testing.T) {
	got := ScoreStage1(Message{
		SenderEmail: "partner@example.com",
		Subject:     "Contract approval",
		Body:        "Please approve the contract terms.",
		Sender:      SenderIntel{VIP: true, MoneyStakeholder: true},
	})

	if got.PreBoostComposite >= VIPSubstantiveFloor {
		t.Fatalf("fixture should exercise the late floor, pre-boost=%0.2f", got.PreBoostComposite)
	}
	if got.Composite != VIPSubstantiveFloor {
		t.Fatalf("composite=%0.2f want floor %0.2f", got.Composite, VIPSubstantiveFloor)
	}
	if !got.VIPSubstantiveBoost || got.VIPSubstantiveFloor != VIPSubstantiveFloor {
		t.Fatalf("missing VIP substantive floor diagnostics: %+v", got)
	}
}

func TestVIPCalendarRSVPLogisticsOnlyStaysRoutine(t *testing.T) {
	got := ScoreStage1(Message{
		SenderEmail: "vip@example.com",
		Subject:     "Accepted: MVP Check In",
		Body:        "vip@example.com has accepted this invitation.\nBEGIN:VCALENDAR\nMETHOD:REPLY\nEND:VCALENDAR",
		Headers: map[string]string{
			"Content-Type": "text/calendar; method=REPLY",
		},
		Sender: SenderIntel{VIP: true},
	})

	if got.Composite >= ImportantThreshold {
		t.Fatalf("VIP RSVP logistics surfaced: %+v", got)
	}
	if got.Label != LabelRoutine {
		t.Fatalf("label=%q want routine", got.Label)
	}
	if got.VIPSubstantiveBoost {
		t.Fatalf("VIP logistics must not receive substantive floor: %+v", got)
	}
	if !got.LogisticsGuardrail || got.Substantive {
		t.Fatalf("expected logistics-only guardrail, got %+v", got)
	}
}

func TestVIPCalendarRSVPLogisticsOnlyWithUserReplyContextStaysRoutine(t *testing.T) {
	got := ScoreStage1(Message{
		SenderEmail: "vip@example.com",
		Subject:     "Accepted: MVP Check In",
		Body:        "vip@example.com has accepted this invitation.\nBEGIN:VCALENDAR\nMETHOD:REPLY\nEND:VCALENDAR",
		Headers: map[string]string{
			"Content-Type": "text/calendar; method=REPLY",
		},
		Sender:            SenderIntel{VIP: true},
		UserRepliedThread: true,
	})

	if got.Composite >= ImportantThreshold {
		t.Fatalf("VIP RSVP logistics with reply context surfaced: %+v", got)
	}
	if got.VIPSubstantiveBoost || got.Substantive {
		t.Fatalf("reply context must not bypass logistics guardrail: %+v", got)
	}
	if hasSignal(got.SubstanceSignals, "human_reply_context") {
		t.Fatalf("logistics-only RSVP must not keep human_reply_context as substance: %+v", got)
	}
	if !got.LogisticsGuardrail {
		t.Fatalf("expected logistics guardrail, got %+v", got)
	}
}

func TestCalendarShapedMessageWithSubstantiveNoteSurfaces(t *testing.T) {
	got := ScoreStage1(Message{
		SenderEmail: "partner@example.com",
		Subject:     "Accepted: MVP Check In",
		Body:        "partner@example.com has accepted this invitation.\nPlease review the attached security decision before our call.",
		Headers: map[string]string{
			"Content-Type": "text/calendar; method=REPLY",
		},
		Sender: SenderIntel{VIP: true, SecurityStakeholder: true},
	})

	if got.Composite < ImportantThreshold {
		t.Fatalf("calendar-shaped message with substantive note should surface: %+v", got)
	}
	if got.LogisticsGuardrail {
		t.Fatalf("substantive note should bypass logistics-only guardrail: %+v", got)
	}
	if !hasSignal(got.SubstanceSignals, "direct_ask") || !hasSignal(got.SubstanceSignals, "security") {
		t.Fatalf("missing substantive note signals: %+v", got)
	}
}

func TestNonVIPHighStakesAskGetsHighContentImportance(t *testing.T) {
	got := ScoreStage1(Message{
		SenderEmail: "founder@example.com",
		Subject:     "Seed round diligence request",
		Body:        "Could you review our funding memo and let me know if you want to invest before Friday?",
	})

	if got.ContentImportance != ImportanceHighStakes {
		t.Fatalf("importance=%0.2f want high-stakes importance (%+v)", got.ContentImportance, got)
	}
	if got.SenderHighStakes || got.VIPSubstantiveBoost {
		t.Fatalf("non-VIP sender must not receive VIP floor: %+v", got)
	}
	if !hasSignal(got.SubstanceSignals, "funding") || !hasSignal(got.SubstanceSignals, "investment_ask") {
		t.Fatalf("missing funding/investment signals: %v", got.SubstanceSignals)
	}
}

func TestVIPGenericNotificationNoAskStaysRoutine(t *testing.T) {
	got := ScoreStage1(Message{
		SenderEmail: "vip@example.com",
		Subject:     "Your weekly dashboard is ready",
		Body:        "This automated notification confirms your report is available.",
		Headers: map[string]string{
			"Auto-Submitted": "auto-generated",
		},
		Sender: SenderIntel{VIP: true},
	})

	if got.Composite >= ImportantThreshold {
		t.Fatalf("generic VIP notification surfaced: %+v", got)
	}
	if got.VIPSubstantiveBoost || got.Substantive {
		t.Fatalf("generic VIP notification must not receive substance gate: %+v", got)
	}
}

func TestVIPInvestorNewsletterWithoutAskDoesNotReceiveFloor(t *testing.T) {
	got := ScoreStage1(Message{
		SenderEmail: "investor@example.com",
		Subject:     "Investor newsletter: funding market update",
		Body:        "This digest summarizes recent investment and funding news from the market.",
		Headers: map[string]string{
			"List-Unsubscribe": "<mailto:unsubscribe@example.com>",
		},
		Sender: SenderIntel{VIP: true, KnownInvestor: true},
	})

	if got.VIPSubstantiveBoost {
		t.Fatalf("investor newsletter without an ask must not receive VIP floor: %+v", got)
	}
	if got.Composite >= ImportantThreshold {
		t.Fatalf("investor newsletter without an ask should stay below threshold: %+v", got)
	}
	if !hasSignal(got.SubstanceSignals, "investment_context") {
		t.Fatalf("expected investment context to remain diagnosable: %+v", got)
	}
	if hasSignal(got.SubstanceSignals, "investment_ask") {
		t.Fatalf("newsletter without ask should not be tagged investment_ask: %+v", got)
	}
}

func TestCalendarInvitationWithSubstantiveInvestmentNoteSurfaces(t *testing.T) {
	got := ScoreStage1(Message{
		SenderEmail: "investor@example.com",
		Subject:     "Invitation: diligence meeting",
		Body:        "Calendar invite attached. Please decide whether to join the investment diligence meeting for the financing round.",
		Headers: map[string]string{
			"Content-Type": "text/calendar",
		},
		Sender: SenderIntel{KnownInvestor: true},
	})

	if got.Composite < ImportantThreshold {
		t.Fatalf("substantive calendar invite was incorrectly suppressed: %+v", got)
	}
	if got.LogisticsGuardrail {
		t.Fatalf("substantive note should override calendar guardrail: %+v", got)
	}
}

func TestVIPCalendarRSVPWithHighStakesEventTitleStaysLogisticsOnly(t *testing.T) {
	got := ScoreStage1(Message{
		SenderEmail: "vip@example.com",
		Subject:     "Accepted: Security Review",
		Body:        "vip@example.com has accepted this invitation.\nBEGIN:VCALENDAR\nMETHOD:REPLY\nEND:VCALENDAR",
		Headers: map[string]string{
			"Content-Type": "text/calendar; method=REPLY",
		},
		Sender: SenderIntel{VIP: true, SecurityStakeholder: true},
	})

	if got.Composite >= ImportantThreshold {
		t.Fatalf("VIP RSVP with high-stakes event title surfaced: %+v", got)
	}
	if got.VIPSubstantiveBoost {
		t.Fatalf("event-title keyword must not qualify RSVP-only calendar artifact for VIP floor: %+v", got)
	}
	if !got.LogisticsGuardrail || got.Substantive {
		t.Fatalf("expected logistics-only guardrail despite high-stakes event title, got %+v", got)
	}
	if hasSignal(got.SubstanceSignals, "security") {
		t.Fatalf("event-title transport text must not leak a security substance signal: %+v", got)
	}
}

func TestVIPCalendarRSVPWithMoneyEventTitleStaysLogisticsOnly(t *testing.T) {
	got := ScoreStage1(Message{
		SenderEmail: "vip@example.com",
		Subject:     "Accepted: Contract Approval",
		Body:        "vip@example.com has accepted this invitation.\nBEGIN:VCALENDAR\nMETHOD:REPLY\nEND:VCALENDAR",
		Headers: map[string]string{
			"Content-Type": "text/calendar; method=REPLY",
		},
		Sender: SenderIntel{VIP: true, MoneyStakeholder: true},
	})

	if got.Composite >= ImportantThreshold {
		t.Fatalf("VIP RSVP with high-stakes event title surfaced: %+v", got)
	}
	if got.VIPSubstantiveBoost {
		t.Fatalf("event-title keyword must not qualify RSVP-only calendar artifact for VIP floor: %+v", got)
	}
	if !got.LogisticsGuardrail || got.Substantive {
		t.Fatalf("expected logistics-only guardrail despite high-stakes event title, got %+v", got)
	}
	if hasSignal(got.SubstanceSignals, "money") {
		t.Fatalf("event-title transport text must not leak a money substance signal: %+v", got)
	}
}

func hasSignal(signals []string, want string) bool {
	for _, signal := range signals {
		if signal == want {
			return true
		}
	}
	return false
}
