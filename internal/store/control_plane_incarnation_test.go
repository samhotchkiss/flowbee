package store_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/samhotchkiss/flowbee/internal/store"
)

const testControlPosture = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
const (
	testIncarnationFirst    = "cpi-11111111111111111111111111111111"
	testIncarnationSecond   = "cpi-22222222222222222222222222222222"
	testIncarnationGraceful = "cpi-33333333333333333333333333333333"
	testIncarnationEarly    = "cpi-44444444444444444444444444444444"
)

func TestControlPlaneIncarnationCannotPublishBeforeMigrations(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "pre-migration.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.AcquireWriterLock(); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 20, 1, 0, 0, 0, time.UTC)
	in := store.StartControlPlaneIncarnationInput{ID: testIncarnationEarly, Version: "v2",
		ConfigPostureSHA256: testControlPosture, ProcessID: 99, StartedAt: now}
	if _, err := st.StartControlPlaneIncarnation(ctx, in); err == nil {
		t.Fatal("control-plane authority published before migrations created its durable ledger")
	}
	if err := store.MigrateUp(ctx, st.DB); err != nil {
		t.Fatal(err)
	}
	if _, err := st.StartControlPlaneIncarnation(ctx, in); err != nil {
		t.Fatalf("post-migration incarnation: %v", err)
	}
}

func TestControlPlaneIncarnationRequiresWriterLockAndRecoversKilledOwner(t *testing.T) {
	ctx := context.Background()
	dsn := filepath.Join(t.TempDir(), "incarnation.db")
	first, err := store.Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.MigrateUp(ctx, first.DB); err != nil {
		t.Fatal(err)
	}
	started := time.Date(2026, 7, 20, 2, 0, 0, 0, time.UTC)
	input1 := store.StartControlPlaneIncarnationInput{ID: testIncarnationFirst, Version: "v2.0.0",
		SourceCommit: "commit-one", ConfigPostureSHA256: testControlPosture,
		ProcessID: 101, StartedAt: started}
	if _, err := first.StartControlPlaneIncarnation(ctx, input1); !errors.Is(err, store.ErrControlPlaneWriterLockRequired) {
		t.Fatalf("incarnation without writer lock err=%v", err)
	}
	if err := first.AcquireWriterLock(); err != nil {
		t.Fatal(err)
	}
	created, err := first.StartControlPlaneIncarnation(ctx, input1)
	if err != nil || created.IdempotentReplay || len(created.RecoveredPriorIDs) != 0 {
		t.Fatalf("first incarnation=%+v err=%v", created, err)
	}
	replay, err := first.StartControlPlaneIncarnation(ctx, input1)
	if err != nil || !replay.IdempotentReplay {
		t.Fatalf("lost-ack replay=%+v err=%v", replay, err)
	}

	// A replacement may open SQLite for reads, but it cannot establish process
	// authority while the original OS writer fence is live.
	contender, err := store.Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	if err := contender.AcquireWriterLock(); err == nil {
		t.Fatal("two control-plane processes acquired the writer lock")
	}
	_ = contender.Close()

	// Simulate SIGKILL: close releases the kernel lock but intentionally records no
	// graceful stop. The next lock owner must recover the stale active row.
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	replacement, err := store.Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer replacement.Close()
	if err := replacement.AcquireWriterLock(); err != nil {
		t.Fatal(err)
	}
	input2 := store.StartControlPlaneIncarnationInput{ID: testIncarnationSecond, Version: "v2.0.1",
		SourceCommit: "commit-two", ConfigPostureSHA256: testControlPosture,
		ProcessID: 202, StartedAt: started.Add(time.Minute)}
	recovered, err := replacement.StartControlPlaneIncarnation(ctx, input2)
	if err != nil || len(recovered.RecoveredPriorIDs) != 1 || recovered.RecoveredPriorIDs[0] != input1.ID {
		t.Fatalf("replacement=%+v err=%v", recovered, err)
	}
	current, err := replacement.CurrentControlPlaneIncarnation(ctx)
	if err != nil || current.ID != input2.ID || current.State != "active" ||
		current.Version != input2.Version || current.SourceCommit != input2.SourceCommit ||
		current.ConfigPostureSHA256 != input2.ConfigPostureSHA256 {
		t.Fatalf("current replacement=%+v err=%v", current, err)
	}
	rows, err := replacement.ListControlPlaneIncarnations(ctx)
	if err != nil || len(rows) != 2 {
		t.Fatalf("incarnation ledger=%+v err=%v", rows, err)
	}
	if rows[0].State != "superseded" || rows[0].SupersededBy != input2.ID ||
		rows[0].StopReason != "unclean_restart" || rows[1].State != "active" {
		t.Fatalf("recovery ledger=%+v", rows)
	}
	var active int
	if err := replacement.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM control_plane_incarnations
		WHERE state='active'`).Scan(&active); err != nil || active != 1 {
		t.Fatalf("active incarnations=%d err=%v", active, err)
	}
	events, err := replacement.ControlPlaneIncarnationEvents(ctx)
	if err != nil || len(events) != 3 || events[0].Kind != "started" ||
		events[1].Kind != "superseded" || events[2].Kind != "started" {
		t.Fatalf("restart audit=%+v err=%v", events, err)
	}
}

func TestControlPlaneIncarnationGracefulStopIsDurableAndIdempotent(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := store.MigrateUp(ctx, st.DB); err != nil {
		t.Fatal(err)
	}
	if err := st.AcquireWriterLock(); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 20, 3, 0, 0, 0, time.UTC)
	in := store.StartControlPlaneIncarnationInput{ID: testIncarnationGraceful, Version: "v2",
		ConfigPostureSHA256: testControlPosture, ProcessID: 303, StartedAt: now}
	if _, err := st.StartControlPlaneIncarnation(ctx, in); err != nil {
		t.Fatal(err)
	}
	if err := st.StopControlPlaneIncarnation(ctx, in.ID, "graceful_shutdown", now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := st.StopControlPlaneIncarnation(ctx, in.ID, "graceful_shutdown", now.Add(2*time.Minute)); err != nil {
		t.Fatalf("exact stop replay: %v", err)
	}
	current, err := st.CurrentControlPlaneIncarnation(ctx)
	if err != nil || current.State != "stopped" || current.StopReason != "graceful_shutdown" ||
		!current.StoppedAt.Equal(now.Add(time.Minute)) {
		t.Fatalf("graceful current=%+v err=%v", current, err)
	}
	events, err := st.ControlPlaneIncarnationEvents(ctx)
	if err != nil || len(events) != 2 || events[1].Kind != "stopped" {
		t.Fatalf("graceful audit=%+v err=%v", events, err)
	}
	if _, err := st.DB.ExecContext(ctx, `UPDATE control_plane_incarnation_events
		SET reason='tampered' WHERE seq=?`, events[0].Seq); err == nil {
		t.Fatal("append-only incarnation audit event was mutable")
	}
	if _, err := st.DB.ExecContext(ctx, `UPDATE control_plane_incarnations
		SET version='tampered' WHERE incarnation_id=?`, in.ID); err == nil {
		t.Fatal("pinned incarnation identity was mutable")
	}
}
