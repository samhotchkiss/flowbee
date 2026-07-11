package llm

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/samhotchkiss/flowbee/internal/testutil"
)

type fakeProvider struct {
	calls []ProviderRequest
	resps []Response
	errs  []error

	streamCalls   []ProviderRequest
	streamErrs    []error
	streamHandles []StreamHandle
}

func (f *fakeProvider) Call(_ context.Context, req ProviderRequest) (Response, error) {
	f.calls = append(f.calls, req)
	i := len(f.calls) - 1
	if i < len(f.errs) && f.errs[i] != nil {
		return Response{}, f.errs[i]
	}
	if i < len(f.resps) {
		return f.resps[i], nil
	}
	return Response{Text: "ok", Usage: Usage{EstimatedCostUSD: 0.01}}, nil
}

func (f *fakeProvider) Embed(ctx context.Context, req ProviderRequest) (Response, error) {
	return f.Call(ctx, req)
}

func (f *fakeProvider) Stream(_ context.Context, req ProviderRequest) (StreamHandle, error) {
	f.streamCalls = append(f.streamCalls, req)
	i := len(f.streamCalls) - 1
	if i < len(f.streamErrs) && f.streamErrs[i] != nil {
		return nil, f.streamErrs[i]
	}
	if i < len(f.streamHandles) {
		return f.streamHandles[i], nil
	}
	return &fakeStreamHandle{chunks: []Response{{Text: "ok", Usage: Usage{EstimatedCostUSD: 0.01}}}}, nil
}

// fakeStreamHandle replays a fixed sequence of chunks, then returns io.EOF
// (or a configured terminal error) on the following Recv call.
type fakeStreamHandle struct {
	chunks []Response
	err    error // terminal error; defaults to io.EOF
	idx    int
	closed bool
}

func (h *fakeStreamHandle) Recv() (Response, error) {
	if h.idx < len(h.chunks) {
		c := h.chunks[h.idx]
		h.idx++
		return c, nil
	}
	if h.err != nil {
		return Response{}, h.err
	}
	return Response{}, io.EOF
}

func (h *fakeStreamHandle) Close() error {
	h.closed = true
	return nil
}

// fakeClock gives tests deterministic control over Router.now() so TTFT and
// latency measurements can be asserted precisely.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{t: time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

type alerts struct {
	n int
}

func (a *alerts) BudgetThreshold(context.Context, SlotKey, *string, string, float64, float64) {
	a.n++
}

func TestBindingResolutionPrefersTenantOverride(t *testing.T) {
	ctx := context.Background()
	st := testutil.NewStore(t)
	seedTestBinding(t, st.DB, SlotClassificationLight)
	tenant := "tenant-a"
	_, err := st.DB.ExecContext(ctx, `
		INSERT INTO model_endpoint_policy
			(model_id, provider, privacy_tier_supported, data_retention_policy_ref)
		VALUES ('tenant-model', 'anthropic', 'confidential', 'test')`)
	if err != nil {
		t.Fatal(err)
	}
	_, err = st.DB.ExecContext(ctx, `
		INSERT INTO model_slot_binding
			(id, slot_key, tenant_id, model_id, provider_pins, effort, params, privacy_tier_required, updated_by)
		VALUES
			('tenant-binding', 'classification-light', ?, 'tenant-model',
			 '{"allowed_providers":["anthropic"],"required_provider":"anthropic","allow_provider_routing":false}',
			 'none', '{}', 'internal', 'test')`, tenant)
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeProvider{}
	r := NewRouter(st.DB, withProviderForTest("anthropic", f))
	resp, err := r.Call(ctx, SlotClassificationLight, Request{TenantID: &tenant, Prompt: "classify"})
	if err != nil {
		t.Fatal(err)
	}
	if resp.ModelID != "tenant-model" || resp.BindingID != "tenant-binding" {
		t.Fatalf("resolved model/binding = %q/%q, want tenant override", resp.ModelID, resp.BindingID)
	}
}

