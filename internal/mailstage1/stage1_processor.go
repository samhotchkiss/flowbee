// Package mailstage1 contains the deterministic Stage1 mail classifier/scorer.
//
// The scorer is intentionally pure: caller-owned sender intelligence and message
// text are supplied as values, and the result carries the diagnostics needed by
// measurement reports to explain why a message surfaced.
package mailstage1

import (
	"regexp"
	"sort"
	"strings"
)

const (
	ImportantThreshold    = 0.60
	VIPSubstantiveFloor   = 0.60
	LabelRoutine          = "routine"
	LabelImportant        = "important"
	LabelActionRequired   = "action_required"
	ImportanceRoutine     = 0.25
	ImportanceSubstantive = 0.75
	ImportanceHighStakes  = 1.00
)

// Message is the Stage1 scoring input. Sender intelligence is resolved by the
// existing contact/Layer 2 pipeline before scoring; Stage1 does not maintain a
// parallel VIP source of truth.
type Message struct {
	SenderEmail       string            `json:"sender_email"`
	Subject           string            `json:"subject"`
	Body              string            `json:"body"`
	Headers           map[string]string `json:"headers,omitempty"`
	Sender            SenderIntel       `json:"sender"`
	UserRepliedThread bool              `json:"user_replied_thread"`
}

// SenderIntel is the existing sender/contact intelligence projected into Stage1.
type SenderIntel struct {
	VIP                 bool `json:"vip"`
	KnownInvestor       bool `json:"known_investor"`
	MoneyStakeholder    bool `json:"money_stakeholder"`
	SecurityStakeholder bool `json:"security_stakeholder"`
	HighStakesContact   bool `json:"high_stakes_contact"`
}

func (s SenderIntel) HighStakes() bool {
	return s.VIP || s.KnownInvestor || s.MoneyStakeholder || s.SecurityStakeholder || s.HighStakesContact
}

// Processor is the production Stage1 processor boundary. Keep callers wired
// through this type so content classification, scoring, and diagnostics stay in
// one path instead of drifting into report-only helpers.
type Processor struct{}

func NewProcessor() Processor {
	return Processor{}
}

func (Processor) Score(m Message) Result {
	return ScoreStage1(m)
}

func (Processor) Classify(m Message) Classification {
	return ClassifyContent(m)
}

func ProcessStage1(m Message) Result {
	return NewProcessor().Score(m)
}

// Classification is the content-only result before sender-specific boosts.
type Classification struct {
	Label              string   `json:"label"`
	Importance         float64  `json:"importance"`
	Substantive        bool     `json:"substantive"`
	HighStakes         bool     `json:"high_stakes"`
	SubstanceSignals   []string `json:"substance_signals,omitempty"`
	LogisticsGuardrail bool     `json:"logistics_guardrail"`
}

// Result is the complete Stage1 score and its measurement/debug metadata.
type Result struct {
	Composite               float64        `json:"composite"`
	Label                   string         `json:"label"`
	Content                 Classification `json:"content"`
	SenderHighStakes        bool           `json:"sender_high_stakes"`
	SenderLean              float64        `json:"sender_lean"`
	VIPSubstantiveBoost     bool           `json:"vip_substantive_boost"`
	VIPSubstantiveFloor     float64        `json:"vip_substantive_floor,omitempty"`
	LogisticsGuardrail      bool           `json:"logistics_guardrail"`
	SubstanceSignals        []string       `json:"substance_signals,omitempty"`
	PreBoostComposite       float64        `json:"pre_boost_composite"`
	ContentImportance       float64        `json:"content_importance"`
	ContentClassifierLabel  string         `json:"content_classifier_label"`
	Substantive             bool           `json:"substantive"`
	ContentHighStakes       bool           `json:"content_high_stakes"`
	ImportanceThresholdUsed float64        `json:"importance_threshold_used"`
}

