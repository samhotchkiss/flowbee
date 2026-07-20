package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/samhotchkiss/flowbee/internal/api"
	"github.com/samhotchkiss/flowbee/internal/auth"
	"github.com/samhotchkiss/flowbee/internal/clock"
	"github.com/samhotchkiss/flowbee/internal/store"
	"github.com/samhotchkiss/flowbee/internal/testutil"
	"github.com/samhotchkiss/flowbee/internal/ulid"
	"github.com/samhotchkiss/flowbee/internal/workintent"
)

const (
	contractAPISecret = "contract-api-worker-secret"
	contractAPISHA    = "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
)

type contractAPIFixture struct {
	server  *httptest.Server
	store   *store.Store
	authn   *auth.BearerAuth
	intent  store.WorkIntent
	binding store.DriverSessionBinding
	body    map[string]any
}

func newContractAPIFixture(t *testing.T, orchestrator, sourceMessage, slug string) contractAPIFixture {
	t.Helper()
	ctx := context.Background()
	st := testutil.NewStore(t)
	st.EnableDriverControlOrigin = true // future-capability fake route
	now := time.Date(2026, 7, 19, 22, 0, 0, 0, time.UTC)
	if err := st.RegisterRepo(ctx, store.Repo{ID: "russ", Owner: "fixture", Repo: "russ", Active: true}); err != nil {
		t.Fatal(err)
	}
	storeID := "contract-api-store-" + orchestrator
	instanceRef := "contract-api-instance-" + orchestrator
	stamp := now.Format(time.RFC3339Nano)
	if _, err := st.DB.ExecContext(ctx, `INSERT INTO driver_instances
		(instance_ref,host_id,store_id,producer_boot_id,state,created_at,updated_at)
		VALUES (?,?,?,'boot','live',?,?)`, instanceRef, "contract-api-host", storeID, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB.ExecContext(ctx, `INSERT INTO driver_observation_cursors
		(store_id,instance_ref,cursor,high_store_seq,uncertainty_epoch,active,updated_at)
		VALUES (?,?,'cursor-10',10,0,1,?)`, storeID, instanceRef, stamp); err != nil {
		t.Fatal(err)
	}
	bindings := []store.DriverSessionBinding{
		{
			WorkerIdentity: store.DriverControlIdentity, Role: store.DriverControlRole,
			HostID: "contract-api-host", StoreID: storeID, TmuxServerDomainID: "flowbee", TmuxServerInstanceID: "contract-api-server",
			LifecycleOwnership: "driver_managed",
			LifecycleKey:       "flowbee-control", TargetEpoch: 1, ProfileID: "flowbee-control",
			WorkspaceRootID: "workspace-root", WorkspaceRelativePath: "flowbee",
			SessionID: "flowbee-control-session", PaneInstanceID: "flowbee-control-pane",
			AgentRunID: "flowbee-control-run",
		},
		{
			WorkerIdentity: orchestrator, Role: store.DriverOrchestratorRole,
			HostID: "contract-api-host", StoreID: storeID, TmuxServerDomainID: "flowbee", TmuxServerInstanceID: "contract-api-server",
			LifecycleOwnership: "driver_managed",
			LifecycleKey:       "orchestrator-" + orchestrator, TargetEpoch: 1, ProfileID: "orchestrator",
			WorkspaceRootID: "workspace-root", WorkspaceRelativePath: "repo",
			SessionID:      "orchestrator-session-" + orchestrator,
			PaneInstanceID: "orchestrator-pane-" + orchestrator,
			AgentRunID:     "orchestrator-agent-" + orchestrator,
		},
	}
	var recipient store.DriverSessionBinding
	for _, binding := range bindings {
		got, err := st.UpsertDriverSessionBinding(ctx, binding, now)
		if err != nil {
			t.Fatal(err)
		}
		if binding.Role == store.DriverOrchestratorRole {
			recipient = got
		}
	}
	intent, err := st.CreateWorkIntent(ctx, store.CreateWorkIntentInput{
		ProjectID: "default", SourceConversationID: "thread-" + sourceMessage,
		SourceMessageID: sourceMessage, SourceMessageVersion: 1,
		InteractorIncarnationID: "interactor-contract-api", Title: "Build durable handoff",
		ArtifactRef: "artifact://intent/" + sourceMessage, ArtifactSHA256: contractAPISHA,
		IntentVersion: 1, DefinitionComplete: true, OwnerActorID: "interactor",
		OrchestratorRegistration: orchestrator,
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.ReconcileWorkIntents(ctx, now.Add(time.Minute), 10*time.Minute); err != nil {
		t.Fatal(err)
	}
	if _, err := st.ReconcileWorkIntents(ctx, now.Add(2*time.Minute), 10*time.Minute); err != nil {
		t.Fatal(err)
	}
	intent, err = st.GetWorkIntent(ctx, "default", intent.ID)
	if err != nil || intent.DeliveryActionID == "" {
		t.Fatalf("delivery projection intent=%+v err=%v", intent, err)
	}
	ackAt := now.Add(3 * time.Minute).Format(time.RFC3339Nano)
	if _, err := st.DB.ExecContext(ctx, `UPDATE work_intent_actions SET state='acknowledged',
		acknowledged_at=?,action_epoch=1 WHERE id=?`, ackAt, intent.DeliveryActionID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB.ExecContext(ctx, `UPDATE work_intents SET state='orchestrating',
		state_version=state_version+1,route_acknowledged_at=?,route_due_at='' WHERE id=?`,
		ackAt, intent.ID); err != nil {
		t.Fatal(err)
	}
	intent, err = st.GetWorkIntent(ctx, "default", intent.ID)
	if err != nil || intent.State != workintent.StateOrchestrating {
		t.Fatalf("orchestrating intent=%+v err=%v", intent, err)
	}
	contract := store.WorkIntentEpicContract{
		Slug: slug, Title: "Durable review handoff", Repositories: []string{"russ"},
		DeliveryRepo: "russ", SpecPath: "epics/" + slug + ".md",
		Scope: []string{"internal/flowbee/**"}, IssueRefs: []string{"#4950", "#4951"},
		Acceptance: []string{"interrupted review dispatch self-heals"},
	}
	contractHash, err := store.WorkIntentEpicContractSHA256(contract)
	if err != nil {
		t.Fatal(err)
	}
	submissionKey, err := workintent.AdmissionKey(workintent.Intent{
		ID: intent.ID, ProjectID: intent.ProjectID, Version: intent.IntentVersion,
	})
	if err != nil {
		t.Fatal(err)
	}
	body := map[string]any{
		"project_id": "default", "intent_version": 1,
		"expected_state_version": intent.StateVersion, "source_artifact_sha256": intent.ArtifactSHA256,
		"contract_version": 1, "contract_ref": "artifact://epic-contract/" + sourceMessage,
		"contract_sha256": contractHash, "contract": contract,
		"orchestrator_binding_id": recipient.BindingID, "submission_key": submissionKey,
	}
	authn := auth.NewBearer([]byte(contractAPISecret), []string{orchestrator}, false)
	clk := clock.NewFake(now.Add(4 * time.Minute))
	srv := api.New(st, clk, ulid.NewMinter(nil), api.Config{Authenticator: authn}, "contract-api-test")
	ts := httptest.NewServer(srv.PrivateHandler())
	t.Cleanup(ts.Close)
	return contractAPIFixture{server: ts, store: st, authn: authn, intent: intent, binding: recipient, body: body}
}

func contractAPIRequest(t *testing.T, fixture contractAPIFixture, body map[string]any, key, token string) (*http.Response, map[string]any) {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodPost,
		fixture.server.URL+"/v1/work-intents/"+fixture.intent.ID+"/epic-contract", bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := fixture.server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	decoded := map[string]any{}
	_ = json.NewDecoder(resp.Body).Decode(&decoded)
	return resp, decoded
}

func TestWorkIntentEpicContractAPIRequiresAuthAndIsExactlyIdempotent(t *testing.T) {
	fixture := newContractAPIFixture(t, "orchestrator-api", "contract-api-replay", "contract-api-replay")
	key := fixture.body["submission_key"].(string)

	resp, _ := contractAPIRequest(t, fixture, fixture.body, key, "")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("anonymous contract status=%d want 401", resp.StatusCode)
	}
	token := fixture.authn.Mint("orchestrator-api")
	resp, first := contractAPIRequest(t, fixture, fixture.body, key, token)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("first submission status=%d body=%v", resp.StatusCode, first)
	}
	firstContract, _ := first["epic_contract"].(map[string]any)
	if first["schema_version"] != "flowbee.work-intent-epic-contract/v1" ||
		firstContract["state"] != "prepared" || firstContract["orchestrator_binding_id"] != fixture.binding.BindingID {
		t.Fatalf("first contract response=%v", first)
	}

	// Simulate a lost HTTP acknowledgement: the exact same authenticated body
	// and idempotency key must return the existing immutable row.
	resp, replay := contractAPIRequest(t, fixture, fixture.body, key, token)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("replay status=%d body=%v", resp.StatusCode, replay)
	}
	replayContract, _ := replay["epic_contract"].(map[string]any)
	if replayContract["id"] != firstContract["id"] {
		t.Fatalf("replay changed contract identity: first=%v replay=%v", firstContract, replayContract)
	}
	var count int
	if err := fixture.store.DB.QueryRow(`SELECT COUNT(*) FROM work_intent_epic_contracts
		WHERE project_id=? AND work_intent_id=?`, "default", fixture.intent.ID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("exact replay created %d contracts, want 1", count)
	}
}

func TestProjectActorCredentialCanOnlySubmitItsExactProjectWorkIntentContract(t *testing.T) {
	fixture := newContractAPIFixture(t, "orchestrator-actor-auth", "contract-actor-auth", "contract-actor-auth")
	fixture.server.Close()
	ctx := context.Background()
	now := time.Date(2026, 7, 19, 22, 0, 0, 0, time.UTC)
	route, err := fixture.store.RegisterProjectActor(ctx, store.ProjectActorRoute{
		ProjectID: "default", Role: store.DriverOrchestratorRole, ActorID: "orchestrator-actor-auth",
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	fixture.store.ProjectActorCredentialMaterializer = func(_, _, _, _ string, _ int64, _ time.Time) (string, error) {
		return "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", nil
	}
	_, action, err := fixture.store.CommitProjectActorLifecycleIntent(ctx, store.ProjectActorLifecycleCommand{
		ProjectID: "default", Role: store.DriverOrchestratorRole, ActorID: "orchestrator-actor-auth",
		ExpectedRouteStateVersion: int64(route.StateVersion), Operation: "ensure",
		IdempotencyKey: "ensure-orchestrator-actor-auth", InstanceRef: "contract-api-instance-orchestrator-actor-auth",
		TargetHostID: fixture.binding.HostID, TargetStoreID: fixture.binding.StoreID,
		TargetServerDomainID: fixture.binding.TmuxServerDomainID, TargetServerID: fixture.binding.TmuxServerInstanceID,
		LifecycleOwnership: "driver_managed", LifecycleKey: fixture.binding.LifecycleKey, TargetEpoch: 1,
		ProfileID: "codex_orchestrator", WorkspaceRootID: "workspace-root", WorkspaceRelativePath: "repo",
	}, now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	stamp := now.Add(2 * time.Minute).Format(time.RFC3339Nano)
	if _, err := fixture.store.DB.ExecContext(ctx, `UPDATE project_actor_lifecycle_actions
		SET state='acknowledged',acknowledged_at=?,updated_at=? WHERE id=?`, stamp, stamp, action.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.store.DB.ExecContext(ctx, `UPDATE project_actor_lifecycles
		SET state='active',current_action_id='',active_binding_id=?,credential_envelope_deleted_at=?,
		state_due_at='',updated_at=? WHERE project_id='default' AND role='orchestrator'`,
		fixture.binding.BindingID, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	fixture.store.ManagedSessionDriverFreshFor = time.Hour
	if _, err := fixture.store.DB.ExecContext(ctx, `INSERT INTO driver_session_projections
		(store_id,session_id,host_id,pane_instance_id,agent_run_id,tmux_server_domain_id,
		 tmux_server_instance_id,lifecycle,phase,last_store_seq,as_of_cursor,source,updated_at)
		VALUES (?,?,?,?,?,?,?,'observing','working',10,'cursor-10','snapshot',?)`,
		fixture.binding.StoreID, fixture.binding.SessionID, fixture.binding.HostID,
		fixture.binding.PaneInstanceID, fixture.binding.AgentRunID, fixture.binding.TmuxServerDomainID,
		fixture.binding.TmuxServerInstanceID, stamp); err != nil {
		t.Fatal(err)
	}
	lifecycle, err := fixture.store.CurrentProjectActorLifecycle(ctx, "default", store.DriverOrchestratorRole)
	if err != nil {
		t.Fatal(err)
	}
	authNow := now.Add(4 * time.Minute)
	authn := auth.NewBearer([]byte(contractAPISecret), []string{"orchestrator-actor-auth", "legacy-worker"}, false).WithNow(func() time.Time {
		return authNow
	})
	authn.WithCredentialVerifier(func(claims auth.CredentialClaims, observedAt time.Time) bool {
		return fixture.store.AuthorizeProjectActorCredential(ctx, claims.Identity, claims.ProjectID,
			claims.WorkerRole, claims.CredentialID, claims.Generation, observedAt)
	})
	fixture.authn = authn
	srv := api.New(fixture.store, clock.NewFake(now.Add(4*time.Minute)), ulid.NewMinter(nil),
		api.Config{Authenticator: authn}, "contract-actor-auth-test")
	fixture.server = httptest.NewServer(srv.PrivateHandler())
	t.Cleanup(fixture.server.Close)
	credentialExpiry, err := time.Parse(time.RFC3339Nano, lifecycle.CredentialExpiresAt)
	if err != nil || credentialExpiry.Year() != 9999 {
		t.Fatalf("actor credential expiry=%q err=%v", lifecycle.CredentialExpiresAt, err)
	}
	token := authn.MintCredential("orchestrator-actor-auth", "default", store.DriverOrchestratorRole,
		lifecycle.CredentialEnvelopeRef, lifecycle.CredentialGeneration, credentialExpiry)
	key := fixture.body["submission_key"].(string)
	resp, body := contractAPIRequest(t, fixture, fixture.body, key, token)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("active project Orchestrator contract status=%d body=%v", resp.StatusCode, body)
	}

	for name, forged := range map[string]string{
		"cross project": authn.MintCredential("orchestrator-actor-auth", "other", store.DriverOrchestratorRole,
			lifecycle.CredentialEnvelopeRef, 1, now.Add(24*time.Hour)),
		"spoof actor": authn.MintCredential("other", "default", store.DriverOrchestratorRole,
			lifecycle.CredentialEnvelopeRef, 1, now.Add(24*time.Hour)),
		"old generation": authn.MintCredential("orchestrator-actor-auth", "default", store.DriverOrchestratorRole,
			lifecycle.CredentialEnvelopeRef, 2, now.Add(24*time.Hour)),
	} {
		resp, _ := contractAPIRequest(t, fixture, fixture.body, key, forged)
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("%s actor credential status=%d want 401", name, resp.StatusCode)
		}
	}
	// Actor credentials never inherit any raw worker or broad operator route.
	// Rejection occurs before body decoding or store mutation.
	forbiddenRoutes := []struct{ method, path string }{
		{http.MethodPost, "/v1/workers/register"}, {http.MethodPost, "/v1/workers/usage"},
		{http.MethodGet, "/v1/lease"}, {http.MethodPost, "/v1/jobs/job/heartbeat"},
		{http.MethodPost, "/v1/jobs/job/result"}, {http.MethodPost, "/v1/jobs/job/review"},
		{http.MethodPost, "/v1/jobs/job/spec"}, {http.MethodPost, "/v1/jobs/job/spec-review"},
		{http.MethodPost, "/v1/jobs/job/release"}, {http.MethodPost, "/v1/jobs/job/rebase-conflict"},
		{http.MethodGet, "/v1/jobs/job/bundle"}, {http.MethodPost, "/v1/control/pause"},
		{http.MethodPost, "/v1/control/resume"}, {http.MethodGet, "/v1/control"},
		{http.MethodGet, "/v1/config"}, {http.MethodGet, "/configz"},
		{http.MethodPost, "/v1/jobs/job/design"}, {http.MethodPost, "/v1/jobs/job/promote"},
		{http.MethodPost, "/v1/jobs/job/adopt"}, {http.MethodPost, "/v1/jobs/job/requeue"},
		{http.MethodPost, "/v1/jobs/job/cancel"}, {http.MethodPost, "/v1/specs"},
		{http.MethodPost, "/v1/epics"}, {http.MethodPost, "/v1/epics/epic/effect-recovery"},
		{http.MethodPost, "/v1/adopt"},
		{http.MethodPost, "/v1/conversations/thread/messages/message/delivery"},
		{http.MethodPost, "/v1/masters/register"}, {http.MethodPost, "/v1/masters/master/heartbeat"},
		{http.MethodPost, "/v1/masters/attention/lease"}, {http.MethodPost, "/v1/masters/attention/item/resolve"},
	}
	var changesBefore, changesAfter int64
	if err := fixture.store.DB.QueryRow(`SELECT total_changes()`).Scan(&changesBefore); err != nil {
		t.Fatal(err)
	}
	for _, route := range forbiddenRoutes {
		req, err := http.NewRequest(route.method, fixture.server.URL+route.path, bytes.NewBufferString(`{}`))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err = fixture.server.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("actor broad route %s %s status=%d want 403", route.method, route.path, resp.StatusCode)
		}
	}
	if err := fixture.store.DB.QueryRow(`SELECT total_changes()`).Scan(&changesAfter); err != nil {
		t.Fatal(err)
	}
	if changesAfter != changesBefore {
		t.Fatalf("forbidden actor routes mutated store: before=%d after=%d", changesBefore, changesAfter)
	}
	legacyWorkerReq, err := http.NewRequest(http.MethodPost, fixture.server.URL+"/v1/specs",
		bytes.NewBufferString(`{"task":"legacy compatibility","acceptance":"accepted"}`))
	if err != nil {
		t.Fatal(err)
	}
	legacyWorkerReq.Header.Set("Authorization", "Bearer "+authn.Mint("legacy-worker"))
	resp, err = fixture.server.Client().Do(legacyWorkerReq)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusUnauthorized {
		t.Fatalf("unregistered legacy worker compatibility was denied: %d", resp.StatusCode)
	}
	legacyActorToken := authn.Mint("orchestrator-actor-auth")
	resp, _ = contractAPIRequest(t, fixture, fixture.body, key, legacyActorToken)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("legacy static token collision status=%d want 403", resp.StatusCode)
	}

	loopbackAuth := auth.NewBearer([]byte(contractAPISecret), []string{"orchestrator-actor-auth"}, true).
		WithNow(func() time.Time { return authNow }).WithCredentialVerifier(func(claims auth.CredentialClaims, observedAt time.Time) bool {
		return fixture.store.AuthorizeProjectActorCredential(ctx, claims.Identity, claims.ProjectID,
			claims.WorkerRole, claims.CredentialID, claims.Generation, observedAt)
	})
	loopbackServer := httptest.NewServer(api.New(fixture.store, clock.NewFake(authNow), ulid.NewMinter(nil),
		api.Config{Authenticator: loopbackAuth}, "actor-loopback-collision").PrivateHandler())
	t.Cleanup(loopbackServer.Close)
	raw, _ := json.Marshal(fixture.body)
	loopbackReq, err := http.NewRequest(http.MethodPost,
		loopbackServer.URL+"/v1/work-intents/"+fixture.intent.ID+"/epic-contract?identity=orchestrator-actor-auth",
		bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	loopbackReq.Header.Set("Idempotency-Key", key)
	resp, err = loopbackServer.Client().Do(loopbackReq)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("loopback actor identity collision status=%d want 403", resp.StatusCode)
	}

	// The lifecycle-bound issuance remains valid beyond 24 hours while exact
	// Driver evidence is fresh; no refresh protocol exists to rotate it in place.
	authNow = now.Add(48 * time.Hour)
	fresh := authNow.Format(time.RFC3339Nano)
	if _, err := fixture.store.DB.ExecContext(ctx, `UPDATE driver_instances SET updated_at=? WHERE instance_ref=?`,
		fresh, "contract-api-instance-orchestrator-actor-auth"); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.store.DB.ExecContext(ctx, `UPDATE driver_observation_cursors SET updated_at=? WHERE store_id=?`,
		fresh, fixture.binding.StoreID); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.store.DB.ExecContext(ctx, `UPDATE driver_session_projections SET updated_at=?
		WHERE store_id=? AND session_id=?`, fresh, fixture.binding.StoreID, fixture.binding.SessionID); err != nil {
		t.Fatal(err)
	}
	resp, body = contractAPIRequest(t, fixture, fixture.body, key, token)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("active actor credential after 24h status=%d body=%v", resp.StatusCode, body)
	}
	if _, err := fixture.store.DB.ExecContext(ctx, `UPDATE project_actor_lifecycles
		SET state='stopped',desired_state='retired',active_binding_id='',credential_revoked_at=?,updated_at=?
		WHERE project_id='default' AND role='orchestrator'`, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	resp, _ = contractAPIRequest(t, fixture, fixture.body, key, token)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("stopped actor credential status=%d want 401", resp.StatusCode)
	}
}

func TestWorkIntentEpicContractAPIEnforcesKeyAndValidatesContract(t *testing.T) {
	fixture := newContractAPIFixture(t, "orchestrator-validation", "contract-api-validation", "contract-api-validation")
	token := fixture.authn.Mint("orchestrator-validation")
	key := fixture.body["submission_key"].(string)

	resp, _ := contractAPIRequest(t, fixture, fixture.body, "", token)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("missing Idempotency-Key status=%d want 400", resp.StatusCode)
	}
	resp, _ = contractAPIRequest(t, fixture, fixture.body, "wrong-submission-key", token)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("mismatched Idempotency-Key status=%d want 409", resp.StatusCode)
	}
	invalid := make(map[string]any, len(fixture.body))
	for k, v := range fixture.body {
		invalid[k] = v
	}
	invalid["contract"] = map[string]any{
		"slug": "INVALID SLUG", "title": "Invalid", "repositories": []string{"russ"},
		"delivery_repo": "russ", "spec_path": "epics/invalid.md",
		"scope": []string{"internal/**"}, "acceptance": []string{"must reject"},
	}
	invalid["contract_sha256"] = "sha256:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"
	resp, _ = contractAPIRequest(t, fixture, invalid, key, token)
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("invalid contract status=%d want 422", resp.StatusCode)
	}
	var count int
	if err := fixture.store.DB.QueryRow(`SELECT COUNT(*) FROM work_intent_epic_contracts
		WHERE work_intent_id=?`, fixture.intent.ID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("invalid/key-rejected submissions persisted %d contracts", count)
	}
}

