package llm

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	"github.com/samhotchkiss/flowbee/internal/store"
)

func TestEnsureDefaultAgentRouterUsesConfiguredPersistentDB(t *testing.T) {
	ctx := context.Background()
	resetDefaultRouterForTest(t)

	dbPath := filepath.Join(t.TempDir(), "flowbee.db")
	t.Setenv("FLOWBEE_DATABASE_URL", dbPath)

	st, err := store.Open(ctx, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.MigrateUp(ctx, st.DB); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB.ExecContext(ctx, `
		UPDATE model_slot_binding
		   SET model_id = 'opus'
		 WHERE slot_key = 'drafting-complex'`); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	if err := EnsureDefaultAgentRouter(ctx); err != nil {
		t.Fatal(err)
	}
	b, err := getDefaultRouter().resolveBinding(ctx, SlotDraftingComplex, nil)
	if err != nil {
		t.Fatal(err)
	}
	if b.ModelID != "opus" {
		t.Fatalf("default router model = %q, want persistent DB update", b.ModelID)
	}
}

func TestEnsureDefaultAgentRouterRejectsEphemeralDB(t *testing.T) {
	ctx := context.Background()
	resetDefaultRouterForTest(t)

	t.Setenv("FLOWBEE_DATABASE_URL", "file:flowbee-llm-router?mode=memory&cache=shared")

	err := EnsureDefaultAgentRouter(ctx)
	if err == nil {
		t.Fatal("EnsureDefaultAgentRouter succeeded with an in-memory DB")
	}
	if !strings.Contains(err.Error(), "not persistent") {
		t.Fatalf("error = %v, want persistent database guidance", err)
	}
}

func TestAgentCommandResolvesUpdatedPersistentBinding(t *testing.T) {
	ctx := context.Background()
	resetDefaultRouterForTest(t)

	dbPath := filepath.Join(t.TempDir(), "flowbee.db")
	t.Setenv("FLOWBEE_DATABASE_URL", dbPath)

	st, err := store.Open(ctx, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.MigrateUp(ctx, st.DB); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB.ExecContext(ctx, `
		UPDATE model_slot_binding
		   SET model_id = 'operator-swapped-model'
		 WHERE slot_key = 'drafting-complex'`); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB.ExecContext(ctx, `
		INSERT INTO model_endpoint_policy
			(model_id, provider, privacy_tier_supported, data_retention_policy_ref)
		VALUES ('operator-swapped-model', 'anthropic', 'confidential', 'test')`); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	var got ProviderRequest
	SetDefaultRouter(NewRouter(mustOpenMigratedDB(t, ctx, dbPath),
		withProviderForTest("anthropic", providerFunc(func(_ context.Context, req ProviderRequest) (Response, error) {
			got = req
			return Response{Text: "ok"}, nil
		}))))

	_, err = Call(ctx, SlotDraftingComplex, Request{
		Prompt: "run agent command",
		Input: AgentCommand{
			Command: "claude -p hi --model sonnet",
			Dir:     t.TempDir(),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.ModelID != "operator-swapped-model" {
		t.Fatalf("provider model = %q, want model_slot_binding update", got.ModelID)
	}
}

type providerFunc func(context.Context, ProviderRequest) (Response, error)

func (f providerFunc) Call(ctx context.Context, req ProviderRequest) (Response, error) {
	return f(ctx, req)
}

func (f providerFunc) Embed(ctx context.Context, req ProviderRequest) (Response, error) {
	return f(ctx, req)
}

func (f providerFunc) Stream(context.Context, ProviderRequest) (StreamHandle, error) {
	return nil, ErrInvalidRequest
}

func mustOpenMigratedDB(t *testing.T, ctx context.Context, dbPath string) *sql.DB {
	t.Helper()
	st, err := store.Open(ctx, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.MigrateUp(ctx, st.DB); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st.DB
}

func resetDefaultRouterForTest(t *testing.T) {
	t.Helper()
	SetDefaultRouter(nil)
	bootstrapDefault.mu.Lock()
	if bootstrapDefault.st != nil {
		_ = bootstrapDefault.st.Close()
		bootstrapDefault.st = nil
	}
	bootstrapDefault.mu.Unlock()
	t.Cleanup(func() {
		SetDefaultRouter(nil)
		bootstrapDefault.mu.Lock()
		if bootstrapDefault.st != nil {
			_ = bootstrapDefault.st.Close()
			bootstrapDefault.st = nil
		}
		bootstrapDefault.mu.Unlock()
	})
}
