package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/samhotchkiss/flowbee/internal/mailstage1"
)

func TestMailStage1ReportScoresDrewInvestmentExample(t *testing.T) {
	input := `{"id":"drew-investing-nm","sender_email":"drew@nmangels.com","subject":"Fwd: Special Invitation: Investing in NM","body":"---------- Forwarded message ---------\nSam, please join us to discuss investing in New Mexico founders and the next diligence steps for this round.","sender":{"vip":true,"known_investor":true}}`
	var out bytes.Buffer
	if err := scoreMailStage1(strings.NewReader(input), &out); err != nil {
		t.Fatal(err)
	}

	got := decodeOneMailStage1Report(t, out.Bytes())
	if got.Composite < mailstage1.ImportantThreshold {
		t.Fatalf("composite=%0.2f want >= %0.2f: %+v", got.Composite, mailstage1.ImportantThreshold, got)
	}
	if got.Label != mailstage1.LabelActionRequired {
		t.Fatalf("label=%q want action_required", got.Label)
	}
	if !got.SenderHighStakes || !got.Substantive || !got.ContentHighStakes {
		t.Fatalf("missing sender/content diagnostics: %+v", got)
	}
	if !reportHasSignal(got.SubstanceSignals, "investment_ask") {
		t.Fatalf("missing investment_ask signal: %+v", got)
	}
	if got.ImportanceThreshold != mailstage1.ImportantThreshold {
		t.Fatalf("threshold diagnostic=%0.2f want %0.2f", got.ImportanceThreshold, mailstage1.ImportantThreshold)
	}
}

func TestMailStage1ReportProjectsFlatMeasurementSenderSignals(t *testing.T) {
	input := `{"id":"flat-drew-investing-nm","sender_email":"drew@nmangels.com","subject":"Fwd: Special Invitation: Investing in NM","body":"---------- Forwarded message ---------\nSam, please review this investment opportunity and decide whether to join the diligence meeting.","known_investor":true}`
	var out bytes.Buffer
	if err := scoreMailStage1(strings.NewReader(input), &out); err != nil {
		t.Fatal(err)
	}

	got := decodeOneMailStage1Report(t, out.Bytes())
	if got.Composite < mailstage1.ImportantThreshold {
		t.Fatalf("flat known_investor signal did not reach Stage1 scorer: %+v", got)
	}
	if !got.SenderHighStakes || !got.VIPSubstantiveBoost {
		t.Fatalf("expected flat sender signal to drive VIP substantive floor diagnostics: %+v", got)
	}
	if !reportHasSignal(got.SubstanceSignals, "investment_ask") {
		t.Fatalf("missing investment_ask signal: %+v", got)
	}
}

func TestMailStage1ReportKeepsVIPRSVPLogisticsRoutine(t *testing.T) {
	input := `[{"id":"accepted-mvp-check-in","sender_email":"vip@example.com","subject":"Accepted: MVP Check In","body":"vip@example.com has accepted this invitation.\nBEGIN:VCALENDAR\nMETHOD:REPLY\nEND:VCALENDAR","headers":{"Content-Type":"text/calendar; method=REPLY"},"sender":{"vip":true}}]`
	var out bytes.Buffer
	if err := scoreMailStage1(strings.NewReader(input), &out); err != nil {
		t.Fatal(err)
	}

	got := decodeOneMailStage1Report(t, out.Bytes())
	if got.Composite >= mailstage1.ImportantThreshold {
		t.Fatalf("composite=%0.2f want < %0.2f: %+v", got.Composite, mailstage1.ImportantThreshold, got)
	}
	if got.Label != mailstage1.LabelRoutine {
		t.Fatalf("label=%q want routine", got.Label)
	}
	if got.VIPSubstantiveBoost || got.Substantive {
		t.Fatalf("VIP logistics must not receive substantive boost: %+v", got)
	}
	if !got.LogisticsGuardrail {
		t.Fatalf("missing logistics guardrail diagnostic: %+v", got)
	}
	if got.ImportanceThreshold != mailstage1.ImportantThreshold {
		t.Fatalf("threshold diagnostic=%0.2f want %0.2f", got.ImportanceThreshold, mailstage1.ImportantThreshold)
	}
}

func decodeOneMailStage1Report(t *testing.T, raw []byte) mailstage1.Output {
	t.Helper()
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(lines) != 1 {
		t.Fatalf("got %d report lines, want 1: %s", len(lines), raw)
	}
	var report mailstage1.Output
	if err := json.Unmarshal([]byte(lines[0]), &report); err != nil {
		t.Fatalf("decode report: %v\n%s", err, raw)
	}
	return report
}

func reportHasSignal(signals []string, want string) bool {
	for _, signal := range signals {
		if signal == want {
			return true
		}
	}
	return false
}
