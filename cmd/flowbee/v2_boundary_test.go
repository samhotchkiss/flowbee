package main

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/samhotchkiss/flowbee/internal/store"
	"github.com/samhotchkiss/flowbee/internal/testutil"
)

func TestEpicCLIActuationIsFencedWhenV2Enabled(t *testing.T) {
	newSessionTestDB(t)
	t.Setenv("FLOWBEE_EPIC_REVIEW_HANDOFF_V2", "1")

	for _, command := range []string{"start", "abandon"} {
		t.Run(command, func(t *testing.T) {
			err := runEpic([]string{command})
			if err == nil || !strings.Contains(err.Error(), "legacy-only") || !strings.Contains(err.Error(), "Driver") {
				t.Fatalf("runEpic(%q) error = %v, want v2 Driver boundary fence", command, err)
			}
		})
	}

	// Read-only local projections stay available under v2. In particular, these
	// calls complete without ever reaching the legacy launch/stop implementations.
	if err := runEpic([]string{"status"}); err != nil {
		t.Fatalf("read-only status was fenced under v2: %v", err)
	}
	if err := runEpic([]string{"plan"}); err != nil {
		t.Fatalf("read-only plan was fenced under v2: %v", err)
	}
}

func TestEpicCLIActuationUsesDurableV2FenceWhenEnvironmentOmitsOrDisablesFlag(t *testing.T) {
	dbPath := newSessionTestDB(t)
	ctx := context.Background()
	st, err := store.Open(ctx, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetDurableEpicReviewHandoffV2(ctx, true); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	// A standalone CLI does not own rollback authority. Even an explicit zero in
	// its process environment cannot reopen the raw-tmux path.
	t.Setenv("FLOWBEE_EPIC_REVIEW_HANDOFF_V2", "0")
	for _, command := range []string{"start", "abandon"} {
		err := runEpic([]string{command})
		if err == nil || !strings.Contains(err.Error(), "legacy-only") {
			t.Fatalf("runEpic(%q) error = %v, want durable v2 fence", command, err)
		}
	}
}

func TestLegacyEpicMutationRequiresWriterOwnershipButReadsDoNot(t *testing.T) {
	dbPath := newSessionTestDB(t)
	ctx := context.Background()
	active, err := store.Open(ctx, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer active.Close()
	if err := active.AcquireWriterLock(); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FLOWBEE_EPIC_REVIEW_HANDOFF_V2", "0")

	if err := runEpic([]string{"start"}); err == nil ||
		!strings.Contains(err.Error(), "writer already active") {
		t.Fatalf("legacy start while serve owns writer lock error=%v", err)
	}
	if err := runEpic([]string{"abandon", "missing"}); err == nil ||
		!strings.Contains(err.Error(), "writer already active") {
		t.Fatalf("legacy abandon while serve owns writer lock error=%v", err)
	}
	if err := runEpic([]string{"status"}); err != nil {
		t.Fatalf("read-only status must remain available: %v", err)
	}
}

func TestServeV2SelectionPersistsAndOnlyExplicitServeRollbackClears(t *testing.T) {
	ctx := context.Background()
	dbPath := newSessionTestDB(t)
	st, err := store.Open(ctx, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.AcquireWriterLock(); err != nil {
		t.Fatal(err)
	}

	t.Setenv("FLOWBEE_EPIC_REVIEW_HANDOFF_V2", "1")
	if enabled, err := selectDurableEpicReviewHandoffV2(ctx, st); err != nil || !enabled {
		t.Fatalf("activate = %v, err=%v", enabled, err)
	}
	t.Setenv("FLOWBEE_EPIC_REVIEW_HANDOFF_V2", "0")
	if enabled, err := selectDurableEpicReviewHandoffV2(ctx, st); err != nil || enabled {
		t.Fatalf("rollback = %v, err=%v", enabled, err)
	}
}

func TestDedicatedWorkerSelectionSurvivesOmittedEnvironment(t *testing.T) {
	ctx := context.Background()
	st := testutil.NewStore(t)
	if err := st.SetDurableEpicDedicatedWorkersV2(ctx, true, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	prior, existed := os.LookupEnv("FLOWBEE_EPIC_DEDICATED_WORKERS_V2")
	if err := os.Unsetenv("FLOWBEE_EPIC_DEDICATED_WORKERS_V2"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if existed {
			_ = os.Setenv("FLOWBEE_EPIC_DEDICATED_WORKERS_V2", prior)
		} else {
			_ = os.Unsetenv("FLOWBEE_EPIC_DEDICATED_WORKERS_V2")
		}
	})
	selected, explicit, err := desiredDurableEpicDedicatedWorkersV2(ctx, st)
	if err != nil || !selected || explicit {
		t.Fatalf("omitted-env restart selection enabled=%v explicit=%v err=%v", selected, explicit, err)
	}
	t.Setenv("FLOWBEE_EPIC_DEDICATED_WORKERS_V2", "0")
	selected, explicit, err = desiredDurableEpicDedicatedWorkersV2(ctx, st)
	if err != nil || selected || !explicit {
		t.Fatalf("explicit disable selection enabled=%v explicit=%v err=%v", selected, explicit, err)
	}
}

func TestLegacyPaneRuntimeActivationPredicate(t *testing.T) {
	tests := []struct {
		name             string
		v2               bool
		explicitDisabled bool
		want             bool
	}{
		{name: "legacy enabled", want: true},
		{name: "legacy kill switch", explicitDisabled: true, want: false},
		{name: "v2 fences raw pane runtime", v2: true, want: false},
		{name: "v2 remains fenced despite old kill switch", v2: true, explicitDisabled: true, want: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := legacyPaneRuntimeEnabled(tc.v2, tc.explicitDisabled); got != tc.want {
				t.Fatalf("legacyPaneRuntimeEnabled(%v, %v) = %v, want %v", tc.v2, tc.explicitDisabled, got, tc.want)
			}
		})
	}
}
