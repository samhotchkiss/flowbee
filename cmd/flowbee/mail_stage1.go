package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/samhotchkiss/flowbee/internal/mailstage1"
)

type mailStage1Record struct {
	ID string `json:"id,omitempty"`
	mailstage1.Message
	VIP                 bool `json:"vip,omitempty"`
	KnownInvestor       bool `json:"known_investor,omitempty"`
	MoneyStakeholder    bool `json:"money_stakeholder,omitempty"`
	SecurityStakeholder bool `json:"security_stakeholder,omitempty"`
	HighStakesContact   bool `json:"high_stakes_contact,omitempty"`
	SenderHighStakes    bool `json:"sender_high_stakes,omitempty"`
	UserReplied         bool `json:"user_replied,omitempty"`
	HumanReplyContext   bool `json:"human_reply_context,omitempty"`
}

type mailStage1Report struct {
	ID                     string                    `json:"id,omitempty"`
	SenderEmail            string                    `json:"sender_email"`
	Subject                string                    `json:"subject"`
	Composite              float64                   `json:"composite"`
	Label                  string                    `json:"label"`
	ContentImportance      float64                   `json:"content_importance"`
	ContentClassifierLabel string                    `json:"content_classifier_label"`
	SenderHighStakes       bool                      `json:"sender_high_stakes"`
	Substantive            bool                      `json:"substantive"`
	ContentHighStakes      bool                      `json:"content_high_stakes"`
	SubstanceSignals       []string                  `json:"substance_signals,omitempty"`
	LogisticsGuardrail     bool                      `json:"logistics_guardrail"`
	VIPSubstantiveBoost    bool                      `json:"vip_substantive_boost"`
	VIPSubstantiveFloor    float64                   `json:"vip_substantive_floor,omitempty"`
	PreBoostComposite      float64                   `json:"pre_boost_composite"`
	ImportanceThreshold    float64                   `json:"importance_threshold_used"`
	Result                 mailstage1.Result         `json:"result"`
	Content                mailstage1.Classification `json:"content"`
}

func runMailStage1(args []string) error {
	fs := flag.NewFlagSet("mail-stage1", flag.ContinueOnError)
	inputPath := fs.String("input", "-", "JSON or JSONL measurement input; '-' reads stdin")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected args: %s", strings.Join(fs.Args(), " "))
	}

	var in io.Reader = os.Stdin
	var f *os.File
	if *inputPath != "-" {
		var err error
		f, err = os.Open(*inputPath)
		if err != nil {
			return fmt.Errorf("open input: %w", err)
		}
		defer f.Close()
		in = f
	}
	return scoreMailStage1(in, os.Stdout)
}

func scoreMailStage1(in io.Reader, out io.Writer) error {
	raw, err := io.ReadAll(in)
	if err != nil {
		return fmt.Errorf("read input: %w", err)
	}
	records, err := decodeMailStage1Records(raw)
	if err != nil {
		return err
	}
	enc := json.NewEncoder(out)
	processor := mailstage1.NewProcessor()
	for _, record := range records {
		msg := record.normalizedMessage()
		result := processor.Score(msg)
		report := mailStage1Report{
			ID:                     record.ID,
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
		if err := enc.Encode(report); err != nil {
			return fmt.Errorf("write report: %w", err)
		}
	}
	return nil
}

func (r mailStage1Record) normalizedMessage() mailstage1.Message {
	msg := r.Message
	msg.Sender.VIP = msg.Sender.VIP || r.VIP
	msg.Sender.KnownInvestor = msg.Sender.KnownInvestor || r.KnownInvestor
	msg.Sender.MoneyStakeholder = msg.Sender.MoneyStakeholder || r.MoneyStakeholder
	msg.Sender.SecurityStakeholder = msg.Sender.SecurityStakeholder || r.SecurityStakeholder
	msg.Sender.HighStakesContact = msg.Sender.HighStakesContact || r.HighStakesContact || r.SenderHighStakes
	msg.UserRepliedThread = msg.UserRepliedThread || r.UserReplied || r.HumanReplyContext
	return msg
}

func decodeMailStage1Records(raw []byte) ([]mailStage1Record, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return nil, fmt.Errorf("empty input")
	}
	switch raw[0] {
	case '[':
		var records []mailStage1Record
		if err := json.Unmarshal(raw, &records); err != nil {
			return nil, fmt.Errorf("decode JSON array: %w", err)
		}
		return records, nil
	case '{':
		var record mailStage1Record
		if err := json.Unmarshal(raw, &record); err == nil {
			return []mailStage1Record{record}, nil
		}
	}

	var records []mailStage1Record
	scanner := bufio.NewScanner(bytes.NewReader(raw))
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		var record mailStage1Record
		if err := json.Unmarshal(line, &record); err != nil {
			return nil, fmt.Errorf("decode JSONL line %d: %w", len(records)+1, err)
		}
		records = append(records, record)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scan JSONL: %w", err)
	}
	if len(records) == 0 {
		return nil, fmt.Errorf("empty input")
	}
	return records, nil
}
