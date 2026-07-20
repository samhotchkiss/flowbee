package bootstrap

import (
	"context"
	"path/filepath"
	"testing"
)

func TestSQLiteCheckpointResumeAfterCloseDoesNotRepeatIssuedEffect(t *testing.T) {
	ctx := context.Background()
	dsn := filepath.Join(t.TempDir(), "flowbee.db")
	first, firstStore, err := OpenSQLiteCheckpointStore(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	fake := NewFakePort("russ")
	fake.AutoLiveActivation = true
	fake.DelayReady["actor:russ-orchestrator"] = true
	result, err := newBootstrap(fake, firstStore).Run(ctx, bootstrapPlan())
	if err != nil || result.Hold != "actor:russ-orchestrator:awaiting_live_fact" {
		t.Fatalf("first Run() = %+v, %v", result, err)
	}
	if got := fake.MutationCount["actor:russ-orchestrator"]; got != 1 {
		t.Fatalf("first actor ensures = %d, want 1", got)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}

	second, secondStore, err := OpenSQLiteCheckpointStore(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	// Mechanical Driver evidence arrives after the issuing CLI died. The reopened
	// checkpoint must consume it without ensuring a second actor.
	fake.ActorFacts["russ-orchestrator"] = true
	delete(fake.DelayReady, "actor:russ-orchestrator")
	result, err = newBootstrap(fake, secondStore).Run(ctx, bootstrapPlan())
	if err != nil || !result.Complete {
		t.Fatalf("reopened Run() = %+v, %v", result, err)
	}
	if got := fake.MutationCount["actor:russ-orchestrator"]; got != 1 {
		t.Fatalf("actor ensure repeated after reopen: %d", got)
	}
}

func TestSQLiteCheckpointIdentityIsImmutable(t *testing.T) {
	ctx := context.Background()
	db, store, err := OpenSQLiteCheckpointStore(ctx, filepath.Join(t.TempDir(), "bootstrap.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	cp, err := store.Create(ctx, Checkpoint{BootstrapID: "one", PlanSHA256: "sha256:one",
		ProjectID: "russ", Issued: map[string]string{}, Completed: map[string]string{}})
	if err != nil {
		t.Fatal(err)
	}
	cp.ProjectID = "other"
	if _, err := store.CompareAndSwap(ctx, cp, cp.Version); err != ErrCheckpointConflict {
		t.Fatalf("identity-changing CAS error = %v, want conflict", err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE bootstrap_checkpoints SET project_id='other' WHERE bootstrap_id='one'`); err == nil {
		t.Fatal("database trigger allowed checkpoint identity mutation")
	}
}
