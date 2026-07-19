package store_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/samhotchkiss/flowbee/internal/capacity"
	"github.com/samhotchkiss/flowbee/internal/store"
	"github.com/samhotchkiss/flowbee/internal/testutil"
	"github.com/samhotchkiss/flowbee/internal/workintent"
)

func addAdmissionCapacitySeat(t *testing.T, st *store.Store, family, host, account string,
	now time.Time) (store.Seat, store.CapacitySeatObservation) {
	t.Helper()
	seat := store.Seat{Box: host, AgentFamily: family, Health: store.SeatReady, MaxConcurrent: 2}
	switch family {
	case "codex":
		seat.CodexHome = "/capacity/" + account
	case "grok", "claude":
		seat.ConfigDir = "/capacity/" + account
	default:
		t.Fatalf("unsupported test family %q", family)
	}
	if err := st.AddSeat(context.Background(), seat, now); err != nil {
		t.Fatal(err)
	}
	seat.ID = seat.ComposeID()
	lineage := "lineage-" + account
	if err := st.BindCapacitySeatIdentity(context.Background(), store.CapacitySeatIdentity{
		SeatID: seat.ID, HostID: host, AccountKey: account, CredentialLineage: lineage,
		ReservePct: 10, AccountMaximum: 4,
	}, now); err != nil {
		t.Fatal(err)
	}
	observation := store.CapacitySeatObservation{
		ObservationID: "observation-" + account, SeatID: seat.ID, HostID: host,
		Provider: family, AccountKey: account, CredentialLineage: lineage,
		CollectorID: "collector-" + account, TrustState: "verified", IntegrityState: "verified",
		FetchedAt: now, RawSHA256: "sha256:" + account, AdapterVersion: family + "-test/v1",
	}
	switch family {
	case "codex":
		observation.Source = "live_app_server"
		observation.Windows = []capacity.RouteWindow{{Kind: "weekly", Applicable: true, Known: true, Percent: 20}}
	case "grok":
		observation.Source = "live_billing"
		observation.BillingPeriodActive = true
		observation.Windows = []capacity.RouteWindow{{Kind: "monthly", Applicable: true, Known: true, Percent: 20}}
	case "claude":
		// Claude capacity is deliberately unsupported by the v2 route predicate
		// today; tests never use it as a positive reviewer route.
		observation.Source = "live_app_server"
	}
	return seat, observation
}

func commitAdmissionCapacity(t *testing.T, st *store.Store, id string, now time.Time,
	seats []store.Seat, observations []store.CapacitySeatObservation) {
	t.Helper()
	ids := make([]string, 0, len(seats))
	for _, seat := range seats {
		ids = append(ids, seat.ID)
	}
	if err := st.CommitCapacityGeneration(context.Background(), store.CapacityGeneration{
		ID: id, StartedAt: now, ExpectedSeatIDs: ids, Observations: observations,
	}, now); err != nil {
		t.Fatal(err)
	}
}

func TestV2AdmissionRejectsNoReviewerWithoutPartialRows(t *testing.T) {
	ctx := context.Background()
	st := testutil.NewStore(t)
	st.EnableCapacityV2 = true
	now := time.Date(2026, 7, 19, 18, 0, 0, 0, time.UTC)
	err := st.AddEpicRun(ctx, store.EpicRun{ID: "no-reviewer", ProjectID: "default",
		AdmissionKey: "no-reviewer:v1", Repo: "russ", Branch: "epic/no-reviewer"}, 1, now)
	if !errors.Is(err, store.ErrEpicDistinctReviewerUnavailable) {
		t.Fatalf("admission error=%v", err)
	}
	var epics, deliveries int
	_ = st.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM epics WHERE id='no-reviewer'`).Scan(&epics)
	_ = st.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM epic_deliveries WHERE epic_id='no-reviewer'`).Scan(&deliveries)
	if epics != 0 || deliveries != 0 {
		t.Fatalf("failed admission was partial: epics=%d deliveries=%d", epics, deliveries)
	}
}

func TestV2AdmissionRejectsFreshSameFamilyCapacity(t *testing.T) {
	ctx := context.Background()
	st := testutil.NewStore(t)
	st.EnableCapacityV2 = true
	now := time.Date(2026, 7, 19, 18, 10, 0, 0, time.UTC)
	codex, codexObservation := addAdmissionCapacitySeat(t, st, "codex", "codex-host", "codex-account", now)
	commitAdmissionCapacity(t, st, "same-family", now, []store.Seat{codex},
		[]store.CapacitySeatObservation{codexObservation})
	err := st.AddEpicRun(ctx, store.EpicRun{ID: "same-family", ProjectID: "default",
		AdmissionKey: "same-family:v1", BuilderModelFamily: "codex", Repo: "russ",
		Branch: "epic/same-family"}, 1, now.Add(time.Minute))
	if !errors.Is(err, store.ErrEpicDistinctReviewerUnavailable) ||
		!strings.Contains(err.Error(), "no configured review seat from a distinct family") {
		t.Fatalf("same-family admission error=%v", err)
	}
}

