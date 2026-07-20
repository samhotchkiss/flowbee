package driver

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/samhotchkiss/flowbee/internal/testutil"
)

func observation(storeID, eventID, sessionID, paneID, kind string, seq uint64, payload string) Observation {
	return Observation{
		SpecVersion: "tmux-driver.events/v2", EventID: eventID, Cursor: "tdc2." + eventID,
		StoreSeq: seq, SessionSeq: seq, TransitionID: "transition-" + eventID,
		TransitionIndex: 0, TransitionCount: 1, ProducerBootID: "boot-1", Kind: kind,
		ObservedAt: "2026-07-19T12:00:00.000Z",
		Identity: Identity{HostID: "host-1", StoreID: storeID, SessionID: sessionID,
			PaneInstanceID: paneID, StateCursor: "tdc2." + eventID},
		Source: json.RawMessage(`{"kind":"process"}`), Correlation: json.RawMessage(`{}`),
		CausedBy: []string{}, Payload: json.RawMessage(payload),
	}
}

func snapshot(storeID, cursor string, sessions ...SessionProjection) SessionSnapshot {
	return SessionSnapshot{HostID: "host-1", StoreID: storeID, AsOfCursor: cursor, Sessions: sessions}
}

func session(storeID, sessionID, paneID, runID string) SessionProjection {
	return SessionProjection{Identity: Identity{HostID: "host-1", StoreID: storeID,
		TmuxServerDomainID: "flowbee", TmuxServerInstanceID: "server-1", SessionID: sessionID, PaneInstanceID: paneID,
		AgentRunID: runID, Provider: "codex"}, Lifecycle: "observing", Phase: "idle",
		StateRevision: 1, RawState: json.RawMessage(`{"working_directory":"/same/repo"}`)}
}

func newObservationHarness(t *testing.T) (ObservationSQLStore, *FakePort, ObservationIngestor) {
	t.Helper()
	st := testutil.NewStore(t)
	now := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	sqlStore := ObservationSQLStore{DB: st.DB, Now: func() time.Time { return now }}
	fake := NewFake()
	fake.Meta = DriverMetadata{HostID: "host-1", StoreID: "store-1", ProducerBootID: "boot-1",
		ReplayFloorCursor: "tdc2.floor-1", DurableHighWaterCursor: "tdc2.snap-1",
		TmuxServer: TmuxServerMetadata{DomainID: "flowbee", Ownership: "managed_dedicated"}}
	ingestor := ObservationIngestor{InstanceRef: "local-driver", Port: fake, Store: sqlStore}
	return sqlStore, fake, ingestor
}

