package mailtrace

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
)

const (
	StageLight = "mail_comprehension_light"
	StageHeavy = "mail_comprehension_heavy"

	StatusComplete = "complete"
	StatusPending  = "pending"
	StatusSkipped  = "skipped"
	StatusMissing  = "missing"
	StatusFailed   = "failed"

	PayloadMissing     = "payload_pending_or_missing"
	LegacyUncorrelated = "unavailable_legacy_uncorrelated"
)

// PostgresMigrationSQL is the production schema change for the mail app. Flowbee's
// own store is SQLite and does not own these tables, so this package keeps the
// runtime assembler and the exact DDL together without applying it to Flowbee jobs.
const PostgresMigrationSQL = `
ALTER TABLE model_invocation
  ADD COLUMN IF NOT EXISTS message_id uuid NULL REFERENCES email_message(id);

ALTER TABLE model_invocation
  ADD COLUMN IF NOT EXISTS stage text NULL;

CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_model_invocation_message_id
  ON model_invocation(message_id);

CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_model_invocation_message_stage_created
  ON model_invocation(message_id, stage, created_at);

ALTER TABLE email_message_comprehension_heavy
  ADD COLUMN IF NOT EXISTS context_bundle_manifest jsonb NULL;
`

type DBTX interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

type Service struct {
	db DBTX
}

func NewService(db DBTX) *Service { return &Service{db: db} }

type Trace struct {
	MessageID     string             `json:"message_id"`
	Message       MessageHeader      `json:"message"`
	Deterministic DeterministicTrace `json:"deterministic"`
	LightLLM      LLMTrace           `json:"light_llm"`
	HeavyLLM      HeavyTrace         `json:"heavy_llm"`
	Invocations   []InvocationTrace  `json:"invocations"`
}

type MessageHeader struct {
	Subject          string   `json:"subject"`
	From             string   `json:"from"`
	To               []string `json:"to"`
	CC               []string `json:"cc"`
	ReceivedAt       *string  `json:"received_at"`
	ProcessingStatus string   `json:"processing_status,omitempty"`
}

type DeterministicTrace struct {
	Status          string          `json:"status"`
	Reason          string          `json:"reason,omitempty"`
	Stage1Band      *string         `json:"stage1_band"`
	Stage2PromptKey *string         `json:"stage2_prompt_key"`
	RoutingDecision RoutingDecision `json:"routing_decision"`
	Facts           map[string]Fact `json:"facts"`
	RankRationale   *string         `json:"rank_rationale"`
	RawDetails      map[string]any  `json:"raw_details"`
}

type RoutingDecision struct {
	SelectedPromptKey *string  `json:"selected_prompt_key"`
	Why               []string `json:"why"`
}

type Fact struct {
	Value  any    `json:"value"`
	Source string `json:"source"`
	Raw    any    `json:"raw"`
}

type LLMTrace struct {
	Status        string          `json:"status"`
	SkipReason    *string         `json:"skip_reason"`
	Error         *string         `json:"error,omitempty"`
	Invocation    *InvocationMeta `json:"invocation"`
	RequestText   *string         `json:"request_text"`
	ResponseText  *string         `json:"response_text"`
	ParsedVerdict map[string]any  `json:"parsed_verdict"`
}

type HeavyTrace struct {
	Status                string          `json:"status"`
	SkipReason            *string         `json:"skip_reason"`
	Error                 *string         `json:"error,omitempty"`
	Escalated             bool            `json:"escalated"`
	EscalationReason      any             `json:"escalation_reason"`
	ContextBundleManifest []any           `json:"context_bundle_manifest"`
	Invocation            *InvocationMeta `json:"invocation"`
	RequestText           *string         `json:"request_text"`
	ResponseText          *string         `json:"response_text"`
	ParsedOutput          map[string]any  `json:"parsed_output"`
}

