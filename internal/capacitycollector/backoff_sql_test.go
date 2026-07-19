package capacitycollector

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/samhotchkiss/flowbee/internal/store"
)

func TestSQLBackoffSurvivesStoreRestartAndHonorsRetryAt(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "flowbee.db")
	st, err := store.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.MigrateUp(ctx, st.DB); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 19, 16, 0, 0, 0, time.UTC)
	retry := now.Add(4 * time.Minute)
	b := SQLBackoffStore{DB: st.DB}
	state, err := b.Failure(ctx, "provider", "codex", "throttled", now, retry, time.Second, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if !state.RetryAt.Equal(retry) || state.Failures != 1 {
		t.Fatalf("state=%+v", state)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	st, err = store.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	b = SQLBackoffStore{DB: st.DB}
	state, err = b.Get(ctx, "provider", "codex")
	if err != nil {
		t.Fatal(err)
	}
	if !state.RetryAt.Equal(retry) || state.Reason != "throttled" {
		t.Fatalf("restart lost backoff: %+v", state)
	}
}
