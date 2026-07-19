package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/samhotchkiss/flowbee/internal/testutil"
)

func TestDurableReconcilerTickRecoversPanicAndRunsAgain(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 19, 15, 0, 0, 0, time.UTC)
	reconcilers, err := beginDurableReconcilers(ctx, st, "serve-test", now, map[string]time.Duration{
		"test_loop": time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	err = reconcilers.tick(ctx, "test_loop", now.Add(time.Second), func() error {
		panic("poison input")
	})
	if err == nil || !strings.Contains(err.Error(), "panic recovered") {
		t.Fatalf("panic tick err=%v", err)
	}
	health, err := st.GetReconcilerHealth(ctx, "test_loop")
	if err != nil || health.State != "panicked" {
		t.Fatalf("panic health=%+v err=%v", health, err)
	}
	if err := reconcilers.tick(ctx, "test_loop", now.Add(2*time.Second), func() error { return nil }); err != nil {
		t.Fatalf("next tick: %v", err)
	}
	health, err = st.GetReconcilerHealth(ctx, "test_loop")
	if err != nil || health.State != "healthy" || health.ConsecutiveFailures != 0 {
		t.Fatalf("recovered health=%+v err=%v", health, err)
	}
}

func TestConversationDriverReconcilerHealthIsRegisteredAndAdvanced(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 19, 22, 0, 0, 0, time.UTC)
	reconcilers, err := beginDurableReconcilers(ctx, st, "serve-conversation-test", now,
		map[string]time.Duration{"conversation_driver": 30 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	health, err := st.GetReconcilerHealth(ctx, "conversation_driver")
	if err != nil || health.Owner != "serve-conversation-test" {
		t.Fatalf("registered health=%+v err=%v", health, err)
	}
	if err := reconcilers.tick(ctx, "conversation_driver", now.Add(time.Second), func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	health, err = st.GetReconcilerHealth(ctx, "conversation_driver")
	if err != nil || health.State != "healthy" || !health.LastSuccessAt.Equal(now.Add(time.Second)) {
		t.Fatalf("advanced health=%+v err=%v", health, err)
	}
}
