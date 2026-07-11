package llm

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"
)

type providerClient interface {
	Call(context.Context, ProviderRequest) (Response, error)
	Embed(context.Context, ProviderRequest) (Response, error)
	Stream(context.Context, ProviderRequest) (StreamHandle, error)
}

type ProviderRequest struct {
	Request
	ModelID  string
	Provider string
	Params   map[string]any
	Effort   string
}

type AlertSink interface {
	BudgetThreshold(ctx context.Context, slot SlotKey, tenantID *string, month string, spendUSD, budgetUSD float64)
}

type Router struct {
	db      *sql.DB
	clients map[string]providerClient
	alerts  AlertSink
	now     func() time.Time
}

type Option func(*Router)

func withProviderForTest(provider string, client providerClient) Option {
	return func(r *Router) {
		r.clients[provider] = client
	}
}

func WithAlertSink(s AlertSink) Option {
	return func(r *Router) {
		r.alerts = s
	}
}

func WithClock(now func() time.Time) Option {
	return func(r *Router) {
		r.now = now
	}
}

func NewRouter(db *sql.DB, opts ...Option) *Router {
	r := &Router{
		db:      db,
		clients: defaultProviderClients(),
		now:     time.Now,
	}
	for _, opt := range opts {
		opt(r)
	}
	return r
}

var defaultRouter struct {
	mu sync.RWMutex
	r  *Router
}

func SetDefaultRouter(r *Router) {
	defaultRouter.mu.Lock()
	defer defaultRouter.mu.Unlock()
	defaultRouter.r = r
}

func Call(ctx context.Context, slot SlotKey, req Request) (Response, error) {
	r := getDefaultRouter()
	if r == nil {
		return Response{}, ErrNoDefaultRouter
	}
	return r.Call(ctx, slot, req)
}

func Stream(ctx context.Context, slot SlotKey, req Request) (StreamHandle, error) {
	r := getDefaultRouter()
	if r == nil {
		return nil, ErrNoDefaultRouter
	}
	return r.Stream(ctx, slot, req)
}

func Embed(ctx context.Context, slot SlotKey, req Request) (Response, error) {
	r := getDefaultRouter()
	if r == nil {
		return Response{}, ErrNoDefaultRouter
	}
	return r.Embed(ctx, slot, req)
}

func ValidateBenchmarkGate(ctx context.Context, candidate BindingUpdate) error {
	r := getDefaultRouter()
	if r == nil {
		return ErrNoDefaultRouter
	}
	return r.ValidateBenchmarkGate(ctx, candidate)
}

func getDefaultRouter() *Router {
	defaultRouter.mu.RLock()
	defer defaultRouter.mu.RUnlock()
	return defaultRouter.r
}

func (r *Router) Call(ctx context.Context, slot SlotKey, req Request) (Response, error) {
	return r.call(ctx, slot, req, "call")
}

func (r *Router) Embed(ctx context.Context, slot SlotKey, req Request) (Response, error) {
	return r.call(ctx, slot, req, "embed")
}

func (r *Router) Stream(ctx context.Context, slot SlotKey, req Request) (StreamHandle, error) {
	b, err := r.resolveAndValidate(ctx, slot, req, "stream")
	if err != nil {
		return nil, err
	}
	attempts := append([]attempt{{Binding: b}}, b.fallbackAttempts()...)
	var last error
	for i, a := range attempts {
		if i > 0 && !fallbackEligible(last) {
			break
		}
		ab := a.Binding
		if err := r.checkBudgetAndPrivacy(ctx, ab, i, i > 0); err != nil {
			return nil, err
		}
		client := r.clients[ab.provider()]
		if client == nil {
			last = ErrProviderUnavailable
			_, _ = r.recordInvocation(ctx, ab, i, i > 0, "failed", Usage{}, 0, 0, "provider_unavailable", ErrProviderUnavailable.Error())
			continue
		}
		start := r.now()
		h, err := client.Stream(ctx, ProviderRequest{Request: req, ModelID: ab.ModelID, Provider: ab.provider(), Params: ab.Params, Effort: ab.Effort})
		if err != nil {
			last = err
			_, _ = r.recordInvocation(ctx, ab, i, i > 0, "failed", Usage{}, sinceMS(start, r.now()), 0, providerErrCode(err), shortErr(err))
			continue
		}
		return newRecordingStream(ctx, r, ab, i, i > 0, start, h), nil
	}
	if last == nil {
		last = ErrNoUsableFallback
	}
	return nil, last
}