type InvocationMeta struct {
	ID            string  `json:"id"`
	RequestID     *string `json:"request_id"`
	Stage         *string `json:"stage,omitempty"`
	Provider      *string `json:"provider,omitempty"`
	Model         *string `json:"model"`
	ModelVersion  *string `json:"model_version"`
	PromptVersion *string `json:"prompt_version,omitempty"`
	StartedAt     *string `json:"started_at"`
	CompletedAt   *string `json:"completed_at"`
	LatencyMS     *int64  `json:"latency_ms"`
	Cost          Cost    `json:"cost"`
	Status        string  `json:"status"`
	Error         *string `json:"error"`
	CreatedAt     *string `json:"created_at,omitempty"`
}

type Cost struct {
	Amount   *float64 `json:"amount"`
	Currency string   `json:"currency"`
}

type InvocationTrace struct {
	Invocation    InvocationMeta `json:"invocation"`
	RequestText   *string        `json:"request_text"`
	ResponseText  *string        `json:"response_text"`
	PayloadStatus *string        `json:"payload_status,omitempty"`
}

type CorrelatedInvocationParams struct {
	ID           string
	MessageID    string
	Stage        string
	RequestID    string
	Provider     string
	Model        string
	ModelVersion string
	Status       string
}

func CreateCorrelatedInvocation(ctx context.Context, db DBTX, p CorrelatedInvocationParams) error {
	if strings.TrimSpace(p.ID) == "" || strings.TrimSpace(p.MessageID) == "" || strings.TrimSpace(p.Stage) == "" {
		return errors.New("id, message_id, and stage are required")
	}
	status := p.Status
	if status == "" {
		status = "pending"
	}
	_, err := db.ExecContext(ctx, `
		INSERT INTO model_invocation (
			id, message_id, stage, request_id, provider, model, model_version, status, created_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, datetime('now'))`,
		p.ID, p.MessageID, p.Stage, nullable(p.RequestID), nullable(p.Provider),
		nullable(p.Model), nullable(p.ModelVersion), status)
	if err != nil {
		return fmt.Errorf("create correlated model invocation: %w", err)
	}
	return nil
}

func (s *Service) Trace(ctx context.Context, messageID string) (Trace, error) {
	msg, err := s.message(ctx, messageID)
	if err != nil {
		return Trace{}, err
	}
	score, err := s.score(ctx, messageID)
	if err != nil {
		return Trace{}, err
	}
	lightParsed, err := s.light(ctx, messageID)
	if err != nil {
		return Trace{}, err
	}
	heavyParsed, err := s.heavy(ctx, messageID)
	if err != nil {
		return Trace{}, err
	}
	invs, err := s.invocations(ctx, messageID)
	if err != nil {
		return Trace{}, err
	}

	tr := Trace{
		MessageID:     messageID,
		Message:       msg,
		Deterministic: deterministic(score),
		Invocations:   invs,
	}
	lightInv := chooseInvocation(invs, StageLight)
	heavyInv := chooseInvocation(invs, StageHeavy)
	tr.LightLLM = lightTrace(score, lightParsed, lightInv)
	tr.HeavyLLM = heavyTrace(tr.LightLLM, heavyParsed, heavyInv)
	return tr, nil
}

type messageRow struct {
	header MessageHeader
}

func (s *Service) message(ctx context.Context, id string) (MessageHeader, error) {
	var subject, from, toJSON, ccJSON, receivedAt, status sql.NullString
	err := s.db.QueryRowContext(ctx, `
		SELECT subject, from_address, to_addresses, cc_addresses, received_at, processing_status
		  FROM email_message
		 WHERE id = ?`, id).Scan(&subject, &from, &toJSON, &ccJSON, &receivedAt, &status)
	if errors.Is(err, sql.ErrNoRows) {
		return MessageHeader{}, fmt.Errorf("message %s not found", id)
	}
	if err != nil {
		return MessageHeader{}, fmt.Errorf("load email_message: %w", err)
	}
	return MessageHeader{
		Subject:          subject.String,
		From:             from.String,
		To:               stringList(toJSON.String),
		CC:               stringList(ccJSON.String),
		ReceivedAt:       stringPtr(receivedAt),
		ProcessingStatus: status.String,
	}, nil
}