func TestMissingBindingFailsClosed(t *testing.T) {
	ctx := context.Background()
	st := testutil.NewStore(t)
	_, err := st.DB.ExecContext(ctx, `UPDATE model_slot_binding SET active = 0 WHERE slot_key = 'judge'`)
	if err != nil {
		t.Fatal(err)
	}
	r := NewRouter(st.DB, withProviderForTest("anthropic", &fakeProvider{}))
	_, err = r.Call(ctx, SlotJudge, Request{Prompt: "judge"})
	if !errors.Is(err, ErrNoActiveBinding) {
		t.Fatalf("err = %v, want ErrNoActiveBinding", err)
	}
}

func TestPrivacyBlockWritesLedgerAndDoesNotContactProvider(t *testing.T) {
	ctx := context.Background()
	st := testutil.NewStore(t)
	seedTestBinding(t, st.DB, SlotClassificationLight)
	_, err := st.DB.ExecContext(ctx, `
		UPDATE model_slot_binding
		   SET privacy_tier_required = 'restricted'
		 WHERE slot_key = 'classification-light'`)
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeProvider{}
	r := NewRouter(st.DB, withProviderForTest("anthropic", f))
	_, err = r.Call(ctx, SlotClassificationLight, Request{Prompt: "classify"})
	if !errors.Is(err, ErrPrivacyTierBlocked) {
		t.Fatalf("err = %v, want privacy block", err)
	}
	if len(f.calls) != 0 {
		t.Fatalf("provider calls = %d, want 0", len(f.calls))
	}
	assertLedgerStatus(t, st.DB, "classification-light", "privacy_blocked", 0, 0)
}

func TestBudgetBlockWritesLedgerAndDoesNotContactProvider(t *testing.T) {
	ctx := context.Background()
	st := testutil.NewStore(t)
	seedTestBinding(t, st.DB, SlotClassificationLight)
	_, err := st.DB.ExecContext(ctx, `
		UPDATE model_slot_binding
		   SET monthly_budget_usd = 1.00
		 WHERE slot_key = 'classification-light'`)
	if err != nil {
		t.Fatal(err)
	}
	_, err = st.DB.ExecContext(ctx, `
		INSERT INTO model_invocation
			(id, slot_key, binding_id, provider, model_id, status, estimated_cost_usd, created_at)
		VALUES ('spent', 'classification-light', 'test-classification-light',
		        'anthropic', 'claude-sonnet-5', 'success', 1.00, ?)`,
		time.Now().UTC().Format("2006-01-02 15:04:05"))
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeProvider{}
	r := NewRouter(st.DB, withProviderForTest("anthropic", f))
	_, err = r.Call(ctx, SlotClassificationLight, Request{Prompt: "classify"})
	if !errors.Is(err, ErrBudgetExceeded) {
		t.Fatalf("err = %v, want budget exceeded", err)
	}
	if len(f.calls) != 0 {
		t.Fatalf("provider calls = %d, want 0", len(f.calls))
	}
	assertLedgerStatus(t, st.DB, "classification-light", "budget_blocked", 0, 0)
}

func TestBudgetAlertEmitsOnce(t *testing.T) {
	ctx := context.Background()
	st := testutil.NewStore(t)
	seedTestBinding(t, st.DB, SlotClassificationLight)
	_, err := st.DB.ExecContext(ctx, `
		UPDATE model_slot_binding
		   SET monthly_budget_usd = 1.00
		 WHERE slot_key = 'classification-light'`)
	if err != nil {
		t.Fatal(err)
	}
	_, err = st.DB.ExecContext(ctx, `
		INSERT INTO model_invocation
			(id, slot_key, binding_id, provider, model_id, status, estimated_cost_usd, created_at)
		VALUES ('spent80', 'classification-light', 'test-classification-light',
		        'anthropic', 'claude-sonnet-5', 'success', 0.80, ?)`,
		time.Now().UTC().Format("2006-01-02 15:04:05"))
	if err != nil {
		t.Fatal(err)
	}
	a := &alerts{}
	r := NewRouter(st.DB, withProviderForTest("anthropic", &fakeProvider{}), WithAlertSink(a))
	for i := 0; i < 2; i++ {
		if _, err := r.Call(ctx, SlotClassificationLight, Request{Prompt: "classify"}); err != nil {
			t.Fatal(err)
		}
	}
	if a.n != 1 {
		t.Fatalf("alerts = %d, want 1", a.n)
	}
}

