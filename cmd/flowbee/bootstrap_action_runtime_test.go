package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/samhotchkiss/flowbee/internal/store"
	"github.com/samhotchkiss/flowbee/internal/testutil"
)

func commitBootstrapRuntimeAction(t *testing.T, st *store.Store, id, projectID, kind string, payload any, now time.Time) {
	t.Helper()
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(body)
	if _, err := st.CommitBootstrapAction(context.Background(), store.BootstrapActionInput{ID: id,
		BootstrapID: "bootstrap-" + projectID, ProjectID: projectID, Kind: kind,
		PayloadJSON: string(body), PayloadSHA256: "sha256:" + hex.EncodeToString(sum[:])}, now); err != nil {
		t.Fatal(err)
	}
}

func TestBootstrapRuntimeFreshProjectRegistersRouteBeforeLifecycle(t *testing.T) {
	ctx := context.Background()
	st := testutil.NewStore(t)
	st.ProjectActorCredentialMaterializer = func(_, _, _, _ string, _ int64, _ time.Time) (string, error) {
		return "sha256:" + strings.Repeat("a", 64), nil
	}
	now := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	if err := st.RegisterRepo(ctx, store.Repo{ID: "russ-repo", Owner: "sam", Repo: "russ", Active: true}); err != nil {
		t.Fatal(err)
	}
	runtime := bootstrapActionRuntime{Store: st, Owner: "serve-test"}
	steps := []struct {
		id, kind string
		payload  any
	}{
		{"01-project", "project_upsert", store.PortfolioProject{ID: "russ", Name: "Russ"}},
		{"02-repo", "repository_attach", map[string]string{"project_id": "russ", "repo_id": "russ-repo"}},
		{"03-route", "actor_route", store.ProjectActorRoute{ProjectID: "russ", Role: store.DriverOrchestratorRole, ActorID: "russ-orchestrator"}},
		{"04-lifecycle", "actor_lifecycle", store.ProjectActorLifecycleCommand{ProjectID: "russ", Role: store.DriverOrchestratorRole,
			ActorID: "russ-orchestrator", ExpectedRouteStateVersion: 1, Operation: "ensure",
			InstanceRef: "managed", TargetHostID: "host-a", TargetStoreID: "store-a",
			TargetServerDomainID: "flowbee", TargetServerID: "server-a", LifecycleOwnership: "driver_managed",
			LifecycleKey: "russ-orchestrator", TargetEpoch: 1, ProfileID: "codex_orchestrator",
			WorkspaceRootID: "dev", WorkspaceRelativePath: "russ"}},
	}
	for i, step := range steps {
		at := now.Add(time.Duration(i*2) * time.Second)
		commitBootstrapRuntimeAction(t, st, step.id, "russ", step.kind, step.payload, at)
		if err := runtime.Tick(ctx, at); err != nil {
			t.Fatalf("execute %s: %v", step.kind, err)
		}
		if step.kind != "actor_lifecycle" {
			if err := runtime.Tick(ctx, at.Add(time.Second)); err != nil {
				t.Fatalf("verify %s: %v", step.kind, err)
			}
		}
	}
	route, err := st.GetProjectActor(ctx, "russ", store.DriverOrchestratorRole)
	if err != nil || route.ActorID != "russ-orchestrator" || route.StateVersion != 1 {
		t.Fatalf("route=%+v err=%v", route, err)
	}
	lifecycle, err := st.GetProjectActorLifecycle(ctx, "russ", store.DriverOrchestratorRole, "russ-orchestrator")
	if err != nil || lifecycle.State != "awaiting_ensure" || lifecycle.RouteStateVersion != 1 {
		action, _ := st.GetBootstrapAction(ctx, "04-lifecycle")
		t.Fatalf("lifecycle=%+v err=%v bootstrap_state=%s bootstrap_error=%q", lifecycle, err, action.State, action.LastError)
	}
	repos, err := st.ProjectRepoIDs(ctx, "russ", false)
	if err != nil || len(repos) != 1 || repos[0] != "russ-repo" {
		t.Fatalf("repos=%v err=%v", repos, err)
	}
}

func TestDecodeStrictBootstrapPayloadRejectsAllTrailingData(t *testing.T) {
	for _, raw := range []string{
		`{"project_id":"russ"}{"project_id":"other"}`,
		`{"project_id":"russ"} garbage`,
	} {
		var value struct {
			ProjectID string `json:"project_id"`
		}
		if err := decodeStrictBootstrapPayload(raw, &value); err == nil {
			t.Fatalf("trailing data accepted: %q", raw)
		}
	}
}

