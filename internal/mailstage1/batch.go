package mailstage1

// Input is the normalized Stage1 production input boundary. Callers that already
// have resolved contact/Layer 2 intelligence can fill Message.Sender directly;
// ingestion and measurement adapters that receive flat sender flags can use the
// top-level fields below and still enter the same scorer.
type Input struct {
	ID string `json:"id,omitempty"`
	Message
	VIP                 bool `json:"vip,omitempty"`
	KnownInvestor       bool `json:"known_investor,omitempty"`
	MoneyStakeholder    bool `json:"money_stakeholder,omitempty"`
	SecurityStakeholder bool `json:"security_stakeholder,omitempty"`
	HighStakesContact   bool `json:"high_stakes_contact,omitempty"`
	SenderHighStakes    bool `json:"sender_high_stakes,omitempty"`
	UserReplied         bool `json:"user_replied,omitempty"`
	HumanReplyContext   bool `json:"human_reply_context,omitempty"`
}

// Output is the Stage1 scoring result plus the flattened diagnostics expected by
// behavior measurement reports. Result remains the canonical nested score.
type Output struct {
	ID                     string         `json:"id,omitempty"`
	SenderEmail            string         `json:"sender_email"`
	Subject                string         `json:"subject"`
	Composite              float64        `json:"composite"`
	Label                  string         `json:"label"`
	ContentImportance      float64        `json:"content_importance"`
	ContentClassifierLabel string         `json:"content_classifier_label"`
	SenderHighStakes       bool           `json:"sender_high_stakes"`
	Substantive            bool           `json:"substantive"`
	ContentHighStakes      bool           `json:"content_high_stakes"`
	SubstanceSignals       []string       `json:"substance_signals,omitempty"`
	LogisticsGuardrail     bool           `json:"logistics_guardrail"`
	VIPSubstantiveBoost    bool           `json:"vip_substantive_boost"`
	VIPSubstantiveFloor    float64        `json:"vip_substantive_floor,omitempty"`
	PreBoostComposite      float64        `json:"pre_boost_composite"`
	ImportanceThreshold    float64        `json:"importance_threshold_used"`
	Result                 Result         `json:"result"`
	Content                Classification `json:"content"`
}

// ResolvedMessage applies flat adapter fields to Message.Sender and thread
// context. This keeps VIP/high-stakes sender identity resolution outside Stage1
// while preventing each caller from inventing a separate projection.
func (in Input) ResolvedMessage() Message {
	msg := in.Message
	msg.Sender.VIP = msg.Sender.VIP || in.VIP
	msg.Sender.KnownInvestor = msg.Sender.KnownInvestor || in.KnownInvestor
	msg.Sender.MoneyStakeholder = msg.Sender.MoneyStakeholder || in.MoneyStakeholder
	msg.Sender.SecurityStakeholder = msg.Sender.SecurityStakeholder || in.SecurityStakeholder
	msg.Sender.HighStakesContact = msg.Sender.HighStakesContact || in.HighStakesContact || in.SenderHighStakes
	msg.UserRepliedThread = msg.UserRepliedThread || in.UserReplied || in.HumanReplyContext
	return msg
}

// ProcessInput runs one message through the canonical Stage1 processor.
func ProcessInput(processor Stage1Processor, in Input) Output {
	if processor == nil {
		processor = NewProcessor()
	}
	msg := in.ResolvedMessage()
	result := processor.Score(msg)
	return Output{
		ID:                     in.ID,
		SenderEmail:            msg.SenderEmail,
		Subject:                msg.Subject,
		Composite:              result.Composite,
		Label:                  result.Label,
		ContentImportance:      result.ContentImportance,
		ContentClassifierLabel: result.ContentClassifierLabel,
		SenderHighStakes:       result.SenderHighStakes,
		Substantive:            result.Substantive,
		ContentHighStakes:      result.ContentHighStakes,
		SubstanceSignals:       result.SubstanceSignals,
		LogisticsGuardrail:     result.LogisticsGuardrail,
		VIPSubstantiveBoost:    result.VIPSubstantiveBoost,
		VIPSubstantiveFloor:    result.VIPSubstantiveFloor,
		PreBoostComposite:      result.PreBoostComposite,
		ImportanceThreshold:    result.ImportanceThresholdUsed,
		Result:                 result,
		Content:                result.Content,
	}
}

// ProcessBatch runs a behavior-labeled or ingest batch through the same Stage1
// scorer used for a single message.
func ProcessBatch(processor Stage1Processor, inputs []Input) []Output {
	outputs := make([]Output, 0, len(inputs))
	for _, input := range inputs {
		outputs = append(outputs, ProcessInput(processor, input))
	}
	return outputs
}