// TestAlertDoesNotDoubleCountCurrentCall pins down the alertIfNeeded
// double-counting bug: the pre-call baseline is 0.70 against a $1.00 budget,
// and this call's own cost is 0.06, for a true total of 0.76 -- below the 80%
// ($0.80) threshold. A buggy implementation that recomputes monthSpend()
// after recordInvocation has already inserted this call's row would see
// 0.76 (baseline+this call) + 0.06 (delta again) = 0.82, incorrectly
// crossing the threshold.
func TestAlertDoesNotDoubleCountCurrentCall(t *testing.T) {
	ctx := context.Background()
	st := testutil.NewStore(t)
	seedTestBinding(t, st.DB, SlotClassificationLight)
	_, err := st.DB.ExecContext(ctx, `
		UPDATE model_slot_binding
		   SET monthly_budget_usd = 1.00
		 WHERE slot_key = 'classification-light'`)
	if err != nil {
		t.Fatal(err)
	}
	_, err = st.DB.ExecContext(ctx, `
		INSERT INTO model_invocation
			(id, slot_key, binding_id, provider, model_id, status, estimated_cost_usd, created_at)
		VALUES ('spent70', 'classification-light', 'test-classification-light',
		        'anthropic', 'claude-sonnet-5', 'success', 0.70, ?)`,
		time.Now().UTC().Format("2006-01-02 15:04:05"))
	if err != nil {
		t.Fatal(err)
	}
	a := &alerts{}
	f := &fakeProvider{resps: []Response{{Text: "ok", Usage: Usage{EstimatedCostUSD: 0.06}}}}
	r := NewRouter(st.DB, withProviderForTest("anthropic", f), WithAlertSink(a))
	if _, err := r.Call(ctx, SlotClassificationLight, Request{Prompt: "classify"}); err != nil {
		t.Fatal(err)
	}
	if a.n != 0 {
		t.Fatalf("alerts = %d, want 0 (true spend 0.76 stays under the 0.80 threshold)", a.n)
	}
}

func TestStreamFallbackOnProviderOpenErrorWritesFallbackLedger(t *testing.T) {
	ctx := context.Background()
	st := testutil.NewStore(t)
	seedTestBinding(t, st.DB, SlotChat)
	_, err := st.DB.ExecContext(ctx, `
		UPDATE model_slot_binding
		   SET fallback_chain = '[{"model_id":"claude-opus-4-6","provider_pins":{"allowed_providers":["anthropic"],"required_provider":"anthropic","allow_provider_routing":false},"effort":"none","params":{}}]'
		 WHERE slot_key = 'chat'`)
	if err != nil {
		t.Fatal(err)
	}
	fallbackHandle := &fakeStreamHandle{chunks: []Response{{Text: "fallback ok", Usage: Usage{EstimatedCostUSD: 0.02}}}}
	f := &fakeProvider{
		streamErrs:    []error{ProviderError{Code: "upstream_500", StatusCode: 500}},
		streamHandles: []StreamHandle{nil, fallbackHandle},
	}
	r := NewRouter(st.DB, withProviderForTest("anthropic", f))
	h, err := r.Stream(ctx, SlotChat, Request{Prompt: "hi"})
	if err != nil {
		t.Fatal(err)
	}
	resp, err := h.Recv()
	if err != nil {
		t.Fatal(err)
	}
	if resp.ModelID != "claude-opus-4-6" || !resp.FallbackAttempted {
		t.Fatalf("fallback chunk = %+v", resp)
	}
	if _, err := h.Recv(); !errors.Is(err, io.EOF) {
		t.Fatalf("final Recv err = %v, want io.EOF", err)
	}
	if len(f.streamCalls) != 2 {
		t.Fatalf("stream open attempts = %d, want 2", len(f.streamCalls))
	}
	assertLedgerStatus(t, st.DB, "chat", "failed", 0, 0)
	assertLedgerStatus(t, st.DB, "chat", "success", 1, 1)
}

