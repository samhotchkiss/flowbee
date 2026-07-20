package config

import (
	"strings"
	"testing"
)

func validBootstrapProject() BootstrapProjectConfig {
	return BootstrapProjectConfig{ProjectID: "russ", Name: "Russ",
		RepositoryIDs: []string{"api", "web"},
		ControlPlane: BootstrapControlPlaneConfig{InstanceRef: "managed-local",
			LifecycleKey: "flowbee-control", TargetEpoch: 1, ProfileID: "flowbee_control",
			WorkspaceRootID: "dev", WorkspaceRelativePath: "flowbee", TmuxServerInstanceID: "server-flowbee"},
		Interactor: BootstrapInteractorConfig{ActorID: "russ-claude",
			PresentationName: "russ-interactor", Operation: "adopt", InstanceRef: "external-local",
			LifecycleKey: "russ-interactor", TargetEpoch: 1, ProfileID: "claude-fable",
			ExternalWatchID: "watch-russ", ExistingSessionID: "session-russ",
			ExpectedPaneInstanceID: "pane-russ", ExpectedAgentRunID: "run-russ",
			RecoveryProfileID: "claude_interactor_managed", RecoveryWorkspaceRootID: "dev",
			RecoveryWorkspaceRelativePath: "russ", TmuxServerInstanceID: "server-default"},
		Orchestrator: BootstrapOrchestratorConfig{ActorID: "russ-orchestrator",
			PresentationName: "russ-orchestrator", InstanceRef: "managed-local",
			LifecycleKey: "russ-orchestrator", TargetEpoch: 1, ProfileID: "codex_orchestrator",
			WorkspaceRootID: "dev", WorkspaceRelativePath: "russ", TmuxServerInstanceID: "server-flowbee"},
		LocalSeats: []BootstrapSeatConfig{{SeatID: "local|codex|one", HostID: "11111111-1111-4111-8111-111111111111",
			AgentFamily: "codex", CodexHome: "/private/codex", MaxConcurrent: 2,
			AccountKey: "codex-account", CredentialLineage: "lineage-1", ReservePct: 10,
			AccountMaximum: 2, InstanceRef: "managed-local", TmuxServerDomainID: "flowbee",
			TmuxServerInstanceID: "server-flowbee", ProfileID: "codex_builder",
			WorkspaceRootID: "dev", WorkspaceRelativeBase: "russ"},
			{SeatID: "local|grok|review", HostID: "11111111-1111-4111-8111-111111111111",
				AgentFamily: "grok", ConfigDir: "/private/grok", MaxConcurrent: 1,
				AccountKey: "grok-account", CredentialLineage: "lineage-2", ReservePct: 10,
				AccountMaximum: 1, InstanceRef: "managed-local", TmuxServerDomainID: "flowbee",
				TmuxServerInstanceID: "server-flowbee", ProfileID: "grok_reviewer",
				WorkspaceRootID: "dev", WorkspaceRelativeBase: "russ"}}}
}

func TestBootstrapProjectInteractorAdoptEnsureUnionIsClosed(t *testing.T) {
	cfg := Default()
	cfg.Repos = []RepoConfig{{ID: "api", Owner: "sam", Repo: "api"},
		{ID: "web", Owner: "sam", Repo: "web"}}
	project := validBootstrapProject()
	project.Interactor.Operation = "ensure"
	project.Interactor.ProfileID = "claude_interactor_managed"
	project.Interactor.WorkspaceRootID = "dev"
	project.Interactor.WorkspaceRelativePath = "russ"
	project.Interactor.ExternalWatchID = ""
	project.Interactor.ExistingSessionID = ""
	project.Interactor.ExpectedPaneInstanceID = ""
	project.Interactor.ExpectedAgentRunID = ""
	project.Interactor.RecoveryProfileID = ""
	project.Interactor.RecoveryWorkspaceRootID = ""
	project.Interactor.RecoveryWorkspaceRelativePath = ""
	cfg.BootstrapProjects = []BootstrapProjectConfig{project}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("managed Interactor ensure rejected: %v", err)
	}
	project.Interactor.ExternalWatchID = "mixed-watch"
	cfg.BootstrapProjects = []BootstrapProjectConfig{project}
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "no adopt identity") {
		t.Fatalf("mixed adopt/ensure identity accepted: %v", err)
	}
}

func TestBootstrapProjectRequiresDistinctDedicatedReviewerPool(t *testing.T) {
	cfg := Default()
	cfg.Repos = []RepoConfig{{ID: "api", Owner: "sam", Repo: "api"},
		{ID: "web", Owner: "sam", Repo: "web"}}
	project := validBootstrapProject()
	project.LocalSeats[1].AgentFamily = "codex"
	project.LocalSeats[1].ProfileID = "codex_reviewer"
	project.LocalSeats[1].ConfigDir = ""
	project.LocalSeats[1].CodexHome = "/private/codex-review"
	cfg.BootstrapProjects = []BootstrapProjectConfig{project}
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "distinct reviewer family") {
		t.Fatalf("same-family-only pool accepted: %v", err)
	}
	project = validBootstrapProject()
	project.LocalSeats = project.LocalSeats[:1]
	cfg.BootstrapProjects = []BootstrapProjectConfig{project}
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "builder and reviewer profile pools") {
		t.Fatalf("missing reviewer pool accepted: %v", err)
	}
}

func TestBootstrapProjectExplicitlySupportsOneProjectWithTwoRepos(t *testing.T) {
	cfg := Default()
	cfg.Repos = []RepoConfig{{ID: "api", Owner: "sam", Repo: "api"},
		{ID: "web", Owner: "sam", Repo: "web"}}
	cfg.BootstrapProjects = []BootstrapProjectConfig{validBootstrapProject()}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestBootstrapProjectRejectsSharedRepoAmbiguity(t *testing.T) {
	cfg := Default()
	cfg.Repos = []RepoConfig{{ID: "api", Owner: "sam", Repo: "api"},
		{ID: "web", Owner: "sam", Repo: "web"}}
	one := validBootstrapProject()
	two := validBootstrapProject()
	two.ProjectID, two.Name = "mail", "Mail"
	two.RepositoryIDs = []string{"api"}
	two.Interactor.PresentationName = "mail-interactor"
	two.Orchestrator.PresentationName = "mail-orchestrator"
	cfg.BootstrapProjects = []BootstrapProjectConfig{one, two}
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "ambiguously mapped") {
		t.Fatalf("Validate() = %v", err)
	}
}

func TestBootstrapProjectRejectsGuessedOrIncompleteActorAndSeatIdentity(t *testing.T) {
	cfg := Default()
	cfg.Repos = []RepoConfig{{ID: "api", Owner: "sam", Repo: "api"},
		{ID: "web", Owner: "sam", Repo: "web"}}
	project := validBootstrapProject()
	project.Orchestrator.WorkspaceRootID = ""
	cfg.BootstrapProjects = []BootstrapProjectConfig{project}
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "Orchestrator requires exact") {
		t.Fatalf("Validate() = %v", err)
	}
	project = validBootstrapProject()
	project.LocalSeats[0].CredentialLineage = ""
	cfg.BootstrapProjects = []BootstrapProjectConfig{project}
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "exact unique seat") {
		t.Fatalf("Validate() = %v", err)
	}
}