// recordingStream wraps a provider StreamHandle so the router can measure real
// time-to-first-token and real usage totals as the stream is consumed, and
// ledger a single invocation once the stream finishes (successfully or not)
// instead of recording a success immediately after opening the connection.
type recordingStream struct {
	ctx      context.Context
	router   *Router
	binding  Binding
	attempt  int
	fallback bool
	start    time.Time
	handle   StreamHandle

	mu       sync.Mutex
	ttftMS   int
	usage    Usage
	recorded bool
}

func newRecordingStream(ctx context.Context, r *Router, b Binding, attempt int, fallback bool, start time.Time, h StreamHandle) *recordingStream {
	return &recordingStream{ctx: ctx, router: r, binding: b, attempt: attempt, fallback: fallback, start: start, handle: h}
}

func (s *recordingStream) Recv() (Response, error) {
	resp, err := s.handle.Recv()

	s.mu.Lock()
	if s.ttftMS == 0 {
		s.ttftMS = sinceMS(s.start, s.router.now())
	}
	if resp.Usage != (Usage{}) {
		s.usage = resp.Usage
	}
	s.mu.Unlock()

	resp.ModelID = s.binding.ModelID
	resp.Provider = s.binding.provider()
	resp.Slot = s.binding.Slot
	resp.BindingID = s.binding.ID
	resp.FallbackAttempted = s.fallback

	if err != nil {
		if errors.Is(err, io.EOF) {
			s.finish("success", "", "")
		} else {
			s.finish("failed", providerErrCode(err), shortErr(err))
		}
	}
	return resp, err
}

func (s *recordingStream) Close() error {
	err := s.handle.Close()
	// If the caller closes the stream before it reached a terminal Recv()
	// (EOF or error), still ledger what was actually observed rather than
	// leaving the invocation unrecorded.
	s.finish("failed", "stream_closed", "stream closed before completion")
	return err
}

func (s *recordingStream) finish(status, code, msg string) {
	s.mu.Lock()
	if s.recorded {
		s.mu.Unlock()
		return
	}
	s.recorded = true
	usage := s.usage
	ttft := s.ttftMS
	s.mu.Unlock()

	latency := sinceMS(s.start, s.router.now())
	var baseline float64
	if status == "success" {
		// Compute the pre-call baseline before recordInvocation inserts this
		// call's row, so alertIfNeeded doesn't double-count the current call.
		baseline, _ = s.router.monthSpend(s.ctx, s.binding)
	}
	invID, _ := s.router.recordInvocation(s.ctx, s.binding, s.attempt, s.fallback, status, usage, latency, ttft, code, msg)
	_ = invID
	if status == "success" {
		_ = s.router.alertIfNeeded(s.ctx, s.binding, baseline, usage.EstimatedCostUSD)
	}
}