func TestStreamRecordsRealTTFTAndUsageOnSuccess(t *testing.T) {
	ctx := context.Background()
	st := testutil.NewStore(t)
	seedTestBinding(t, st.DB, SlotChat)
	clock := newFakeClock()
	handle := &fakeStreamHandle{chunks: []Response{
		{Text: "partial"},
		{Text: "partial more", Usage: Usage{PromptTokens: 10, CompletionTokens: 20, TotalTokens: 30, EstimatedCostUSD: 0.03}},
	}}
	f := &fakeProvider{streamHandles: []StreamHandle{handle}}
	r := NewRouter(st.DB, withProviderForTest("anthropic", f), WithClock(clock.now))
	h, err := r.Stream(ctx, SlotChat, Request{Prompt: "hi"})
	if err != nil {
		t.Fatal(err)
	}
	clock.advance(150 * time.Millisecond) // time to first chunk
	if _, err := h.Recv(); err != nil {
		t.Fatal(err)
	}
	clock.advance(350 * time.Millisecond) // time to second chunk
	if _, err := h.Recv(); err != nil {
		t.Fatal(err)
	}
	clock.advance(10 * time.Millisecond) // time to EOF
	if _, err := h.Recv(); !errors.Is(err, io.EOF) {
		t.Fatalf("final Recv err = %v, want io.EOF", err)
	}

	var ttft, latency, promptTok, completionTok, totalTok int
	var cost float64
	err = st.DB.QueryRow(`
		SELECT ttft_ms, latency_ms, prompt_tokens, completion_tokens, total_tokens, estimated_cost_usd
		  FROM model_invocation
		 WHERE slot_key = 'chat' AND status = 'success'`).
		Scan(&ttft, &latency, &promptTok, &completionTok, &totalTok, &cost)
	if err != nil {
		t.Fatal(err)
	}
	if ttft != 150 {
		t.Fatalf("ttft_ms = %d, want 150", ttft)
	}
	if latency != 510 {
		t.Fatalf("latency_ms = %d, want 510", latency)
	}
	if promptTok != 10 || completionTok != 20 || totalTok != 30 || cost != 0.03 {
		t.Fatalf("usage = prompt=%d completion=%d total=%d cost=%v, want 10/20/30/0.03", promptTok, completionTok, totalTok, cost)
	}
}

