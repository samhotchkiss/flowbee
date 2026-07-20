package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/samhotchkiss/flowbee/internal/bootstrap"
	"github.com/samhotchkiss/flowbee/internal/config"
)

const (
	bareLocalHost  = "11111111-1111-4111-8111-111111111111"
	bareRemoteHost = "22222222-2222-4222-8222-222222222222"
)

func localBarePlanFixture(root string) (config.Config, config.DriverEndpointInventory, bareBootstrapPreflight, bootstrap.ProjectInit) {
	project := config.BootstrapProjectConfig{ProjectID: "russ", Name: "Russ", RepositoryIDs: []string{"russ"},
		ControlPlane: config.BootstrapControlPlaneConfig{InstanceRef: "managed", LifecycleKey: "flowbee-control",
			TargetEpoch: 1, ProfileID: "flowbee_control", WorkspaceRootID: "dev",
			WorkspaceRelativePath: "flowbee", TmuxServerInstanceID: "server-flowbee"},
		Interactor: config.BootstrapInteractorConfig{ActorID: "russ-claude", PresentationName: "russ-interactor",
			Operation: "adopt", InstanceRef: "external", LifecycleKey: "russ-interactor", TargetEpoch: 1,
			ProfileID: "claude-fable", ExternalWatchID: "watch-russ", ExistingSessionID: "session-russ",
			ExpectedPaneInstanceID: "pane-russ", ExpectedAgentRunID: "run-russ",
			RecoveryProfileID: "claude_interactor_managed", RecoveryWorkspaceRootID: "dev",
			RecoveryWorkspaceRelativePath: "russ", TmuxServerInstanceID: "server-default"},
		Orchestrator: config.BootstrapOrchestratorConfig{ActorID: "russ-orchestrator", PresentationName: "russ-orchestrator",
			InstanceRef: "managed", LifecycleKey: "russ-orchestrator", TargetEpoch: 1, ProfileID: "codex_orchestrator",
			WorkspaceRootID: "dev", WorkspaceRelativePath: "russ", TmuxServerInstanceID: "server-flowbee"},
		LocalSeats: []config.BootstrapSeatConfig{
			{SeatID: "|codex|/private/codex", HostID: bareLocalHost, AgentFamily: "codex", CodexHome: "/private/codex",
				MaxConcurrent: 2, AccountKey: "codex-account", CredentialLineage: "codex-lineage", ReservePct: 10,
				AccountMaximum: 2, InstanceRef: "managed", TmuxServerDomainID: "flowbee",
				TmuxServerInstanceID: "server-flowbee", ProfileID: "codex_builder", WorkspaceRootID: "dev", WorkspaceRelativeBase: "russ"},
			{SeatID: "|grok|/private/grok", HostID: bareLocalHost, AgentFamily: "grok", ConfigDir: "/private/grok",
				MaxConcurrent: 1, AccountKey: "grok-account", CredentialLineage: "grok-lineage", ReservePct: 10,
				AccountMaximum: 1, InstanceRef: "managed", TmuxServerDomainID: "flowbee",
				TmuxServerInstanceID: "server-flowbee", ProfileID: "grok_reviewer", WorkspaceRootID: "dev", WorkspaceRelativeBase: "russ"},
		}}
	cfg := config.Config{DatabaseURL: root, GithubOwner: "sam", GithubRepo: "russ",
		Repos:             []config.RepoConfig{{ID: "russ", Owner: "sam", Repo: "russ"}},
		BootstrapProjects: []config.BootstrapProjectConfig{project}}
	inventory := config.DriverEndpointInventory{FormatVersion: "flowbee.driver-endpoints/v1", Endpoints: []config.DriverEndpoint{
		{InstanceRef: "external", ExpectedHostID: bareLocalHost, ExpectedStoreID: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa",
			ExpectedTmuxServerDomainID: "default", ExpectedTmuxServerOwnership: "external"},
		{InstanceRef: "managed", ExpectedHostID: bareLocalHost, ExpectedStoreID: "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb",
			ExpectedTmuxServerDomainID: "flowbee", ExpectedTmuxServerOwnership: "managed_dedicated"},
	}}
	preflight := bareBootstrapPreflight{RepoRoot: root, GitInfoDir: filepath.Join(root, ".git", "info"),
		Origin: "github.com/sam/russ", ProjectID: "russ", RepositoryID: "russ"}
	init := bootstrap.ProjectInit{ProjectID: "russ", RepositoryOrigin: preflight.Origin, CWD: root}
	return cfg, inventory, preflight, init
}