func (r *Router) call(ctx context.Context, slot SlotKey, req Request, kind string) (Response, error) {
	b, err := r.resolveAndValidate(ctx, slot, req, kind)
	if err != nil {
		return Response{}, err
	}
	attempts := append([]attempt{{Binding: b}}, b.fallbackAttempts()...)
	var last error
	for i, a := range attempts {
		if i > 0 && !fallbackEligible(last) {
			break
		}
		ab := a.Binding
		if err := r.checkBudgetAndPrivacy(ctx, ab, i, i > 0); err != nil {
			return Response{}, err
		}
		client := r.clients[ab.provider()]
		if client == nil {
			last = ErrProviderUnavailable
			_, _ = r.recordInvocation(ctx, ab, i, i > 0, "failed", Usage{}, 0, 0, "provider_unavailable", ErrProviderUnavailable.Error())
			continue
		}
		start := r.now()
		preq := ProviderRequest{Request: req, ModelID: ab.ModelID, Provider: ab.provider(), Params: ab.Params, Effort: ab.Effort}
		var resp Response
		if kind == "embed" {
			resp, err = client.Embed(ctx, preq)
		} else {
			resp, err = client.Call(ctx, preq)
		}
		latency := sinceMS(start, r.now())
		if err != nil {
			last = err
			_, _ = r.recordInvocation(ctx, ab, i, i > 0, "failed", Usage{}, latency, 0, providerErrCode(err), shortErr(err))
			continue
		}
		if emptyResponse(kind, req, resp) {
			last = ErrNoUsableFallback
			_, _ = r.recordInvocation(ctx, ab, i, i > 0, "empty", resp.Usage, latency, 0, "empty_response", "provider returned no usable content")
			continue
		}
		// Compute the pre-call baseline before recordInvocation inserts this
		// call's row, so alertIfNeeded doesn't double-count the current call.
		baseline, _ := r.monthSpend(ctx, ab)
		invID, _ := r.recordInvocation(ctx, ab, i, i > 0, "success", resp.Usage, latency, 0, "", "")
		_ = r.alertIfNeeded(ctx, ab, baseline, resp.Usage.EstimatedCostUSD)
		resp.ModelID = ab.ModelID
		resp.Provider = ab.provider()
		resp.Slot = ab.Slot
		resp.BindingID = ab.ID
		resp.InvocationID = invID
		resp.FallbackAttempted = i > 0
		return resp, nil
	}
	if last == nil {
		last = ErrNoUsableFallback
	}
	return Response{}, last
}

func (r *Router) resolveAndValidate(ctx context.Context, slot SlotKey, req Request, kind string) (Binding, error) {
	if _, ok := validSlots[slot]; !ok {
		return Binding{}, ErrInvalidSlot
	}
	if kind != "embed" && len(req.Messages) == 0 && req.Prompt == "" {
		return Binding{}, ErrInvalidRequest
	}
	if slot == SlotChat && kind != "stream" {
		return Binding{}, fmt.Errorf("%w: chat requires Stream", ErrInvalidRequest)
	}
	if kind == "embed" && req.Input == nil && req.Prompt == "" {
		return Binding{}, ErrInvalidRequest
	}
	b, err := r.resolveBinding(ctx, slot, req.TenantID)
	if err != nil {
		return Binding{}, err
	}
	if err := validateBinding(b); err != nil {
		return Binding{}, err
	}
	return b, nil
}

func validateBinding(b Binding) error {
	p, ok := Policy(b.Slot)
	if !ok {
		return ErrInvalidSlot
	}
	if !p.AllowsEffort && b.Effort != "" && b.Effort != "none" {
		return fmt.Errorf("%w: slot %s rejects effort %q", ErrInvalidRequest, b.Slot, b.Effort)
	}
	if tierRank(b.PrivacyTierRequired) > tierRank("public") && b.ProviderPins.RequiredProvider == "" && len(b.ProviderPins.AllowedProviders) == 0 {
		return fmt.Errorf("%w: provider pins required above public tier", ErrPrivacyTierBlocked)
	}
	return nil
}

func (r *Router) checkBudgetAndPrivacy(ctx context.Context, b Binding, attempt int, fallback bool) error {
	if err := r.enforcePrivacy(ctx, b); err != nil {
		_, _ = r.recordInvocation(ctx, b, attempt, fallback, "privacy_blocked", Usage{}, 0, 0, "privacy_blocked", err.Error())
		return err
	}
	if err := r.enforceBudget(ctx, b); err != nil {
		_, _ = r.recordInvocation(ctx, b, attempt, fallback, "budget_blocked", Usage{}, 0, 0, "budget_exceeded", err.Error())
		return err
	}
	return nil
}

