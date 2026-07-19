package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/samhotchkiss/flowbee/internal/store"
	"github.com/samhotchkiss/flowbee/internal/testutil"
)

func TestWorkerAuthRuntimePostureIsDurableAndReplaceable(t *testing.T) {
	ctx := context.Background()
	st := testutil.NewStore(t)
	t0 := time.Date(2026, 7, 19, 20, 0, 0, 0, time.UTC)
	first := store.WorkerAuthRuntimePosture{Fingerprint: "sha256:first", PID: 41, UpdatedAt: t0}
	if err := st.RecordWorkerAuthRuntimePosture(ctx, first); err != nil {
		t.Fatal(err)
	}
	got, err := st.WorkerAuthRuntimePosture(ctx)
	if err != nil || got.Fingerprint != first.Fingerprint || got.PID != first.PID || !got.UpdatedAt.Equal(t0) {
		t.Fatalf("posture=%+v err=%v", got, err)
	}
	second := store.WorkerAuthRuntimePosture{Fingerprint: "sha256:second", PID: 42, UpdatedAt: t0.Add(time.Minute)}
	if err := st.RecordWorkerAuthRuntimePosture(ctx, second); err != nil {
		t.Fatal(err)
	}
	got, err = st.WorkerAuthRuntimePosture(ctx)
	if err != nil || got.Fingerprint != second.Fingerprint || got.PID != second.PID || !got.UpdatedAt.Equal(second.UpdatedAt) {
		t.Fatalf("replaced posture=%+v err=%v", got, err)
	}
}
