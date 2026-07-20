package store_test

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/samhotchkiss/flowbee/client"
	"github.com/samhotchkiss/flowbee/internal/api"
	"github.com/samhotchkiss/flowbee/internal/auth"
	"github.com/samhotchkiss/flowbee/internal/capacity"
	"github.com/samhotchkiss/flowbee/internal/clock"
	"github.com/samhotchkiss/flowbee/internal/driver"
	"github.com/samhotchkiss/flowbee/internal/driverbridge"
	"github.com/samhotchkiss/flowbee/internal/store"
	"github.com/samhotchkiss/flowbee/internal/ulid"
	"github.com/samhotchkiss/flowbee/internal/worker"
)

type lostWorkerStopResponsePort struct{ *driver.FakePort }

type testLifecycleWorkspaceManager struct{}

func (testLifecycleWorkspaceManager) PrepareLifecycleWorkspace(context.Context, driver.Action, time.Time) error {
	return nil
}
func (testLifecycleWorkspaceManager) FinalizeLifecycleWorkspace(context.Context, driver.Action,
	driver.LifecycleReceipt, time.Time) error {
	return nil
}
func (testLifecycleWorkspaceManager) FinalizePreEffectLifecycleWorkspace(context.Context, driver.Action, time.Time) error {
	return nil
}

type materialFailureAfterPrepare struct{}

func (materialFailureAfterPrepare) ResolveLifecycleLaunch(context.Context, driver.Action, time.Time) (driver.Action, func(bool), error) {
	return driver.Action{}, func(bool) {}, errors.New("immutable worker material is unavailable after workspace prepare")
}

type preEffectWorkspaceRecorder struct {
	prepared, cleaned int
	cleanedAction     driver.Action
}

func (w *preEffectWorkspaceRecorder) PrepareLifecycleWorkspace(context.Context, driver.Action, time.Time) error {
	w.prepared++
	return nil
}
func (w *preEffectWorkspaceRecorder) FinalizeLifecycleWorkspace(context.Context, driver.Action, driver.LifecycleReceipt, time.Time) error {
	return errors.New("unexpected Driver-backed worker Stop")
}
func (w *preEffectWorkspaceRecorder) FinalizePreEffectLifecycleWorkspace(_ context.Context, action driver.Action, _ time.Time) error {
	w.cleaned++
	w.cleanedAction = action
	return nil
}

func testEpicWorkerMaterials(e store.EpicRun) store.EpicWorkerBootstrapMaterials {
	material := store.EpicWorkerBootstrapMaterials{
		GoalFormat:       store.EpicWorkerGoalFormat,
		EpicSpecGoalUTF8: "---\ntitle: " + e.Title + "\nscope:\n  - " + e.Slug + "/**\n---\n## Goal\nBuild " + e.Slug + ".\n## Steps\n1. Implement it.\n   Validate: go test ./...\n",
		AdmissionContractSHA256: func() string {
			if e.ContractHash != "" {
				return e.ContractHash
			}
			return "sha256:1111111111111111111111111111111111111111111111111111111111111111"
		}(),
		SourceCommitSHA:        "1111111111111111111111111111111111111111",
		BuilderDisciplineUTF8:  "Builder discipline: never bury; plant RED, fix, prove GREEN; enforce scope and migration ladder.",
		ReviewerDisciplineUTF8: "Reviewer discipline: mutate the guard; verify cited mechanisms and exact head/base; independently run tests.",
		ReferenceDocuments: []store.EpicWorkerReferenceDocument{{
			Reference: "flowbee://discipline/epics-instructions", Format: "text/markdown",
			ContentUTF8: "# Epic instructions\nStay inside scope and preserve the migration ladder.\n",
		}},
	}
	bindTestEpicWorkerSourceHash(&material)
	return material
}

func bindTestEpicWorkerSourceHash(material *store.EpicWorkerBootstrapMaterials) {
	sourceHash := sha256.Sum256([]byte(material.EpicSpecGoalUTF8))
	material.SourceArtifactSHA256 = "sha256:" + fmt.Sprintf("%x", sourceHash)
}

func installTestEpicWorkerMaterialProvider(st *store.Store) {
	st.EpicWorkerBootstrapMaterialProvider = func(_ context.Context, e store.EpicRun) (store.EpicWorkerBootstrapMaterials, error) {
		return testEpicWorkerMaterials(e), nil
	}
}

func (p *lostWorkerStopResponsePort) StopSession(ctx context.Context, target driver.SessionTarget,
	action driver.Action) (driver.LifecycleReceipt, error) {
	if _, err := p.FakePort.StopSession(ctx, target, action); err != nil {
		return driver.LifecycleReceipt{}, err
	}
	return driver.LifecycleReceipt{}, errors.New("connection closed after durable Driver Stop")
}