func (r *Router) enforcePrivacy(ctx context.Context, b Binding) error {
	var supported string
	err := r.db.QueryRowContext(ctx, `
		SELECT privacy_tier_supported
		  FROM model_endpoint_policy
		 WHERE model_id = ? AND provider = ?`, b.ModelID, b.provider()).Scan(&supported)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrPrivacyTierBlocked
		}
		return err
	}
	if tierRank(supported) < tierRank(b.PrivacyTierRequired) {
		return ErrPrivacyTierBlocked
	}
	return nil
}

func (r *Router) enforceBudget(ctx context.Context, b Binding) error {
	if b.MonthlyBudgetUSD == nil {
		return nil
	}
	spend, err := r.monthSpend(ctx, b)
	if err != nil {
		return err
	}
	if spend >= *b.MonthlyBudgetUSD {
		return ErrBudgetExceeded
	}
	_ = r.alertIfNeeded(ctx, b, spend, 0)
	return nil
}

func (r *Router) monthSpend(ctx context.Context, b Binding) (float64, error) {
	start := r.now().UTC().Format("2006-01") + "-01 00:00:00"
	var spend sql.NullFloat64
	var err error
	if b.TenantID != nil {
		err = r.db.QueryRowContext(ctx, `
			SELECT COALESCE(SUM(estimated_cost_usd), 0)
			  FROM model_invocation
			 WHERE slot_key = ? AND tenant_id = ? AND created_at >= ?`,
			string(b.Slot), *b.TenantID, start).Scan(&spend)
	} else {
		err = r.db.QueryRowContext(ctx, `
			SELECT COALESCE(SUM(estimated_cost_usd), 0)
			  FROM model_invocation
			 WHERE slot_key = ? AND tenant_id IS NULL AND created_at >= ?`,
			string(b.Slot), start).Scan(&spend)
	}
	return spend.Float64, err
}

// alertIfNeeded checks whether baselineSpend (the month's spend NOT including
// the call currently being recorded) plus projectedDelta (that call's own
// cost) crosses the 80% budget threshold. Callers must compute baselineSpend
// before inserting the current call's invocation row, otherwise the just-
// inserted row would be double-counted alongside projectedDelta.
func (r *Router) alertIfNeeded(ctx context.Context, b Binding, baselineSpend, projectedDelta float64) error {
	if b.MonthlyBudgetUSD == nil || *b.MonthlyBudgetUSD <= 0 {
		return nil
	}
	spend := baselineSpend + projectedDelta
	if spend < *b.MonthlyBudgetUSD*0.8 {
		return nil
	}
	scope := "global"
	if b.TenantID != nil {
		scope = *b.TenantID
	}
	month := r.now().UTC().Format("2006-01")
	res, err := r.db.ExecContext(ctx, `
		INSERT OR IGNORE INTO model_budget_alert (scope_key, slot_key, budget_month, threshold_pct)
		VALUES (?, ?, ?, 80)`, scope, string(b.Slot), month)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 1 && r.alerts != nil {
		r.alerts.BudgetThreshold(ctx, b.Slot, b.TenantID, month, spend, *b.MonthlyBudgetUSD)
	}
	return nil
}

func (r *Router) recordInvocation(ctx context.Context, b Binding, attempt int, fallback bool, status string, usage Usage, latencyMS, ttftMS int, code, msg string) (string, error) {
	id := newID()
	isFallback := 0
	if fallback {
		isFallback = 1
	}
	var tenant any
	if b.TenantID != nil {
		tenant = *b.TenantID
	}
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO model_invocation
		    (id, slot_key, binding_id, tenant_id, provider, model_id, status, attempt_index, is_fallback,
		     prompt_tokens, completion_tokens, total_tokens, estimated_cost_usd, latency_ms, ttft_ms,
		     error_code, error_message, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, string(b.Slot), b.ID, tenant, b.provider(), b.ModelID, status, attempt, isFallback,
		nullInt(usage.PromptTokens), nullInt(usage.CompletionTokens), nullInt(usage.TotalTokens),
		usage.EstimatedCostUSD, nullInt(latencyMS), nullInt(ttftMS), nullString(code), nullString(msg),
		r.now().UTC().Format("2006-01-02 15:04:05"))
	return id, err
}