// ScoreStage1 classifies and scores a message. VIP/high-stakes sender influence
// keeps the historical senderLean behavior, then applies a late composite floor
// only when content or thread context proves the message is substantive.
func ScoreStage1(m Message) Result {
	classification := ClassifyContent(m)
	senderHighStakes := m.Sender.HighStakes()
	senderLean := 0.0
	if senderHighStakes {
		senderLean = 1.0
	}

	threadLean := 0.0
	if m.UserRepliedThread {
		threadLean = 1.0
	}

	shapeLean := 0.35
	if classification.LogisticsGuardrail {
		shapeLean = 0.05
	} else if classification.Substantive {
		shapeLean = 0.65
	}

	composite := clamp01(
		0.32*classification.Importance +
			0.12*senderLean +
			0.10*threadLean +
			0.10*shapeLean,
	)
	preBoost := composite

	vipBoost := false
	floor := 0.0
	if senderHighStakes && qualifiesForVIPSubstantiveFloor(classification) {
		floor = VIPSubstantiveFloor
		if composite < floor {
			composite = floor
			vipBoost = true
		}
	}

	label := LabelRoutine
	if classification.Label == LabelActionRequired || composite >= ImportantThreshold {
		label = LabelImportant
	}
	if classification.Label == LabelActionRequired && composite >= ImportantThreshold {
		label = LabelActionRequired
	}

	return Result{
		Composite:               composite,
		Label:                   label,
		Content:                 classification,
		SenderHighStakes:        senderHighStakes,
		SenderLean:              senderLean,
		VIPSubstantiveBoost:     vipBoost,
		VIPSubstantiveFloor:     floor,
		LogisticsGuardrail:      classification.LogisticsGuardrail,
		SubstanceSignals:        append([]string(nil), classification.SubstanceSignals...),
		PreBoostComposite:       preBoost,
		ContentImportance:       classification.Importance,
		ContentClassifierLabel:  classification.Label,
		Substantive:             classification.Substantive,
		ContentHighStakes:       classification.HighStakes,
		ImportanceThresholdUsed: ImportantThreshold,
	}
}

// ClassifyContent inspects subject, body, headers, and forwarded body text before
// letting notification/calendar shape suppress importance. High-stakes substance
// wins over Fwd:/Invitation:/calendar-looking prefixes, except for logistics-only
// RSVP/transport artifacts.
func ClassifyContent(m Message) Classification {
	text := normalizedMessageText(m)
	logisticsShape := isLogisticsShape(m, text)

	// RSVP/logistics subjects are transport shape ("Accepted: <event title>"),
	// not a substantive note. Once a message is recognized as logistics-shaped,
	// only the body and headers can supply substance signals.
	signalText := text
	if logisticsShape {
		signalText = normalizedBodyText(m)
	}
	contentSignals := detectContentSubstanceSignals(signalText)
	if logisticsShape {
		contentSignals = filterLogisticsTransportSignals(contentSignals)
	}
	logisticsOnly := logisticsShape && len(contentSignals) == 0
	signals := append([]string(nil), contentSignals...)
	if m.UserRepliedThread && !logisticsOnly {
		signals = appendSignal(signals, "human_reply_context")
	}

	c := Classification{
		Label:              LabelRoutine,
		Importance:         ImportanceRoutine,
		Substantive:        len(signals) > 0,
		HighStakes:         hasHighStakesSignal(signals),
		SubstanceSignals:   signals,
		LogisticsGuardrail: logisticsOnly,
	}

	if logisticsOnly {
		return c
	}
	if c.HighStakes && hasActionableSignal(signals) {
		c.Label = LabelActionRequired
		c.Importance = ImportanceHighStakes
		return c
	}
	if c.HighStakes || hasActionableSignal(signals) {
		c.Label = LabelImportant
		c.Importance = ImportanceSubstantive
		return c
	}
	return c
}

func normalizedMessageText(m Message) string {
	var b strings.Builder
	b.WriteString(m.Subject)
	b.WriteByte('\n')
	b.WriteString(m.Body)
	for k, v := range m.Headers {
		b.WriteByte('\n')
		b.WriteString(k)
		b.WriteByte(':')
		b.WriteString(v)
	}
	return normalizeWhitespace(strings.ToLower(b.String()))
}

func normalizedBodyText(m Message) string {
	var b strings.Builder
	b.WriteString(m.Body)
	for k, v := range m.Headers {
		b.WriteByte('\n')
		b.WriteString(k)
		b.WriteByte(':')
		b.WriteString(v)
	}
	return normalizeWhitespace(strings.ToLower(b.String()))
}

