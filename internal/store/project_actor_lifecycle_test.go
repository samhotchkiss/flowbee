package store_test

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/samhotchkiss/flowbee/internal/store"
	"github.com/samhotchkiss/flowbee/internal/testutil"
)

func actorLifecycleFixture(t *testing.T, actorID string) (*store.Store, context.Context, time.Time, store.ProjectActorRoute) {
	return actorLifecycleFixtureRole(t, store.DriverOrchestratorRole, actorID)
}

func actorLifecycleFixtureRole(t *testing.T, role, actorID string) (*store.Store, context.Context, time.Time, store.ProjectActorRoute) {
	t.Helper()
	st := testutil.NewStore(t)
	st.ProjectActorCredentialMaterializer = func(_, _, _, _ string, _ int64, _ time.Time) (string, error) {
		return "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", nil
	}
	ctx := context.Background()
	now := time.Date(2026, 7, 19, 20, 0, 0, 0, time.UTC)
	if _, err := st.CreatePortfolioProject(ctx, store.PortfolioProject{
		ID: "russ", Name: "Russ", Priority: 10, SchedulerWeight: 1, ConcurrencyCap: 2,
	}, now); err != nil {
		t.Fatal(err)
	}
	if err := st.RegisterRepo(ctx, store.Repo{ID: "russ-repo", Owner: "fixture", Repo: "russ", Active: true}); err != nil {
		t.Fatal(err)
	}
	if err := st.AddProjectRepo(ctx, "russ", "russ-repo", now); err != nil {
		t.Fatal(err)
	}
	route, err := st.RegisterProjectActor(ctx, store.ProjectActorRoute{
		ProjectID: "russ", Role: role, ActorID: actorID,
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	return st, ctx, now, route
}

func managedEnsureCommand(route store.ProjectActorRoute, key string) store.ProjectActorLifecycleCommand {
	return store.ProjectActorLifecycleCommand{
		ProjectID: route.ProjectID, Role: route.Role, ActorID: route.ActorID,
		ExpectedRouteStateVersion: int64(route.StateVersion), Operation: "ensure", IdempotencyKey: key,
		InstanceRef: "managed-driver", TargetHostID: "local", TargetStoreID: "managed-store",
		TargetServerDomainID: "managed_dedicated", TargetServerID: "server-managed",
		LifecycleOwnership: "driver_managed", LifecycleKey: "orchestrator-russ", TargetEpoch: 1,
		ProfileID: "codex_orchestrator", WorkspaceRootID: "russ-root", WorkspaceRelativePath: "russ",
	}
}

func externalAdoptCommand(route store.ProjectActorRoute, key string) store.ProjectActorLifecycleCommand {
	return store.ProjectActorLifecycleCommand{
		ProjectID: route.ProjectID, Role: route.Role, ActorID: route.ActorID,
		ExpectedRouteStateVersion: int64(route.StateVersion), Operation: "adopt", IdempotencyKey: key,
		InstanceRef: "external-driver", TargetHostID: "local", TargetStoreID: "external-store",
		TargetServerDomainID: "external_default", TargetServerID: "server-external",
		LifecycleOwnership: "external_observed", LifecycleKey: "external-russ-claude", TargetEpoch: 1,
		ProfileID: "claude-interactor", ExternalWatchID: "watch-russ-claude",
		ExpectedSessionID: "session-russ-claude", ExpectedPaneInstanceID: "pane-russ-claude",
		ExpectedAgentRunID:                   "run-russ-claude-1",
		ManagedRecoveryProfileID:             "claude_interactor_managed",
		ManagedRecoveryWorkspaceRootID:       "russ-root",
		ManagedRecoveryWorkspaceRelativePath: "russ",
	}
}

func terminalReceipt(action store.ProjectActorLifecycleAction, status string) store.ProjectActorLifecycleReceipt {
	return store.ProjectActorLifecycleReceipt{
		ActionID: action.ID, Operation: action.Operation, LifecycleKey: action.LifecycleKey,
		Status: status, ActionEpoch: action.ActionEpoch, TargetEpoch: action.TargetEpoch,
		LeaseID: action.LeaseID, LeaseEpoch: action.LeaseEpoch,
		TmuxServerDomainID: action.TargetServerDomainID, ExternalWatchID: action.ExternalWatchID,
	}
}

func actionExpectedIdentity(action store.ProjectActorLifecycleAction) store.ProjectActorLifecycleIdentity {
	return store.ProjectActorLifecycleIdentity{
		HostID: action.TargetHostID, StoreID: action.TargetStoreID,
		TmuxServerDomainID: action.TargetServerDomainID, TmuxServerInstanceID: action.TargetServerID,
		LifecycleOwnership: action.LifecycleOwnership, LifecycleKey: action.LifecycleKey,
		TargetEpoch: action.TargetEpoch, SessionID: action.ExpectedSessionID,
		PaneInstanceID: action.ExpectedPaneInstanceID, AgentRunID: action.ExpectedAgentRunID,
		Provider: "claude", ConversationID: "conversation-russ-claude",
	}
}

func projectAndAckActorReceipt(t *testing.T, st *store.Store, ctx context.Context,
	action store.ProjectActorLifecycleAction, owner string, receipt store.ProjectActorLifecycleReceipt, now time.Time) {
	t.Helper()
	if _, err := st.PersistProjectActorLifecycleReceipt(ctx, receipt, now); err != nil {
		t.Fatal(err)
	}
	if err := st.ProjectPersistedProjectActorLifecycleReceipt(ctx, action.ID, now); err != nil {
		t.Fatal(err)
	}
	if err := st.AcknowledgeProjectActorLifecycleAction(ctx, action.ID, owner, action.ActionEpoch, now); err != nil {
		t.Fatal(err)
	}
}

func seedCurrentActorDriverProjection(t *testing.T, st *store.Store, ctx context.Context,
	binding store.DriverSessionBinding, instanceRef string, now time.Time) {
	t.Helper()
	stamp := now.UTC().Format(time.RFC3339Nano)
	if _, err := st.DB.ExecContext(ctx, `INSERT INTO driver_instances
		(instance_ref,host_id,store_id,producer_boot_id,state,created_at,updated_at)
		VALUES (?,?,?,'actor-auth-boot','live',?,?)
		ON CONFLICT(instance_ref) DO UPDATE SET host_id=excluded.host_id,store_id=excluded.store_id,
		state='live',updated_at=excluded.updated_at`, instanceRef, binding.HostID, binding.StoreID, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB.ExecContext(ctx, `INSERT INTO driver_observation_cursors
		(store_id,instance_ref,cursor,high_store_seq,uncertainty_epoch,active,updated_at)
		VALUES (?,?,'actor-auth-cursor',10,0,1,?)
		ON CONFLICT(store_id) DO UPDATE SET instance_ref=excluded.instance_ref,cursor=excluded.cursor,
		high_store_seq=excluded.high_store_seq,active=1,updated_at=excluded.updated_at`,
		binding.StoreID, instanceRef, stamp); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB.ExecContext(ctx, `INSERT INTO driver_session_projections
		(store_id,session_id,host_id,pane_instance_id,agent_run_id,tmux_server_domain_id,
		 tmux_server_instance_id,lifecycle,phase,last_store_seq,as_of_cursor,source,updated_at)
		VALUES (?,?,?,?,?,?,?,'observing','working',10,'actor-auth-cursor','snapshot',?)
		ON CONFLICT(store_id,session_id) DO UPDATE SET host_id=excluded.host_id,
		pane_instance_id=excluded.pane_instance_id,agent_run_id=excluded.agent_run_id,
		tmux_server_domain_id=excluded.tmux_server_domain_id,
		tmux_server_instance_id=excluded.tmux_server_instance_id,lifecycle='observing',updated_at=excluded.updated_at`,
		binding.StoreID, binding.SessionID, binding.HostID, binding.PaneInstanceID, binding.AgentRunID,
		binding.TmuxServerDomainID, binding.TmuxServerInstanceID, stamp); err != nil {
		t.Fatal(err)
	}
}

func TestProjectActorLifecycleCommitIsAtomicImmutableAndIdempotent(t *testing.T) {
	st, ctx, now, route := actorLifecycleFixture(t, "russ-codex")
	command := managedEnsureCommand(route, "ensure-russ-orchestrator-v1")
	lifecycle, action, err := st.CommitProjectActorLifecycleIntent(ctx, command, now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if lifecycle.State != "awaiting_ensure" || lifecycle.CurrentActionID != action.ID || action.State != "pending" {
		t.Fatalf("intent/action were not atomically visible: lifecycle=%+v action=%+v", lifecycle, action)
	}
	if action.BootstrapFormat != "initial_prompt_utf8/v1" || action.BootstrapSHA256 == "" ||
		action.CredentialEnvelopeRef == "" || action.CredentialGeneration != 1 ||
		action.PresentationName != "russ-orchestrator" {
		t.Fatalf("Q3 action material incomplete: %+v", action)
	}
	var bootstrap struct {
		Format                     string   `json:"format"`
		ProjectID                  string   `json:"project_id"`
		ProjectName                string   `json:"project_name"`
		Role                       string   `json:"role"`
		ActorID                    string   `json:"actor_id"`
		ProfileID                  string   `json:"profile_id"`
		ModelFamily                string   `json:"model_family"`
		RepositoryIDs              []string `json:"repository_ids"`
		RoleCharterVersion         string   `json:"role_charter_version"`
		RoleCharterUTF8            string   `json:"role_charter_utf8"`
		RoleCharterSHA256          string   `json:"role_charter_sha256"`
		OperatingDisciplineVersion string   `json:"operating_discipline_version"`
		OperatingDisciplineUTF8    string   `json:"operating_discipline_utf8"`
		OperatingDisciplineSHA256  string   `json:"operating_discipline_sha256"`
		RoutingDisciplineUTF8      string   `json:"routing_discipline_utf8"`
		RoutingDisciplineSHA256    string   `json:"routing_discipline_sha256"`
		InitialHandoffUTF8         string   `json:"initial_handoff_utf8"`
		CredentialInstallRef       string   `json:"credential_install_ref"`
	}
	if err := json.Unmarshal([]byte(action.BootstrapPayload), &bootstrap); err != nil {
		t.Fatal(err)
	}
	hashText := func(value string) string {
		sum := sha256.Sum256([]byte(value))
		return "sha256:" + fmt.Sprintf("%x", sum)
	}
	if bootstrap.Format != "flowbee.actor-bootstrap/v1" || bootstrap.ProjectID != "russ" ||
		bootstrap.ProjectName != "Russ" || len(bootstrap.RepositoryIDs) != 1 || bootstrap.RepositoryIDs[0] != "russ-repo" ||
		bootstrap.Role != store.DriverOrchestratorRole || bootstrap.ActorID != "russ-codex" ||
		bootstrap.ProfileID != "codex_orchestrator" || bootstrap.ModelFamily != "codex" ||
		bootstrap.RoleCharterVersion != "v1" || bootstrap.OperatingDisciplineVersion != "v1" ||
		bootstrap.RoleCharterUTF8 == "" || bootstrap.OperatingDisciplineUTF8 == "" ||
		bootstrap.RoutingDisciplineUTF8 == "" || bootstrap.InitialHandoffUTF8 == "" ||
		bootstrap.CredentialInstallRef != action.CredentialInstallRef ||
		bootstrap.RoleCharterSHA256 != hashText(bootstrap.RoleCharterUTF8) ||
		bootstrap.OperatingDisciplineSHA256 != hashText(bootstrap.OperatingDisciplineUTF8) ||
		bootstrap.RoutingDisciplineSHA256 != hashText(bootstrap.RoutingDisciplineUTF8) {
		t.Fatalf("incomplete or unpinned actor bootstrap: %+v", bootstrap)
	}
	if _, err := st.DB.ExecContext(ctx, `UPDATE project_actor_lifecycle_actions
		SET bootstrap_payload='{}' WHERE id=?`, action.ID); err == nil {
		t.Fatal("immutable actor bootstrap bytes were rewritten")
	}
	if _, err := st.DB.ExecContext(ctx, `UPDATE project_actor_lifecycles
		SET credential_generation=0 WHERE project_id='russ' AND role='orchestrator'`); err == nil {
		t.Fatal("actor credential generation regressed")
	}
	var bindings int
	if err := st.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM driver_session_bindings WHERE project_id='russ'`).Scan(&bindings); err != nil {
		t.Fatal(err)
	}
	if bindings != 0 {
		t.Fatalf("commit-before-effect violated: got %d session bindings", bindings)
	}

	// A lost response retries with the caller's original expected version. The
	// payload-bound key must return the same durable action, not conflict.
	replayedLifecycle, replayedAction, err := st.CommitProjectActorLifecycleIntent(ctx, command, now.Add(2*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if replayedLifecycle.StateVersion != lifecycle.StateVersion || replayedAction.ID != action.ID {
		t.Fatalf("lost-response replay changed intent/action: lifecycle=%+v action=%+v", replayedLifecycle, replayedAction)
	}
	changed := command
	changed.ProfileID = "different-profile"
	if _, _, err := st.CommitProjectActorLifecycleIntent(ctx, changed, now.Add(3*time.Minute)); !errors.Is(err, store.ErrProjectActorLifecycleConflict) {
		t.Fatalf("changed body under idempotency key: got %v", err)
	}
	if _, err := st.DB.ExecContext(ctx, `UPDATE project_actor_lifecycle_actions SET target_store_id='forged' WHERE id=?`, action.ID); err == nil ||
		!strings.Contains(err.Error(), "immutable") {
		t.Fatalf("immutable action accepted mutation: %v", err)
	}
}

func TestProjectActorManagedEnsureFailsClosedWithoutExactProfileOrProjectContext(t *testing.T) {
	t.Run("wrong frozen profile", func(t *testing.T) {
		st, ctx, now, route := actorLifecycleFixture(t, "russ-codex")
		command := managedEnsureCommand(route, "wrong-profile")
		command.ProfileID = "grok_reviewer"
		if _, _, err := st.CommitProjectActorLifecycleIntent(ctx, command, now.Add(time.Minute)); err == nil ||
			!strings.Contains(err.Error(), "frozen lifecycle profile") {
			t.Fatalf("wrong profile/model was accepted: %v", err)
		}
	})
	t.Run("missing repository context", func(t *testing.T) {
		st := testutil.NewStore(t)
		st.ProjectActorCredentialMaterializer = func(_, _, _, _ string, _ int64, _ time.Time) (string, error) {
			return "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", nil
		}
		ctx := context.Background()
		now := time.Date(2026, 7, 19, 20, 0, 0, 0, time.UTC)
		if _, err := st.CreatePortfolioProject(ctx, store.PortfolioProject{ID: "empty", Name: "Empty"}, now); err != nil {
			t.Fatal(err)
		}
		route, err := st.RegisterProjectActor(ctx, store.ProjectActorRoute{
			ProjectID: "empty", Role: store.DriverOrchestratorRole, ActorID: "empty-codex"}, now)
		if err != nil {
			t.Fatal(err)
		}
		command := managedEnsureCommand(route, "missing-project-context")
		command.ProjectID, command.ActorID, command.LifecycleKey = "empty", "empty-codex", "empty-codex"
		if _, _, err := st.CommitProjectActorLifecycleIntent(ctx, command, now.Add(time.Minute)); err == nil ||
			!strings.Contains(err.Error(), "active repository set") {
			t.Fatalf("missing repository context was accepted: %v", err)
		}
		var actions int
		_ = st.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM project_actor_lifecycle_actions WHERE project_id='empty'`).Scan(&actions)
		if actions != 0 {
			t.Fatalf("failed context committed %d actions", actions)
		}
	})
}

func TestProjectActorLifecycleLegacyManagedActorStopsThenReplacesWithQ3Material(t *testing.T) {
	st, ctx, now, route := actorLifecycleFixture(t, "russ-codex")
	_, _, err := st.CommitProjectActorLifecycleIntent(ctx,
		managedEnsureCommand(route, "ensure-russ-orchestrator-v1"), now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	ensure, err := st.ClaimNextProjectActorLifecycleAction(ctx, "executor-a",
		now.Add(2*time.Minute), now.Add(7*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	ensureReceipt := terminalReceipt(ensure, "ensured")
	ensureReceipt.IdentityAfter = actionExpectedIdentity(ensure)
	ensureReceipt.IdentityAfter.SessionID = "session-russ-codex-v1"
	ensureReceipt.IdentityAfter.PaneInstanceID = "pane-russ-codex-v1"
	ensureReceipt.IdentityAfter.AgentRunID = "run-russ-codex-v1"
	ensureReceipt.IdentityAfter.Provider = "codex"
	projectAndAckActorReceipt(t, st, ctx, ensure, "executor-a", ensureReceipt, now.Add(3*time.Minute))

	// Simulate a row created before migration 0065. Such a managed actor has an
	// exact active binding but no Q3 material. The supported upgrade is an exact
	// legacy-shape Stop, followed by a fresh higher-epoch Q3 Ensure.
	if _, err := st.DB.ExecContext(ctx, `DROP TRIGGER trg_project_actor_q3_credential_no_regression`); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB.ExecContext(ctx, `UPDATE project_actor_lifecycles SET
		bootstrap_format='',bootstrap_payload='',bootstrap_sha256='',credential_install_ref='',
		credential_generation=0,credential_envelope_ref='',credential_payload_sha256='',
		credential_expires_at='',credential_envelope_deleted_at='',credential_revoked_at='',presentation_name=''
		WHERE project_id='russ' AND role='orchestrator'`); err != nil {
		t.Fatal(err)
	}
	legacy, err := st.CurrentProjectActorLifecycle(ctx, "russ", store.DriverOrchestratorRole)
	if err != nil {
		t.Fatal(err)
	}
	_, stopAction, err := st.CommitProjectActorLifecycleIntent(ctx, store.ProjectActorLifecycleCommand{
		ProjectID: "russ", Role: store.DriverOrchestratorRole, ActorID: "russ-codex",
		ExpectedRouteStateVersion: int64(route.StateVersion), ExpectedLifecycleStateVersion: legacy.StateVersion,
		Operation: "stop", IdempotencyKey: "stop-legacy-russ-codex", InstanceRef: "managed-driver",
	}, now.Add(4*time.Minute))
	if err != nil {
		t.Fatalf("legacy managed Stop must remain representable: %v", err)
	}
	if stopAction.CredentialGeneration != 0 || stopAction.CredentialEnvelopeRef != "" {
		t.Fatalf("legacy Stop unexpectedly invented Q3 material: %+v", stopAction)
	}
	stop, err := st.ClaimNextProjectActorLifecycleAction(ctx, "executor-b",
		now.Add(5*time.Minute), now.Add(10*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	stopReceipt := terminalReceipt(stop, "stopped")
	stopReceipt.IdentityBefore = ensureReceipt.IdentityAfter
	stopReceipt.AbsenceObservedAt = now.Add(6 * time.Minute).Format(time.RFC3339Nano)
	projectAndAckActorReceipt(t, st, ctx, stop, "executor-b", stopReceipt, now.Add(6*time.Minute))
	stopped, err := st.CurrentProjectActorLifecycle(ctx, "russ", store.DriverOrchestratorRole)
	if err != nil {
		t.Fatal(err)
	}
	replacement := managedEnsureCommand(route, "ensure-russ-orchestrator-v2")
	replacement.ExpectedLifecycleStateVersion = stopped.StateVersion
	replacement.TargetEpoch = 2
	upgraded, upgradedAction, err := st.CommitProjectActorLifecycleIntent(ctx, replacement, now.Add(7*time.Minute))
	if err != nil {
		t.Fatalf("higher-epoch Q3 replacement after legacy Stop failed: %v", err)
	}
	if upgraded.CredentialGeneration != 2 || upgradedAction.CredentialGeneration != 2 ||
		upgradedAction.BootstrapSHA256 == "" || upgradedAction.CredentialEnvelopeRef == "" {
		t.Fatalf("legacy replacement did not acquire fresh Q3 material: lifecycle=%+v action=%+v",
			upgraded, upgradedAction)
	}
}

func TestProjectActorLifecycleQ3ParentRejectsMetadataEmptyStopAction(t *testing.T) {
	st, ctx, now, route := actorLifecycleFixture(t, "russ-codex")
	_, _, err := st.CommitProjectActorLifecycleIntent(ctx,
		managedEnsureCommand(route, "ensure-russ-orchestrator-v1"), now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	ensure, err := st.ClaimNextProjectActorLifecycleAction(ctx, "executor-a",
		now.Add(2*time.Minute), now.Add(7*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	ensureReceipt := terminalReceipt(ensure, "ensured")
	ensureReceipt.IdentityAfter = actionExpectedIdentity(ensure)
	ensureReceipt.IdentityAfter.SessionID = "session-russ-codex-v1"
	ensureReceipt.IdentityAfter.PaneInstanceID = "pane-russ-codex-v1"
	ensureReceipt.IdentityAfter.AgentRunID = "run-russ-codex-v1"
	projectAndAckActorReceipt(t, st, ctx, ensure, "executor-a", ensureReceipt, now.Add(3*time.Minute))
	active, err := st.CurrentProjectActorLifecycle(ctx, "russ", store.DriverOrchestratorRole)
	if err != nil {
		t.Fatal(err)
	}
	_, stop, err := st.CommitProjectActorLifecycleIntent(ctx, store.ProjectActorLifecycleCommand{
		ProjectID: "russ", Role: store.DriverOrchestratorRole, ActorID: "russ-codex",
		ExpectedRouteStateVersion: int64(route.StateVersion), ExpectedLifecycleStateVersion: active.StateVersion,
		Operation: "stop", IdempotencyKey: "stop-q3-russ-codex", InstanceRef: "managed-driver",
	}, now.Add(4*time.Minute))
	if err != nil {
		t.Fatal(err)
	}

	// Copy the otherwise-valid Stop shape while stripping all Q3 metadata. The
	// 0065 legacy carve-out must consult the parent lifecycle generation and
	// reject this downgrade for an actor that was launched with Q3 credentials.
	rows, err := st.DB.QueryContext(ctx, `PRAGMA table_info(project_actor_lifecycle_actions)`)
	if err != nil {
		t.Fatal(err)
	}
	var columns, expressions []string
	for rows.Next() {
		var cid, notNull, primaryKey int
		var name, kind string
		var defaultValue sql.NullString
		if err := rows.Scan(&cid, &name, &kind, &notNull, &defaultValue, &primaryKey); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		columns = append(columns, `"`+name+`"`)
		expr := `"` + name + `"`
		switch name {
		case "id", "idempotency_key", "dedup_key":
			expr += ` || '-stripped'`
		case "state":
			expr = `'acknowledged'`
		case "bootstrap_format", "bootstrap_payload", "bootstrap_sha256", "credential_install_ref",
			"credential_envelope_ref", "credential_payload_sha256", "credential_expires_at", "presentation_name":
			expr = `''`
		case "credential_generation":
			expr = `0`
		}
		expressions = append(expressions, expr)
	}
	if err := rows.Close(); err != nil {
		t.Fatal(err)
	}
	query := fmt.Sprintf(`INSERT INTO project_actor_lifecycle_actions (%s) SELECT %s
		FROM project_actor_lifecycle_actions WHERE id=?`, strings.Join(columns, ","), strings.Join(expressions, ","))
	if _, err := st.DB.ExecContext(ctx, query, stop.ID); err == nil ||
		!strings.Contains(err.Error(), "invalid project actor Q3 action material shape") {
		t.Fatalf("Q3 parent accepted metadata-empty Stop copy: %v", err)
	}
}

func TestProjectActorCredentialAuthorityFencesScopeStopAndReplacement(t *testing.T) {
	st, ctx, now, route := actorLifecycleFixture(t, "russ-codex")
	_, _, err := st.CommitProjectActorLifecycleIntent(ctx,
		managedEnsureCommand(route, "ensure-russ-orchestrator-v1"), now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	ensure, err := st.ClaimNextProjectActorLifecycleAction(ctx, "executor-a",
		now.Add(2*time.Minute), now.Add(7*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	ensureReceipt := terminalReceipt(ensure, "ensured")
	ensureReceipt.IdentityAfter = actionExpectedIdentity(ensure)
	ensureReceipt.IdentityAfter.SessionID = "session-russ-codex-v1"
	ensureReceipt.IdentityAfter.PaneInstanceID = "pane-russ-codex-v1"
	ensureReceipt.IdentityAfter.AgentRunID = "run-russ-codex-v1"
	projectAndAckActorReceipt(t, st, ctx, ensure, "executor-a", ensureReceipt, now.Add(3*time.Minute))
	active, err := st.CurrentProjectActorLifecycle(ctx, "russ", store.DriverOrchestratorRole)
	if err != nil {
		t.Fatal(err)
	}
	st.ManagedSessionDriverFreshFor = time.Hour
	activeBinding, err := st.ActiveDriverSessionBinding(ctx, "russ", "russ-codex", store.DriverOrchestratorRole)
	if err != nil {
		t.Fatal(err)
	}
	seedCurrentActorDriverProjection(t, st, ctx, activeBinding, "managed-driver", now.Add(3*time.Minute))
	authorized := func(identity, project, role, credential string, generation int64, at time.Time) bool {
		return st.AuthorizeProjectActorCredential(ctx, identity, project, role, credential, generation, at)
	}
	if !authorized("russ-codex", "russ", store.DriverOrchestratorRole,
		active.CredentialEnvelopeRef, 1, now.Add(4*time.Minute)) {
		t.Fatal("active exact actor credential was rejected")
	}
	expiresAt, err := time.Parse(time.RFC3339Nano, active.CredentialExpiresAt)
	if err != nil || expiresAt.Year() != 9999 {
		t.Fatalf("actor credential is not lifecycle-bound/practically non-expiring: %q err=%v",
			active.CredentialExpiresAt, err)
	}
	stale := now.Add(-2 * time.Hour).Format(time.RFC3339Nano)
	if _, err := st.DB.ExecContext(ctx, `UPDATE driver_observation_cursors SET updated_at=?
		WHERE store_id=?`, stale, activeBinding.StoreID); err != nil {
		t.Fatal(err)
	}
	if authorized("russ-codex", "russ", store.DriverOrchestratorRole,
		active.CredentialEnvelopeRef, 1, now.Add(4*time.Minute)) {
		t.Fatal("stale Driver cursor remained actor credential authority")
	}
	seedCurrentActorDriverProjection(t, st, ctx, activeBinding, "managed-driver", now.Add(4*time.Minute))
	if _, err := st.DB.ExecContext(ctx, `UPDATE driver_session_projections SET pane_instance_id='reused-pane',
		agent_run_id='replacement-run',updated_at=? WHERE store_id=? AND session_id=?`,
		now.Add(4*time.Minute).Format(time.RFC3339Nano), activeBinding.StoreID, activeBinding.SessionID); err != nil {
		t.Fatal(err)
	}
	if authorized("russ-codex", "russ", store.DriverOrchestratorRole,
		active.CredentialEnvelopeRef, 1, now.Add(4*time.Minute)) {
		t.Fatal("pane/run replacement remained old actor credential authority")
	}
	seedCurrentActorDriverProjection(t, st, ctx, activeBinding, "managed-driver", now.Add(4*time.Minute))
	if _, err := st.DB.ExecContext(ctx, `UPDATE driver_observation_cursors SET active=0
		WHERE store_id=?`, activeBinding.StoreID); err != nil {
		t.Fatal(err)
	}
	if authorized("russ-codex", "russ", store.DriverOrchestratorRole,
		active.CredentialEnvelopeRef, 1, now.Add(4*time.Minute)) {
		t.Fatal("store reset/inactive cursor remained actor credential authority")
	}
	seedCurrentActorDriverProjection(t, st, ctx, activeBinding, "managed-driver", now.Add(48*time.Hour))
	if !authorized("russ-codex", "russ", store.DriverOrchestratorRole,
		active.CredentialEnvelopeRef, 1, now.Add(48*time.Hour)) {
		t.Fatal("live exact actor credential self-deauthorized after 24h")
	}
	for name, allowed := range map[string]bool{
		"spoofed identity": authorized("other", "russ", store.DriverOrchestratorRole, active.CredentialEnvelopeRef, 1, now.Add(4*time.Minute)),
		"cross project":    authorized("russ-codex", "other", store.DriverOrchestratorRole, active.CredentialEnvelopeRef, 1, now.Add(4*time.Minute)),
		"cross role":       authorized("russ-codex", "russ", store.DriverInteractorRole, active.CredentialEnvelopeRef, 1, now.Add(4*time.Minute)),
		"wrong issuance":   authorized("russ-codex", "russ", store.DriverOrchestratorRole, "forged", 1, now.Add(4*time.Minute)),
		"wrong generation": authorized("russ-codex", "russ", store.DriverOrchestratorRole, active.CredentialEnvelopeRef, 2, now.Add(4*time.Minute)),
	} {
		if allowed {
			t.Fatalf("%s actor credential was authorized", name)
		}
	}
	_, _, err = st.CommitProjectActorLifecycleIntent(ctx, store.ProjectActorLifecycleCommand{
		ProjectID: "russ", Role: store.DriverOrchestratorRole, ActorID: "russ-codex",
		ExpectedRouteStateVersion: int64(route.StateVersion), ExpectedLifecycleStateVersion: active.StateVersion,
		Operation: "stop", IdempotencyKey: "stop-russ-codex-auth", InstanceRef: "managed-driver",
	}, now.Add(5*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	stop, err := st.ClaimNextProjectActorLifecycleAction(ctx, "executor-b",
		now.Add(6*time.Minute), now.Add(11*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	stopReceipt := terminalReceipt(stop, "stopped")
	stopReceipt.IdentityBefore = ensureReceipt.IdentityAfter
	stopReceipt.AbsenceObservedAt = now.Add(7 * time.Minute).Format(time.RFC3339Nano)
	projectAndAckActorReceipt(t, st, ctx, stop, "executor-b", stopReceipt, now.Add(7*time.Minute))
	if authorized("russ-codex", "russ", store.DriverOrchestratorRole,
		active.CredentialEnvelopeRef, 1, now.Add(8*time.Minute)) {
		t.Fatal("stopped actor credential remained authoritative")
	}
	stopped, err := st.CurrentProjectActorLifecycle(ctx, "russ", store.DriverOrchestratorRole)
	if err != nil {
		t.Fatal(err)
	}
	replacement := managedEnsureCommand(route, "ensure-russ-orchestrator-v2-auth")
	replacement.ExpectedLifecycleStateVersion = stopped.StateVersion
	replacement.TargetEpoch = 2
	_, _, err = st.CommitProjectActorLifecycleIntent(ctx, replacement, now.Add(9*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	ensure2, err := st.ClaimNextProjectActorLifecycleAction(ctx, "executor-c",
		now.Add(10*time.Minute), now.Add(15*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	ensure2Receipt := terminalReceipt(ensure2, "ensured")
	ensure2Receipt.IdentityAfter = actionExpectedIdentity(ensure2)
	ensure2Receipt.IdentityAfter.SessionID = "session-russ-codex-v2"
	ensure2Receipt.IdentityAfter.PaneInstanceID = "pane-russ-codex-v2"
	ensure2Receipt.IdentityAfter.AgentRunID = "run-russ-codex-v2"
	projectAndAckActorReceipt(t, st, ctx, ensure2, "executor-c", ensure2Receipt, now.Add(11*time.Minute))
	active2, err := st.CurrentProjectActorLifecycle(ctx, "russ", store.DriverOrchestratorRole)
	if err != nil {
		t.Fatal(err)
	}
	activeBinding2, err := st.ActiveDriverSessionBinding(ctx, "russ", "russ-codex", store.DriverOrchestratorRole)
	if err != nil {
		t.Fatal(err)
	}
	seedCurrentActorDriverProjection(t, st, ctx, activeBinding2, "managed-driver", now.Add(11*time.Minute))
	if authorized("russ-codex", "russ", store.DriverOrchestratorRole,
		active.CredentialEnvelopeRef, 1, now.Add(12*time.Minute)) ||
		!authorized("russ-codex", "russ", store.DriverOrchestratorRole,
			active2.CredentialEnvelopeRef, 2, now.Add(12*time.Minute)) {
		t.Fatal("replacement did not fence old actor credential and authorize exact successor")
	}
}

func TestProjectActorLifecycleSchemaHasOnlyStablePaneAuthority(t *testing.T) {
	st, ctx, _, _ := actorLifecycleFixture(t, "russ-codex")
	rows, err := st.DB.QueryContext(ctx, `PRAGMA table_info(project_actor_lifecycle_actions)`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	columns := map[string]bool{}
	for rows.Next() {
		var cid, notNull, primaryKey int
		var name, kind string
		var defaultValue sql.NullString
		if err := rows.Scan(&cid, &name, &kind, &notNull, &defaultValue, &primaryKey); err != nil {
			t.Fatal(err)
		}
		columns[name] = true
	}
	if !columns["expected_pane_instance_id"] {
		t.Fatal("stable pane_instance_id fence is missing")
	}
	for _, forbidden := range []string{"pane_id", "pane_selector", "pane_name", "tmux_session_name", "cwd", "pid", "socket_path"} {
		if columns[forbidden] {
			t.Fatalf("raw process/tmux authority leaked into durable action schema: %s", forbidden)
		}
	}
}

func TestProjectActorLifecycleCrashAfterProjectionReplaysWithoutDuplicateEffect(t *testing.T) {
	st, ctx, now, route := actorLifecycleFixture(t, "russ-codex")
	_, pending, err := st.CommitProjectActorLifecycleIntent(ctx,
		managedEnsureCommand(route, "ensure-russ-orchestrator-v1"), now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := st.ClaimNextProjectActorLifecycleAction(ctx, "executor-a", now.Add(2*time.Minute), now.Add(7*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if claimed.ID != pending.ID || claimed.ActionEpoch != 1 || claimed.State != "delivering" {
		t.Fatalf("unexpected claim: %+v", claimed)
	}
	receipt := store.ProjectActorLifecycleReceipt{
		ActionID: claimed.ID, Operation: claimed.Operation, LifecycleKey: claimed.LifecycleKey,
		Status: "ensured", ActionEpoch: claimed.ActionEpoch, TargetEpoch: claimed.TargetEpoch,
		LeaseID: claimed.LeaseID, LeaseEpoch: claimed.LeaseEpoch, TmuxServerDomainID: claimed.TargetServerDomainID,
		IdentityAfter: store.ProjectActorLifecycleIdentity{
			HostID: claimed.TargetHostID, StoreID: claimed.TargetStoreID,
			TmuxServerDomainID: claimed.TargetServerDomainID, TmuxServerInstanceID: claimed.TargetServerID,
			LifecycleOwnership: claimed.LifecycleOwnership, LifecycleKey: claimed.LifecycleKey,
			TargetEpoch: claimed.TargetEpoch, SessionID: "session-russ-orch",
			PaneInstanceID: "pane-instance-russ-orch", AgentRunID: "agent-run-russ-orch",
			Provider: "codex", ConversationID: "conversation-russ-orch",
		},
	}
	stale := receipt
	stale.TmuxServerDomainID = "external_default"
	if _, err := st.PersistProjectActorLifecycleReceipt(ctx, stale, now.Add(3*time.Minute)); !errors.Is(err, store.ErrProjectActorActionStale) {
		t.Fatalf("stale domain receipt was accepted: %v", err)
	}
	persisted, err := st.PersistProjectActorLifecycleReceipt(ctx, receipt, now.Add(3*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if persisted.ID == "" {
		t.Fatal("receipt was not durably identified")
	}
	if _, err := st.PersistProjectActorLifecycleReceipt(ctx, receipt, now.Add(3*time.Minute)); err != nil {
		t.Fatalf("exact receipt replay failed: %v", err)
	}
	changedReceipt := receipt
	changedReceipt.Status = "different"
	if _, err := st.PersistProjectActorLifecycleReceipt(ctx, changedReceipt, now.Add(3*time.Minute)); !errors.Is(err, store.ErrProjectActorActionStale) {
		t.Fatalf("changed receipt replay was accepted: %v", err)
	}
	if err := st.ProjectPersistedProjectActorLifecycleReceipt(ctx, receipt.ActionID, now.Add(3*time.Minute)); err != nil {
		t.Fatal(err)
	}
	// Simulate a crash after projection but before the executor acknowledges its
	// outbox row. Re-folding the same receipt must be a no-op.
	if err := st.ProjectPersistedProjectActorLifecycleReceipt(ctx, receipt.ActionID, now.Add(4*time.Minute)); err != nil {
		t.Fatal(err)
	}
	var bindings, projectedEvents int
	if err := st.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM driver_session_bindings
		WHERE project_id='russ' AND worker_identity='russ-codex' AND role='orchestrator'`).Scan(&bindings); err != nil {
		t.Fatal(err)
	}
	if err := st.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM control_events
		WHERE project_id='russ' AND kind='project_actor_lifecycle_projected'`).Scan(&projectedEvents); err != nil {
		t.Fatal(err)
	}
	if bindings != 1 || projectedEvents != 1 {
		t.Fatalf("projection replay duplicated effect: bindings=%d events=%d", bindings, projectedEvents)
	}
	if err := st.AcknowledgeProjectActorLifecycleAction(ctx, claimed.ID, "executor-a", claimed.ActionEpoch,
		now.Add(5*time.Minute)); err != nil {
		t.Fatal(err)
	}
	lifecycle, err := st.CurrentProjectActorLifecycle(ctx, "russ", store.DriverOrchestratorRole)
	if err != nil {
		t.Fatal(err)
	}
	action, err := st.GetProjectActorLifecycleAction(ctx, claimed.ID)
	if err != nil {
		t.Fatal(err)
	}
	if lifecycle.State != "active" || lifecycle.CurrentActionID != "" || lifecycle.ActiveBindingID == "" || action.State != "acknowledged" {
		t.Fatalf("projection/ack did not converge: lifecycle=%+v action=%+v", lifecycle, action)
	}
	if _, err := st.PersistProjectActorLifecycleReceipt(ctx, receipt, now.Add(6*time.Minute)); err != nil {
		t.Fatalf("receipt replay after acknowledgement failed: %v", err)
	}
	if err := st.ProjectPersistedProjectActorLifecycleReceipt(ctx, receipt.ActionID, now.Add(6*time.Minute)); err != nil {
		t.Fatalf("projection replay after acknowledgement failed: %v", err)
	}
}

func TestProjectActorLifecycleExpiredClaimBecomesVerificationThenDeadLetter(t *testing.T) {
	st, ctx, now, route := actorLifecycleFixture(t, "russ-codex")
	_, pending, err := st.CommitProjectActorLifecycleIntent(ctx,
		managedEnsureCommand(route, "ensure-russ-orchestrator-v1"), now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := st.ClaimNextProjectActorLifecycleAction(ctx, "executor-a", now.Add(2*time.Minute), now.Add(3*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	report, err := st.ReconcileExpiredProjectActorLifecycleClaims(ctx, now.Add(4*time.Minute), 1, 10)
	if err != nil {
		t.Fatal(err)
	}
	if report.DeliveryUncertain != 1 || report.DeadLettered != 0 {
		t.Fatalf("delivery expiry was not made uncertain: %+v", report)
	}
	verification, err := st.ClaimNextProjectActorLifecycleVerification(ctx, "verifier-b",
		now.Add(5*time.Minute), now.Add(6*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if verification.ID != pending.ID || verification.ActionEpoch != claimed.ActionEpoch || verification.State != "verifying" {
		t.Fatalf("verification changed effect authority: claimed=%+v verification=%+v", claimed, verification)
	}
	report, err = st.ReconcileExpiredProjectActorLifecycleClaims(ctx, now.Add(7*time.Minute), 1, 10)
	if err != nil {
		t.Fatal(err)
	}
	if report.DeadLettered != 1 {
		t.Fatalf("bounded verification did not dead-letter: %+v", report)
	}
	action, err := st.GetProjectActorLifecycleAction(ctx, pending.ID)
	if err != nil {
		t.Fatal(err)
	}
	lifecycle, err := st.CurrentProjectActorLifecycle(ctx, "russ", store.DriverOrchestratorRole)
	if err != nil {
		t.Fatal(err)
	}
	var alerts int
	if err := st.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM control_alerts
		WHERE project_id='russ' AND kind='project_actor_lifecycle_stalled'`).Scan(&alerts); err != nil {
		t.Fatal(err)
	}
	if action.State != "dead_letter" || lifecycle.State != "failed" || !lifecycle.AlertPending || alerts != 1 {
		t.Fatalf("dead-letter was not visible/durable: action=%+v lifecycle=%+v alerts=%d", action, lifecycle, alerts)
	}
	if _, err := st.ClaimNextProjectActorLifecycleAction(ctx, "executor-c", now.Add(8*time.Minute), now.Add(9*time.Minute)); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("dead-letter became resendable: %v", err)
	}
}

func TestProjectActorLifecycleUncertainDeliveryDoesNotReturnToPending(t *testing.T) {
	st, ctx, now, route := actorLifecycleFixture(t, "russ-codex")
	_, pending, err := st.CommitProjectActorLifecycleIntent(ctx,
		managedEnsureCommand(route, "ensure-russ-orchestrator-v1"), now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := st.ClaimNextProjectActorLifecycleAction(ctx, "executor-a", now.Add(2*time.Minute), now.Add(7*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if err := st.MarkProjectActorLifecycleActionVerifying(ctx, pending.ID, "executor-a", claimed.ActionEpoch,
		now.Add(3*time.Minute), now.Add(13*time.Minute), "driver delivery uncertain"); err != nil {
		t.Fatal(err)
	}
	action, err := st.GetProjectActorLifecycleAction(ctx, pending.ID)
	if err != nil {
		t.Fatal(err)
	}
	lifecycle, err := st.CurrentProjectActorLifecycle(ctx, "russ", store.DriverOrchestratorRole)
	if err != nil {
		t.Fatal(err)
	}
	if action.State != "verifying" || lifecycle.State != "verifying_ensure" || lifecycle.StateDueAt.IsZero() {
		t.Fatalf("uncertain delivery was not made durable/visible: action=%+v lifecycle=%+v", action, lifecycle)
	}
	if _, err := st.ClaimNextProjectActorLifecycleAction(ctx, "executor-b", now.Add(20*time.Minute), now.Add(25*time.Minute)); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("uncertain action became resendable: %v", err)
	}
}

func TestProjectActorLifecycleRetriesOnlyCertifiedPreEffectFailure(t *testing.T) {
	st, ctx, now, route := actorLifecycleFixture(t, "russ-codex")
	_, pending, err := st.CommitProjectActorLifecycleIntent(ctx,
		managedEnsureCommand(route, "ensure-russ-orchestrator-v1"), now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := st.ClaimNextProjectActorLifecycleAction(ctx, "executor-a", now.Add(2*time.Minute), now.Add(7*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if err := st.RecordProjectActorLifecyclePreEffectFailure(ctx, pending.ID, "executor-a", claimed.ActionEpoch,
		"route denied before submission", now.Add(3*time.Minute), now.Add(4*time.Minute), 3); err != nil {
		t.Fatal(err)
	}
	action, err := st.GetProjectActorLifecycleAction(ctx, pending.ID)
	if err != nil {
		t.Fatal(err)
	}
	lifecycle, err := st.CurrentProjectActorLifecycle(ctx, "russ", store.DriverOrchestratorRole)
	if err != nil {
		t.Fatal(err)
	}
	if action.State != "pending" || action.RecoveryCount != 1 || action.ClaimOwner != "" ||
		lifecycle.State != "awaiting_ensure" || lifecycle.StateDueAt.IsZero() {
		t.Fatalf("pre-effect retry was not durably scheduled: action=%+v lifecycle=%+v", action, lifecycle)
	}
}

func TestProjectActorLifecycleReconcilerRepairsMissingMaterialization(t *testing.T) {
	st, ctx, now, route := actorLifecycleFixture(t, "russ-codex")
	lifecycle, action, err := st.CommitProjectActorLifecycleIntent(ctx,
		managedEnsureCommand(route, "ensure-russ-orchestrator-v1"), now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	// Model a cancelled pre-effect action whose durable intent survived. The
	// reconciler must re-arm the exact immutable action. It may not mint another
	// action that reuses the one-shot credential envelope or target epoch.
	stamp := now.Add(2 * time.Minute).Format(time.RFC3339Nano)
	if _, err := st.DB.ExecContext(ctx, `UPDATE project_actor_lifecycle_actions
		SET state='cancelled_superseded',updated_at=? WHERE id=?`, stamp, action.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB.ExecContext(ctx, `UPDATE project_actor_lifecycles SET current_action_id='',
		state_due_at=?,updated_at=? WHERE project_id=? AND role=? AND actor_id=?`, stamp, stamp,
		lifecycle.ProjectID, lifecycle.Role, lifecycle.ActorID); err != nil {
		t.Fatal(err)
	}
	report, err := st.ReconcileProjectActorLifecycleActions(ctx, now.Add(3*time.Minute), 10)
	if err != nil {
		t.Fatal(err)
	}
	if report.Materialized != 1 {
		t.Fatalf("expected one recovered action, got %+v", report)
	}
	current, err := st.CurrentProjectActorLifecycle(ctx, "russ", store.DriverOrchestratorRole)
	if err != nil {
		t.Fatal(err)
	}
	recovered, err := st.GetProjectActorLifecycleAction(ctx, current.CurrentActionID)
	if err != nil {
		t.Fatal(err)
	}
	if recovered.ID != action.ID || recovered.ActionGeneration != action.ActionGeneration ||
		recovered.PayloadSHA != action.PayloadSHA || recovered.BootstrapSHA256 != action.BootstrapSHA256 ||
		recovered.CredentialEnvelopeRef != action.CredentialEnvelopeRef || recovered.State != "pending" {
		t.Fatalf("reconciler did not re-arm the exact immutable pre-effect action: old=%+v new=%+v", action, recovered)
	}
}

func TestProjectActorLifecycleExternalAdoptThenRelease(t *testing.T) {
	st, ctx, now, route := actorLifecycleFixtureRole(t, store.DriverInteractorRole, "russ-claude")
	_, _, err := st.CommitProjectActorLifecycleIntent(ctx,
		externalAdoptCommand(route, "adopt-russ-claude-v1"), now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	adopt, err := st.ClaimNextProjectActorLifecycleAction(ctx, "executor-a", now.Add(2*time.Minute), now.Add(7*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	adoptReceipt := terminalReceipt(adopt, "adopted")
	adoptReceipt.IdentityAfter = actionExpectedIdentity(adopt)
	projectAndAckActorReceipt(t, st, ctx, adopt, "executor-a", adoptReceipt, now.Add(3*time.Minute))
	active, err := st.CurrentProjectActorLifecycle(ctx, "russ", store.DriverInteractorRole)
	if err != nil {
		t.Fatal(err)
	}
	if active.State != "active" || active.ExternalWatchID != "watch-russ-claude" || active.ActiveBindingID == "" {
		t.Fatalf("external adopt did not activate exact watch binding: %+v", active)
	}

	releaseCommand := store.ProjectActorLifecycleCommand{
		ProjectID: "russ", Role: store.DriverInteractorRole, ActorID: "russ-claude",
		ExpectedRouteStateVersion: int64(route.StateVersion), ExpectedLifecycleStateVersion: active.StateVersion,
		Operation: "release", IdempotencyKey: "release-russ-claude-v1", InstanceRef: "external-driver",
	}
	if _, _, err := st.CommitProjectActorLifecycleIntent(ctx, releaseCommand, now.Add(4*time.Minute)); err != nil {
		t.Fatal(err)
	}
	release, err := st.ClaimNextProjectActorLifecycleAction(ctx, "executor-b", now.Add(5*time.Minute), now.Add(10*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	releaseReceipt := terminalReceipt(release, "released")
	releaseReceipt.IdentityBefore = actionExpectedIdentity(release)
	projectAndAckActorReceipt(t, st, ctx, release, "executor-b", releaseReceipt, now.Add(6*time.Minute))
	retired, err := st.CurrentProjectActorLifecycle(ctx, "russ", store.DriverInteractorRole)
	if err != nil {
		t.Fatal(err)
	}
	if retired.State != "released" || retired.ActiveBindingID != "" {
		t.Fatalf("external release did not retire binding: %+v", retired)
	}
	var activeBindings int
	if err := st.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM driver_session_bindings
		WHERE project_id='russ' AND worker_identity='russ-claude' AND role='interactor' AND state='active'`).Scan(&activeBindings); err != nil {
		t.Fatal(err)
	}
	if activeBindings != 0 {
		t.Fatalf("release left %d active external bindings", activeBindings)
	}
}

func TestProjectActorLifecycleExternalReattachFencesPriorRun(t *testing.T) {
	st, ctx, now, route := actorLifecycleFixtureRole(t, store.DriverInteractorRole, "russ-claude")
	_, _, err := st.CommitProjectActorLifecycleIntent(ctx,
		externalAdoptCommand(route, "adopt-russ-claude-v1"), now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	adopt, err := st.ClaimNextProjectActorLifecycleAction(ctx, "executor-a", now.Add(2*time.Minute), now.Add(7*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	adoptReceipt := terminalReceipt(adopt, "adopted")
	adoptReceipt.IdentityAfter = actionExpectedIdentity(adopt)
	projectAndAckActorReceipt(t, st, ctx, adopt, "executor-a", adoptReceipt, now.Add(3*time.Minute))
	active, err := st.CurrentProjectActorLifecycle(ctx, "russ", store.DriverInteractorRole)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.CommitProjectActorLifecycleIntent(ctx, store.ProjectActorLifecycleCommand{
		ProjectID: "russ", Role: store.DriverInteractorRole, ActorID: "russ-claude",
		ExpectedRouteStateVersion: int64(route.StateVersion), ExpectedLifecycleStateVersion: active.StateVersion,
		Operation: "reattach", IdempotencyKey: "reattach-russ-claude-v2", InstanceRef: "external-driver",
	}, now.Add(4*time.Minute)); err != nil {
		t.Fatal(err)
	}
	reattach, err := st.ClaimNextProjectActorLifecycleAction(ctx, "executor-b", now.Add(5*time.Minute), now.Add(10*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	receipt := terminalReceipt(reattach, "reattached")
	receipt.IdentityBefore = actionExpectedIdentity(reattach)
	receipt.IdentityAfter = actionExpectedIdentity(reattach)
	receipt.IdentityAfter.SessionID = "session-russ-claude-v2"
	receipt.IdentityAfter.PaneInstanceID = "pane-russ-claude-v2"
	receipt.IdentityAfter.AgentRunID = "run-russ-claude-2"
	staleProjection := store.ProjectActorLifecycleReceiptProjection{
		ActionID: receipt.ActionID, Operation: receipt.Operation, LifecycleKey: receipt.LifecycleKey,
		Status: receipt.Status, ActionEpoch: receipt.ActionEpoch, TargetEpoch: receipt.TargetEpoch,
		LeaseID: receipt.LeaseID, LeaseEpoch: receipt.LeaseEpoch,
		TmuxServerDomainID: receipt.TmuxServerDomainID, ExternalWatchID: receipt.ExternalWatchID,
		IdentityBefore: receipt.IdentityBefore, IdentityAfter: receipt.IdentityAfter,
	}
	staleProjection.IdentityBefore.AgentRunID = "run-russ-claude-stale"
	if err := st.ProjectProjectActorLifecycleResult(ctx, staleProjection, now.Add(6*time.Minute)); !errors.Is(err, store.ErrProjectActorActionStale) {
		t.Fatalf("reattach accepted stale prior agent run: %v", err)
	}
	projectAndAckActorReceipt(t, st, ctx, reattach, "executor-b", receipt, now.Add(7*time.Minute))
	binding, err := st.ActiveDriverSessionBinding(ctx, "russ", "russ-claude", store.DriverInteractorRole)
	if err != nil {
		t.Fatal(err)
	}
	if binding.AgentRunID != "run-russ-claude-2" || binding.ExternalWatchID != "watch-russ-claude" || binding.BindingEpoch != 2 {
		t.Fatalf("reattach did not fence old run/watch authority: %+v", binding)
	}
}

func TestProjectActorLifecycleReplacementHeldUntilPriorRelease(t *testing.T) {
	st, ctx, now, route := actorLifecycleFixtureRole(t, store.DriverInteractorRole, "russ-claude")
	_, _, err := st.CommitProjectActorLifecycleIntent(ctx,
		externalAdoptCommand(route, "adopt-russ-claude-v1"), now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	adopt, err := st.ClaimNextProjectActorLifecycleAction(ctx, "executor-a", now.Add(2*time.Minute), now.Add(7*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	adoptReceipt := terminalReceipt(adopt, "adopted")
	adoptReceipt.IdentityAfter = actionExpectedIdentity(adopt)
	projectAndAckActorReceipt(t, st, ctx, adopt, "executor-a", adoptReceipt, now.Add(3*time.Minute))
	prior, err := st.GetProjectActorLifecycle(ctx, "russ", store.DriverInteractorRole, "russ-claude")
	if err != nil {
		t.Fatal(err)
	}

	nextRoute, err := st.RegisterProjectActor(ctx, store.ProjectActorRoute{
		ProjectID: "russ", Role: store.DriverInteractorRole, ActorID: "russ-claude-v2",
	}, now.Add(4*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	nextCommand := externalAdoptCommand(nextRoute, "adopt-russ-claude-v2")
	nextCommand.LifecycleKey = "external-russ-claude-v2"
	nextCommand.ExternalWatchID = "watch-russ-claude-v2"
	nextCommand.ExpectedSessionID = "session-russ-claude-v2"
	nextCommand.ExpectedPaneInstanceID = "pane-russ-claude-v2"
	nextCommand.ExpectedAgentRunID = "run-russ-claude-v2"
	held, action, err := st.CommitProjectActorLifecycleIntent(ctx, nextCommand, now.Add(5*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if held.State != "held" || held.HoldKind != "prior_actor_retirement" || action.ID != "" {
		t.Fatalf("replacement was not held before prior retirement: lifecycle=%+v action=%+v", held, action)
	}

	if _, _, err := st.CommitProjectActorLifecycleIntent(ctx, store.ProjectActorLifecycleCommand{
		ProjectID: "russ", Role: store.DriverInteractorRole, ActorID: "russ-claude",
		ExpectedRouteStateVersion: int64(nextRoute.StateVersion), ExpectedLifecycleStateVersion: prior.StateVersion,
		Operation: "release", IdempotencyKey: "release-prior-russ-claude", InstanceRef: "external-driver",
	}, now.Add(6*time.Minute)); err != nil {
		t.Fatal(err)
	}
	release, err := st.ClaimNextProjectActorLifecycleAction(ctx, "executor-b", now.Add(7*time.Minute), now.Add(12*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	releaseReceipt := terminalReceipt(release, "released")
	releaseReceipt.IdentityBefore = actionExpectedIdentity(release)
	projectAndAckActorReceipt(t, st, ctx, release, "executor-b", releaseReceipt, now.Add(8*time.Minute))
	report, err := st.ReconcileProjectActorLifecycleActions(ctx, now.Add(10*time.Minute), 10)
	if err != nil {
		t.Fatal(err)
	}
	if report.Resumed != 1 || report.Materialized != 1 {
		t.Fatalf("replacement did not resume after prior release: %+v", report)
	}
	current, err := st.CurrentProjectActorLifecycle(ctx, "russ", store.DriverInteractorRole)
	if err != nil {
		t.Fatal(err)
	}
	if current.ActorID != "russ-claude-v2" || current.State != "awaiting_adopt" || current.CurrentActionID == "" {
		t.Fatalf("replacement materialization is not current/visible: %+v", current)
	}
}