func TestObservationSnapshotKeepsSameCWDSessionsSeparateByStableIdentity(t *testing.T) {
	sqlStore, fake, ingestor := newObservationHarness(t)
	fake.Snapshot = snapshot("store-1", "tdc2.snap-1",
		session("store-1", "session-a", "pane-a", "run-a"),
		session("store-1", "session-b", "pane-b", "run-b"))
	fake.Batches = []ObservationBatch{{StoreID: "store-1", NextCursor: "tdc2.snap-1",
		DurableHighWaterCursor: "tdc2.snap-1", HistoryComplete: true}}

	result, err := ingestor.Tick(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !result.SnapshotReplaced || !result.CaughtUp {
		t.Fatalf("result=%+v", result)
	}
	for _, want := range []Identity{
		{HostID: "host-1", StoreID: "store-1", TmuxServerDomainID: "flowbee", TmuxServerInstanceID: "server-1", SessionID: "session-a", PaneInstanceID: "pane-a", AgentRunID: "run-a"},
		{HostID: "host-1", StoreID: "store-1", TmuxServerDomainID: "flowbee", TmuxServerInstanceID: "server-1", SessionID: "session-b", PaneInstanceID: "pane-b", AgentRunID: "run-b"},
	} {
		ok, err := sqlStore.IsCurrentIdentity(context.Background(), "local-driver", want)
		if err != nil || !ok {
			t.Fatalf("identity %+v current=%v err=%v", want, ok, err)
		}
	}
}

func TestObservationFoldFencesPaneReuseAndDeduplicatesEvent(t *testing.T) {
	sqlStore, fake, ingestor := newObservationHarness(t)
	fake.Snapshot = snapshot("store-1", "tdc2.snap-1", session("store-1", "session-a", "pane-old", "run-a"))
	event := observation("store-1", "event-2", "session-a", "pane-new", "session.metadata_changed", 2,
		`{"provider":"codex"}`)
	fake.Batches = []ObservationBatch{{StoreID: "store-1", NextCursor: "tdc2.event-2",
		DurableHighWaterCursor: "tdc2.event-2", HistoryComplete: true, Events: []Observation{event}}}

	result, err := ingestor.Tick(context.Background())
	if err != nil || result.Inserted != 1 {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	old := Identity{HostID: "host-1", StoreID: "store-1", TmuxServerDomainID: "flowbee", TmuxServerInstanceID: "server-1",
		SessionID: "session-a", PaneInstanceID: "pane-old", AgentRunID: "run-a"}
	current := old
	current.PaneInstanceID = "pane-new"
	if ok, _ := sqlStore.IsCurrentIdentity(context.Background(), "local-driver", old); ok {
		t.Fatal("reused pane left old incarnation routable")
	}
	if ok, err := sqlStore.IsCurrentIdentity(context.Background(), "local-driver", current); err != nil || !ok {
		t.Fatalf("new incarnation current=%v err=%v", ok, err)
	}

	result, err = sqlStore.Fold(context.Background(), "local-driver", ObservationBatch{
		StoreID: "store-1", NextCursor: "tdc2.event-2", DurableHighWaterCursor: "tdc2.event-2",
		HistoryComplete: true, Events: []Observation{event}})
	if err != nil || result.Inserted != 0 || result.Deduplicated != 1 {
		t.Fatalf("replay result=%+v err=%v", result, err)
	}
	var count int
	if err := sqlStore.DB.QueryRow(`SELECT COUNT(*) FROM driver_observation_events`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("event count=%d err=%v", count, err)
	}
}

func TestCursorGapReplacesOnlyDriverProjection(t *testing.T) {
	sqlStore, fake, ingestor := newObservationHarness(t)
	fake.Snapshot = snapshot("store-1", "tdc2.snap-1", session("store-1", "session-a", "pane-a", "run-a"))
	fake.Batches = []ObservationBatch{{StoreID: "store-1", NextCursor: "tdc2.snap-1", DurableHighWaterCursor: "tdc2.snap-1", HistoryComplete: true}}
	if _, err := ingestor.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := sqlStore.DB.Exec(`INSERT INTO control_events(project_id,kind,payload_json) VALUES ('default','flowbee_truth','{}')`); err != nil {
		t.Fatal(err)
	}
	replacement := session("store-1", "session-a", "pane-a", "run-a")
	replacement.Phase = "waiting"
	fake.Snapshot = snapshot("store-1", "tdc2.snap-9", replacement)
	fake.Batches = []ObservationBatch{{CursorGap: true}}
	result, err := ingestor.Tick(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !result.CursorGap || !result.SnapshotReplaced {
		t.Fatalf("result=%+v", result)
	}
	got, err := sqlStore.Session(context.Background(), "store-1", "session-a")
	if err != nil || got.Phase != "waiting" || got.AsOfCursor != "tdc2.snap-9" {
		t.Fatalf("session=%+v err=%v", got, err)
	}
	var controlCount int
	if err := sqlStore.DB.QueryRow(`SELECT COUNT(*) FROM control_events WHERE kind='flowbee_truth'`).Scan(&controlCount); err != nil || controlCount != 1 {
		t.Fatalf("Flowbee audit truth changed: count=%d err=%v", controlCount, err)
	}
}

func TestStoreResetFencesOldDomainWithoutDeletingObservationOrFlowbeeAudit(t *testing.T) {
	sqlStore, fake, ingestor := newObservationHarness(t)
	fake.Snapshot = snapshot("store-1", "tdc2.snap-1", session("store-1", "session-old", "pane-old", "run-old"))
	event := observation("store-1", "event-old", "session-old", "pane-old", "phase.changed", 2, `{"phase":"working"}`)
	fake.Batches = []ObservationBatch{{StoreID: "store-1", NextCursor: "tdc2.event-old",
		DurableHighWaterCursor: "tdc2.event-old", HistoryComplete: true, Events: []Observation{event}}}
	if _, err := ingestor.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := sqlStore.DB.Exec(`INSERT INTO control_events(project_id,kind,payload_json) VALUES ('default','product_audit','{}')`); err != nil {
		t.Fatal(err)
	}

	fake.Meta = DriverMetadata{HostID: "host-1", StoreID: "store-2", ProducerBootID: "boot-2",
		ReplayFloorCursor: "tdc2.floor-2", DurableHighWaterCursor: "tdc2.snap-2",
		TmuxServer: TmuxServerMetadata{DomainID: "flowbee", Ownership: "managed_dedicated"}}
	fake.Snapshot = snapshot("store-2", "tdc2.snap-2", session("store-2", "session-new", "pane-new", "run-new"))
	fake.Batches = []ObservationBatch{{StoreID: "store-2", NextCursor: "tdc2.snap-2",
		DurableHighWaterCursor: "tdc2.snap-2", HistoryComplete: true}}
	result, err := ingestor.Tick(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !result.StoreReset || !result.SnapshotReplaced {
		t.Fatalf("result=%+v", result)
	}
	if _, err := sqlStore.Session(context.Background(), "store-1", "session-old"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("old-store projection survived reset: %v", err)
	}
	if _, err := sqlStore.Session(context.Background(), "store-2", "session-new"); err != nil {
		t.Fatalf("new projection: %v", err)
	}
	var oldEvents, productAudit int
	if err := sqlStore.DB.QueryRow(`SELECT COUNT(*) FROM driver_observation_events WHERE store_id='store-1'`).Scan(&oldEvents); err != nil {
		t.Fatal(err)
	}
	if err := sqlStore.DB.QueryRow(`SELECT COUNT(*) FROM control_events WHERE kind='product_audit'`).Scan(&productAudit); err != nil {
		t.Fatal(err)
	}
	if oldEvents != 1 || productAudit != 1 {
		t.Fatalf("old events=%d product audit=%d", oldEvents, productAudit)
	}
	instance, err := sqlStore.Instance(context.Background(), "local-driver")
	if err != nil || instance.StoreID != "store-2" || instance.ResetCount != 1 {
		t.Fatalf("instance=%+v err=%v", instance, err)
	}
}
