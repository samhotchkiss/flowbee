// Package llm is the single backend boundary for model-backed calls. Callers name
// a semantic slot; the router resolves the active database binding and owns the
// provider attempt, policy checks, fallback behavior, and invocation ledger.
package llm

import (
	"context"
	"errors"
	"time"
)

type SlotKey string

const (
	SlotChat                SlotKey = "chat"
	SlotDraftingComplex     SlotKey = "drafting-complex"
	SlotFactCheck           SlotKey = "fact-check"
	SlotMemoryExtraction    SlotKey = "memory-extraction"
	SlotComprehensionLight  SlotKey = "comprehension-light"
	SlotComprehensionHeavy  SlotKey = "comprehension-heavy"
	SlotClassificationLight SlotKey = "classification-light"
	SlotEmbeddings          SlotKey = "embeddings"
	SlotVision              SlotKey = "vision"
	SlotJudge               SlotKey = "judge"
)

var validSlots = map[SlotKey]SlotPolicy{
	SlotChat:                {LatencyClass: "ttft-critical", Budget: 0, StreamingRequired: true, AllowsEffort: false},
	SlotDraftingComplex:     {LatencyClass: "work-task-relaxed", Budget: 3 * time.Minute, AllowsEffort: true},
	SlotFactCheck:           {LatencyClass: "work-task-relaxed", Budget: 3 * time.Minute, AllowsEffort: true},
	SlotMemoryExtraction:    {LatencyClass: "email-pipeline-bounded", Budget: 15 * time.Second, AllowsEffort: true},
	SlotComprehensionLight:  {LatencyClass: "email-pipeline-synchronous", Budget: 8 * time.Second, AllowsEffort: true},
	SlotComprehensionHeavy:  {LatencyClass: "email-pipeline-async", Budget: 90 * time.Second, AllowsEffort: true},
	SlotClassificationLight: {LatencyClass: "email-pipeline-synchronous", Budget: 15 * time.Second, AllowsEffort: true},
	SlotEmbeddings:          {LatencyClass: "pipeline-bounded", Budget: 0, AllowsEffort: false},
	SlotVision:              {LatencyClass: "task-dependent", Budget: 0, AllowsEffort: false},
	SlotJudge:               {LatencyClass: "evaluation-internal", Budget: 3 * time.Minute, AllowsEffort: true},
}

type SlotPolicy struct {
	LatencyClass      string
	Budget            time.Duration
	StreamingRequired bool
	AllowsEffort      bool
}

func Policy(slot SlotKey) (SlotPolicy, bool) {
	p, ok := validSlots[slot]
	return p, ok
}

type Message struct {
	Role    string
	Content string
}

type ResponseFormat struct {
	Type   string
	Schema any
}

type Tool struct {
	Name        string
	Description string
	Schema      any
}

type ToolCall struct {
	Name      string
	Arguments string
}

type Usage struct {
	PromptTokens     int
	CompletionTokens int
	TotalTokens      int
	EstimatedCostUSD float64
}

type Request struct {
	Messages       []Message
	Prompt         string
	Input          any
	ResponseFormat *ResponseFormat
	Tools          []Tool
	Metadata       map[string]string
	TenantID       *string
	UserID         *string
	TraceID        string
	IdempotencyKey string
	Stream         bool
}

// AgentCommand is Flowbee's production provider payload for worker-backed LLM
// agents. It is intentionally owned by internal/llm so shell/CLI provider
// invocation has the same routing, budget, privacy, fallback, and ledger boundary
// as direct API-backed model calls.
type AgentCommand struct {
	Command    string
	Dir        string
	Env        []string
	TTLSeconds int
	Heartbeat  func(context.Context) bool
}

type Response struct {
	Text              string
	ToolCalls         []ToolCall
	Embedding         []float32
	Usage             Usage
	ModelID           string
	Provider          string
	Slot              SlotKey
	BindingID         string
	InvocationID      string
	FallbackAttempted bool
}

type StreamHandle interface {
	Recv() (Response, error)
	Close() error
}

type ProviderPins struct {
	AllowedProviders     []string `json:"allowed_providers"`
	RequiredProvider     string   `json:"required_provider"`
	AllowProviderRouting bool     `json:"allow_provider_routing"`
}

type BindingUpdate struct {
	BindingID        string
	Slot             SlotKey
	ModelID          string
	ProviderPinsJSON string
	PromptVersionRef string
	VerdictRef       string
}

var (
	ErrNoDefaultRouter     = errors.New("llm: no default router configured")
	ErrNoActiveBinding     = errors.New("llm: no active model slot binding")
	ErrInvalidSlot         = errors.New("llm: invalid slot")
	ErrInvalidRequest      = errors.New("llm: invalid request")
	ErrPrivacyTierBlocked  = errors.New("llm: privacy tier blocked")
	ErrBudgetExceeded      = errors.New("llm: budget exceeded")
	ErrNoUsableFallback    = errors.New("llm: no usable fallback")
	ErrProviderUnavailable = errors.New("llm: provider unavailable")
	ErrBenchmarkGate       = errors.New("llm: benchmark gate failed")
)