type scoreRow struct {
	ok              bool
	stage1Band      sql.NullString
	stage2PromptKey sql.NullString
	details         map[string]any
	rankRationale   sql.NullString
}

func (s *Service) score(ctx context.Context, messageID string) (scoreRow, error) {
	var band, prompt, details, rationale sql.NullString
	err := s.db.QueryRowContext(ctx, `
		SELECT stage1_band, stage2_prompt_key, details, rank_rationale
		  FROM mail_item_score
		 WHERE message_id = ?`, messageID).Scan(&band, &prompt, &details, &rationale)
	if errors.Is(err, sql.ErrNoRows) {
		return scoreRow{}, nil
	}
	if err != nil {
		return scoreRow{}, fmt.Errorf("load mail_item_score: %w", err)
	}
	return scoreRow{ok: true, stage1Band: band, stage2PromptKey: prompt, details: object(details.String), rankRationale: rationale}, nil
}

type lightRow struct {
	ok             bool
	promptVersion  sql.NullString
	fields         map[string]any
	escalate       bool
	escalateKnown  bool
	escalateReason any
}

func (s *Service) light(ctx context.Context, messageID string) (lightRow, error) {
	var promptVersion, contentClass, scores, summary, keyPoints, quickReply, openLoop, escalateReason, parsedJSON sql.NullString
	var escalate sql.NullBool
	err := s.db.QueryRowContext(ctx, `
		SELECT prompt_version, content_class, scores, summary, key_points, quick_reply,
		       open_loop, escalate, escalate_reason, parsed_json
		  FROM email_message_comprehension
		 WHERE message_id = ?`, messageID).Scan(&promptVersion, &contentClass, &scores, &summary, &keyPoints, &quickReply, &openLoop, &escalate, &escalateReason, &parsedJSON)
	if errors.Is(err, sql.ErrNoRows) {
		return lightRow{fields: lightDefaults()}, nil
	}
	if err != nil {
		return lightRow{}, fmt.Errorf("load email_message_comprehension: %w", err)
	}
	fields := object(parsedJSON.String)
	overlay(fields, "content_class", nullStringValue(contentClass))
	overlay(fields, "scores", anyJSON(scores.String, map[string]any{}))
	overlay(fields, "summary", nullStringValue(summary))
	overlay(fields, "key_points", anyJSON(keyPoints.String, []any{}))
	overlay(fields, "quick_reply", anyJSONOrString(quickReply))
	overlay(fields, "open_loop", anyJSONOrString(openLoop))
	if escalate.Valid {
		fields["escalate"] = escalate.Bool
	} else if _, ok := fields["escalate"]; !ok {
		fields["escalate"] = nil
	}
	overlay(fields, "escalate_reason", anyJSONOrString(escalateReason))
	return lightRow{
		ok:             true,
		promptVersion:  promptVersion,
		fields:         fields,
		escalate:       escalate.Valid && escalate.Bool,
		escalateKnown:  escalate.Valid,
		escalateReason: fields["escalate_reason"],
	}, nil
}

type heavyRow struct {
	ok               bool
	fields           map[string]any
	manifest         []any
	escalationReason any
}

func (s *Service) heavy(ctx context.Context, messageID string) (heavyRow, error) {
	var draft, options, beliefDelta, pushVerdict, manifest, escalationReason, outputJSON sql.NullString
	err := s.db.QueryRowContext(ctx, `
		SELECT draft, options, belief_delta, push_verdict, context_bundle_manifest,
		       escalation_reason, output_json
		  FROM email_message_comprehension_heavy
		 WHERE message_id = ?`, messageID).Scan(&draft, &options, &beliefDelta, &pushVerdict, &manifest, &escalationReason, &outputJSON)
	if errors.Is(err, sql.ErrNoRows) {
		return heavyRow{fields: heavyDefaults()}, nil
	}
	if err != nil {
		return heavyRow{}, fmt.Errorf("load email_message_comprehension_heavy: %w", err)
	}
	fields := object(outputJSON.String)
	overlay(fields, "draft", anyJSONOrString(draft))
	overlay(fields, "options", anyJSON(options.String, []any{}))
	overlay(fields, "belief_delta", anyJSONOrString(beliefDelta))
	overlay(fields, "push_verdict", anyJSONOrString(pushVerdict))
	return heavyRow{
		ok:               true,
		fields:           fields,
		manifest:         anyList(manifest.String),
		escalationReason: anyJSONOrString(escalationReason),
	}, nil
}

