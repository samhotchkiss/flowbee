package mailurgency

import (
	"context"
	"errors"
	"log/slog"
	"net/mail"
	"strings"
	"sync"
)

const (
	ReasonSentOrDraft            = "sent_or_draft"
	ReasonFirstPartySender       = "first_party_sender"
	ReasonBulkWithoutPersonalAsk = "bulk_without_personal_ask"
	ReasonMissingLLMVerdict      = "missing_llm_verdict"
	ReasonLLMRejectedUrgency     = "llm_rejected_urgency"
)

const (
	StatusCompleted = "completed"

	EventMessageReceived = "message.received"
	EventStage3Completed = "stage3.completed"
)

var (
	ErrMissingAttentionStore      = errors.New("mail urgency: missing attention store")
	ErrMissingNeedStore           = errors.New("mail urgency: missing need store")
	ErrMissingNeedEventPublisher  = errors.New("mail urgency: missing need event publisher")
	ErrMissingMessageLoader       = errors.New("mail urgency: missing message loader")
	ErrMissingComprehensionLoader = errors.New("mail urgency: missing comprehension loader")
	ErrMissingComprehensionQueue  = errors.New("mail urgency: missing comprehension queue")
)

type Message struct {
	TenantID          string
	UserID            string
	MessageID         string
	ThreadID          string
	Status            string
	SenderEmail       string
	SenderDomain      string
	FirstPartyDomains []string
	Headers           map[string]string
	Bulk              bool
}

type Comprehension struct {
	ID                     string
	MessageID              string
	Status                 string
	VerdictRecorded        bool
	PersonalAskConfirmed   bool
	AllowedUrgencies       []string
	ImpactStatement        string
	ImpactStatementAllowed bool
}

type Decision struct {
	Eligible              bool
	Reason                string
	RequiresComprehension bool
	VerdictID             string
}

type RegexCandidate struct {
	Priority        string
	ImpactStatement string
	Terms           []string
}

type AttentionItem struct {
	TenantID         string
	UserID           string
	MessageID        string
	ThreadID         string
	Priority         string
	ImpactStatement  string
	ComprehensionID  string
	ClassificationBy string
}

type NeedItem struct {
	TenantID        string
	UserID          string
	MessageID       string
	ThreadID        string
	Priority        string
	ComprehensionID string
}

type Event struct {
	Type            string
	TenantID        string
	UserID          string
	MessageID       string
	ComprehensionID string
	Priority        string
}

type AttentionStore interface {
	InsertMailAttention(context.Context, AttentionItem) error
}

type NeedStore interface {
	InsertEmailNeed(context.Context, NeedItem) error
}

type NeedEventPublisher interface {
	PublishEmailNeedDerivation(context.Context, Event) error
}

type MessageLoader func(context.Context, Event) (Message, error)

type ComprehensionLoader func(context.Context, Event) (*Comprehension, error)

type ComprehensionQueue interface {
	EnqueueComprehension(context.Context, Message) error
}

type Deriver struct {
	LoadMessage       MessageLoader
	LoadComprehension ComprehensionLoader
	Store             NeedStore
	Observer          Observer
}

type Observer interface {
	CountSkip(stage, reason string, msg Message)
	CountDeferred(stage string, msg Message)
}

type MemoryObserver struct {
	mu       sync.Mutex
	Skips    map[string]int
	Deferred map[string]int
}

func NewMemoryObserver() *MemoryObserver {
	return &MemoryObserver{Skips: map[string]int{}, Deferred: map[string]int{}}
}