func TestDedicatedEpicAdmissionPlantsExactlyTwoImmutableWorkerContracts(t *testing.T) {
	h := newBuilderLaunchHarness(t, 2)
	h.st.EnableEpicReviewHandoffV2 = true
	h.st.EnableEpicDedicatedWorkersV2 = true
	h.addEpic(t, "worker-plan")

	rows, err := h.st.DB.Query(`SELECT worker_role,model_family,lifecycle_key,display_name,
		bootstrap_payload,bootstrap_sha256 FROM epic_worker_sessions
		WHERE epic_id='worker-plan' ORDER BY worker_role`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	seen := map[string]string{}
	keys := map[string]string{}
	for rows.Next() {
		var role, family, key, display, payload, hash string
		if err := rows.Scan(&role, &family, &key, &display, &payload, &hash); err != nil {
			t.Fatal(err)
		}
		var manifest map[string]any
		if err := json.Unmarshal([]byte(payload), &manifest); err != nil {
			t.Fatal(err)
		}
		if manifest["format"] != store.EpicWorkerBootstrapFormat || hash == "" ||
			manifest["epic_spec_goal_format"] != store.EpicWorkerGoalFormat ||
			manifest["epic_spec_goal_utf8"] == "" || manifest["epic_spec_goal_sha256"] == "" ||
			manifest["role_charter"] == "" || manifest["role_charter_sha256"] == "" ||
			manifest["discipline_kind"] != role || manifest["discipline_utf8"] == "" || manifest["discipline_sha256"] == "" ||
			manifest["reference_manifest_sha256"] == "" || manifest["artifact_context_ref"] == "" ||
			manifest["credential_policy_ref"] != "flowbee://credential-policies/worker-control-plane-only" ||
			manifest["credential_install_ref"] == "" || manifest["flowbee_worker_identity"] == "" {
			t.Fatalf("incomplete %s manifest=%v hash=%q", role, manifest, hash)
		}
		references, ok := manifest["reference_documents"].([]any)
		if !ok || len(references) != 1 {
			t.Fatalf("%s bootstrap dropped reference documents: %v", role, manifest["reference_documents"])
		}
		reference, ok := references[0].(map[string]any)
		if !ok || reference["content_utf8"] != "# Epic instructions\nStay inside scope and preserve the migration ladder.\n" ||
			reference["sha256"] == "" {
			t.Fatalf("%s bootstrap dropped exact reference bytes/hash: %v", role, reference)
		}
		seen[role] = family + "/" + display
		keys[role] = key
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(seen) != 2 || seen["builder"] != "codex/flowbee-worker-codex-default-worker-plan" ||
		seen["reviewer"] != "grok/flowbee-worker-grok-default-worker-plan" {
		t.Fatalf("worker plan=%v", seen)
	}
	if keys["builder"] != store.EpicWorkerLifecycleKey("worker-plan", "builder") ||
		keys["reviewer"] != store.EpicWorkerLifecycleKey("worker-plan", "reviewer") ||
		keys["builder"] == seen["builder"] {
		t.Fatalf("lifecycle/display authority collapsed: keys=%v display=%v", keys, seen)
	}
	if _, err := h.st.DB.Exec(`UPDATE epic_worker_sessions SET lifecycle_key='forged'
		WHERE epic_id='worker-plan' AND worker_role='builder'`); err == nil {
		t.Fatal("immutable worker lifecycle key was rewritten")
	}
}

func TestDedicatedEpicAdmissionFailsClosedWithoutExactContextMaterial(t *testing.T) {
	t.Run("provider unavailable", func(t *testing.T) {
		h := newBuilderLaunchHarness(t, 2)
		h.st.EnableEpicDedicatedWorkersV2 = true
		h.st.EpicWorkerBootstrapMaterialProvider = nil
		err := h.st.AddEpicRun(context.Background(), store.EpicRun{ID: "no-provider", Repo: "russ",
			Branch: "epic/no-provider", Slug: "no-provider", Title: "No provider",
			FilePath: "epics/no-provider.md", Scope: []string{"missing/**"}}, 1, h.now)
		if err == nil || !strings.Contains(err.Error(), "material provider unavailable") {
			t.Fatalf("provider-less dedicated admission error=%v", err)
		}
	})
	tests := []struct {
		name   string
		mutate func(*store.EpicWorkerBootstrapMaterials)
	}{
		{name: "missing spec", mutate: func(m *store.EpicWorkerBootstrapMaterials) { m.EpicSpecGoalUTF8 = "" }},
		{name: "missing builder discipline", mutate: func(m *store.EpicWorkerBootstrapMaterials) { m.BuilderDisciplineUTF8 = "" }},
		{name: "missing reviewer discipline", mutate: func(m *store.EpicWorkerBootstrapMaterials) { m.ReviewerDisciplineUTF8 = "" }},
		{name: "missing reference manifest", mutate: func(m *store.EpicWorkerBootstrapMaterials) { m.ReferenceDocuments = nil }},
		{name: "changed reference bytes with stale hash", mutate: func(m *store.EpicWorkerBootstrapMaterials) {
			m.ReferenceDocuments[0].SHA256 = "sha256:stale"
			m.ReferenceDocuments[0].ContentUTF8 += "mutated"
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := newBuilderLaunchHarness(t, 2)
			h.st.EnableEpicDedicatedWorkersV2 = true
			h.st.EpicWorkerBootstrapMaterialProvider = func(_ context.Context, e store.EpicRun) (store.EpicWorkerBootstrapMaterials, error) {
				m := testEpicWorkerMaterials(e)
				tc.mutate(&m)
				return m, nil
			}
			err := h.st.AddEpicRun(context.Background(), store.EpicRun{ID: "empty-context", Repo: "russ",
				Branch: "epic/empty-context", Slug: "empty-context", Title: "Empty context",
				FilePath: "epics/empty-context.md", Scope: []string{"empty/**"}}, 1, h.now)
			if err == nil {
				t.Fatal("dedicated admission accepted incomplete or unauthenticated context")
			}
			var epics, workers, actions int
			_ = h.st.DB.QueryRow(`SELECT COUNT(*) FROM epics WHERE id='empty-context'`).Scan(&epics)
			_ = h.st.DB.QueryRow(`SELECT COUNT(*) FROM epic_worker_sessions WHERE epic_id='empty-context'`).Scan(&workers)
			_ = h.st.DB.QueryRow(`SELECT COUNT(*) FROM epic_actions WHERE epic_id='empty-context'`).Scan(&actions)
			if epics != 0 || workers != 0 || actions != 0 {
				t.Fatalf("failed context leaked epic=%d workers=%d launch_actions=%d", epics, workers, actions)
			}
		})
	}
}

func TestDedicatedWorkerBootstrapHashBindsActualSpecAndDisciplineBytes(t *testing.T) {
	hashFor := func(t *testing.T, mutate func(*store.EpicWorkerBootstrapMaterials), role string) string {
		t.Helper()
		h := newBuilderLaunchHarness(t, 2)
		h.st.EnableEpicDedicatedWorkersV2 = true
		h.st.EpicWorkerBootstrapMaterialProvider = func(_ context.Context, e store.EpicRun) (store.EpicWorkerBootstrapMaterials, error) {
			m := testEpicWorkerMaterials(e)
			if mutate != nil {
				mutate(&m)
				bindTestEpicWorkerSourceHash(&m)
			}
			return m, nil
		}
		h.addEpic(t, "material-hash")
		var hash string
		if err := h.st.DB.QueryRow(`SELECT bootstrap_sha256 FROM epic_worker_sessions
			WHERE epic_id='material-hash' AND worker_role=?`, role).Scan(&hash); err != nil {
			t.Fatal(err)
		}
		return hash
	}
	baseBuilder := hashFor(t, nil, "builder")
	changedSpec := hashFor(t, func(m *store.EpicWorkerBootstrapMaterials) {
		m.EpicSpecGoalUTF8 = strings.Replace(m.EpicSpecGoalUTF8, "Build material-hash", "Build the mutated goal", 1)
	}, "builder")
	changedBuilderDiscipline := hashFor(t, func(m *store.EpicWorkerBootstrapMaterials) {
		m.BuilderDisciplineUTF8 += " Plant an additional falsifier."
	}, "builder")
	baseReviewer := hashFor(t, nil, "reviewer")
	changedReviewerDiscipline := hashFor(t, func(m *store.EpicWorkerBootstrapMaterials) {
		m.ReviewerDisciplineUTF8 += " Verify the live artifact."
	}, "reviewer")
	if baseBuilder == changedSpec || baseBuilder == changedBuilderDiscipline ||
		baseReviewer == changedReviewerDiscipline {
		t.Fatalf("bootstrap hash did not bind actual bytes builder=%q spec=%q discipline=%q reviewer=%q changed=%q",
			baseBuilder, changedSpec, changedBuilderDiscipline, baseReviewer, changedReviewerDiscipline)
	}
}

func TestDedicatedWorkerContextRejectsWireInvalidAndContractMismatchedBytes(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*store.EpicWorkerBootstrapMaterials)
	}{
		{name: "nul spec", mutate: func(m *store.EpicWorkerBootstrapMaterials) { m.EpicSpecGoalUTF8 += "\x00" }},
		{name: "unsupported reference format", mutate: func(m *store.EpicWorkerBootstrapMaterials) {
			m.ReferenceDocuments[0].Format = "application/octet-stream"
		}},
		{name: "unsafe logical reference", mutate: func(m *store.EpicWorkerBootstrapMaterials) {
			m.ReferenceDocuments[0].Reference = "../../epics/INSTRUCTIONS.md"
		}},
		{name: "oversize discipline", mutate: func(m *store.EpicWorkerBootstrapMaterials) { m.BuilderDisciplineUTF8 = strings.Repeat("x", 4097) }},
		{name: "contract mismatch", mutate: func(m *store.EpicWorkerBootstrapMaterials) {
			m.AdmissionContractSHA256 = "sha256:2222222222222222222222222222222222222222222222222222222222222222"
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := newBuilderLaunchHarness(t, 2)
			h.st.EnableEpicDedicatedWorkersV2 = true
			h.st.EpicWorkerBootstrapMaterialProvider = func(_ context.Context, e store.EpicRun) (store.EpicWorkerBootstrapMaterials, error) {
				m := testEpicWorkerMaterials(e)
				tc.mutate(&m)
				return m, nil
			}
			err := h.st.AddEpicRun(context.Background(), store.EpicRun{ID: "wire-invalid", Repo: "russ",
				Branch: "epic/wire-invalid", Slug: "wire-invalid", Title: "Wire invalid",
				ContractHash: "sha256:1111111111111111111111111111111111111111111111111111111111111111",
				FilePath:     "epics/wire-invalid.md", Scope: []string{"wire/**"}}, 1, h.now)
			if err == nil {
				t.Fatal("wire-invalid worker context was committed")
			}
		})
	}
}

