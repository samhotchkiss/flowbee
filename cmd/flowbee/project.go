package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/samhotchkiss/flowbee/internal/config"
	"github.com/samhotchkiss/flowbee/internal/driver"
	"github.com/samhotchkiss/flowbee/internal/store"
)

const (
	projectActivationCapacityFreshness = 5 * time.Minute
	workerAuthRuntimeHeartbeatInterval = 10 * time.Second
	workerAuthRuntimeFreshness         = 45 * time.Second
)

// runProject is the deterministic, offline-safe onboarding surface for v2
// projects and their exact actor incarnations. Mutations take the same OS writer
// lock as serve; status/list remain usable while serve owns the database.
func runProject(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: flowbee project <add|list|attach-repo|bind-actor|actor-lifecycle|bind-session|status> ...")
	}
	sub, rest := args[0], args[1:]
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	ctx := context.Background()
	st, err := store.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer st.Close()
	if sub != "list" && sub != "ls" && sub != "status" {
		if err := st.AcquireWriterLock(); err != nil {
			return fmt.Errorf("project onboarding requires the control-plane writer to be stopped: %w", err)
		}
	}
	switch sub {
	case "add":
		return runProjectAdd(ctx, st, rest)
	case "list", "ls":
		return runProjectList(ctx, st, rest)
	case "attach-repo":
		return runProjectAttachRepo(ctx, st, rest)
	case "bind-actor":
		return runProjectBindActor(ctx, st, rest)
	case "actor-lifecycle":
		return runProjectActorLifecycle(ctx, st, rest)
	case "bind-session":
		return runProjectBindSession(ctx, st, rest)
	case "status":
		return runProjectStatus(ctx, st, cfg, rest)
	default:
		return fmt.Errorf("unknown `flowbee project` subcommand %q (want add|list|attach-repo|bind-actor|actor-lifecycle|bind-session|status)", sub)
	}
}

func runProjectAdd(ctx context.Context, st *store.Store, args []string) error {
	fs := flag.NewFlagSet("project add", flag.ContinueOnError)
	id := fs.String("id", "", "stable project id (required)")
	name := fs.String("name", "", "human-readable project name (required)")
	priority := fs.Int("priority", 100, "scheduler priority; lower runs first")
	weight := fs.Int("scheduler-weight", 1, "weighted-fair scheduler weight")
	cap := fs.Int("concurrency-cap", 0, "project concurrency limit; 0 means fleet limit")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 || strings.TrimSpace(*id) == "" || strings.TrimSpace(*name) == "" {
		return errors.New("usage: flowbee project add --id <id> --name <name> [--priority N] [--scheduler-weight N] [--concurrency-cap N]")
	}
	p := store.PortfolioProject{ID: *id, Name: *name, State: "active", Priority: *priority,
		SchedulerWeight: *weight, ConcurrencyCap: *cap}
	p, err := st.CreatePortfolioProjectCommand(ctx, p, projectCLIKey("create", *id, *name,
		fmt.Sprint(*priority), fmt.Sprint(*weight), fmt.Sprint(*cap)), time.Now())
	if err != nil {
		return err
	}
	fmt.Printf("✓ project %q (%s) registered\n", p.ID, p.Name)
	return nil
}

func runProjectAttachRepo(ctx context.Context, st *store.Store, args []string) error {
	fs := flag.NewFlagSet("project attach-repo", flag.ContinueOnError)
	projectID := fs.String("project-id", "", "project id (required)")
	repoID := fs.String("repo-id", "", "configured repository id (required)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 || *projectID == "" || *repoID == "" {
		return errors.New("usage: flowbee project attach-repo --project-id <id> --repo-id <configured-repo-id>")
	}
	if err := st.AddProjectRepoCommand(ctx, *projectID, *repoID,
		projectCLIKey("repo", *projectID, *repoID), time.Now()); err != nil {
		return err
	}
	fmt.Printf("✓ attached repository %q to project %q\n", *repoID, *projectID)
	return nil
}