func (o *MemoryObserver) CountSkip(stage, reason string, _ Message) {
	if o == nil {
		return
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	o.Skips[stage+":"+reason]++
}

func (o *MemoryObserver) CountDeferred(stage string, _ Message) {
	if o == nil {
		return
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	o.Deferred[stage]++
}

type SlogObserver struct {
	Logger *slog.Logger
}

func (o SlogObserver) CountSkip(stage, reason string, msg Message) {
	if o.Logger == nil {
		return
	}
	o.Logger.Info("mail urgency source gate skipped",
		"stage", stage,
		"reason", reason,
		"tenant_id", msg.TenantID,
		"user_id", msg.UserID,
		"message_id", msg.MessageID,
	)
}

func (o SlogObserver) CountDeferred(stage string, msg Message) {
	if o.Logger == nil {
		return
	}
	o.Logger.Info("mail urgency deferred waiting for comprehension",
		"stage", stage,
		"reason", ReasonMissingLLMVerdict,
		"tenant_id", msg.TenantID,
		"user_id", msg.UserID,
		"message_id", msg.MessageID,
	)
}

func IsMailAttentionEligible(_ context.Context, msg Message, comp *Comprehension) Decision {
	switch normalizedStatus(msg.Status) {
	case "sent", "draft", "drafted":
		return Decision{Eligible: false, Reason: ReasonSentOrDraft}
	}

	if matchesFirstPartyDomain(msg) {
		return Decision{Eligible: false, Reason: ReasonFirstPartySender}
	}

	if isBulk(msg) && !hasCompletedPersonalAsk(comp) {
		return Decision{
			Eligible:              false,
			Reason:                ReasonBulkWithoutPersonalAsk,
			RequiresComprehension: !hasCompletedVerdict(comp),
		}
	}

	decision := Decision{Eligible: true}
	if hasCompletedVerdict(comp) {
		decision.VerdictID = comp.ID
	}
	return decision
}

func AuthorizeUserVisibleUrgency(ctx context.Context, msg Message, comp *Comprehension, priority string, wantsImpactStatement bool) Decision {
	source := IsMailAttentionEligible(ctx, msg, comp)
	if !source.Eligible {
		return source
	}

	if !isUserVisibleUrgency(priority, wantsImpactStatement) {
		return source
	}

	if !hasCompletedVerdict(comp) {
		return Decision{
			Eligible:              false,
			Reason:                ReasonMissingLLMVerdict,
			RequiresComprehension: true,
		}
	}

	if isUrgentPriority(priority) && !allowsUrgency(comp, priority) {
		return Decision{Eligible: false, Reason: ReasonLLMRejectedUrgency, VerdictID: comp.ID}
	}
	if wantsImpactStatement && strings.TrimSpace(comp.ImpactStatement) == "" && !comp.ImpactStatementAllowed {
		return Decision{Eligible: false, Reason: ReasonLLMRejectedUrgency, VerdictID: comp.ID}
	}

	return Decision{Eligible: true, VerdictID: comp.ID}
}

func ClassifyMailImpact(ctx context.Context, msg Message, comp *Comprehension, candidate RegexCandidate, obs Observer) (*AttentionItem, Decision) {
	decision := AuthorizeUserVisibleUrgency(ctx, msg, comp, candidate.Priority, strings.TrimSpace(candidate.ImpactStatement) != "")
	if !decision.Eligible {
		recordSkip(obs, "classifier", decision, msg)
		return nil, decision
	}

	impact := ""
	if strings.TrimSpace(candidate.ImpactStatement) != "" {
		impact = strings.TrimSpace(comp.ImpactStatement)
		if impact == "" && comp != nil && comp.ImpactStatementAllowed {
			impact = strings.TrimSpace(candidate.ImpactStatement)
		}
	}

	return &AttentionItem{
		TenantID:         msg.TenantID,
		UserID:           msg.UserID,
		MessageID:        msg.MessageID,
		ThreadID:         msg.ThreadID,
		Priority:         normalizedPriority(candidate.Priority),
		ImpactStatement:  impact,
		ComprehensionID:  decision.VerdictID,
		ClassificationBy: "llm_confirmed",
	}, decision
}

func ClassifyAndPersistMailImpact(ctx context.Context, store AttentionStore, msg Message, comp *Comprehension, candidate RegexCandidate, obs Observer) (Decision, error) {
	item, decision := ClassifyMailImpact(ctx, msg, comp, candidate, obs)
	if item == nil {
		return decision, nil
	}
	if store == nil {
		return decision, ErrMissingAttentionStore
	}
	if err := store.InsertMailAttention(ctx, *item); err != nil {
		return decision, err
	}
	return decision, nil
}

func PublishEmailNeedDerivation(ctx context.Context, publisher NeedEventPublisher, event Event, msg Message, comp *Comprehension, priority string, obs Observer) (Decision, error) {
	if event.Type != EventStage3Completed {
		decision := Decision{Eligible: false, Reason: ReasonMissingLLMVerdict, RequiresComprehension: true}
		recordSkip(obs, "deriver", decision, msg)
		return decision, nil
	}
	decision := AuthorizeUserVisibleUrgency(ctx, msg, comp, priority, false)
	if !decision.Eligible {
		recordSkip(obs, "deriver", decision, msg)
		return decision, nil
	}
	if !hasCompletedPersonalAsk(comp) {
		decision = Decision{Eligible: false, Reason: ReasonLLMRejectedUrgency, VerdictID: verdictID(comp)}
		recordSkip(obs, "deriver", decision, msg)
		return decision, nil
	}
	event.Type = EventStage3Completed
	event.TenantID = msg.TenantID
	event.UserID = msg.UserID
	event.MessageID = msg.MessageID
	event.ComprehensionID = decision.VerdictID
	event.Priority = normalizedPriority(priority)
	if publisher == nil {
		return decision, ErrMissingNeedEventPublisher
	}
	if err := publisher.PublishEmailNeedDerivation(ctx, event); err != nil {
		return decision, err
	}
	return decision, nil
}

func DeriveEmailNeed(ctx context.Context, eventType string, msg Message, comp *Comprehension, priority string, obs Observer) (*NeedItem, Decision) {
	if eventType != EventStage3Completed {
		decision := Decision{Eligible: false, Reason: ReasonMissingLLMVerdict, RequiresComprehension: true}
		recordSkip(obs, "deriver", decision, msg)
		return nil, decision
	}

	decision := AuthorizeUserVisibleUrgency(ctx, msg, comp, priority, false)
	if !decision.Eligible {
		recordSkip(obs, "deriver", decision, msg)
		return nil, decision
	}
	if !hasCompletedPersonalAsk(comp) {
		decision = Decision{Eligible: false, Reason: ReasonLLMRejectedUrgency, VerdictID: verdictID(comp)}
		recordSkip(obs, "deriver", decision, msg)
		return nil, decision
	}

	return &NeedItem{
		TenantID:        msg.TenantID,
		UserID:          msg.UserID,
		MessageID:       msg.MessageID,
		ThreadID:        msg.ThreadID,
		Priority:        normalizedPriority(priority),
		ComprehensionID: decision.VerdictID,
	}, decision
}

func EmailNeedDerivationEventTypes() []string {
	return []string{EventStage3Completed}
}

func (d Deriver) SubscribedEvents() []string {
	return EmailNeedDerivationEventTypes()
}

func (d Deriver) HandleEvent(ctx context.Context, event Event) (Decision, error) {
	if event.Type != EventStage3Completed {
		decision := Decision{Eligible: false, Reason: ReasonMissingLLMVerdict, RequiresComprehension: true}
		recordSkip(d.Observer, "deriver", decision, Message{TenantID: event.TenantID, UserID: event.UserID, MessageID: event.MessageID})
		return decision, nil
	}
	if d.LoadMessage == nil {
		return Decision{}, ErrMissingMessageLoader
	}
	if d.LoadComprehension == nil {
		return Decision{}, ErrMissingComprehensionLoader
	}
	msg, err := d.LoadMessage(ctx, event)
	if err != nil {
		return Decision{}, err
	}
	comp, err := d.LoadComprehension(ctx, event)
	if err != nil {
		return Decision{}, err
	}
	need, decision := DeriveEmailNeed(ctx, event.Type, msg, comp, event.Priority, d.Observer)
	if need == nil {
		return decision, nil
	}
	if d.Store == nil {
		return decision, ErrMissingNeedStore
	}
	if err := d.Store.InsertEmailNeed(ctx, *need); err != nil {
		return decision, err
	}
	return decision, nil
}

func IngestBackfillMessage(ctx context.Context, queue ComprehensionQueue, msg Message) error {
	if queue == nil {
		return ErrMissingComprehensionQueue
	}
	return queue.EnqueueComprehension(ctx, msg)
}

func recordSkip(obs Observer, stage string, decision Decision, msg Message) {
	if obs == nil {
		return
	}
	obs.CountSkip(stage, decision.Reason, msg)
	if decision.RequiresComprehension {
		obs.CountDeferred(stage, msg)
	}
}

func normalizedStatus(status string) string {
	return strings.ToLower(strings.TrimSpace(status))
}

func normalizedPriority(priority string) string {
	return strings.ToLower(strings.TrimSpace(priority))
}

func isUserVisibleUrgency(priority string, wantsImpactStatement bool) bool {
	return isUrgentPriority(priority) || wantsImpactStatement
}

func isUrgentPriority(priority string) bool {
	switch normalizedPriority(priority) {
	case "p0", "p1", "urgent", "critical":
		return true
	default:
		return false
	}
}

func hasCompletedVerdict(comp *Comprehension) bool {
	return comp != nil &&
		strings.EqualFold(strings.TrimSpace(comp.Status), StatusCompleted) &&
		comp.VerdictRecorded &&
		strings.TrimSpace(comp.ID) != ""
}

func hasCompletedPersonalAsk(comp *Comprehension) bool {
	return hasCompletedVerdict(comp) && comp.PersonalAskConfirmed
}

func verdictID(comp *Comprehension) string {
	if comp == nil {
		return ""
	}
	return comp.ID
}

func allowsUrgency(comp *Comprehension, priority string) bool {
	if comp == nil {
		return false
	}
	want := normalizedPriority(priority)
	for _, allowed := range comp.AllowedUrgencies {
		if normalizedPriority(allowed) == want {
			return true
		}
	}
	return false
}

func isBulk(msg Message) bool {
	if msg.Bulk {
		return true
	}
	for key, value := range msg.Headers {
		if strings.EqualFold(strings.TrimSpace(key), "List-Unsubscribe") && strings.TrimSpace(value) != "" {
			return true
		}
	}
	return false
}

func matchesFirstPartyDomain(msg Message) bool {
	sender := senderComparisonDomain(msg)
	if sender == "" {
		return false
	}
	for _, configured := range msg.FirstPartyDomains {
		domain := normalizeDomain(configured)
		if domain == "" {
			continue
		}
		if sender == domain || strings.HasSuffix(sender, "."+domain) {
			return true
		}
	}
	return false
}

func senderComparisonDomain(msg Message) string {
	if strings.TrimSpace(msg.SenderDomain) != "" {
		return normalizeDomain(msg.SenderDomain)
	}
	if strings.TrimSpace(msg.SenderEmail) == "" {
		return ""
	}
	if addr, err := mail.ParseAddress(msg.SenderEmail); err == nil {
		return domainFromAddress(addr.Address)
	}
	return domainFromAddress(msg.SenderEmail)
}

func domainFromAddress(addr string) string {
	at := strings.LastIndex(addr, "@")
	if at < 0 || at == len(addr)-1 {
		return ""
	}
	return normalizeDomain(addr[at+1:])
}

func normalizeDomain(domain string) string {
	return strings.TrimSuffix(strings.ToLower(strings.TrimSpace(domain)), ".")
}