func TestBareBootstrapRejectsEveryRemoteActorControlAndSeatBeforeMutation(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*config.Config, *config.DriverEndpointInventory)
	}{
		{name: "managed actor and control endpoint", mutate: func(_ *config.Config, inventory *config.DriverEndpointInventory) {
			inventory.Endpoints[1].ExpectedHostID = bareRemoteHost
		}},
		{name: "seat host", mutate: func(cfg *config.Config, inventory *config.DriverEndpointInventory) {
			inventory.Endpoints = append(inventory.Endpoints, config.DriverEndpoint{InstanceRef: "remote-managed",
				ExpectedHostID: bareRemoteHost, ExpectedStoreID: "cccccccc-cccc-4ccc-8ccc-cccccccccccc",
				ExpectedTmuxServerDomainID: "flowbee", ExpectedTmuxServerOwnership: "managed_dedicated"})
			cfg.BootstrapProjects[0].LocalSeats[0].InstanceRef = "remote-managed"
			cfg.BootstrapProjects[0].LocalSeats[0].HostID = bareRemoteHost
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			if err := os.MkdirAll(filepath.Join(root, ".git", "info"), 0o700); err != nil {
				t.Fatal(err)
			}
			cfg, inventory, _, _ := localBarePlanFixture(root)
			tc.mutate(&cfg, &inventory)
			fake := &bareSystemFake{cfg: cfg, inventory: inventory}
			err := executeBareBootstrap(context.Background(), fake)
			if err == nil || !strings.Contains(err.Error(), "local-only") {
				t.Fatalf("remote topology error=%v", err)
			}
			if _, statErr := os.Stat(filepath.Join(root, ".flowbee", "project.json")); !os.IsNotExist(statErr) {
				t.Fatalf("remote topology wrote project marker: %v", statErr)
			}
		})
	}
}

func TestBareBootstrapPlanGenerationReplaysAndSupersedesWithoutOverwrite(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	cfg, inventory, preflight, init := localBarePlanFixture(root)
	first, err := buildBareServerActionPlan(cfg, inventory, preflight, init)
	if err != nil {
		t.Fatal(err)
	}
	replay, err := buildBareServerActionPlan(cfg, inventory, preflight, init)
	if err != nil {
		t.Fatal(err)
	}
	if replay.BootstrapID != first.BootstrapID || len(replay.Actions) != len(first.Actions) {
		t.Fatalf("same immutable plan did not replay generation: first=%q replay=%q", first.BootstrapID, replay.BootstrapID)
	}
	for i := range first.Actions {
		if first.Actions[i].ActionID != replay.Actions[i].ActionID {
			t.Fatalf("same plan action %d changed: %q != %q", i, first.Actions[i].ActionID, replay.Actions[i].ActionID)
		}
	}

	cfg.BootstrapProjects[0].ControlPlane.TargetEpoch++
	changed, err := buildBareServerActionPlan(cfg, inventory, preflight, init)
	if err != nil {
		t.Fatal(err)
	}
	if changed.BootstrapID == first.BootstrapID {
		t.Fatal("authorized target epoch change reused immutable bootstrap generation")
	}
	firstIDs := map[string]bool{}
	for _, action := range first.Actions {
		firstIDs[action.ActionID] = true
	}
	for _, action := range changed.Actions {
		if firstIDs[action.ActionID] {
			t.Fatalf("new generation reused old action id %q", action.ActionID)
		}
	}

	db, checkpoints, err := bootstrap.OpenSQLiteCheckpointStore(ctx, filepath.Join(root, "bootstrap.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := initializeBarePlanCheckpoint(ctx, checkpoints, first); err != nil {
		t.Fatal(err)
	}
	if _, err := initializeBarePlanCheckpoint(ctx, checkpoints, changed); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := checkpoints.Load(ctx, first.BootstrapID); err != nil || !ok {
		t.Fatalf("old checkpoint was overwritten by superseding plan: ok=%v err=%v", ok, err)
	}
	if _, ok, err := checkpoints.Load(ctx, changed.BootstrapID); err != nil || !ok {
		t.Fatalf("new checkpoint missing: ok=%v err=%v", ok, err)
	}
}