func TestBootstrapActorLifecycleUsesCurrentPreexistingRouteVersion(t *testing.T) {
	ctx := context.Background()
	st := testutil.NewStore(t)
	st.ProjectActorCredentialMaterializer = func(_, _, _, _ string, _ int64, _ time.Time) (string, error) {
		return "sha256:" + strings.Repeat("b", 64), nil
	}
	now := time.Date(2026, 7, 19, 13, 0, 0, 0, time.UTC)
	if _, err := st.CreatePortfolioProject(ctx, store.PortfolioProject{ID: "russ", Name: "Russ"}, now); err != nil {
		t.Fatal(err)
	}
	if err := st.RegisterRepo(ctx, store.Repo{ID: "russ-repo", Owner: "fixture", Repo: "russ", Active: true}); err != nil {
		t.Fatal(err)
	}
	if err := st.AddProjectRepo(ctx, "russ", "russ-repo", now); err != nil {
		t.Fatal(err)
	}
	route := store.ProjectActorRoute{ProjectID: "russ", Role: store.DriverOrchestratorRole, ActorID: "russ-orchestrator"}
	if _, err := st.RegisterProjectActorCommand(ctx, route, "preexisting-route-v1", now); err != nil {
		t.Fatal(err)
	}
	// A legitimate prior rotation away and back advances the route state.
	rotated := route
	rotated.ActorID = "russ-orchestrator-old"
	if _, err := st.RegisterProjectActorCommand(ctx, rotated, "preexisting-route-v2", now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := st.RegisterProjectActorCommand(ctx, route, "preexisting-route-v3", now.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	current, err := st.GetProjectActor(ctx, "russ", store.DriverOrchestratorRole)
	if err != nil || current.StateVersion < 2 {
		t.Fatalf("preexisting route=%+v err=%v", current, err)
	}
	command := store.ProjectActorLifecycleCommand{ProjectID: "russ", Role: store.DriverOrchestratorRole,
		ActorID: "russ-orchestrator", Operation: "ensure", InstanceRef: "managed",
		TargetHostID: "host-a", TargetStoreID: "store-a", TargetServerDomainID: "flowbee",
		TargetServerID: "server-a", LifecycleOwnership: "driver_managed", LifecycleKey: "russ-orchestrator",
		TargetEpoch: 1, ProfileID: "codex_orchestrator", WorkspaceRootID: "dev", WorkspaceRelativePath: "russ"}
	commitBootstrapRuntimeAction(t, st, "lifecycle-current-route", "russ", "actor_lifecycle", command, now.Add(3*time.Second))
	if err := (bootstrapActionRuntime{Store: st, Owner: "route-version-test"}).Tick(ctx, now.Add(4*time.Second)); err != nil {
		t.Fatal(err)
	}
	lifecycle, err := st.GetProjectActorLifecycle(ctx, "russ", store.DriverOrchestratorRole, "russ-orchestrator")
	if err != nil || lifecycle.RouteStateVersion != int64(current.StateVersion) {
		t.Fatalf("lifecycle route version=%d want=%d err=%v", lifecycle.RouteStateVersion, current.StateVersion, err)
	}
}

func TestBootstrapSeatBindCannotRewriteIdentityOwnedByAnotherProject(t *testing.T) {
	ctx := context.Background()
	st := testutil.NewStore(t)
	now := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	for _, project := range []string{"russ", "mail"} {
		if _, err := st.CreatePortfolioProject(ctx, store.PortfolioProject{ID: project, Name: strings.ToUpper(project)}, now); err != nil {
			t.Fatal(err)
		}
	}
	seat := store.Seat{AgentFamily: "codex", CodexHome: "/codex/shared", Enabled: true}
	if err := st.AddSeat(ctx, seat, now); err != nil {
		t.Fatal(err)
	}
	seatID := seat.ComposeID()
	first := store.CapacitySeatIdentity{SeatID: seatID, HostID: "host-a", AccountKey: "acct-a", CredentialLineage: "lineage-a", ReservePct: 10, AccountMaximum: 2}
	if err := st.BindCapacitySeatIdentity(ctx, first, now); err != nil {
		t.Fatal(err)
	}
	stamp := now.Format(time.RFC3339Nano)
	if _, err := st.DB.ExecContext(ctx, `INSERT INTO driver_instances
		(instance_ref,host_id,store_id,producer_boot_id,tmux_server_domain_id,tmux_server_ownership,state,created_at,updated_at)
		VALUES ('managed','host-a','store-a','boot','flowbee','managed_dedicated','live',?,?)`, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertBuilderDriverTarget(ctx, store.BuilderDriverTarget{ProjectID: "mail", SeatID: seatID,
		InstanceRef: "managed", TmuxServerDomainID: "flowbee", TmuxServerInstanceID: "server-a",
		ProfileID: "codex-builder", WorkspaceRootID: "root", WorkspaceRelativeBase: "repos", Enabled: true}, now); err != nil {
		t.Fatal(err)
	}
	changed := first
	changed.AccountKey = "acct-russ"
	target := store.BuilderDriverTarget{ProjectID: "russ", SeatID: seatID, InstanceRef: "managed",
		TmuxServerDomainID: "flowbee", TmuxServerInstanceID: "server-a", ProfileID: "codex-builder",
		WorkspaceRootID: "root", WorkspaceRelativeBase: "repos", Enabled: true}
	if err := st.BindBootstrapSeat(ctx, store.BootstrapSeatBinding{ProjectID: "russ", Seat: seat,
		Capacity: changed, Target: target}, now); err == nil {
		t.Fatal("cross-project seat identity rewrite was accepted")
	}
	var account string
	if err := st.DB.QueryRowContext(ctx, `SELECT expected_account_key FROM seats WHERE id=?`, seatID).Scan(&account); err != nil || account != "acct-a" {
		t.Fatalf("seat identity changed account=%q err=%v", account, err)
	}
	if err := st.BindBootstrapSeat(ctx, store.BootstrapSeatBinding{ProjectID: "russ", Seat: seat,
		Capacity: first, Target: target}, now); err != nil {
		t.Fatalf("exact shared identity rejected: %v", err)
	}
}