func TestWorkIntentEpicContractAPIRejectsSupersededOrchestratorBinding(t *testing.T) {
	fixture := newContractAPIFixture(t, "orchestrator-fenced", "contract-api-fenced", "contract-api-fenced")
	replacement := fixture.binding
	replacement.BindingID = ""
	replacement.BindingEpoch = 0
	replacement.TargetEpoch++
	replacement.SessionID += "-replacement"
	replacement.PaneInstanceID += "-replacement"
	replacement.AgentRunID += "-replacement"
	if _, err := fixture.store.UpsertDriverSessionBinding(context.Background(), replacement,
		time.Date(2026, 7, 19, 22, 5, 0, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}
	key := fixture.body["submission_key"].(string)
	token := fixture.authn.Mint("orchestrator-fenced")
	resp, _ := contractAPIRequest(t, fixture, fixture.body, key, token)
	if resp.StatusCode != http.StatusPreconditionFailed {
		t.Fatalf("superseded Orchestrator binding status=%d want 412", resp.StatusCode)
	}
	var count int
	if err := fixture.store.DB.QueryRow(`SELECT COUNT(*) FROM work_intent_epic_contracts
		WHERE work_intent_id=?`, fixture.intent.ID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("superseded binding persisted %d contracts", count)
	}
}
