package store_test

import (
	"context"
	"testing"

	"github.com/samhotchkiss/flowbee/internal/store"
)

func TestPendingMigrationsAndExistingSchemaAreReadOnly(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, t.TempDir()+"/flowbee.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	pending, err := store.PendingMigrations(ctx, st.DB)
	if err != nil || len(pending) == 0 {
		t.Fatalf("fresh pending=%d err=%v", len(pending), err)
	}
	if existing, err := store.HasUserSchema(ctx, st.DB); err != nil || existing {
		t.Fatalf("fresh existing=%t err=%v", existing, err)
	}
	if err := store.MigrateUp(ctx, st.DB); err != nil {
		t.Fatal(err)
	}
	if pending, err = store.PendingMigrations(ctx, st.DB); err != nil || len(pending) != 0 {
		t.Fatalf("migrated pending=%v err=%v", pending, err)
	}
	if existing, err := store.HasUserSchema(ctx, st.DB); err != nil || !existing {
		t.Fatalf("migrated existing=%t err=%v", existing, err)
	}
}