func (r *Router) ValidateBenchmarkGate(ctx context.Context, candidate BindingUpdate) error {
	var slot string
	var modelID, pins, prompt sql.NullString
	err := r.db.QueryRowContext(ctx, `
		SELECT slot_key, model_id, provider_pins, prompt_version_ref
		  FROM model_slot_binding
		 WHERE id = ?`, candidate.BindingID).Scan(&slot, &modelID, &pins, &prompt)
	if err != nil {
		return err
	}
	if candidate.Slot == "" || string(candidate.Slot) != slot {
		return ErrBenchmarkGate
	}
	if modelID.String == candidate.ModelID && pins.String == candidate.ProviderPinsJSON && prompt.String == candidate.PromptVersionRef {
		return nil
	}
	if candidate.VerdictRef == "" {
		return ErrBenchmarkGate
	}
	var n int
	err = r.db.QueryRowContext(ctx, `
		SELECT COUNT(*)
		  FROM model_benchmark_verdict
		 WHERE id = ? AND area = ? AND model_id = ? AND provider_pins = ? AND COALESCE(prompt_version_ref, '') = ?
		   AND status = 'pass'
		   AND (expires_at IS NULL OR expires_at > datetime('now'))`,
		candidate.VerdictRef, string(candidate.Slot), candidate.ModelID, candidate.ProviderPinsJSON, candidate.PromptVersionRef).Scan(&n)
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrBenchmarkGate
	}
	return nil
}

type Binding struct {
	ID                  string
	Slot                SlotKey
	TenantID            *string
	ModelID             string
	ProviderPins        ProviderPins
	Effort              string
	Params              map[string]any
	PrivacyTierRequired string
	MonthlyBudgetUSD    *float64
	FallbackChain       []FallbackBinding
	PromptVersionRef    string
}

type FallbackBinding struct {
	ModelID      string         `json:"model_id"`
	ProviderPins ProviderPins   `json:"provider_pins"`
	Effort       string         `json:"effort"`
	Params       map[string]any `json:"params"`
}

type attempt struct {
	Binding Binding
}

func (r *Router) resolveBinding(ctx context.Context, slot SlotKey, tenantID *string) (Binding, error) {
	if tenantID != nil {
		b, err := r.queryBinding(ctx, `
			SELECT id, slot_key, tenant_id, model_id, provider_pins, COALESCE(effort, ''), params,
			       privacy_tier_required, monthly_budget_usd, fallback_chain, COALESCE(prompt_version_ref, '')
			  FROM model_slot_binding
			 WHERE active = 1 AND tenant_id = ? AND slot_key = ?`, *tenantID, string(slot))
		if err == nil {
			return b, nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return Binding{}, err
		}
	}
	b, err := r.queryBinding(ctx, `
		SELECT id, slot_key, tenant_id, model_id, provider_pins, COALESCE(effort, ''), params,
		       privacy_tier_required, monthly_budget_usd, fallback_chain, COALESCE(prompt_version_ref, '')
		  FROM model_slot_binding
		 WHERE active = 1 AND tenant_id IS NULL AND slot_key = ?`, string(slot))
	if errors.Is(err, sql.ErrNoRows) {
		return Binding{}, ErrNoActiveBinding
	}
	return b, err
}