func TestV2AdmissionWithDistinctFreshReviewerIsExactlyOnceAcrossLostAck(t *testing.T) {
	ctx := context.Background()
	st := testutil.NewStore(t)
	st.EnableCapacityV2 = true
	now := time.Date(2026, 7, 19, 18, 20, 0, 0, time.UTC)
	grok, grokObservation := addAdmissionCapacitySeat(t, st, "grok", "grok-host", "grok-account", now)
	commitAdmissionCapacity(t, st, "distinct-reviewer", now, []store.Seat{grok},
		[]store.CapacitySeatObservation{grokObservation})
	epic := store.EpicRun{ID: "distinct-reviewer", ProjectID: "default",
		AdmissionKey: "distinct-reviewer:v1", ContractHash: "contract-v1",
		BuilderModelFamily: "codex", Repo: "russ", Branch: "epic/distinct-reviewer"}
	if err := st.AddEpicRun(ctx, epic, 1, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	// Simulate a lost admission acknowledgement. Capacity is stale by the retry,
	// but the stable key must resolve the already-committed admission rather than
	// evaluating a second admission or creating a duplicate obligation.
	epic.ID = "different-retry-id"
	if err := st.AddEpicRun(ctx, epic, 1, now.Add(time.Hour)); err != nil {
		t.Fatalf("lost-ack replay: %v", err)
	}
	var epics, deliveries int
	_ = st.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM epics WHERE admission_key=?`, epic.AdmissionKey).Scan(&epics)
	_ = st.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM epic_deliveries`).Scan(&deliveries)
	if epics != 1 || deliveries != 1 {
		t.Fatalf("replay duplicated admission: epics=%d deliveries=%d", epics, deliveries)
	}
}

func TestWorkIntentAdmissionCapacityHoldIsDurableAndRecovers(t *testing.T) {
	ctx := context.Background()
	st := testutil.NewStore(t)
	st.EnableEpicReviewHandoffV2 = true
	st.EnableCapacityV2 = true
	now := time.Date(2026, 7, 19, 18, 30, 0, 0, time.UTC)
	intent, binding := seedOrchestratingWorkIntent(t, st, "capacity-contract", "capacity-orchestrator", now)
	contract := validPreparedContract("capacity-held")
	hash, err := store.WorkIntentEpicContractSHA256(contract)
	if err != nil {
		t.Fatal(err)
	}
	key, _ := workintent.AdmissionKey(workintent.Intent{ID: intent.ID, ProjectID: "default", Version: 1})
	_, err = st.RecordWorkIntentEpicContract(ctx, store.RecordWorkIntentEpicContractInput{
		ProjectID: "default", WorkIntentID: intent.ID, IntentVersion: 1,
		ExpectedStateVersion: intent.StateVersion, SourceArtifactSHA256: intent.ArtifactSHA256,
		ContractVersion: 1, ContractRef: "artifact://contract/capacity", ContractSHA256: hash,
		Contract: contract, OrchestratorBindingID: binding.BindingID, SubmissionKey: key,
	}, now.Add(4*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	report, err := st.ReconcileWorkIntentAdmissions(ctx, now.Add(5*time.Minute))
	if err != nil || report.Held != 1 || report.Admitted != 0 {
		t.Fatalf("held admission=%+v err=%v", report, err)
	}
	intent, err = st.GetWorkIntent(ctx, "default", intent.ID)
	if err != nil || intent.HoldKind != "epic_admission_blocked" ||
		!strings.Contains(intent.HoldReason, "distinct routable review family") {
		t.Fatalf("intent hold=%+v err=%v", intent, err)
	}
	var epics, deliveries, alerts int
	_ = st.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM epics WHERE work_intent_id=?`, intent.ID).Scan(&epics)
	_ = st.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM epic_deliveries`).Scan(&deliveries)
	_ = st.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM control_alerts
		WHERE kind='work_intent_epic_admission_blocked'`).Scan(&alerts)
	if epics != 0 || deliveries != 0 || alerts != 1 {
		t.Fatalf("held admission durability: epics=%d deliveries=%d alerts=%d", epics, deliveries, alerts)
	}

	// A later fresh distinct-family generation makes the original prepared
	// contract admissible. It retains the original idempotency key and creates
	// exactly one epic/delivery despite the prior failed attempt.
	grok, observation := addAdmissionCapacitySeat(t, st, "grok", "grok-recovery", "grok-recovery", now.Add(6*time.Minute))
	commitAdmissionCapacity(t, st, "recovered-reviewer", now.Add(6*time.Minute),
		[]store.Seat{grok}, []store.CapacitySeatObservation{observation})
	report, err = st.ReconcileWorkIntentAdmissions(ctx, now.Add(7*time.Minute))
	if err != nil || report.Admitted != 1 {
		t.Fatalf("recovered admission=%+v err=%v", report, err)
	}
	report, err = st.ReconcileWorkIntentAdmissions(ctx, now.Add(8*time.Minute))
	if err != nil || report.Scanned != 0 || report.Admitted != 0 {
		t.Fatalf("idempotent recovered admission=%+v err=%v", report, err)
	}
	_ = st.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM epics WHERE work_intent_id=?`, intent.ID).Scan(&epics)
	_ = st.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM epic_deliveries`).Scan(&deliveries)
	if epics != 1 || deliveries != 1 {
		t.Fatalf("recovery duplicated admission: epics=%d deliveries=%d", epics, deliveries)
	}
}

func TestCapacityDisabledPreservesLegacyAdmission(t *testing.T) {
	ctx := context.Background()
	st := testutil.NewStore(t)
	now := time.Date(2026, 7, 19, 18, 40, 0, 0, time.UTC)
	if err := st.AddEpicRun(ctx, store.EpicRun{ID: "legacy-no-reviewer", ProjectID: "default",
		AdmissionKey: "legacy-no-reviewer:v1", Repo: "russ", Branch: "epic/legacy"}, 1, now); err != nil {
		t.Fatalf("capacity-disabled legacy admission: %v", err)
	}
}
