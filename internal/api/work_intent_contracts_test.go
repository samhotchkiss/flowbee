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
			HostID: "contract-api-host", StoreID: storeID, TmuxServerInstanceID: "contract-api-server",
			LifecycleKey: "flowbee-control", TargetEpoch: 1, ProfileID: "flowbee-control",
			WorkspaceRootID: "workspace-root", WorkspaceRelativePath: "flowbee",
			SessionID: "flowbee-control-session", PaneInstanceID: "flowbee-control-pane",
			AgentRunID: "flowbee-control-run",
		},
		{
			WorkerIdentity: orchestrator, Role: store.DriverOrchestratorRole,
			HostID: "contract-api-host", StoreID: storeID, TmuxServerInstanceID: "contract-api-server",
			LifecycleKey: "orchestrator-" + orchestrator, TargetEpoch: 1, ProfileID: "orchestrator",
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