func (r *Router) queryBinding(ctx context.Context, q string, args ...any) (Binding, error) {
	var b Binding
	var tenant sql.NullString
	var pinsJSON, paramsJSON, fallbackJSON string
	var budget sql.NullFloat64
	err := r.db.QueryRowContext(ctx, q, args...).Scan(
		&b.ID, &b.Slot, &tenant, &b.ModelID, &pinsJSON, &b.Effort, &paramsJSON,
		&b.PrivacyTierRequired, &budget, &fallbackJSON, &b.PromptVersionRef)
	if err != nil {
		return Binding{}, err
	}
	if tenant.Valid {
		b.TenantID = &tenant.String
	}
	if budget.Valid {
		b.MonthlyBudgetUSD = &budget.Float64
	}
	if err := json.Unmarshal([]byte(pinsJSON), &b.ProviderPins); err != nil {
		return Binding{}, fmt.Errorf("decode provider_pins: %w", err)
	}
	if err := json.Unmarshal([]byte(paramsJSON), &b.Params); err != nil {
		return Binding{}, fmt.Errorf("decode params: %w", err)
	}
	if b.Params == nil {
		b.Params = map[string]any{}
	}
	if err := json.Unmarshal([]byte(fallbackJSON), &b.FallbackChain); err != nil {
		return Binding{}, fmt.Errorf("decode fallback_chain: %w", err)
	}
	return b, nil
}

func (b Binding) provider() string {
	if b.ProviderPins.RequiredProvider != "" {
		return b.ProviderPins.RequiredProvider
	}
	if len(b.ProviderPins.AllowedProviders) > 0 {
		return b.ProviderPins.AllowedProviders[0]
	}
	return ""
}

func (b Binding) fallbackAttempts() []attempt {
	out := make([]attempt, 0, len(b.FallbackChain))
	for _, fb := range b.FallbackChain {
		nb := b
		nb.ModelID = fb.ModelID
		nb.ProviderPins = fb.ProviderPins
		if fb.Effort != "" {
			nb.Effort = fb.Effort
		}
		if fb.Params != nil {
			nb.Params = fb.Params
		}
		out = append(out, attempt{Binding: nb})
	}
	return out
}

type ProviderError struct {
	Code       string
	StatusCode int
	Temporary  bool
	Message    string
}

func (e ProviderError) Error() string {
	if e.Message != "" {
		return e.Message
	}
	return e.Code
}

func fallbackEligible(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, ErrPrivacyTierBlocked) || errors.Is(err, ErrBudgetExceeded) || errors.Is(err, ErrInvalidRequest) {
		return false
	}
	if errors.Is(err, ErrNoUsableFallback) {
		return true
	}
	var pe ProviderError
	if errors.As(err, &pe) {
		if pe.StatusCode >= 500 {
			return true
		}
		if pe.Temporary && pe.StatusCode == 0 {
			return true
		}
	}
	return false
}

func emptyResponse(kind string, req Request, resp Response) bool {
	if _, ok := req.Input.(AgentCommand); ok {
		return false
	}
	if kind == "embed" {
		return len(resp.Embedding) == 0
	}
	return resp.Text == "" && len(resp.ToolCalls) == 0
}

func providerErrCode(err error) string {
	var pe ProviderError
	if errors.As(err, &pe) && pe.Code != "" {
		return pe.Code
	}
	return "provider_error"
}

func shortErr(err error) string {
	if err == nil {
		return ""
	}
	s := err.Error()
	if len(s) > 240 {
		return s[:240]
	}
	return s
}

func tierRank(t string) int {
	switch strings.ToLower(t) {
	case "public":
		return 0
	case "internal":
		return 1
	case "confidential":
		return 2
	case "restricted":
		return 3
	default:
		return 99
	}
}

func sinceMS(start, end time.Time) int {
	if end.Before(start) {
		return 0
	}
	return int(end.Sub(start).Milliseconds())
}

func nullInt(v int) any {
	if v == 0 {
		return nil
	}
	return v
}

func nullString(v string) any {
	if v == "" {
		return nil
	}
	return v
}

func newID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("llm-%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}