func (s *Service) invocations(ctx context.Context, messageID string) ([]InvocationTrace, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT mi.id, mi.request_id, mi.stage, mi.provider, mi.model, mi.model_version,
		       mi.started_at, mi.completed_at, mi.latency_ms, mi.cost_amount,
		       COALESCE(mi.cost_currency, 'USD'), mi.status, mi.error, mi.created_at,
		       mip.request_text, mip.response_text
		  FROM model_invocation mi
		  LEFT JOIN model_invocation_payload mip ON mip.model_invocation_id = mi.id
		 WHERE mi.message_id = ?
		 ORDER BY mi.created_at DESC, mi.id DESC`, messageID)
	if err != nil {
		return nil, fmt.Errorf("load model_invocation: %w", err)
	}
	defer rows.Close()
	var out []InvocationTrace
	for rows.Next() {
		var id, requestID, stage, provider, model, modelVersion, startedAt, completedAt, currency, status, errText, createdAt, reqText, respText sql.NullString
		var latency sql.NullInt64
		var cost sql.NullFloat64
		if err := rows.Scan(&id, &requestID, &stage, &provider, &model, &modelVersion, &startedAt, &completedAt, &latency, &cost, &currency, &status, &errText, &createdAt, &reqText, &respText); err != nil {
			return nil, fmt.Errorf("scan model_invocation: %w", err)
		}
		meta := InvocationMeta{
			ID:           id.String,
			RequestID:    stringPtr(requestID),
			Stage:        stringPtr(stage),
			Provider:     stringPtr(provider),
			Model:        stringPtr(model),
			ModelVersion: stringPtr(modelVersion),
			StartedAt:    stringPtr(startedAt),
			CompletedAt:  stringPtr(completedAt),
			LatencyMS:    int64Ptr(latency),
			Cost:         Cost{Amount: floatPtr(cost), Currency: currency.String},
			Status:       status.String,
			Error:        stringPtr(errText),
			CreatedAt:    stringPtr(createdAt),
		}
		it := InvocationTrace{Invocation: meta, RequestText: stringPtr(reqText), ResponseText: stringPtr(respText)}
		if it.RequestText == nil || it.ResponseText == nil {
			v := PayloadMissing
			it.PayloadStatus = &v
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

func deterministic(score scoreRow) DeterministicTrace {
	if !score.ok {
		return DeterministicTrace{
			Status:     StatusMissing,
			Reason:     "mail_item_score_missing",
			Facts:      requiredFacts(nil),
			RawDetails: map[string]any{},
		}
	}
	facts := requiredFacts(score.details)
	for k, v := range score.details {
		key := normalizeKey(k)
		if _, exists := facts[key]; !exists {
			facts[key] = Fact{Value: v, Source: "mail_item_score.details." + k}
		}
	}
	why := []string{}
	if score.stage1Band.Valid {
		why = append(why, "stage1_band="+score.stage1Band.String)
	}
	if score.stage2PromptKey.Valid {
		why = append(why, "stage2_prompt_key="+score.stage2PromptKey.String)
	}
	for _, k := range []string{"known_contact", "contact_type", "vip", "recipient_position", "first_party", "sender_lean", "open_loop_candidate"} {
		if f, ok := facts[k]; ok && f.Value != nil {
			why = append(why, k+"="+fmt.Sprint(f.Value))
		}
	}
	return DeterministicTrace{
		Status:          StatusComplete,
		Stage1Band:      stringPtr(score.stage1Band),
		Stage2PromptKey: stringPtr(score.stage2PromptKey),
		RoutingDecision: RoutingDecision{SelectedPromptKey: stringPtr(score.stage2PromptKey), Why: why},
		Facts:           facts,
		RankRationale:   stringPtr(score.rankRationale),
		RawDetails:      score.details,
	}
}

func requiredFacts(details map[string]any) map[string]Fact {
	out := map[string]Fact{}
	for _, key := range []string{"known_contact", "contact_type", "vip", "recipient_position", "first_party", "sender_lean", "correlations", "entity_match"} {
		value, source, raw := lookup(details, key)
		out[key] = Fact{Value: value, Source: source, Raw: raw}
	}
	return out
}

func lightTrace(score scoreRow, parsed lightRow, inv *InvocationTrace) LLMTrace {
	if score.ok && !score.stage2PromptKey.Valid && !parsed.ok && inv == nil {
		reason := "deterministic_route_no_llm"
		return LLMTrace{Status: StatusSkipped, SkipReason: &reason, ParsedVerdict: lightDefaults()}
	}
	if inv == nil {
		if parsed.ok {
			req, resp := LegacyUncorrelated, LegacyUncorrelated
			return LLMTrace{Status: StatusComplete, RequestText: &req, ResponseText: &resp, ParsedVerdict: parsed.fields}
		}
		return LLMTrace{Status: StatusMissing, ParsedVerdict: lightDefaults()}
	}
	meta := inv.Invocation
	meta.PromptVersion = stringPtr(parsed.promptVersion)
	status := statusFromInvocation(meta.Status)
	if parsed.ok && status != StatusFailed {
		status = StatusComplete
	}
	return LLMTrace{
		Status:        status,
		Error:         meta.Error,
		Invocation:    &meta,
		RequestText:   textOrPayloadStatus(inv.RequestText),
		ResponseText:  textOrPayloadStatus(inv.ResponseText),
		ParsedVerdict: parsed.fields,
	}
}

func heavyTrace(light LLMTrace, parsed heavyRow, inv *InvocationTrace) HeavyTrace {
	escalated := false
	var reason any
	if light.ParsedVerdict != nil {
		if b, ok := light.ParsedVerdict["escalate"].(bool); ok {
			escalated = b
		}
		reason = light.ParsedVerdict["escalate_reason"]
	}
	if parsed.ok {
		escalated = true
		if parsed.escalationReason != nil {
			reason = parsed.escalationReason
		}
	}
	if inv == nil {
		if !escalated && light.Status == StatusComplete {
			skip := "light_llm_did_not_escalate"
			return HeavyTrace{Status: StatusSkipped, SkipReason: &skip, Escalated: false, EscalationReason: reason, ContextBundleManifest: []any{}, ParsedOutput: heavyDefaults()}
		}
		if parsed.ok {
			req, resp := LegacyUncorrelated, LegacyUncorrelated
			return HeavyTrace{Status: StatusComplete, Escalated: true, EscalationReason: reason, ContextBundleManifest: parsed.manifest, RequestText: &req, ResponseText: &resp, ParsedOutput: parsed.fields}
		}
		return HeavyTrace{Status: StatusMissing, Escalated: escalated, EscalationReason: reason, ContextBundleManifest: []any{}, ParsedOutput: heavyDefaults()}
	}
	meta := inv.Invocation
	status := statusFromInvocation(meta.Status)
	if parsed.ok && status != StatusFailed {
		status = StatusComplete
	}
	return HeavyTrace{
		Status:                status,
		Error:                 meta.Error,
		Escalated:             escalated || parsed.ok || inv != nil,
		EscalationReason:      reason,
		ContextBundleManifest: parsed.manifest,
		Invocation:            &meta,
		RequestText:           textOrPayloadStatus(inv.RequestText),
		ResponseText:          textOrPayloadStatus(inv.ResponseText),
		ParsedOutput:          parsed.fields,
	}
}

func chooseInvocation(invs []InvocationTrace, stage string) *InvocationTrace {
	var latest *InvocationTrace
	for i := range invs {
		if invs[i].Invocation.Stage == nil || *invs[i].Invocation.Stage != stage {
			continue
		}
		if strings.EqualFold(invs[i].Invocation.Status, "succeeded") || strings.EqualFold(invs[i].Invocation.Status, "success") || strings.EqualFold(invs[i].Invocation.Status, "complete") {
			return &invs[i]
		}
		if latest == nil {
			latest = &invs[i]
		}
	}
	return latest
}

func statusFromInvocation(status string) string {
	switch strings.ToLower(status) {
	case "succeeded", "success", "complete", "completed":
		return StatusComplete
	case "failed", "error":
		return StatusFailed
	case "pending", "running", "started", "in_progress", "":
		return StatusPending
	default:
		return StatusPending
	}
}

func lookup(details map[string]any, want string) (any, string, any) {
	if details == nil {
		return nil, "mail_item_score.details." + want, nil
	}
	for _, key := range aliases(want) {
		if v, ok := details[key]; ok {
			source := "mail_item_score.details." + key
			if key != want {
				return v, source, map[string]any{key: v}
			}
			return v, source, nil
		}
	}
	return nil, "mail_item_score.details." + want, nil
}

func aliases(key string) []string {
	base := []string{key, strings.ReplaceAll(key, "_", "-")}
	switch key {
	case "known_contact":
		base = append(base, "knownContact")
	case "contact_type":
		base = append(base, "contactType")
	case "recipient_position":
		base = append(base, "recipientPosition")
	case "first_party":
		base = append(base, "firstParty")
	case "sender_lean":
		base = append(base, "senderLean")
	case "entity_match":
		base = append(base, "entityMatch")
	}
	return base
}

func normalizeKey(k string) string {
	return strings.ReplaceAll(strings.ToLower(k), "-", "_")
}

func lightDefaults() map[string]any {
	return map[string]any{
		"content_class":   nil,
		"scores":          map[string]any{},
		"summary":         nil,
		"key_points":      []any{},
		"quick_reply":     nil,
		"open_loop":       nil,
		"escalate":        nil,
		"escalate_reason": nil,
	}
}

func heavyDefaults() map[string]any {
	return map[string]any{
		"draft":        nil,
		"options":      []any{},
		"belief_delta": nil,
		"push_verdict": nil,
	}
}

func object(raw string) map[string]any {
	out := map[string]any{}
	if strings.TrimSpace(raw) == "" {
		return out
	}
	_ = json.Unmarshal([]byte(raw), &out)
	return out
}

func anyJSON(raw string, fallback any) any {
	if strings.TrimSpace(raw) == "" {
		return fallback
	}
	var v any
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		return fallback
	}
	return v
}

func anyList(raw string) []any {
	v := anyJSON(raw, []any{})
	if out, ok := v.([]any); ok {
		return out
	}
	return []any{}
}

func anyJSONOrString(ns sql.NullString) any {
	if !ns.Valid {
		return nil
	}
	var v any
	if json.Unmarshal([]byte(ns.String), &v) == nil {
		return v
	}
	return ns.String
}

func overlay(m map[string]any, key string, value any) {
	if value != nil {
		m[key] = value
		return
	}
	if _, ok := m[key]; !ok {
		m[key] = nil
	}
}

func stringList(raw string) []string {
	if strings.TrimSpace(raw) == "" {
		return []string{}
	}
	var out []string
	if json.Unmarshal([]byte(raw), &out) == nil {
		return out
	}
	return []string{raw}
}

func nullStringValue(ns sql.NullString) any {
	if !ns.Valid {
		return nil
	}
	return ns.String
}

func stringPtr(ns sql.NullString) *string {
	if !ns.Valid {
		return nil
	}
	v := ns.String
	return &v
}

func int64Ptr(ns sql.NullInt64) *int64 {
	if !ns.Valid {
		return nil
	}
	v := ns.Int64
	return &v
}

func floatPtr(ns sql.NullFloat64) *float64 {
	if !ns.Valid {
		return nil
	}
	v := ns.Float64
	return &v
}

func textOrPayloadStatus(v *string) *string {
	if v != nil {
		return v
	}
	missing := PayloadMissing
	return &missing
}

func nullable(v string) any {
	if v == "" {
		return nil
	}
	return v
}

func SortedFactKeys(facts map[string]Fact) []string {
	keys := make([]string, 0, len(facts))
	for k := range facts {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