func TestStreamRecordsFailureOnProviderStreamError(t *testing.T) {
	ctx := context.Background()
	st := testutil.NewStore(t)
	seedTestBinding(t, st.DB, SlotChat)
	handle := &fakeStreamHandle{
		chunks: []Response{{Text: "partial"}},
		err:    ProviderError{Code: "stream_dropped", Message: "connection reset"},
	}
	f := &fakeProvider{streamHandles: []StreamHandle{handle}}
	r := NewRouter(st.DB, withProviderForTest("anthropic", f))
	h, err := r.Stream(ctx, SlotChat, Request{Prompt: "hi"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.Recv(); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Recv(); err == nil {
		t.Fatal("expected provider stream error")
	}
	assertLedgerStatus(t, st.DB, "chat", "failed", 0, 0)
}

func TestFallbackOnProvider5xxWritesFallbackLedger(t *testing.T) {
	ctx := context.Background()
	st := testutil.NewStore(t)
	seedTestBinding(t, st.DB, SlotClassificationLight)
	_, err := st.DB.ExecContext(ctx, `
		UPDATE model_slot_binding
		   SET fallback_chain = '[{"model_id":"claude-opus-4-6","provider_pins":{"allowed_providers":["anthropic"],"required_provider":"anthropic","allow_provider_routing":false},"effort":"none","params":{}}]'
		 WHERE slot_key = 'classification-light'`)
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeProvider{
		errs: []error{ProviderError{Code: "upstream_500", StatusCode: 500}},
		resps: []Response{
			{},
			{Text: "fallback ok", Usage: Usage{EstimatedCostUSD: 0.02}},
		},
	}
	r := NewRouter(st.DB, withProviderForTest("anthropic", f))
	resp, err := r.Call(ctx, SlotClassificationLight, Request{Prompt: "classify"})
	if err != nil {
		t.Fatal(err)
	}
	if !resp.FallbackAttempted || resp.ModelID != "claude-opus-4-6" {
		t.Fatalf("fallback response = %+v", resp)
	}
	assertLedgerStatus(t, st.DB, "classification-light", "failed", 0, 0)
	assertLedgerStatus(t, st.DB, "classification-light", "success", 1, 1)
}

func TestFallbackOnEmptyResponse(t *testing.T) {
	ctx := context.Background()
	st := testutil.NewStore(t)
	seedTestBinding(t, st.DB, SlotClassificationLight)
	_, err := st.DB.ExecContext(ctx, `
		UPDATE model_slot_binding
		   SET fallback_chain = '[{"model_id":"claude-opus-4-6","provider_pins":{"allowed_providers":["anthropic"],"required_provider":"anthropic","allow_provider_routing":false},"effort":"none","params":{}}]'
		 WHERE slot_key = 'classification-light'`)
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeProvider{
		resps: []Response{
			{},
			{Text: "fallback ok"},
		},
	}
	r := NewRouter(st.DB, withProviderForTest("anthropic", f))
	resp, err := r.Call(ctx, SlotClassificationLight, Request{Prompt: "classify"})
	if err != nil {
		t.Fatal(err)
	}
	if !resp.FallbackAttempted {
		t.Fatalf("expected fallback response, got %+v", resp)
	}
	assertLedgerStatus(t, st.DB, "classification-light", "empty", 0, 0)
	assertLedgerStatus(t, st.DB, "classification-light", "success", 1, 1)
}

func TestNoFallbackOnInvalidRequest(t *testing.T) {
	ctx := context.Background()
	st := testutil.NewStore(t)
	seedTestBinding(t, st.DB, SlotClassificationLight)
	f := &fakeProvider{}
	r := NewRouter(st.DB, withProviderForTest("anthropic", f))
	_, err := r.Call(ctx, SlotClassificationLight, Request{})
	if !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("err = %v, want invalid request", err)
	}
	if len(f.calls) != 0 {
		t.Fatalf("provider calls = %d, want 0", len(f.calls))
	}
}

func TestChatRejectsReasoningEffort(t *testing.T) {
	ctx := context.Background()
	st := testutil.NewStore(t)
	seedTestBinding(t, st.DB, SlotChat)
	_, err := st.DB.ExecContext(ctx, `UPDATE model_slot_binding SET effort = 'low' WHERE slot_key = 'chat'`)
	if err != nil {
		t.Fatal(err)
	}
	r := NewRouter(st.DB, withProviderForTest("anthropic", &fakeProvider{}))
	_, err = r.Call(ctx, SlotChat, Request{Prompt: "hi"})
	if !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("err = %v, want invalid request", err)
	}
}

func TestParamNormalizationPassesBindingParamsAndEffort(t *testing.T) {
	ctx := context.Background()
	st := testutil.NewStore(t)
	seedTestBinding(t, st.DB, SlotClassificationLight)
	_, err := st.DB.ExecContext(ctx, `
		UPDATE model_slot_binding
		   SET effort = 'low', params = '{"max_tokens":128,"temperature":0.2}'
		 WHERE slot_key = 'classification-light'`)
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeProvider{}
	r := NewRouter(st.DB, withProviderForTest("anthropic", f))
	if _, err := r.Call(ctx, SlotClassificationLight, Request{Prompt: "classify"}); err != nil {
		t.Fatal(err)
	}
	if got := f.calls[0].Effort; got != "low" {
		t.Fatalf("effort = %q, want low", got)
	}
	if got := f.calls[0].Params["max_tokens"]; got != float64(128) {
		t.Fatalf("max_tokens = %#v, want 128", got)
	}
}