func TestDedicatedWorkerLaunchFailsBeforeDriverWhenFinalBootstrapExceeds16KiB(t *testing.T) {
	h := newBuilderLaunchHarness(t, 2)
	h.st.EnableEpicDedicatedWorkersV2 = true
	h.st.EpicWorkerBootstrapMaterialProvider = func(_ context.Context, e store.EpicRun) (store.EpicWorkerBootstrapMaterials, error) {
		m := testEpicWorkerMaterials(e)
		// Reference bytes are delivered in Driver's single initial prompt, not
		// through a second materialization protocol. They therefore count against
		// the same exact 16 KiB wire bound.
		m.ReferenceDocuments[0].ContentUTF8 = strings.Repeat("reference-evidence-", 900)
		m.ReferenceDocuments[0].SHA256 = ""
		return m, nil
	}
	if _, err := h.st.DB.Exec(`UPDATE builder_driver_targets SET profile_id='codex_builder'
		WHERE project_id='default' AND seat_id=?`, h.seat.ID); err != nil {
		t.Fatal(err)
	}
	h.addEpic(t, "oversize-final-bootstrap")
	rep, err := h.st.ReconcileBuilderLaunches(context.Background(), h.now.Add(time.Minute),
		5*time.Minute, "codex", 5)
	if err == nil || !strings.Contains(err.Error(), "Driver limit") {
		t.Fatalf("oversize final bootstrap report=%+v error=%v", rep, err)
	}
	var actions int
	_ = h.st.DB.QueryRow(`SELECT COUNT(*) FROM epic_actions
		WHERE epic_id='oversize-final-bootstrap' AND kind='builder_launch'`).Scan(&actions)
	if actions != 0 {
		t.Fatalf("oversize final bootstrap committed %d Driver actions", actions)
	}
}