func runProjectBindActor(ctx context.Context, st *store.Store, args []string) error {
	fs := flag.NewFlagSet("project bind-actor", flag.ContinueOnError)
	projectID := fs.String("project-id", "", "project id (required)")
	role := fs.String("role", "", "interactor or orchestrator (required)")
	actorID := fs.String("actor-id", "", "stable Flowbee actor identity (required)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 || *projectID == "" || *actorID == "" ||
		(*role != store.DriverInteractorRole && *role != store.DriverOrchestratorRole) {
		return errors.New("usage: flowbee project bind-actor --project-id <id> --role <interactor|orchestrator> --actor-id <id>")
	}
	route, err := st.RegisterProjectActorCommand(ctx, store.ProjectActorRoute{
		ProjectID: *projectID, Role: *role, ActorID: *actorID,
	}, projectCLIKey("actor", *projectID, *role, *actorID), time.Now())
	if err != nil {
		return err
	}
	fmt.Printf("✓ project %q %s route bound to actor %q\n", route.ProjectID, route.Role, route.ActorID)
	return nil
}

func runProjectBindSession(ctx context.Context, st *store.Store, args []string) error {
	fs := flag.NewFlagSet("project bind-session", flag.ContinueOnError)
	projectID := fs.String("project-id", "", "project id (required)")
	role := fs.String("role", "", "interactor, orchestrator, or code_reviewer (required)")
	workerID := fs.String("worker-identity", "", "exact Flowbee worker/actor identity (required)")
	seatID := fs.String("seat-id", "", "exact authenticated capacity seat (required for code_reviewer)")
	lifecycleKey := fs.String("lifecycle-key", "", "Driver lifecycle target key (required)")
	targetEpoch := fs.Int64("target-epoch", 0, "Driver lifecycle target epoch (required)")
	profileID := fs.String("profile-id", "", "Driver launch profile id (required)")
	workspaceRootID := fs.String("workspace-root-id", "", "Driver workspace root id (required)")
	workspacePath := fs.String("workspace-relative-path", "", "Driver workspace-relative path (required)")
	hostID := fs.String("host-id", "", "exact observed Driver host id (observation-only binding)")
	storeID := fs.String("store-id", "", "exact observed Driver store id (observation-only binding)")
	sessionID := fs.String("session-id", "", "exact observed Driver session id (observation-only binding)")
	paneInstanceID := fs.String("pane-instance-id", "", "exact observed Driver pane incarnation id (observation-only binding)")
	agentRunID := fs.String("agent-run-id", "", "exact observed Driver agent run id (observation-only binding)")
	serverID := fs.String("tmux-server-instance-id", "", "exact observed Driver tmux server incarnation (observation-only binding)")
	domainID := fs.String("tmux-server-domain-id", "", "exact observed Driver tmux server domain (observation-only binding)")
	socket := fs.String("driver-socket", strings.TrimSpace(os.Getenv("FLOWBEE_DRIVER_SOCKET")), "tmux-driver UDS path")
	tokenFile := fs.String("driver-token-file", strings.TrimSpace(os.Getenv("FLOWBEE_DRIVER_TOKEN_FILE")), "owner-only Driver bearer file")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *role == store.DriverInteractorRole || *role == store.DriverOrchestratorRole {
		return errors.New("project actor sessions are lifecycle-managed; use `flowbee project actor-lifecycle` (direct bind-session is disabled)")
	}
	managed := *lifecycleKey != "" || *targetEpoch != 0 || *profileID != "" || *workspaceRootID != "" || *workspacePath != ""
	observed := *hostID != "" || *storeID != "" || *sessionID != "" || *paneInstanceID != "" || *agentRunID != "" || *domainID != "" || *serverID != ""
	managedComplete := *lifecycleKey != "" && *targetEpoch > 0 && *profileID != "" && *workspaceRootID != "" && *workspacePath != ""
	observedComplete := *hostID != "" && *storeID != "" && *sessionID != "" && *paneInstanceID != "" && *agentRunID != "" && *domainID != "" && *serverID != ""
	if fs.NArg() != 0 || *projectID == "" || *workerID == "" || *socket == "" || *tokenFile == "" ||
		!bindableProjectRole(*role) || (*role == store.DriverReviewerRole) != (*seatID != "") ||
		managed == observed || managed && !managedComplete || observed && !observedComplete {
		return errors.New("usage: flowbee project bind-session --project-id <id> --role <interactor|orchestrator|code_reviewer> --worker-identity <id> [--seat-id ID (required only for code_reviewer)] [managed: --lifecycle-key K --target-epoch N --profile-id P --workspace-root-id R --workspace-relative-path PATH | observed: --host-id H --store-id S --tmux-server-domain-id D --tmux-server-instance-id T --session-id ID --pane-instance-id P --agent-run-id R] [--driver-socket PATH --driver-token-file PATH]")
	}
	token, err := readOwnerOnlySecret(*tokenFile)
	if err != nil {
		return fmt.Errorf("read Driver control token: %w", err)
	}
	binding, err := bindObservedProjectSession(ctx, st, driver.NewUDSPort(*socket, token), projectSessionBindingInput{
		ProjectID: *projectID, Role: *role, WorkerIdentity: *workerID, SeatID: *seatID,
		LifecycleKey: *lifecycleKey, TargetEpoch: *targetEpoch, ProfileID: *profileID,
		WorkspaceRootID: *workspaceRootID, WorkspaceRelativePath: *workspacePath,
		HostID: *hostID, StoreID: *storeID, TmuxServerDomainID: *domainID, SessionID: *sessionID,
		PaneInstanceID: *paneInstanceID, AgentRunID: *agentRunID, TmuxServerInstanceID: *serverID,
	}, time.Now())
	if err != nil {
		return err
	}
	fmt.Printf("✓ bound project %q %s %q to exact Driver session %s (pane %s, run %s, epoch %d)\n",
		binding.ProjectID, binding.Role, binding.WorkerIdentity, binding.SessionID,
		binding.PaneInstanceID, binding.AgentRunID, binding.BindingEpoch)
	return nil
}

func runProjectActorLifecycle(ctx context.Context, st *store.Store, args []string) error {
	fs := flag.NewFlagSet("project actor-lifecycle", flag.ContinueOnError)
	projectID := fs.String("project-id", "", "project id (required)")
	role := fs.String("role", "", "interactor or orchestrator (required)")
	actorID := fs.String("actor-id", "", "active or retiring actor id (required)")
	operation := fs.String("operation", "", "ensure, adopt, reattach, stop, or release (required)")
	idempotencyKey := fs.String("idempotency-key", "", "stable caller-supplied intent key (required)")
	instanceRef := fs.String("instance-ref", "", "configured Driver endpoint reference (required)")
	hostID := fs.String("host-id", "", "exact Driver host id (ensure/adopt)")
	storeID := fs.String("store-id", "", "exact Driver store id (ensure/adopt)")
	domainID := fs.String("tmux-server-domain-id", "", "exact Driver server domain (ensure/adopt)")
	serverID := fs.String("tmux-server-instance-id", "", "exact Driver server incarnation (ensure/adopt)")
	lifecycleKey := fs.String("lifecycle-key", "", "Driver lifecycle key (ensure/adopt)")
	targetEpoch := fs.Int64("target-epoch", 0, "Driver lifecycle target epoch (ensure/adopt)")
	profileID := fs.String("profile-id", "", "Driver lifecycle profile (ensure/adopt)")
	workspaceRootID := fs.String("workspace-root-id", "", "managed workspace root id (ensure only)")
	workspacePath := fs.String("workspace-relative-path", "", "managed workspace-relative path (ensure only)")
	recoveryProfileID := fs.String("recovery-profile-id", "", "managed replacement profile (adopted Interactor only)")
	recoveryWorkspaceRootID := fs.String("recovery-workspace-root-id", "", "managed replacement workspace root (adopted Interactor only)")
	recoveryWorkspacePath := fs.String("recovery-workspace-relative-path", "", "managed replacement workspace path (adopted Interactor only)")
	externalWatchID := fs.String("external-watch-id", "", "existing exact Driver watch UUID (adopt only)")
	sessionID := fs.String("session-id", "", "exact Driver session UUID (adopt only)")
	paneInstanceID := fs.String("pane-instance-id", "", "exact Driver pane-incarnation UUID (adopt only; never raw %N)")
	agentRunID := fs.String("agent-run-id", "", "exact Driver agent-run UUID (adopt only)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 || *projectID == "" || *actorID == "" || *idempotencyKey == "" || *instanceRef == "" ||
		(*role != store.DriverInteractorRole && *role != store.DriverOrchestratorRole) {
		return errors.New("usage: flowbee project actor-lifecycle --project-id P --role <interactor|orchestrator> --actor-id A --operation <ensure|adopt|reattach|stop|release> --idempotency-key K --instance-ref R [exact operation target flags]")
	}
	route, err := st.GetProjectActor(ctx, *projectID, *role)
	if err != nil {
		return err
	}
	command := store.ProjectActorLifecycleCommand{ProjectID: *projectID, Role: *role, ActorID: *actorID,
		ExpectedRouteStateVersion: int64(route.StateVersion), Operation: *operation,
		IdempotencyKey: *idempotencyKey, InstanceRef: *instanceRef}
	if lifecycle, lifecycleErr := st.GetProjectActorLifecycle(ctx, *projectID, *role, *actorID); lifecycleErr == nil {
		command.ExpectedLifecycleStateVersion = lifecycle.StateVersion
	} else if !errors.Is(lifecycleErr, sql.ErrNoRows) {
		return lifecycleErr
	}
	switch *operation {
	case "ensure":
		if route.ActorID != *actorID || *hostID == "" || *storeID == "" || *domainID == "" || *serverID == "" ||
			*lifecycleKey == "" || *targetEpoch < 1 || *profileID == "" || *workspaceRootID == "" || *workspacePath == "" ||
			*externalWatchID != "" || *sessionID != "" || *paneInstanceID != "" || *agentRunID != "" ||
			*recoveryProfileID != "" || *recoveryWorkspaceRootID != "" || *recoveryWorkspacePath != "" {
			return errors.New("ensure requires the active actor plus exact endpoint, lifecycle, profile, and managed workspace flags")
		}
		command.TargetHostID, command.TargetStoreID = *hostID, *storeID
		command.TargetServerDomainID, command.TargetServerID = *domainID, *serverID
		command.LifecycleOwnership, command.LifecycleKey, command.TargetEpoch = "driver_managed", *lifecycleKey, *targetEpoch
		command.ProfileID, command.WorkspaceRootID, command.WorkspaceRelativePath = *profileID, *workspaceRootID, *workspacePath
	case "adopt":
		if route.ActorID != *actorID || *hostID == "" || *storeID == "" || *domainID == "" || *serverID == "" ||
			*lifecycleKey == "" || *targetEpoch < 1 || *profileID == "" || *externalWatchID == "" ||
			*sessionID == "" || *paneInstanceID == "" || *agentRunID == "" ||
			strings.HasPrefix(*paneInstanceID, "%") || *workspaceRootID != "" || *workspacePath != "" ||
			*role != store.DriverInteractorRole || *recoveryProfileID != "claude_interactor_managed" ||
			*recoveryWorkspaceRootID == "" || *recoveryWorkspacePath == "" {
			return errors.New("adopt requires an existing Interactor watch and exact stable endpoint/session/pane/run plus a managed v3 recovery profile/workspace; raw pane selectors are forbidden")
		}
		command.TargetHostID, command.TargetStoreID = *hostID, *storeID
		command.TargetServerDomainID, command.TargetServerID = *domainID, *serverID
		command.LifecycleOwnership, command.LifecycleKey, command.TargetEpoch = "external_observed", *lifecycleKey, *targetEpoch
		command.ProfileID, command.ExternalWatchID = *profileID, *externalWatchID
		command.ManagedRecoveryProfileID = *recoveryProfileID
		command.ManagedRecoveryWorkspaceRootID = *recoveryWorkspaceRootID
		command.ManagedRecoveryWorkspaceRelativePath = *recoveryWorkspacePath
		command.ExpectedSessionID, command.ExpectedPaneInstanceID, command.ExpectedAgentRunID = *sessionID, *paneInstanceID, *agentRunID
	case "reattach", "stop", "release":
		// Exact target authority is derived from the current active binding inside
		// the commit transaction. CLI-supplied target coordinates are rejected so
		// they cannot override a stale pane/run fence.
		if *hostID != "" || *storeID != "" || *domainID != "" || *serverID != "" || *lifecycleKey != "" ||
			*targetEpoch != 0 || *profileID != "" || *workspaceRootID != "" || *workspacePath != "" ||
			*externalWatchID != "" || *sessionID != "" || *paneInstanceID != "" || *agentRunID != "" ||
			*recoveryProfileID != "" || *recoveryWorkspaceRootID != "" || *recoveryWorkspacePath != "" {
			return errors.New("reattach/stop/release derive their exact target from the durable active binding; target override flags are forbidden")
		}
	default:
		return errors.New("operation must be ensure, adopt, reattach, stop, or release")
	}
	lifecycle, action, err := st.CommitProjectActorLifecycleIntent(ctx, command, time.Now())
	if err != nil {
		return err
	}
	if action.ID == "" {
		fmt.Printf("✓ project %q %s actor %q lifecycle intent is durably %s (%s)\n",
			*projectID, *role, *actorID, lifecycle.State, lifecycle.HoldKind)
		return nil
	}
	fmt.Printf("✓ committed project %q %s actor %q %s intent and immutable action %s\n",
		*projectID, *role, *actorID, *operation, action.ID)
	return nil
}

type projectSessionBindingInput struct {
	ProjectID, Role, WorkerIdentity, SeatID                string
	LifecycleKey, ProfileID, WorkspaceRootID               string
	WorkspaceRelativePath                                  string
	HostID, StoreID, SessionID, PaneInstanceID, AgentRunID string
	TmuxServerDomainID                                     string
	TmuxServerInstanceID                                   string
	TargetEpoch                                            int64
}

// bindObservedProjectSession accepts only a Driver-owned lifecycle target that
// is presently observed under the daemon's exact cursor-domain identity. It is
// intentionally impossible to bind a raw tmux name, pane number, CWD, PID, or a
// session merely because it is nearby.
func bindObservedProjectSession(ctx context.Context, st *store.Store, port driver.DriverPort,
	in projectSessionBindingInput, now time.Time) (store.DriverSessionBinding, error) {
	managed := in.LifecycleKey != "" || in.TargetEpoch != 0 || in.ProfileID != "" ||
		in.WorkspaceRootID != "" || in.WorkspaceRelativePath != ""
	observed := in.HostID != "" || in.StoreID != "" || in.SessionID != "" || in.PaneInstanceID != "" ||
		in.AgentRunID != "" || in.TmuxServerDomainID != "" || in.TmuxServerInstanceID != ""
	managedComplete := in.LifecycleKey != "" && in.TargetEpoch > 0 && in.ProfileID != "" &&
		in.WorkspaceRootID != "" && in.WorkspaceRelativePath != ""
	observedComplete := in.HostID != "" && in.StoreID != "" && in.SessionID != "" &&
		in.PaneInstanceID != "" && in.AgentRunID != "" && in.TmuxServerDomainID != "" && in.TmuxServerInstanceID != ""
	if st == nil || port == nil || in.ProjectID == "" || in.WorkerIdentity == "" ||
		!bindableProjectRole(in.Role) || (in.Role == store.DriverReviewerRole) != (in.SeatID != "") ||
		managed == observed || managed && !managedComplete || observed && !observedComplete {
		return store.DriverSessionBinding{}, errors.New("project Driver binding is incomplete")
	}
	if in.Role == store.DriverInteractorRole || in.Role == store.DriverOrchestratorRole {
		route, err := st.GetProjectActor(ctx, in.ProjectID, in.Role)
		if err != nil || route.State != "active" || route.ActorID != in.WorkerIdentity {
			return store.DriverSessionBinding{}, fmt.Errorf("project %s has no active %s route for actor %s",
				in.ProjectID, in.Role, in.WorkerIdentity)
		}
	}
	meta, err := port.Metadata(ctx)
	if err != nil {
		return store.DriverSessionBinding{}, fmt.Errorf("read Driver metadata: %w", err)
	}
	var id driver.Identity
	if managed {
		presence, err := port.LifecycleTargetPresence(ctx, in.LifecycleKey, in.TargetEpoch)
		if err != nil {
			return store.DriverSessionBinding{}, fmt.Errorf("prove Driver lifecycle target: %w", err)
		}
		if presence.Presence != "present" || presence.Identity.LifecycleKey != in.LifecycleKey ||
			presence.Identity.TargetEpoch != in.TargetEpoch {
			return store.DriverSessionBinding{}, fmt.Errorf("Driver lifecycle target %s epoch %d is not exactly present (state %s)",
				in.LifecycleKey, in.TargetEpoch, presence.Presence)
		}
		id = presence.Identity
	} else {
		id = driver.Identity{HostID: in.HostID, StoreID: in.StoreID,
			TmuxServerDomainID: in.TmuxServerDomainID, TmuxServerInstanceID: in.TmuxServerInstanceID, SessionID: in.SessionID,
			PaneInstanceID: in.PaneInstanceID, AgentRunID: in.AgentRunID}
	}
	if meta.TmuxServer.DomainID == "" || meta.TmuxServer.InstanceID == "" ||
		id.HostID != meta.HostID || id.StoreID != meta.StoreID ||
		id.TmuxServerDomainID != meta.TmuxServer.DomainID || id.TmuxServerInstanceID != meta.TmuxServer.InstanceID || id.SessionID == "" ||
		id.PaneInstanceID == "" || id.AgentRunID == "" || id.TmuxServerInstanceID == "" {
		return store.DriverSessionBinding{}, errors.New("Driver lifecycle target identity is incomplete or outside the exact host/store/server domain")
	}
	snapshot, err := port.SnapshotSessions(ctx)
	if err != nil {
		return store.DriverSessionBinding{}, fmt.Errorf("read Driver session snapshot: %w", err)
	}
	if snapshot.HostID != meta.HostID || snapshot.StoreID != meta.StoreID {
		return store.DriverSessionBinding{}, errors.New("Driver snapshot cursor domain differs from metadata")
	}
	current := false
	for _, session := range snapshot.Sessions {
		got := session.Identity
		if got.SessionID != id.SessionID {
			continue
		}
		current = session.Lifecycle != "ended" && got.HostID == id.HostID && got.StoreID == id.StoreID &&
			got.TmuxServerInstanceID == id.TmuxServerInstanceID && got.PaneInstanceID == id.PaneInstanceID &&
			got.AgentRunID == id.AgentRunID
		if current {
			id.Provider, id.ConversationID = got.Provider, got.ConversationID
		}
		break
	}
	if !current {
		return store.DriverSessionBinding{}, errors.New("Driver lifecycle target is not the exact active session in the current snapshot")
	}
	return st.UpsertDriverSessionBinding(ctx, store.DriverSessionBinding{
		ProjectID: in.ProjectID, WorkerIdentity: in.WorkerIdentity, Role: in.Role, SeatID: in.SeatID,
		HostID: id.HostID, StoreID: id.StoreID, TmuxServerDomainID: id.TmuxServerDomainID,
		TmuxServerInstanceID: id.TmuxServerInstanceID, LifecycleOwnership: id.Ownership,
		LifecycleKey: in.LifecycleKey, TargetEpoch: in.TargetEpoch, ProfileID: in.ProfileID,
		WorkspaceRootID: in.WorkspaceRootID, WorkspaceRelativePath: in.WorkspaceRelativePath,
		SessionID: id.SessionID, PaneInstanceID: id.PaneInstanceID, AgentRunID: id.AgentRunID,
		Provider: id.Provider, ConversationID: id.ConversationID, ObservedAt: now,
	}, now)
}

func bindableProjectRole(role string) bool {
	return role == store.DriverInteractorRole || role == store.DriverOrchestratorRole || role == store.DriverReviewerRole
}

func runProjectList(ctx context.Context, st *store.Store, args []string) error {
	fs := flag.NewFlagSet("project list", flag.ContinueOnError)
	jsonOutput := fs.Bool("json", false, "emit machine-readable JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	projects, err := st.ListPortfolioProjects(ctx)
	if err != nil {
		return err
	}
	if *jsonOutput {
		return json.NewEncoder(os.Stdout).Encode(map[string]any{"schema_version": "flowbee.projects/v1", "projects": projects})
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tSTATE\tPRIORITY\tWEIGHT\tCAP\tNAME")
	for _, p := range projects {
		fmt.Fprintf(tw, "%s\t%s\t%d\t%d\t%d\t%s\n", p.ID, p.State, p.Priority,
			p.SchedulerWeight, p.ConcurrencyCap, p.Name)
	}
	return tw.Flush()
}

type workerAuthActivationStatus struct {
	Secure                      bool     `json:"secure"`
	InsecureOptIn               bool     `json:"insecure_opt_in"`
	SecretConfigured            bool     `json:"secret_configured"`
	LoopbackBypass              bool     `json:"loopback_bypass"`
	EnrolledIdentityCount       int      `json:"enrolled_identity_count"`
	CapacityCollectorID         string   `json:"capacity_collector_id,omitempty"`
	AttestationPolicyConfigured bool     `json:"attestation_policy_configured"`
	RuntimeConfigVerified       bool     `json:"runtime_config_verified"`
	RuntimePID                  int      `json:"runtime_pid,omitempty"`
	RuntimeUpdatedAt            string   `json:"runtime_updated_at,omitempty"`
	MissingIdentities           []string `json:"missing_identities"`
	MissingAttestations         []string `json:"missing_attestations"`
	Holds                       []string `json:"holds"`
}

func runProjectStatus(ctx context.Context, st *store.Store, cfg config.Config, args []string) error {
	fs := flag.NewFlagSet("project status", flag.ContinueOnError)
	projectID := fs.String("project-id", "default", "project id")
	jsonOutput := fs.Bool("json", false, "emit machine-readable JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	status, err := st.ProjectActivation(ctx, *projectID, time.Now(), projectActivationCapacityFreshness)
	if err != nil {
		return err
	}
	var runtimePosture *store.WorkerAuthRuntimePosture
	if posture, postureErr := st.WorkerAuthRuntimePosture(ctx); postureErr == nil {
		runtimePosture = &posture
	} else if !errors.Is(postureErr, sql.ErrNoRows) {
		return postureErr
	}
	workerAuth := projectWorkerAuthStatus(cfg, status, runtimePosture, time.Now())
	if *jsonOutput {
		return json.NewEncoder(os.Stdout).Encode(map[string]any{
			"schema_version": "flowbee.project-activation/v1", "activation": status,
			"worker_auth": workerAuth,
		})
	}
	fmt.Printf("PROJECT %s (%s)\n", status.Project.ID, status.Project.State)
	fmt.Printf("configured=%t live_ready=%t repos=%d enabled_seats=%d capacity_bound=%d routable=%d builder_targets=%d/%d reviewers=%d generation=%s\n",
		status.Configured, status.LiveReady, len(status.RepositoryIDs), status.EnabledSeats,
		status.CapacityBoundSeats, status.RoutableSeats, status.CurrentBuilderTargets, status.EnabledBuilderTargets,
		status.CurrentReviewerBindings, dashIfEmpty(status.ActiveCapacityGeneration))
	for _, actor := range status.Actors {
		fmt.Printf("%s actor=%s route=%s binding=%s current=%t session=%s\n", actor.Role,
			dashIfEmpty(actor.ActorID), dashIfEmpty(actor.RouteState), dashIfEmpty(actor.BindingID),
			actor.BindingCurrent, dashIfEmpty(actor.SessionID))
	}
	if len(status.Holds) > 0 {
		fmt.Printf("holds=%s\n", strings.Join(status.Holds, ","))
	}
	fmt.Printf("worker_auth_secure=%t secret=%t enrolled=%d loopback_bypass=%t insecure=%t collector=%s\n",
		workerAuth.Secure, workerAuth.SecretConfigured, workerAuth.EnrolledIdentityCount,
		workerAuth.LoopbackBypass, workerAuth.InsecureOptIn, dashIfEmpty(workerAuth.CapacityCollectorID))
	fmt.Printf("worker_attestations=%t runtime_verified=%t runtime_pid=%d runtime_updated=%s\n",
		workerAuth.AttestationPolicyConfigured, workerAuth.RuntimeConfigVerified, workerAuth.RuntimePID,
		dashIfEmpty(workerAuth.RuntimeUpdatedAt))
	if len(workerAuth.Holds) > 0 {
		fmt.Printf("worker_auth_holds=%s\n", strings.Join(workerAuth.Holds, ","))
	}
	return nil
}

func projectWorkerAuthStatus(cfg config.Config, activation store.ProjectActivationStatus,
	runtime *store.WorkerAuthRuntimePosture, now time.Time) workerAuthActivationStatus {
	out := workerAuthActivationStatus{SecretConfigured: strings.TrimSpace(cfg.WorkerAuthSecret) != "",
		LoopbackBypass: cfg.AuthLoopbackBypass, InsecureOptIn: strings.TrimSpace(os.Getenv("FLOWBEE_INSECURE")) != "",
		CapacityCollectorID:         strings.TrimSpace(os.Getenv("FLOWBEE_CAPACITY_COLLECTOR_ID")),
		AttestationPolicyConfigured: len(cfg.WorkerAttestations) > 0}
	enrolled := map[string]bool{}
	enrolledFamily := map[string]string{}
	for _, entry := range cfg.EnrolledIdentities {
		identity, family, _ := strings.Cut(strings.TrimSpace(entry), ":")
		if identity != "" {
			enrolled[identity] = true
			enrolledFamily[identity] = family
		}
	}
	out.EnrolledIdentityCount = len(enrolled)
	if !out.SecretConfigured {
		out.Holds = append(out.Holds, "worker_auth_secret_missing")
	}
	if out.InsecureOptIn {
		out.Holds = append(out.Holds, "flowbee_insecure_enabled")
	}
	if out.LoopbackBypass {
		out.Holds = append(out.Holds, "worker_auth_loopback_bypass_enabled")
	}
	if !out.AttestationPolicyConfigured {
		out.Holds = append(out.Holds, "worker_attestation_policy_missing")
	}
	required := append([]string(nil), activation.ReviewerIdentities...)
	if out.CapacityCollectorID == "" {
		out.Holds = append(out.Holds, "capacity_collector_identity_missing")
	} else {
		required = append(required, out.CapacityCollectorID)
	}
	seen := map[string]bool{}
	for _, identity := range required {
		if identity != "" && !seen[identity] && !enrolled[identity] {
			out.MissingIdentities = append(out.MissingIdentities, identity)
			seen[identity] = true
		}
	}
	if len(out.MissingIdentities) > 0 {
		out.Holds = append(out.Holds, "required_worker_identity_not_enrolled")
	}
	requiredReviewers := cfg.RequiredReviewers
	attached := map[string]bool{}
	for _, repoID := range activation.RepositoryIDs {
		attached[repoID] = true
	}
	for _, repo := range cfg.Repos {
		if attached[repo.ID] && repo.RequiredReviewers > requiredReviewers {
			requiredReviewers = repo.RequiredReviewers
		}
	}
	if requiredReviewers < 1 {
		requiredReviewers = 1
	}
	if len(activation.Reviewers) < requiredReviewers {
		out.Holds = append(out.Holds, "required_reviewer_identity_missing")
	}
	for _, reviewer := range activation.Reviewers {
		caps := cfg.WorkerAttestations[reviewer.WorkerIdentity]
		if !containsString(caps, "role:code_reviewer") {
			out.MissingAttestations = append(out.MissingAttestations,
				reviewer.WorkerIdentity+":role:code_reviewer")
		}
		family := enrolledFamily[reviewer.WorkerIdentity]
		if reviewer.ModelFamily == "" || family == "" || family != reviewer.ModelFamily {
			out.MissingAttestations = append(out.MissingAttestations,
				reviewer.WorkerIdentity+":model_family:"+reviewer.ModelFamily)
		}
	}
	if len(out.MissingAttestations) > 0 {
		out.Holds = append(out.Holds, "required_worker_attestation_missing_or_wrong_family")
	}
	if runtime == nil {
		out.Holds = append(out.Holds, "running_service_auth_posture_missing")
	} else {
		out.RuntimePID = runtime.PID
		out.RuntimeUpdatedAt = runtime.UpdatedAt.UTC().Format(time.RFC3339Nano)
		out.RuntimeConfigVerified = runtime.Fingerprint == workerAuthRuntimeFingerprint(cfg) &&
			!runtime.UpdatedAt.IsZero() && now.Sub(runtime.UpdatedAt) >= 0 &&
			now.Sub(runtime.UpdatedAt) <= workerAuthRuntimeFreshness
		if !out.RuntimeConfigVerified {
			out.Holds = append(out.Holds, "running_service_auth_posture_stale_or_mismatched")
		}
	}
	out.Secure = out.SecretConfigured && !out.InsecureOptIn && !out.LoopbackBypass &&
		out.AttestationPolicyConfigured && out.RuntimeConfigVerified && len(enrolled) > 0 &&
		out.CapacityCollectorID != "" && len(out.MissingIdentities) == 0 &&
		len(out.MissingAttestations) == 0 && len(activation.Reviewers) >= requiredReviewers
	return out
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if strings.TrimSpace(value) == want {
			return true
		}
	}
	return false
}

// workerAuthRuntimeFingerprint contains no secret material; it hashes the
// effective secret-presence, enrollment, attestation, collector, bypass and insecure
// posture so an offline CLI can prove it is inspecting the running service's
// actual configuration rather than a coincidentally green invoking shell.
func workerAuthRuntimeFingerprint(cfg config.Config) string {
	parts := []string{
		"secret_key_id=" + workerAuthSecretKeyID(cfg.WorkerAuthSecret),
		fmt.Sprintf("loopback_bypass=%t", cfg.AuthLoopbackBypass),
		fmt.Sprintf("insecure=%t", strings.TrimSpace(os.Getenv("FLOWBEE_INSECURE")) != ""),
		"collector=" + strings.TrimSpace(os.Getenv("FLOWBEE_CAPACITY_COLLECTOR_ID")),
	}
	enrolled := append([]string(nil), cfg.EnrolledIdentities...)
	sort.Strings(enrolled)
	for _, entry := range enrolled {
		parts = append(parts, "enrolled="+strings.TrimSpace(entry))
	}
	identities := make([]string, 0, len(cfg.WorkerAttestations))
	for identity := range cfg.WorkerAttestations {
		identities = append(identities, identity)
	}
	sort.Strings(identities)
	for _, identity := range identities {
		caps := append([]string(nil), cfg.WorkerAttestations[identity]...)
		sort.Strings(caps)
		parts = append(parts, "attest="+identity+"="+strings.Join(caps, ","))
	}
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// workerAuthSecretKeyID is a stable, domain-separated verifier for the exact
// active HMAC key. It lets offline status detect an operator shell using a
// different secret without persisting or displaying the secret itself.
func workerAuthSecretKeyID(secret string) string {
	if strings.TrimSpace(secret) == "" {
		return "absent"
	}
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte("flowbee-worker-auth-key-id/v1"))
	return hex.EncodeToString(mac.Sum(nil))
}

func projectCLIKey(parts ...string) string {
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return "flowbee-cli-" + hex.EncodeToString(sum[:16])
}