func detectContentSubstanceSignals(text string) []string {
	signals := map[string]bool{}
	investment := investmentRE.MatchString(text)
	funding := fundingRE.MatchString(text)
	money := moneyRE.MatchString(text)
	security := securityRE.MatchString(text)
	decision := decisionRE.MatchString(text)
	meeting := meetingRE.MatchString(text) && !rsvpOnlyRE.MatchString(text)
	request := requestRE.MatchString(text)

	if investment && (request || decision || meeting) {
		signals["investment_ask"] = true
	}
	if investment {
		signals["investment_context"] = true
	}
	if funding {
		signals["funding"] = true
	}
	if money {
		signals["money"] = true
	}
	if security {
		signals["security"] = true
	}
	if decision {
		signals["decision_required"] = true
	}
	if meeting {
		signals["meeting_request"] = true
	}
	if request {
		signals["direct_ask"] = true
	}

	out := make([]string, 0, len(signals))
	for signal := range signals {
		out = append(out, signal)
	}
	sort.Strings(out)
	return out
}

func filterLogisticsTransportSignals(signals []string) []string {
	filtered := signals[:0]
	for _, signal := range signals {
		switch signal {
		case "meeting_request":
			continue
		default:
			filtered = append(filtered, signal)
		}
	}
	return filtered
}

func appendSignal(signals []string, signal string) []string {
	for _, existing := range signals {
		if existing == signal {
			return signals
		}
	}
	signals = append(signals, signal)
	sort.Strings(signals)
	return signals
}

func hasHighStakesSignal(signals []string) bool {
	for _, signal := range signals {
		switch signal {
		case "investment_ask", "investment_context", "funding", "money", "security", "decision_required", "human_reply_context":
			return true
		}
	}
	return false
}

func qualifiesForVIPSubstantiveFloor(c Classification) bool {
	return c.Substantive && !c.LogisticsGuardrail && hasVIPFloorSignal(c.SubstanceSignals)
}

func hasVIPFloorSignal(signals []string) bool {
	for _, signal := range signals {
		switch signal {
		case "direct_ask", "decision_required", "meeting_request", "investment_ask", "money", "security", "human_reply_context":
			return true
		}
	}
	return false
}

func hasActionableSignal(signals []string) bool {
	for _, signal := range signals {
		switch signal {
		case "direct_ask", "decision_required", "meeting_request", "investment_ask":
			return true
		}
	}
	return false
}

func isLogisticsShape(m Message, text string) bool {
	subject := strings.ToLower(strings.TrimSpace(m.Subject))
	if logisticsSubjectRE.MatchString(subject) {
		return true
	}
	for k, v := range m.Headers {
		header := strings.ToLower(strings.TrimSpace(k + ": " + v))
		if strings.Contains(header, "text/calendar") ||
			strings.Contains(header, "method=reply") ||
			strings.Contains(header, "method:reply") ||
			strings.Contains(header, "auto-submitted: auto-generated") {
			return true
		}
	}
	return strings.Contains(text, "begin:vcalendar") ||
		strings.Contains(text, "method:reply") ||
		strings.Contains(text, "has accepted this invitation") ||
		strings.Contains(text, "has declined this invitation") ||
		strings.Contains(text, "has tentatively accepted this invitation")
}

func clamp01(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}

func normalizeWhitespace(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

var (
	investmentRE = regexp.MustCompile(`\b(invest|investing|investment|investor|angel|founder|term sheet|valuation|diligence|lp|acquisition)\b`)
	fundingRE    = regexp.MustCompile(`\b(fund|funding|financing|raise|round|capital|venture|seed|series [a-z])\b`)
	moneyRE      = regexp.MustCompile(`\b(invoice|payment|pay|paid|wire|ach|contract|budget|price|pricing|cost|refund|collections?|revenue|purchase)\b`)
	securityRE   = regexp.MustCompile(`\b(security|breach|compromis(?:e|ed)|fraud|login|password|mfa|2fa|account lock|suspicious|vulnerability)\b`)
	decisionRE   = regexp.MustCompile(`\b(decide|decision|approve|approval|sign off|authorize|confirm|choose|go/no-go|deadline|due by|by friday|by monday|today|tomorrow)\b`)
	meetingRE    = regexp.MustCompile(`\b(meet|meeting|call|schedule|calendar|invite|invitation|join us|zoom|diligence session|check in)\b`)
	requestRE    = regexp.MustCompile(`\b(please|can you|could you|would you|will you|let me know|please reply|please respond|reply by|respond by|request|proposal|next step|follow up|introduction|intro|interested|are you available|do you want)\b`)
	rsvpOnlyRE   = regexp.MustCompile(`\b(accepted|declined|tentative|canceled|cancelled):`)

	logisticsSubjectRE = regexp.MustCompile(`^(accepted|declined|tentative|canceled|cancelled|updated|rescheduled):\s+`)
)