func TestDedicatedReviewerLaunchGatesReviewAndMergeStopsBothWorkers(t *testing.T) {
	ctx := context.Background()
	h := newBuilderLaunchHarness(t, 2)
	h.st.EnableEpicReviewHandoffV2 = true
	h.st.EnableEpicDedicatedWorkersV2 = true
	h.addEpic(t, "dedicated-workers")
	envelopeSecret := []byte("fake-envelope-secret-never-persisted")
	envelopeDir := t.TempDir()
	materials := driver.SQLLifecycleLaunchMaterials{DB: h.st.DB,
		EnvelopeDirectory: envelopeDir, WorkerAuthSecret: envelopeSecret}
	h.st.EpicWorkerCredentialMaterializer = materials.PrepareEnvelope

	var reviewerSeat string
	if err := h.st.DB.QueryRow(`SELECT id FROM seats WHERE agent_family='grok'`).Scan(&reviewerSeat); err != nil {
		t.Fatal(err)
	}
	stamp := h.now.UTC().Format(time.RFC3339Nano)
	if _, err := h.st.DB.Exec(`INSERT INTO driver_instances
		(instance_ref,host_id,store_id,producer_boot_id,tmux_server_domain_id,
		 tmux_server_ownership,state,created_at,updated_at)
		VALUES ('reviewer-driver','host-review','store-review','boot-review','flowbee',
		'managed_dedicated','live',?,?)`, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	if err := h.st.UpsertBuilderDriverTarget(ctx, store.BuilderDriverTarget{ProjectID: "default",
		SeatID: reviewerSeat, InstanceRef: "reviewer-driver", TmuxServerDomainID: "flowbee",
		TmuxServerInstanceID: "server-review", ProfileID: "grok_reviewer",
		WorkspaceRootID: "workspace-review", WorkspaceRelativeBase: "reviews", Enabled: true}, h.now); err != nil {
		t.Fatal(err)
	}
	if _, err := h.st.DB.Exec(`INSERT INTO driver_observation_cursors
		(store_id,instance_ref,cursor,high_store_seq,uncertainty_epoch,last_event_id,active,updated_at)
		VALUES ('store-review','reviewer-driver','tdc2.review',9,0,'review-baseline',1,?)`, stamp); err != nil {
		t.Fatal(err)
	}
	green := h.now.Add(time.Minute)
	if err := h.st.ObserveEpicArtifactFact(ctx, store.EpicArtifactFact{EpicID: "dedicated-workers",
		Repo: "russ", Branch: "epic/dedicated-workers", PRNumber: 6100, PROpen: true,
		HeadSHA: "head-dedicated", BaseSHA: "base-dedicated", CIState: "green",
		CIHasRealSuccess: true, RequiredChecksPresentPassed: true}, green); err != nil {
		t.Fatal(err)
	}
	if rep, err := h.st.ReconcileEpicReviewHandoffs(ctx, green.Add(10*time.Minute), time.Minute); err != nil || rep.Dispatched != 0 {
		t.Fatalf("review job escaped before dedicated Ensure: rep=%+v err=%v", rep, err)
	}
	var jobs, launches int
	_ = h.st.DB.QueryRow(`SELECT COUNT(*) FROM jobs WHERE epic_delivery_id='dedicated-workers'`).Scan(&jobs)
	_ = h.st.DB.QueryRow(`SELECT COUNT(*) FROM epic_actions WHERE epic_id='dedicated-workers'
		AND kind='reviewer_launch'`).Scan(&launches)
	var capacityAlerts int
	_ = h.st.DB.QueryRow(`SELECT COUNT(*) FROM control_alerts WHERE epic_id='dedicated-workers'
		AND kind='reviewer_lifecycle_unavailable'`).Scan(&capacityAlerts)
	if jobs != 0 || launches != 0 || capacityAlerts != 1 {
		t.Fatalf("pre-Ensure jobs=%d reviewer_launches=%d", jobs, launches)
	}
	capacityAt := green.Add(11 * time.Minute)
	if err := h.st.CommitCapacityGeneration(ctx, store.CapacityGeneration{ID: "dedicated-review-generation",
		StartedAt: capacityAt, ExpectedSeatIDs: []string{h.seat.ID, reviewerSeat},
		Observations: []store.CapacitySeatObservation{{ObservationID: "dedicated-builder-capacity",
			SeatID: h.seat.ID, HostID: "host-build", Provider: "codex", AccountKey: "account-build",
			CredentialLineage: "lineage-build", CollectorID: "dedicated-capacity", Source: "live_app_server",
			TrustState: "verified", IntegrityState: "verified", FetchedAt: capacityAt,
			Windows:   []capacity.RouteWindow{{Kind: "weekly", Applicable: true, Known: true, Percent: 20}},
			RawSHA256: "sha256:dedicated-builder", AdapterVersion: "fixture/v1"},
			{ObservationID: "dedicated-reviewer-capacity", SeatID: reviewerSeat, HostID: "host-review",
				Provider: "grok", AccountKey: "account-review", CredentialLineage: "lineage-review",
				CollectorID: "dedicated-capacity", Source: "live_billing", TrustState: "verified",
				IntegrityState: "verified", BillingPeriodActive: true, FetchedAt: capacityAt,
				Windows:   []capacity.RouteWindow{{Kind: "monthly", Applicable: true, Known: true, Percent: 20}},
				RawSHA256: "sha256:dedicated-reviewer", AdapterVersion: "fixture/v1"}},
	}, capacityAt); err != nil {
		t.Fatal(err)
	}
	if rep, err := h.st.ReconcileEpicReviewHandoffs(ctx, capacityAt, time.Minute); err != nil || rep.Dispatched != 0 {
		t.Fatalf("fresh-capacity reviewer launch=%+v err=%v", rep, err)
	}

	fake := driver.NewFake()
	actionStore := driver.SQLActionStore{DB: h.st.DB, ControlOriginAvailable: true}
	blockedRuntime := driver.LifecycleRuntime{Port: fake, Store: actionStore,
		Projector: driverbridge.Projector{Store: h.st}, Owner: "reviewer-lifecycle-held",
		ClaimTTL: time.Minute, MaximumTries: 5, RequireManagedAgentV3: true}
	if rep, err := blockedRuntime.Tick(ctx, capacityAt.Add(time.Minute)); err != nil ||
		rep.Retried != 1 || fake.EnsureCalls != 0 {
		t.Fatalf("missing material must hold before Driver mutation: report=%+v calls=%d err=%v",
			rep, fake.EnsureCalls, err)
	}
	// Rotate the issuer configuration after the action transaction committed.
	// Resolution must replay the already-fsynced envelope, never mint a changed
	// body from this new key.
	executionMaterials := materials
	executionMaterials.WorkerAuthSecret = []byte("rotated-secret-must-not-change-committed-envelope")
	files, err := os.ReadDir(envelopeDir)
	if err != nil || len(files) != 1 {
		t.Fatalf("committed envelope files=%d err=%v", len(files), err)
	}
	envelopePath := filepath.Join(envelopeDir, files[0].Name())
	committedEnvelope, err := os.ReadFile(envelopePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(envelopePath); err != nil {
		t.Fatal(err)
	}
	missingRuntime := driver.LifecycleRuntime{Port: fake, Store: actionStore,
		Projector: driverbridge.Projector{Store: h.st}, Owner: "reviewer-lifecycle-missing-envelope",
		ClaimTTL: time.Minute, MaximumTries: 5, RequireManagedAgentV3: true, Materials: executionMaterials}
	if rep, err := missingRuntime.Tick(ctx, capacityAt.Add(2*time.Minute)); err != nil || rep.Retried != 1 || fake.EnsureCalls != 0 {
		t.Fatalf("missing committed envelope regenerated or mutated Driver: report=%+v calls=%d err=%v", rep, fake.EnsureCalls, err)
	}
	if err := os.WriteFile(envelopePath, committedEnvelope, 0o600); err != nil {
		t.Fatal(err)
	}
	runtime := driver.LifecycleRuntime{Port: fake, Store: actionStore,
		Projector: driverbridge.Projector{Store: h.st}, Owner: "reviewer-lifecycle",
		ClaimTTL: time.Minute, MaximumTries: 5, RequireManagedAgentV3: true,
		Materials: executionMaterials, Workspaces: testLifecycleWorkspaceManager{}}
	if rep, err := runtime.Tick(ctx, capacityAt.Add(4*time.Minute)); err != nil || rep.Executed != 1 || fake.EnsureCalls != 1 {
		t.Fatalf("reviewer Ensure=%+v calls=%d err=%v", rep, fake.EnsureCalls, err)
	}
	if len(fake.EnsureTargets) != 1 || fake.EnsureTargets[0].Bootstrap == nil ||
		!strings.Contains(fake.EnsureTargets[0].Bootstrap.ContentUTF8, "head-dedicated") ||
		!strings.Contains(fake.EnsureTargets[0].Bootstrap.ContentUTF8, "base-dedicated") ||
		!strings.Contains(fake.EnsureTargets[0].Bootstrap.ContentUTF8,
			"flowbee://projects/default/epics/dedicated-workers/pulls/6100/diff/base-dedicated..head-dedicated") ||
		!strings.Contains(fake.EnsureTargets[0].Bootstrap.ContentUTF8, "## Goal") ||
		!strings.Contains(fake.EnsureTargets[0].Bootstrap.ContentUTF8, "Reviewer discipline") ||
		!strings.Contains(fake.EnsureTargets[0].Bootstrap.ContentUTF8,
			`# Epic instructions\nStay inside scope and preserve the migration ladder.\n`) ||
		fake.EnsureTargets[0].CredentialEnvelope == nil ||
		fake.EnsureTargets[0].PresentationName != "flowbee-worker-grok-default-dedicated-workers" {
		t.Fatalf("SQL-claimed v3 launch material=%+v", fake.EnsureTargets)
	}
	var envelopeDeleted string
	if err := h.st.DB.QueryRow(`SELECT envelope_deleted_at FROM epic_worker_credentials
		WHERE epic_id='dedicated-workers' AND worker_role='reviewer'`).Scan(&envelopeDeleted); err != nil || envelopeDeleted == "" {
		t.Fatalf("installed one-shot envelope not tombstoned: %q err=%v", envelopeDeleted, err)
	}
	if rep, err := h.st.ReconcileEpicReviewHandoffs(ctx, capacityAt.Add(5*time.Minute), time.Minute); err != nil || rep.Dispatched != 1 {
		t.Fatalf("post-Ensure handoff=%+v err=%v", rep, err)
	}

	// Resolve the one-shot credential envelope outside the durable ledger. The
	// only persisted bytes are its opaque reference, generation/expiry/lineage,
	// and the exact Flowbee identity. A restarted worker re-registers with the
	// same credential-bound identity without an operator copying a token.
	var bootstrapPayload, dedicatedIdentity string
	if err := h.st.DB.QueryRow(`SELECT bootstrap_payload,flowbee_identity FROM epic_worker_sessions
		WHERE epic_id='dedicated-workers' AND worker_role='reviewer'`).
		Scan(&bootstrapPayload, &dedicatedIdentity); err != nil {
		t.Fatal(err)
	}
	var bootstrap struct {
		CredentialInstallRef string `json:"credential_install_ref"`
		WorkerIdentity       string `json:"flowbee_worker_identity"`
	}
	if err := json.Unmarshal([]byte(bootstrapPayload), &bootstrap); err != nil {
		t.Fatal(err)
	}
	secret := envelopeSecret
	var envelopeRef, expiresAt string
	var generation int
	if err := h.st.DB.QueryRow(`SELECT envelope_ref,generation,expires_at FROM epic_worker_credentials
		WHERE epic_id='dedicated-workers' AND worker_role='reviewer' AND state='issued'`).
		Scan(&envelopeRef, &generation, &expiresAt); err != nil {
		t.Fatal(err)
	}
	authNow := capacityAt.Add(2 * time.Minute)
	authn := auth.NewBearer(secret, nil, false).WithNow(func() time.Time { return authNow }).
		WithCredentialVerifier(func(claims auth.CredentialClaims, observedAt time.Time) bool {
			return h.st.AuthorizeEpicWorkerCredential(context.Background(), claims.Identity,
				claims.ProjectID, claims.WorkerRole, claims.CredentialID, claims.Generation, observedAt)
		})
	expiresTime, err := time.Parse(time.RFC3339Nano, expiresAt)
	if err != nil {
		t.Fatal(err)
	}
	envelopeContents := map[string]string{envelopeRef: authn.MintCredential(dedicatedIdentity,
		"default", "reviewer", envelopeRef, int64(generation), expiresTime)}
	token := envelopeContents[envelopeRef]
	if got := fake.EnsureTargets[0].CredentialEnvelope.SecretUTF8; got != token {
		t.Fatal("issuer rotation changed the credential body after lifecycle action commit")
	}
	if strings.Contains(fake.EnsureTargets[0].Bootstrap.ContentUTF8, token) {
		t.Fatal("secret credential leaked into public lifecycle bootstrap")
	}
	var reviewerActionID string
	if err := h.st.DB.QueryRow(`SELECT id FROM epic_actions WHERE epic_id='dedicated-workers'
		AND kind='reviewer_launch'`).Scan(&reviewerActionID); err != nil {
		t.Fatal(err)
	}
	firstReceipt := fake.LifecycleReceipts[reviewerActionID]
	firstBootstrap := fake.EnsureTargets[0].Bootstrap.ContentUTF8
	if _, err := fake.EnsureLifecycleSession(ctx, fake.EnsureTargets[0], driver.Action{
		ActionID: reviewerActionID, Epoch: firstReceipt.ActionEpoch}); err != nil {
		t.Fatalf("exact restart replay failed: %v", err)
	}
	if fake.EnsureCalls != 1 || fake.EnsureTargets[0].Bootstrap.ContentUTF8 != firstBootstrap ||
		fake.LifecycleReceipts[reviewerActionID].BootstrapArtifact.PayloadSHA256 !=
			fake.EnsureTargets[0].Bootstrap.PayloadSHA256 {
		t.Fatal("restart replay changed immutable public bootstrap or duplicated Ensure")
	}
	if token == "" || generation != 1 || expiresAt == "" || bootstrap.CredentialInstallRef == "" ||
		bootstrap.WorkerIdentity != dedicatedIdentity {
		t.Fatalf("unresolvable credential envelope=%+v identity=%q", bootstrap, dedicatedIdentity)
	}
	apiServer := api.New(h.st, clock.NewFake(capacityAt.Add(2*time.Minute)), ulid.NewMinter(nil), api.Config{
		Authenticator: authn, LeaseTTLS: 300, HeartbeatIntervalS: 30,
		Allowlist: worker.Allowlist{Permit: map[string][]string{dedicatedIdentity: {
			"role:code_reviewer", "model_family:grok",
		}}},
	}, "worker-envelope-test")
	httpServer := httptest.NewServer(apiServer.PrivateHandler())
	t.Cleanup(httpServer.Close)
	workerClient := client.NewWithToken(httpServer.URL, token)
	registration := client.Registration{Identity: dedicatedIdentity, Host: "host-review",
		Capabilities: []string{"role:code_reviewer", "model_family:grok"}}
	registered, err := workerClient.Register(ctx, registration)
	if err != nil || registered.WorkerID == "" {
		t.Fatalf("zero-input worker registration=%+v err=%v", registered, err)
	}
	registration.WorkerID = registered.WorkerID
	if restarted, err := workerClient.Register(ctx, registration); err != nil || restarted.WorkerID != registered.WorkerID {
		t.Fatalf("restart registration=%+v err=%v", restarted, err)
	}
	var reviewJobID string
	if err := h.st.DB.QueryRow(`SELECT id FROM jobs WHERE epic_delivery_id='dedicated-workers'
		AND workflow_domain='epic_v2'`).Scan(&reviewJobID); err != nil {
		t.Fatal(err)
	}
	if reviewLease, err := h.st.ClaimReviewJob(ctx, store.ClaimReviewParams{JobID: reviewJobID,
		LeaseID: "dedicated-review-lease", Identity: dedicatedIdentity, SeatID: reviewerSeat,
		ModelFamily: "grok", Model: "grok", Lens: "correctness",
		Attested: []string{"role:code_reviewer", "model_family:grok"},
		TTL:      5 * time.Minute, Now: capacityAt.Add(3 * time.Minute)}); err != nil || reviewLease == nil {
		t.Fatalf("credential-bound dedicated claim lease=%+v err=%v", reviewLease, err)
	}
	var immutableBootstrapHash string
	if err := h.st.DB.QueryRow(`SELECT bootstrap_sha256 FROM epic_worker_sessions
		WHERE epic_id='dedicated-workers' AND worker_role='reviewer'`).Scan(&immutableBootstrapHash); err != nil {
		t.Fatal(err)
	}
	rotatedAt := capacityAt.Add(25 * time.Hour)
	if _, err := h.st.RotateEpicWorkerCredential(ctx, "dedicated-workers", "reviewer", rotatedAt); err == nil {
		t.Fatal("unsafe envelope-only credential rotation was accepted")
	}
	var afterRotationHash string
	if err := h.st.DB.QueryRow(`SELECT bootstrap_sha256 FROM epic_worker_sessions
		WHERE epic_id='dedicated-workers' AND worker_role='reviewer'`).Scan(&afterRotationHash); err != nil {
		t.Fatal(err)
	}
	if afterRotationHash != immutableBootstrapHash {
		t.Fatalf("rejected credential rotation rewrote bootstrap %q -> %q", immutableBootstrapHash, afterRotationHash)
	}
	authNow = rotatedAt
	if _, err := workerClient.Register(ctx, registration); err != nil {
		t.Fatalf("active exact worker credential self-deauthorized after 24h: %v", err)
	}
	var secretLeaks int
	_ = h.st.DB.QueryRow(`SELECT COUNT(*) FROM epic_worker_credentials
		WHERE envelope_ref LIKE '%fake-envelope-secret%' OR refresh_lineage LIKE '%fake-envelope-secret%'`).Scan(&secretLeaks)
	if secretLeaks != 0 {
		t.Fatal("credential secret leaked into durable worker ledger")
	}

	// Model the already-running builder exact incarnation; merge shutdown is
	// independently obligated even if review finished much later.
	builder, err := h.st.UpsertDriverSessionBinding(ctx, store.DriverSessionBinding{ProjectID: "default",
		WorkerIdentity: store.BuilderDriverIdentity("dedicated-workers"), Role: store.DriverBuilderRole,
		HostID: "host-build", StoreID: "store-build", TmuxServerDomainID: "flowbee",
		TmuxServerInstanceID: "server-build", LifecycleOwnership: "driver_managed",
		LifecycleKey: store.EpicWorkerLifecycleKey("dedicated-workers", "builder"),
		TargetEpoch:  1, ProfileID: "codex-builder", WorkspaceRootID: "workspace-build",
		WorkspaceRelativePath: "repos/default/dedicated-workers", SessionID: "builder-session",
		PaneInstanceID: "builder-pane", AgentRunID: "builder-run", Provider: "codex", ObservedAt: green}, green)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.st.DB.Exec(`UPDATE epic_worker_sessions SET state='active',binding_id=?
		WHERE epic_id='dedicated-workers' AND worker_role='builder'`, builder.BindingID); err != nil {
		t.Fatal(err)
	}
	merged := store.EpicArtifactFact{EpicID: "dedicated-workers", Repo: "russ",
		Branch: "epic/dedicated-workers", PRNumber: 6100, HeadSHA: "head-dedicated",
		BaseSHA: "base-dedicated", CIState: "green", CIHasRealSuccess: true,
		RequiredChecksPresentPassed: true, Merged: true, MergeCommitSHA: "merge-dedicated"}
	if err := h.st.ObserveEpicArtifactFact(ctx, merged, rotatedAt.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := h.st.ObserveEpicArtifactFact(ctx, merged, rotatedAt.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	var stops int
	if err := h.st.DB.QueryRow(`SELECT COUNT(*) FROM epic_actions WHERE epic_id='dedicated-workers'
		AND kind='worker_stop'`).Scan(&stops); err != nil || stops != 2 {
		t.Fatalf("idempotent merge stop actions=%d err=%v", stops, err)
	}
	// Crash after Driver durably stopped the first exact incarnation but before
	// Flowbee received the receipt. Recovery must project by action receipt and
	// never issue the Stop twice.
	lost := driver.LifecycleRuntime{Port: &lostWorkerStopResponsePort{FakePort: fake}, Store: actionStore,
		Projector: driverbridge.Projector{Store: h.st}, Owner: "lost-stop-runtime",
		ClaimTTL: time.Minute, MaximumTries: 5}
	if rep, err := lost.Tick(ctx, rotatedAt.Add(3*time.Minute)); err != nil || rep.Executed != 0 || fake.StopCalls != 1 {
		t.Fatalf("lost stop response=%+v calls=%d err=%v", rep, fake.StopCalls, err)
	}
	if rep, err := runtime.Tick(ctx, rotatedAt.Add(5*time.Minute)); err != nil || rep.Verified != 1 || fake.StopCalls != 1 {
		t.Fatalf("stop receipt recovery=%+v calls=%d err=%v", rep, fake.StopCalls, err)
	}
	if rep, err := runtime.Tick(ctx, rotatedAt.Add(6*time.Minute)); err != nil || rep.Executed != 1 {
		t.Fatalf("second worker stop=%+v err=%v", rep, err)
	}
	var stopped int
	if err := h.st.DB.QueryRow(`SELECT COUNT(*) FROM epic_worker_sessions
		WHERE epic_id='dedicated-workers' AND state='stopped' AND stopped_at<>''`).Scan(&stopped); err != nil || stopped != 2 {
		t.Fatalf("verified stopped workers=%d err=%v", stopped, err)
	}
	if fake.StopCalls != 2 {
		t.Fatalf("Driver stop calls=%d", fake.StopCalls)
	}
	if h.st.AuthorizeEpicWorkerCredential(ctx, dedicatedIdentity, "default", "reviewer",
		envelopeRef, int64(generation), capacityAt.Add(4*time.Minute)) {
		t.Fatal("stopped worker credential remained authorized")
	}
}

func TestMergedEpicNeverLaunchedWorkersCloseByDurableNoEffectProof(t *testing.T) {
	ctx := context.Background()
	h := newBuilderLaunchHarness(t, 1)
	h.st.EnableEpicReviewHandoffV2 = true
	h.st.EnableEpicDedicatedWorkersV2 = true
	h.addEpic(t, "never-launched-stop")
	if err := h.st.SetDurableEpicDedicatedWorkersV2(ctx, true, h.now.Add(30*time.Second)); err != nil {
		t.Fatal(err)
	}
	// Simulate a process restart with the environment omitted. The durable bit,
	// not the old process boolean, must keep shutdown reconciliation active.
	h.st.EnableEpicDedicatedWorkersV2 = false
	stamp := h.now.UTC().Format(time.RFC3339Nano)
	if _, err := h.st.DB.Exec(`UPDATE epic_artifacts SET merged=1,updated_at=? WHERE epic_id=?`,
		stamp, "never-launched-stop"); err != nil {
		t.Fatal(err)
	}
	if _, err := h.st.DB.Exec(`UPDATE epic_deliveries SET state='cleanup_pending',updated_at=? WHERE epic_id=?`,
		stamp, "never-launched-stop"); err != nil {
		t.Fatal(err)
	}
	report, err := h.st.ReconcileEpicWorkerStops(ctx, h.now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	var stopped, revoked, stopActions int
	_ = h.st.DB.QueryRow(`SELECT COUNT(*) FROM epic_worker_sessions WHERE epic_id=? AND state='stopped'`,
		"never-launched-stop").Scan(&stopped)
	_ = h.st.DB.QueryRow(`SELECT COUNT(*) FROM epic_worker_credentials WHERE epic_id=? AND state='revoked'`,
		"never-launched-stop").Scan(&revoked)
	_ = h.st.DB.QueryRow(`SELECT COUNT(*) FROM epic_actions WHERE epic_id=? AND kind='worker_stop'`,
		"never-launched-stop").Scan(&stopActions)
	if report.Scanned != 1 || stopped != 2 || revoked != 2 || stopActions != 0 {
		t.Fatalf("report=%+v stopped=%d revoked=%d stop_actions=%d", report, stopped, revoked, stopActions)
	}
}

// This is the production drop reproduction: workspace preparation succeeds,
// material resolution fails before Ensure, the action is retried and then merge
// arrives.  Cleanup must use the certified local path rather than inventing a
// Driver Stop, and it must close both durable worker obligations.
func TestMergedEpicPreEffectWorkerEnsureCleansPreparedWorkspaceWithoutDriverCall(t *testing.T) {
	ctx := context.Background()
	h := newBuilderLaunchHarness(t, 1)
	h.st.EnableEpicReviewHandoffV2 = true
	h.st.EnableEpicDedicatedWorkersV2 = true
	h.st.EpicWorkerCredentialMaterializer = func(_, _, _, _ string, _ int64, _ time.Time) (string, error) {
		return "sha256:pre-effect-envelope", nil
	}
	h.addEpic(t, "pre-effect-workspace")
	if _, err := h.st.DB.Exec(`UPDATE builder_driver_targets SET profile_id='codex_builder' WHERE seat_id=?`, h.seat.ID); err != nil {
		t.Fatal(err)
	}
	if err := h.st.SetDurableEpicDedicatedWorkersV2(ctx, true, h.now.Add(10*time.Second)); err != nil {
		t.Fatal(err)
	}
	if rep, err := h.st.ReconcileBuilderLaunches(ctx, h.now.Add(time.Minute), 5*time.Minute, "codex", 5); err != nil || rep.ActionsCreated != 1 {
		t.Fatalf("launch=%+v err=%v", rep, err)
	}
	fake := driver.NewFake()
	workspaces := &preEffectWorkspaceRecorder{}
	runtime := driver.LifecycleRuntime{Port: fake, Store: driver.SQLActionStore{DB: h.st.DB},
		Projector: driverbridge.Projector{Store: h.st}, Materials: materialFailureAfterPrepare{},
		Workspaces: workspaces, Owner: "pre-effect-material-failure", ClaimTTL: time.Minute, MaximumTries: 2}
	first, err := runtime.Tick(ctx, h.now.Add(2*time.Minute))
	if err != nil || first.Retried != 1 || workspaces.prepared != 1 || fake.EnsureCalls != 0 || fake.StopCalls != 0 {
		t.Fatalf("first=%+v prepared=%d ensure=%d stop=%d err=%v", first, workspaces.prepared, fake.EnsureCalls, fake.StopCalls, err)
	}
	// The retry proves the certificate remains durable through a claim/retry;
	// MaximumTries=2 now puts the original Ensure in dead_letter, still without
	// ever contacting Driver.
	second, err := runtime.Tick(ctx, h.now.Add(4*time.Minute))
	if err != nil || second.DeadLettered != 1 || workspaces.prepared != 2 || fake.EnsureCalls != 0 || fake.StopCalls != 0 {
		t.Fatalf("second=%+v prepared=%d ensure=%d stop=%d err=%v", second, workspaces.prepared, fake.EnsureCalls, fake.StopCalls, err)
	}
	stamp := h.now.Add(5 * time.Minute).UTC().Format(time.RFC3339Nano)
	if _, err := h.st.DB.Exec(`UPDATE epic_artifacts SET merged=1,updated_at=? WHERE epic_id=?`, stamp, "pre-effect-workspace"); err != nil {
		t.Fatal(err)
	}
	if _, err := h.st.DB.Exec(`UPDATE epic_deliveries SET state='cleanup_pending',updated_at=? WHERE epic_id=?`, stamp, "pre-effect-workspace"); err != nil {
		t.Fatal(err)
	}
	shutdown, err := h.st.ReconcileEpicWorkerStops(ctx, h.now.Add(6*time.Minute))
	if err != nil || shutdown.ActionsEnsured != 0 || shutdown.Held != 0 {
		t.Fatalf("shutdown=%+v err=%v", shutdown, err)
	}
	var cleanupID, cleanupKind, originalState string
	if err := h.st.DB.QueryRow(`SELECT id,kind FROM epic_actions WHERE epic_id=? AND kind='worker_workspace_cleanup'`,
		"pre-effect-workspace").Scan(&cleanupID, &cleanupKind); err != nil {
		t.Fatal(err)
	}
	if err := h.st.DB.QueryRow(`SELECT state FROM epic_actions WHERE epic_id=(?) AND kind='builder_launch'`,
		"pre-effect-workspace").Scan(&originalState); err != nil {
		t.Fatal(err)
	}
	if cleanupKind != "worker_workspace_cleanup" || originalState != "dead_letter" {
		t.Fatalf("cleanup=%s/%s original=%s", cleanupID, cleanupKind, originalState)
	}
	cleaned, err := runtime.Tick(ctx, h.now.Add(7*time.Minute))
	if err != nil || cleaned.Executed != 1 || workspaces.cleaned != 1 ||
		workspaces.cleanedAction.ActionID != cleanupID || fake.EnsureCalls != 0 || fake.StopCalls != 0 {
		t.Fatalf("cleaned=%+v workspace=%+v ensure=%d stop=%d err=%v", cleaned, workspaces, fake.EnsureCalls, fake.StopCalls, err)
	}
	var stopped, revoked int
	if err := h.st.DB.QueryRow(`SELECT COUNT(*) FROM epic_worker_sessions WHERE epic_id=? AND state='stopped'`,
		"pre-effect-workspace").Scan(&stopped); err != nil {
		t.Fatal(err)
	}
	if err := h.st.DB.QueryRow(`SELECT COUNT(*) FROM epic_worker_credentials WHERE epic_id=? AND state='revoked'`,
		"pre-effect-workspace").Scan(&revoked); err != nil {
		t.Fatal(err)
	}
	if stopped != 2 || revoked != 2 {
		t.Fatalf("workers stopped=%d credentials revoked=%d", stopped, revoked)
	}
}

func TestDurableDedicatedWorkerRestartBlocksCleanupUntilBothStops(t *testing.T) {
	ctx := context.Background()
	h := newBuilderLaunchHarness(t, 1)
	h.st.EnableEpicReviewHandoffV2 = true
	h.st.EnableEpicDedicatedWorkersV2 = true
	h.addEpic(t, "restart-cleanup-fence")
	if err := h.st.SetDurableEpicDedicatedWorkersV2(ctx, true, h.now.Add(30*time.Second)); err != nil {
		t.Fatal(err)
	}
	h.st.EnableEpicDedicatedWorkersV2 = false
	stamp := h.now.Add(time.Minute).UTC().Format(time.RFC3339Nano)
	if _, err := h.st.DB.Exec(`UPDATE epic_deliveries SET state='cleanup_pending',updated_at=? WHERE epic_id=?`,
		stamp, "restart-cleanup-fence"); err != nil {
		t.Fatal(err)
	}
	if _, err := h.st.DB.Exec(`INSERT INTO epic_actions
		(id,project_id,epic_id,kind,state,action_epoch,dedup_key,payload_json,payload_sha256,
		 executor_kind,next_attempt_at,created_at,updated_at)
		VALUES ('restart-cleanup','default','restart-cleanup-fence','cleanup','delivering',1,
		 'restart-cleanup','{}','sha256:test','github',?,?,?)`, stamp, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	err := h.st.CompleteEpicCleanup(ctx, store.EpicDomainAction{ID: "restart-cleanup",
		EpicID: "restart-cleanup-fence", Epoch: 1}, "", h.now.Add(2*time.Minute))
	if err == nil || !strings.Contains(err.Error(), "mechanically stopped") {
		t.Fatalf("cleanup crossed durable restart stop fence: %v", err)
	}
	var deliveryState, actionState string
	if err := h.st.DB.QueryRow(`SELECT state FROM epic_deliveries WHERE epic_id='restart-cleanup-fence'`).Scan(&deliveryState); err != nil {
		t.Fatal(err)
	}
	if err := h.st.DB.QueryRow(`SELECT state FROM epic_actions WHERE id='restart-cleanup'`).Scan(&actionState); err != nil {
		t.Fatal(err)
	}
	if deliveryState != "cleanup_pending" || actionState != "delivering" {
		t.Fatalf("blocked cleanup mutated delivery=%s action=%s", deliveryState, actionState)
	}
}

func TestMergedEpicUncertainEnsureRemainsVisibleAndRetryable(t *testing.T) {
	ctx := context.Background()
	h := newBuilderLaunchHarness(t, 1)
	h.st.EnableEpicReviewHandoffV2 = true
	h.st.EnableEpicDedicatedWorkersV2 = true
	h.addEpic(t, "uncertain-launch-stop")
	stamp := h.now.UTC().Format(time.RFC3339Nano)
	if _, err := h.st.DB.Exec(`INSERT INTO epic_actions
		(id,project_id,epic_id,kind,state,action_epoch,dedup_key,payload_json,payload_sha256,
		 executor_kind,next_attempt_at,created_at,updated_at)
		VALUES ('uncertain-ensure','default','uncertain-launch-stop','reviewer_launch','verifying',1,
		 'uncertain-ensure','{}','sha256:test','driver_lifecycle',?,?,?)`, stamp, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	if _, err := h.st.DB.Exec(`UPDATE epic_worker_sessions SET state='ensure_pending',ensure_action_id='uncertain-ensure'
		WHERE epic_id='uncertain-launch-stop' AND worker_role='reviewer'`); err != nil {
		t.Fatal(err)
	}
	if _, err := h.st.DB.Exec(`UPDATE epic_artifacts SET merged=1,updated_at=? WHERE epic_id=?`,
		stamp, "uncertain-launch-stop"); err != nil {
		t.Fatal(err)
	}
	if _, err := h.st.DB.Exec(`UPDATE epic_deliveries SET state='cleanup_pending',updated_at=? WHERE epic_id=?`,
		stamp, "uncertain-launch-stop"); err != nil {
		t.Fatal(err)
	}
	first := h.now.Add(time.Minute)
	report, err := h.st.ReconcileEpicWorkerStops(ctx, first)
	if err != nil {
		t.Fatal(err)
	}
	var state, due string
	if err := h.st.DB.QueryRow(`SELECT state,state_due_at FROM epic_worker_sessions
		WHERE epic_id='uncertain-launch-stop' AND worker_role='reviewer'`).Scan(&state, &due); err != nil {
		t.Fatal(err)
	}
	var attention int
	_ = h.st.DB.QueryRow(`SELECT COUNT(*) FROM attention_items WHERE epic_id='uncertain-launch-stop'
		AND kind='epic_worker_stop_unresolved' AND state='open'`).Scan(&attention)
	if report.Held != 1 || state != "held" || due == "" || attention != 1 {
		t.Fatalf("report=%+v state=%s due=%s attention=%d", report, state, due, attention)
	}
	// A later pass revisits the hold; held is not terminal limbo.
	second, err := h.st.ReconcileEpicWorkerStops(ctx, first.Add(6*time.Minute))
	if err != nil || second.Held != 1 {
		t.Fatalf("retry report=%+v err=%v", second, err)
	}
}

func TestDedicatedWorkerDiesAfterAckGetsHigherEpochReplacementAndOldPresenceCannotResurrect(t *testing.T) {
	ctx := context.Background()
	h := newBuilderLaunchHarness(t, 2)
	h.st.EnableEpicReviewHandoffV2 = true
	h.st.EnableEpicDedicatedWorkersV2 = true
	h.addEpic(t, "worker-dies-after-ack")
	materialDir := t.TempDir()
	materials := driver.SQLLifecycleLaunchMaterials{DB: h.st.DB, EnvelopeDirectory: materialDir,
		WorkerAuthSecret: []byte("worker-liveness-replacement-secret")}
	h.st.EpicWorkerCredentialMaterializer = materials.PrepareEnvelope

	oldIdentity := driver.Identity{HostID: "host-build", StoreID: "store-build",
		TmuxServerDomainID: "flowbee", TmuxServerInstanceID: "server-build",
		Ownership: "driver_managed", LifecycleKey: store.EpicWorkerLifecycleKey("worker-dies-after-ack", "builder"),
		TargetEpoch: 1, SessionID: "old-session", PaneInstanceID: "old-pane",
		AgentRunID: "old-run", Provider: "codex"}
	binding, err := h.st.UpsertDriverSessionBinding(ctx, store.DriverSessionBinding{ProjectID: "default",
		WorkerIdentity: store.BuilderDriverIdentity("worker-dies-after-ack"), Role: store.DriverBuilderRole,
		HostID: oldIdentity.HostID, StoreID: oldIdentity.StoreID,
		TmuxServerDomainID:   oldIdentity.TmuxServerDomainID,
		TmuxServerInstanceID: oldIdentity.TmuxServerInstanceID, LifecycleOwnership: oldIdentity.Ownership,
		LifecycleKey: oldIdentity.LifecycleKey, TargetEpoch: oldIdentity.TargetEpoch,
		ProfileID: "codex_builder", WorkspaceRootID: "workspace-build",
		WorkspaceRelativePath: "repos/default/worker-dies-after-ack", SessionID: oldIdentity.SessionID,
		PaneInstanceID: oldIdentity.PaneInstanceID, AgentRunID: oldIdentity.AgentRunID,
		Provider: "codex", ObservedAt: h.now}, h.now)
	if err != nil {
		t.Fatal(err)
	}
	stamp := h.now.UTC().Format(time.RFC3339Nano)
	expires := h.now.Add(24 * time.Hour).UTC().Format(time.RFC3339Nano)
	if _, err := h.st.DB.Exec(`UPDATE epic_worker_sessions SET state='active',seat_id=?,binding_id=?,
		ensure_action_id='initial-builder-ensure',target_epoch=1 WHERE epic_id=? AND worker_role='builder'`,
		h.seat.ID, binding.BindingID, "worker-dies-after-ack"); err != nil {
		t.Fatal(err)
	}
	if _, err := h.st.DB.Exec(`UPDATE epic_worker_credentials SET state='installed',generation=1,
		envelope_ref='flowbee://old-envelope',payload_sha256='sha256:old',
		ensure_action_id='initial-builder-ensure',issued_at=?,refresh_after=?,expires_at=?,installed_at=?
		WHERE epic_id=? AND worker_role='builder'`, stamp, expires, expires, stamp,
		"worker-dies-after-ack"); err != nil {
		t.Fatal(err)
	}

	fake := driver.NewFake() // no matching session: exact positive absence
	liveness := driver.EpicWorkerLivenessRuntime{Port: fake, Store: h.st}
	recoverAt := h.now.Add(time.Minute)
	report, err := liveness.Tick(ctx, recoverAt)
	if err != nil || report.Recovered != 1 || fake.EnsureCalls != 0 {
		t.Fatalf("liveness recovery=%+v ensures=%d err=%v", report, fake.EnsureCalls, err)
	}
	var workerState, replacementAction string
	var targetEpoch, generation int64
	if err := h.st.DB.QueryRow(`SELECT w.state,w.target_epoch,w.ensure_action_id,c.generation
		FROM epic_worker_sessions w JOIN epic_worker_credentials c
		ON c.epic_id=w.epic_id AND c.worker_role=w.worker_role
		WHERE w.epic_id=? AND w.worker_role='builder'`, "worker-dies-after-ack").
		Scan(&workerState, &targetEpoch, &replacementAction, &generation); err != nil {
		t.Fatal(err)
	}
	if workerState != "ensure_pending" || targetEpoch != 2 || generation != 2 || replacementAction == "" {
		t.Fatalf("replacement state=%s target=%d generation=%d action=%q",
			workerState, targetEpoch, generation, replacementAction)
	}
	var oldBindingState string
	if err := h.st.DB.QueryRow(`SELECT state FROM driver_session_bindings WHERE binding_id=?`,
		binding.BindingID).Scan(&oldBindingState); err != nil || oldBindingState != "superseded" {
		t.Fatalf("old binding state=%q err=%v", oldBindingState, err)
	}

	lifecycle := driver.LifecycleRuntime{Port: fake, Store: driver.SQLActionStore{DB: h.st.DB},
		Projector: driverbridge.Projector{Store: h.st}, Materials: materials,
		Workspaces:            testLifecycleWorkspaceManager{},
		RequireManagedAgentV3: true, Owner: "worker-recovery-runtime", ClaimTTL: time.Minute,
		MaximumTries: 5}
	if run, err := lifecycle.Tick(ctx, recoverAt.Add(time.Minute)); err != nil || run.Executed != 1 || fake.EnsureCalls != 1 {
		t.Fatalf("replacement Ensure=%+v calls=%d err=%v", run, fake.EnsureCalls, err)
	}
	var activeTarget int64
	var activeRun string
	if err := h.st.DB.QueryRow(`SELECT target_epoch,agent_run_id FROM driver_session_bindings
		WHERE project_id='default' AND worker_identity=? AND role='builder' AND state='active'`,
		store.BuilderDriverIdentity("worker-dies-after-ack")).Scan(&activeTarget, &activeRun); err != nil {
		t.Fatal(err)
	}
	if activeTarget != 2 || activeRun == oldIdentity.AgentRunID {
		t.Fatalf("active replacement target=%d run=%q", activeTarget, activeRun)
	}
	// A restart may omit the legacy env flag; durable activation remains the
	// authority for post-ack supervision.
	h.st.EnableEpicDedicatedWorkersV2 = false
	if err := h.st.SetDurableEpicDedicatedWorkersV2(ctx, true, recoverAt.Add(90*time.Second)); err != nil {
		t.Fatal(err)
	}

	// Remove the replacement from the fake and expose only the stale target-1
	// incarnation. The liveness fold must hold on the mismatch; it may never
	// promote or resurrect the old session/run tuple.
	fake.Sessions = map[string]driver.Identity{oldIdentity.SessionID: oldIdentity}
	staleReport, err := liveness.Tick(ctx, recoverAt.Add(2*time.Minute))
	if err != nil || staleReport.Held != 1 || staleReport.Recovered != 0 {
		t.Fatalf("stale presence report=%+v err=%v", staleReport, err)
	}
	var stillActive int
	if err := h.st.DB.QueryRow(`SELECT COUNT(*) FROM driver_session_bindings WHERE project_id='default'
		AND worker_identity=? AND role='builder' AND state='active' AND target_epoch=2 AND agent_run_id=?`,
		store.BuilderDriverIdentity("worker-dies-after-ack"), activeRun).Scan(&stillActive); err != nil || stillActive != 1 {
		t.Fatalf("stale presence resurrected old authority: active=%d err=%v", stillActive, err)
	}
}