func TestBenchmarkGateRequiresPassingVerdictOnTupleChange(t *testing.T) {
	ctx := context.Background()
	st := testutil.NewStore(t)
	seedTestBinding(t, st.DB, SlotClassificationLight)
	r := NewRouter(st.DB)
	pins := `{"allowed_providers":["anthropic"],"required_provider":"anthropic","allow_provider_routing":false}`
	candidate := BindingUpdate{
		BindingID:        "test-classification-light",
		Slot:             SlotClassificationLight,
		ModelID:          "claude-opus-4-6",
		ProviderPinsJSON: pins,
		VerdictRef:       "verdict-1",
	}
	if err := r.ValidateBenchmarkGate(ctx, candidate); !errors.Is(err, ErrBenchmarkGate) {
		t.Fatalf("err = %v, want benchmark gate", err)
	}
	_, err := st.DB.ExecContext(ctx, `
		INSERT INTO model_benchmark_verdict
			(id, area, model_id, provider_pins, prompt_version_ref, status)
		VALUES ('verdict-1', 'classification-light', 'claude-opus-4-6', ?, NULL, 'pass')`, pins)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.ValidateBenchmarkGate(ctx, candidate); err != nil {
		t.Fatalf("gate with passing verdict: %v", err)
	}
}

func TestBenchmarkGateRejectsVerdictForDifferentSlot(t *testing.T) {
	ctx := context.Background()
	st := testutil.NewStore(t)
	seedTestBinding(t, st.DB, SlotClassificationLight)
	r := NewRouter(st.DB)
	pins := `{"allowed_providers":["anthropic"],"required_provider":"anthropic","allow_provider_routing":false}`
	_, err := st.DB.ExecContext(ctx, `
		INSERT INTO model_benchmark_verdict
			(id, area, model_id, provider_pins, prompt_version_ref, status)
		VALUES ('wrong-area', 'judge', 'claude-opus-4-6', ?, NULL, 'pass')`, pins)
	if err != nil {
		t.Fatal(err)
	}
	candidate := BindingUpdate{
		BindingID:        "test-classification-light",
		Slot:             SlotClassificationLight,
		ModelID:          "claude-opus-4-6",
		ProviderPinsJSON: pins,
		VerdictRef:       "wrong-area",
	}
	if err := r.ValidateBenchmarkGate(ctx, candidate); !errors.Is(err, ErrBenchmarkGate) {
		t.Fatalf("err = %v, want benchmark gate for cross-slot verdict", err)
	}
}

func TestBenchmarkGateRejectsCandidateSlotMismatch(t *testing.T) {
	ctx := context.Background()
	st := testutil.NewStore(t)
	seedTestBinding(t, st.DB, SlotClassificationLight)
	r := NewRouter(st.DB)
	pins := `{"allowed_providers":["anthropic"],"required_provider":"anthropic","allow_provider_routing":false}`
	candidate := BindingUpdate{
		BindingID:        "test-classification-light",
		Slot:             SlotJudge,
		ModelID:          "claude-sonnet-5",
		ProviderPinsJSON: pins,
	}
	if err := r.ValidateBenchmarkGate(ctx, candidate); !errors.Is(err, ErrBenchmarkGate) {
		t.Fatalf("err = %v, want benchmark gate for slot mismatch", err)
	}
}

func assertLedgerStatus(t *testing.T, db *sql.DB, slot, status string, attempt, fallback int) {
	t.Helper()
	var n int
	err := db.QueryRow(`
		SELECT COUNT(*)
		  FROM model_invocation
		 WHERE slot_key = ? AND status = ? AND attempt_index = ? AND is_fallback = ?`,
		slot, status, attempt, fallback).Scan(&n)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("ledger rows for %s/%s attempt %d fallback %d = %d, want 1", slot, status, attempt, fallback, n)
	}
}

func seedTestBinding(t *testing.T, db *sql.DB, slot SlotKey) {
	t.Helper()
	model := "claude-sonnet-5"
	effort := "none"
	if slot == SlotJudge {
		model = "claude-opus-4-6"
	}
	_, err := db.Exec(`
		INSERT OR IGNORE INTO model_slot_binding
			(id, slot_key, tenant_id, model_id, provider_pins, effort, params, privacy_tier_required,
			 monthly_budget_usd, fallback_chain, prompt_version_ref, active, updated_by, benchmark_verdict_ref)
		VALUES
			(?, ?, NULL, ?,
			 '{"allowed_providers":["anthropic"],"required_provider":"anthropic","allow_provider_routing":false}',
			 ?, '{}', 'internal', NULL, '[]', NULL, 1, 'test', NULL)`,
		"test-"+string(slot), string(slot), model, effort)
	if err != nil {
		t.Fatal(err)
	}
}
