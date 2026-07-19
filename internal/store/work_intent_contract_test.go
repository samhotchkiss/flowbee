package store_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/samhotchkiss/flowbee/internal/store"
	"github.com/samhotchkiss/flowbee/internal/testutil"
	"github.com/samhotchkiss/flowbee/internal/workintent"
)

func seedOrchestratingWorkIntent(t *testing.T, st *store.Store, sourceMessage, orchestrator string,
	now time.Time) (store.WorkIntent, store.DriverSessionBinding) {
	t.Helper()
	ctx := context.Background()
	bindWorkIntentDriverRoute(t, st, orchestrator, now)
	intent, err := st.CreateWorkIntent(ctx, store.CreateWorkIntentInput{
		ProjectID: "default", SourceConversationID: "thread-" + sourceMessage,
		SourceMessageID: sourceMessage, SourceMessageVersion: 1,
		InteractorIncarnationID: "interactor-1", Title: "Build durable control plane",
		ArtifactRef: "artifact://intent/" + sourceMessage, ArtifactSHA256: workIntentSHA,
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
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB.ExecContext(ctx, `UPDATE work_intent_actions SET state='acknowledged',
		acknowledged_at=?,action_epoch=1 WHERE id=?`, now.Add(3*time.Minute).Format(time.RFC3339Nano),
		intent.DeliveryActionID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB.ExecContext(ctx, `UPDATE work_intents SET state='orchestrating',
		state_version=state_version+1,route_acknowledged_at=?,route_due_at=''
		WHERE id=?`, now.Add(3*time.Minute).Format(time.RFC3339Nano), intent.ID); err != nil {
		t.Fatal(err)
	}
	intent, err = st.GetWorkIntent(ctx, "default", intent.ID)
	if err != nil || intent.State != workintent.StateOrchestrating {
		t.Fatalf("orchestrating=%+v err=%v", intent, err)
	}
	binding, err := st.ActiveDriverSessionBinding(ctx, "default", orchestrator, store.DriverOrchestratorRole)
	if err != nil {
		t.Fatal(err)
	}
	return intent, binding
}

func validPreparedContract(slug string) store.WorkIntentEpicContract {
	return store.WorkIntentEpicContract{
		Slug: slug, Title: "Durable handoff", Repositories: []string{"russ"},
		DeliveryRepo: "russ", SpecPath: "epics/" + slug + ".md",
		Scope: []string{"internal/flowbee/**"}, IssueRefs: []string{"#4950", "#4951"},
		Acceptance: []string{"interrupted review dispatch self-heals"},
	}
}

func TestWorkIntentContractAutomaticallyAdmitsExactlyOneEpicAndLinksAcknowledgement(t *testing.T) {
	ctx := context.Background()
	st := testutil.NewStore(t)
	st.EnableEpicReviewHandoffV2 = true
	now := time.Date(2026, 7, 19, 22, 0, 0, 0, time.UTC)
	intent, binding := seedOrchestratingWorkIntent(t, st, "contract-message", "contract-orchestrator", now)
	contract := validPreparedContract("durable-handoff")
	hash, err := store.WorkIntentEpicContractSHA256(contract)
	if err != nil {
		t.Fatal(err)
	}
	key, _ := workintent.AdmissionKey(workintent.Intent{ID: intent.ID, ProjectID: "default", Version: 1})
	input := store.RecordWorkIntentEpicContractInput{
		ProjectID: "default", WorkIntentID: intent.ID, IntentVersion: 1,
		ExpectedStateVersion: intent.StateVersion, SourceArtifactSHA256: intent.ArtifactSHA256,
		ContractVersion: 1, ContractRef: "artifact://epic-contract/1", ContractSHA256: hash,
		Contract: contract, OrchestratorBindingID: binding.BindingID, SubmissionKey: key,
	}
	prepared, err := st.RecordWorkIntentEpicContract(ctx, input, now.Add(4*time.Minute))
	if err != nil || prepared.State != "prepared" {
		t.Fatalf("prepared=%+v err=%v", prepared, err)
	}
	// A lost HTTP acknowledgement reuses the exact contract and returns the row.
	replay, err := st.RecordWorkIntentEpicContract(ctx, input, now.Add(5*time.Minute))
	if err != nil || replay.ID != prepared.ID {
		t.Fatalf("replay=%+v err=%v", replay, err)
	}
	rep, err := st.ReconcileWorkIntentAdmissions(ctx, now.Add(6*time.Minute))
	if err != nil || rep.Admitted != 1 {
		t.Fatalf("admission=%+v err=%v", rep, err)
	}
	intent, err = st.GetWorkIntent(ctx, "default", intent.ID)
	if err != nil || intent.State != workintent.StateAdmitted || intent.AdmittedEpicID == "" {
		t.Fatalf("admitted intent=%+v err=%v", intent, err)
	}
	epic, err := st.GetEpicRun(ctx, intent.AdmittedEpicID)
	if err != nil || epic.WorkIntentID != intent.ID || epic.AdmissionKey != key ||
		epic.Slug != contract.Slug || epic.Branch != "epic/"+contract.Slug ||
		epic.DeliveryState != "admitted" {
		t.Fatalf("epic=%+v err=%v", epic, err)
	}
	prepared, err = st.GetPreparedWorkIntentEpicContract(ctx, "default", intent.ID, 1)
	if err != nil || prepared.State != "admitted" || prepared.AdmittedEpicID != epic.ID {
		t.Fatalf("contract=%+v err=%v", prepared, err)
	}
	rep, err = st.ReconcileWorkIntentAdmissions(ctx, now.Add(7*time.Minute))
	if err != nil || rep.Admitted != 0 || rep.Scanned != 0 {
		t.Fatalf("idempotent admission=%+v err=%v", rep, err)
	}
	var epics, deliveries int
	_ = st.DB.QueryRow(`SELECT COUNT(*) FROM epics WHERE work_intent_id=?`, intent.ID).Scan(&epics)
	_ = st.DB.QueryRow(`SELECT COUNT(*) FROM epic_deliveries WHERE epic_id=?`, epic.ID).Scan(&deliveries)
	if epics != 1 || deliveries != 1 {
		t.Fatalf("epics=%d deliveries=%d", epics, deliveries)
	}
}

func TestWorkIntentContractRejectsChangedReplayAndSupersededOrchestrator(t *testing.T) {
	ctx := context.Background()
	st := testutil.NewStore(t)
	now := time.Date(2026, 7, 19, 23, 0, 0, 0, time.UTC)
	intent, binding := seedOrchestratingWorkIntent(t, st, "contract-fence", "fenced-orchestrator", now)
	contract := validPreparedContract("fenced-contract")
	hash, _ := store.WorkIntentEpicContractSHA256(contract)
	key, _ := workintent.AdmissionKey(workintent.Intent{ID: intent.ID, ProjectID: "default", Version: 1})
	input := store.RecordWorkIntentEpicContractInput{ProjectID: "default", WorkIntentID: intent.ID,
		IntentVersion: 1, ExpectedStateVersion: intent.StateVersion,
		SourceArtifactSHA256: intent.ArtifactSHA256, ContractVersion: 1,
		ContractRef: "artifact://contract/fenced", ContractSHA256: hash, Contract: contract,
		OrchestratorBindingID: binding.BindingID, SubmissionKey: key}
	if _, err := st.RecordWorkIntentEpicContract(ctx, input, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	changed := input
	changed.ContractRef = "artifact://contract/changed"
	if _, err := st.RecordWorkIntentEpicContract(ctx, changed, now.Add(2*time.Minute)); !errors.Is(err, store.ErrWorkIntentContractFenced) {
		t.Fatalf("changed replay err=%v", err)
	}

	// A second intent may not use a superseded Orchestrator incarnation.
	other, old := seedOrchestratingWorkIntent(t, st, "contract-old-binding", "old-orchestrator", now.Add(3*time.Minute))
	if _, err := st.UpsertDriverSessionBinding(ctx, store.DriverSessionBinding{
		WorkerIdentity: "old-orchestrator", Role: store.DriverOrchestratorRole,
		HostID: old.HostID, StoreID: old.StoreID, TmuxServerDomainID: "flowbee", TmuxServerInstanceID: old.TmuxServerInstanceID, LifecycleOwnership: "driver_managed",
		LifecycleKey: old.LifecycleKey, TargetEpoch: old.TargetEpoch + 1, ProfileID: old.ProfileID,
		WorkspaceRootID: old.WorkspaceRootID, WorkspaceRelativePath: old.WorkspaceRelativePath,
		SessionID: old.SessionID + "-new", PaneInstanceID: old.PaneInstanceID + "-new",
		AgentRunID: old.AgentRunID + "-new",
	}, now.Add(4*time.Minute)); err != nil {
		t.Fatal(err)
	}
	otherContract := validPreparedContract("old-binding-contract")
	otherHash, _ := store.WorkIntentEpicContractSHA256(otherContract)
	otherKey, _ := workintent.AdmissionKey(workintent.Intent{ID: other.ID, ProjectID: "default", Version: 1})
	_, err := st.RecordWorkIntentEpicContract(ctx, store.RecordWorkIntentEpicContractInput{
		ProjectID: "default", WorkIntentID: other.ID, IntentVersion: 1,
		ExpectedStateVersion: other.StateVersion, SourceArtifactSHA256: other.ArtifactSHA256,
		ContractVersion: 1, ContractRef: "artifact://contract/old", ContractSHA256: otherHash,
		Contract: otherContract, OrchestratorBindingID: old.BindingID, SubmissionKey: otherKey,
	}, now.Add(5*time.Minute))
	if !errors.Is(err, store.ErrWorkIntentContractFenced) {
		t.Fatalf("superseded binding err=%v", err)
	}
}

func TestPreparedWorkIntentAdmissionHonorsPauseAcrossRestartAndRecoversOnResume(t *testing.T) {
	ctx := context.Background()
	dsn := filepath.Join(t.TempDir(), "paused-admission.db")
	st, err := store.Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.MigrateUp(ctx, st.DB); err != nil {
		_ = st.Close()
		t.Fatal(err)
	}
	st.EnableEpicReviewHandoffV2 = true
	now := time.Date(2026, 7, 20, 0, 0, 0, 0, time.UTC)
	intent, binding := seedOrchestratingWorkIntent(t, st, "paused-contract", "paused-orchestrator", now)
	contract := validPreparedContract("paused-contract")
	hash, err := store.WorkIntentEpicContractSHA256(contract)
	if err != nil {
		t.Fatal(err)
	}
	key, err := workintent.AdmissionKey(workintent.Intent{
		ID: intent.ID, ProjectID: intent.ProjectID, Version: intent.IntentVersion,
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = st.RecordWorkIntentEpicContract(ctx, store.RecordWorkIntentEpicContractInput{
		ProjectID: intent.ProjectID, WorkIntentID: intent.ID, IntentVersion: intent.IntentVersion,
		ExpectedStateVersion: intent.StateVersion, SourceArtifactSHA256: intent.ArtifactSHA256,
		ContractVersion: 1, ContractRef: "artifact://contract/paused", ContractSHA256: hash,
		Contract: contract, OrchestratorBindingID: binding.BindingID, SubmissionKey: key,
	}, now.Add(4*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	intent, err = st.GetWorkIntent(ctx, intent.ProjectID, intent.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.PauseWorkIntent(ctx, intent.ProjectID, intent.ID, intent.StateVersion,
		"human:sam", "hold before admission", now.Add(5*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	// Simulate the control-plane dying after contract preparation and pause, then
	// running its startup admission reconciliation against only durable state.
	restarted, err := store.Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = restarted.Close() })
	restarted.EnableEpicReviewHandoffV2 = true
	report, err := restarted.ReconcileWorkIntentAdmissions(ctx, now.Add(6*time.Minute))
	if err != nil || report.Scanned != 0 || report.Admitted != 0 {
		t.Fatalf("paused startup admission=%+v err=%v", report, err)
	}
	var epics int
	if err := restarted.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM epics
		WHERE work_intent_id=?`, intent.ID).Scan(&epics); err != nil {
		t.Fatal(err)
	}
	if epics != 0 {
		t.Fatalf("paused intent admitted %d epics after restart", epics)
	}

	intent, err = restarted.GetWorkIntent(ctx, intent.ProjectID, intent.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := restarted.ResumeWorkIntent(ctx, intent.ProjectID, intent.ID, intent.StateVersion,
		"human:sam", now.Add(7*time.Minute)); err != nil {
		t.Fatal(err)
	}
	report, err = restarted.ReconcileWorkIntentAdmissions(ctx, now.Add(8*time.Minute))
	if err != nil || report.Scanned != 1 || report.Admitted != 1 {
		t.Fatalf("resumed admission=%+v err=%v", report, err)
	}
	intent, err = restarted.GetWorkIntent(ctx, intent.ProjectID, intent.ID)
	if err != nil || intent.State != workintent.StateAdmitted || intent.AdmittedEpicID == "" {
		t.Fatalf("resumed intent=%+v err=%v", intent, err)
	}
}

func TestCancellingPreparedWorkIntentTerminalizesAdmissionObligation(t *testing.T) {
	ctx := context.Background()
	st := testutil.NewStore(t)
	now := time.Date(2026, 7, 20, 1, 0, 0, 0, time.UTC)
	intent, binding := seedOrchestratingWorkIntent(t, st, "cancelled-contract", "cancelled-orchestrator", now)
	contract := validPreparedContract("cancelled-contract")
	hash, err := store.WorkIntentEpicContractSHA256(contract)
	if err != nil {
		t.Fatal(err)
	}
	key, err := workintent.AdmissionKey(workintent.Intent{
		ID: intent.ID, ProjectID: intent.ProjectID, Version: intent.IntentVersion,
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = st.RecordWorkIntentEpicContract(ctx, store.RecordWorkIntentEpicContractInput{
		ProjectID: intent.ProjectID, WorkIntentID: intent.ID, IntentVersion: intent.IntentVersion,
		ExpectedStateVersion: intent.StateVersion, SourceArtifactSHA256: intent.ArtifactSHA256,
		ContractVersion: 1, ContractRef: "artifact://contract/cancelled", ContractSHA256: hash,
		Contract: contract, OrchestratorBindingID: binding.BindingID, SubmissionKey: key,
	}, now.Add(4*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	intent, err = st.GetWorkIntent(ctx, intent.ProjectID, intent.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.CancelWorkIntent(ctx, intent.ProjectID, intent.ID, intent.StateVersion,
		"human:sam", "withdraw before admission", now.Add(5*time.Minute)); err != nil {
		t.Fatal(err)
	}
	prepared, err := st.GetPreparedWorkIntentEpicContract(ctx, intent.ProjectID, intent.ID, intent.IntentVersion)
	if err != nil || prepared.State != "cancelled" || prepared.AdmittedEpicID != "" {
		t.Fatalf("cancelled contract=%+v err=%v", prepared, err)
	}
	report, err := st.ReconcileWorkIntentAdmissions(ctx, now.Add(6*time.Minute))
	if err != nil || report.Scanned != 0 || report.Admitted != 0 {
		t.Fatalf("cancelled admission reconcile=%+v err=%v", report, err)
	}
}
