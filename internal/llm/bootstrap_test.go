package llm

import (
	"context"
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
