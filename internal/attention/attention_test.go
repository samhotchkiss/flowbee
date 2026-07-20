package attention

import (
	"reflect"
	"testing"
	"time"
)

var t0 = time.Date(2026, 7, 16, 12, 0, 0, 0, time.UTC)

func at(d time.Duration) time.Time { return t0.Add(d) }

func TestOrder(t *testing.T) {
	items := []Item{
		{ID: "c", Priority: 20, FirstSeenAt: at(0)},
		{ID: "a", Priority: 5, FirstSeenAt: at(2 * time.Minute)},
		{ID: "b", Priority: 5, FirstSeenAt: at(1 * time.Minute)}, // same prio as a, older -> first
		{ID: "d", Priority: 5, FirstSeenAt: at(1 * time.Minute)}, // tie with b on prio+age -> id breaks
	}
	got := Order(items)
	want := []string{"b", "d", "a", "c"}
	var gotIDs []string
	for _, it := range got {
		gotIDs = append(gotIDs, it.ID)
	}
	if !reflect.DeepEqual(gotIDs, want) {
		t.Fatalf("Order = %v, want %v", gotIDs, want)
	}
	// purity: input not mutated.
	if items[0].ID != "c" {
		t.Fatalf("Order mutated its input")
	}
}

func TestGrantLease(t *testing.T) {
	mk := func(id, epic string, prio int, kind Kind) Item {
		return Item{ID: id, EpicID: epic, Priority: prio, Kind: kind, State: StateOpen, FirstSeenAt: at(0)}
	}
	tests := []struct {
		name     string
		open     []Item
		inflight map[string]bool
		max      int
		kinds    []string
		want     []string
	}{
		{
			name: "cap at max, most-urgent first",
			open: []Item{
				mk("a", "e1", 20, KindNeedsInput),
				mk("b", "e2", 5, KindScopeViolation),
				mk("c", "e3", 10, KindLaunchFailed),
			},
			max:  2,
			want: []string{"b", "c"}, // prio 5,10 before 20
		},
		{
			name:     "epic already in-flight is skipped",
			open:     []Item{mk("a", "e1", 5, KindScopeViolation), mk("b", "e2", 10, KindLaunchFailed)},
			inflight: map[string]bool{"e1": true},
			max:      5,
			want:     []string{"b"},
		},
		{
			name: "one-in-flight-per-epic within a single batch",
			open: []Item{
				mk("a", "e1", 5, KindScopeViolation),
				mk("b", "e1", 6, KindStalled), // same epic as a -> suppressed this batch
				mk("c", "e2", 7, KindLaunchFailed),
			},
			max:  5,
			want: []string{"a", "c"},
		},
		{
			name: "empty-epic items are never epic-suppressed",
			open: []Item{
				mk("a", "", 3, KindMasterAbsent),
				mk("b", "", 3, KindMasterAbsent),
			},
			max:  5,
			want: []string{"a", "b"},
		},
		{
			name:  "kinds filter",
			open:  []Item{mk("a", "e1", 5, KindScopeViolation), mk("b", "e2", 6, KindNeedsInput)},
			max:   5,
			kinds: []string{string(KindNeedsInput)},
			want:  []string{"b"},
		},
		{
			name: "max<=0 grants nothing",
			open: []Item{mk("a", "e1", 5, KindScopeViolation)},
			max:  0,
			want: nil,
		},
		{
			name: "non-open items are ineligible",
			open: []Item{{ID: "a", EpicID: "e1", Priority: 5, Kind: KindScopeViolation, State: StateLeased, FirstSeenAt: at(0)}},
			max:  5,
			want: nil,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := GrantLease(tc.open, tc.inflight, tc.max, tc.kinds)
			var ids []string
			for _, it := range got {
				ids = append(ids, it.ID)
			}
			if !reflect.DeepEqual(ids, tc.want) {
				t.Fatalf("GrantLease = %v, want %v", ids, tc.want)
			}
		})
	}
}

func TestFenceOK(t *testing.T) {
	base := Fence{
		State: StateLeased, ExpectState: StateLeased,
		LeasedBy: "master-a", Caller: "master-a",
		ClaimItemEpoch: 3, LiveItemEpoch: 3,
		ClaimSupervisorEpoch: 7, LiveSupervisorEpoch: 7,
	}
	if !FenceOK(base) {
		t.Fatalf("expected the fully-matching fence to pass")
	}
	tests := []struct {
		name  string
		mut   func(f *Fence)
		wantN string
	}{
		{"wrong state", func(f *Fence) { f.State = StateDelivering }, "state"},
		{"different leaseholder", func(f *Fence) { f.LeasedBy = "master-b" }, "leaseholder"},
		{"empty leaseholder", func(f *Fence) { f.LeasedBy = ""; f.Caller = "" }, "empty"},
		{"stale item epoch", func(f *Fence) { f.ClaimItemEpoch = 2 }, "item epoch"},
		{"stale supervisor epoch", func(f *Fence) { f.ClaimSupervisorEpoch = 6 }, "supervisor epoch"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := base
			tc.mut(&f)
			if FenceOK(f) {
				t.Fatalf("expected fence to REJECT (%s)", tc.wantN)
			}
		})
	}
}

func TestTierFor(t *testing.T) {
	tests := []struct {
		kind Kind
		want Tier
	}{
		{KindAuthDead, TierHumanImmediate},
		{KindWedgedUI, TierHumanImmediate},
		{KindMasterAbsent, TierHumanImmediate},
		{KindLaunchFailed, TierHumanImmediate},
		{KindReviewDispatchStalled, TierHumanImmediate},
		{KindReviewVerdictOverdue, TierHumanImmediate},
		{KindSendUnverified, TierFastRetry},
		{KindEpicFinished, TierNeverPage},
		{KindMergeMainSuggested, TierNeverPage},
		{KindCIInfraIncident, TierNeverPage},
		{KindUsageCritical, TierMasterFirst},
		{KindDriftSuspect, TierMasterFirst},
		{KindCIRedOnEpicPR, TierMasterFirst},
		{KindNeedsInput, TierMasterFirst},
		{KindStalled, TierMasterFirst},
		{KindScopeViolation, TierMasterFirst},
		{KindBlockedNonResumable, TierMasterFirst},
		{KindDepFailed, TierMasterFirst},
	}
	for _, tc := range tests {
		if got := TierFor(tc.kind); got != tc.want {
			t.Fatalf("TierFor(%s) = %v, want %v", tc.kind, got, tc.want)
		}
	}
}

func TestNeedsHuman(t *testing.T) {
	pol := DefaultPolicy()
	mk := func(kind Kind, state string, ageMin int, blocking bool) Item {
		return Item{Kind: kind, State: state, Blocking: blocking, FirstSeenAt: at(0)}
	}
	now := func(ageMin int) time.Time { return at(time.Duration(ageMin) * time.Minute) }
	tests := []struct {
		name   string
		item   Item
		ageMin int
		want   bool
	}{
		{"human-immediate fires the instant it is open", mk(KindAuthDead, StateOpen, 0, false), 0, true},
		{"master-first below window: not yet", mk(KindUsageCritical, StateOpen, 0, false), 9, false},
		{"master-first past window: escalate", mk(KindUsageCritical, StateOpen, 0, false), 11, true},
		{"needs_input blocking uses the short window", mk(KindNeedsInput, StateOpen, 0, true), 11, true},
		{"needs_input non-blocking still waits at 11m", mk(KindNeedsInput, StateOpen, 0, false), 11, false},
		{"needs_input non-blocking escalates past 30m", mk(KindNeedsInput, StateOpen, 0, false), 31, true},
		{"fast-retry never pages a human", mk(KindSendUnverified, StateOpen, 0, false), 999, false},
		{"never-page kind never pages", mk(KindEpicFinished, StateOpen, 0, false), 999, false},
		{"leased item is being handled", mk(KindAuthDead, StateLeased, 0, false), 999, false},
		{"resolved item never pages", mk(KindAuthDead, StateResolved, 0, false), 999, false},
		{"dep_failed escalates past 15m", mk(KindDepFailed, StateOpen, 0, false), 16, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := NeedsHuman(tc.item, now(tc.ageMin), pol); got != tc.want {
				t.Fatalf("NeedsHuman = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestShouldMasterRetry(t *testing.T) {
	pol := DefaultPolicy()
	item := Item{Kind: KindSendUnverified, State: StateOpen, FirstSeenAt: at(0)}
	if !ShouldMasterRetry(item, at(2*time.Minute), pol) {
		t.Fatalf("within the retry window: expected a retry")
	}
	if ShouldMasterRetry(item, at(6*time.Minute), pol) {
		t.Fatalf("past the retry window: expected NO retry (escalate instead)")
	}
	// a non-fast-retry kind never retries.
	if ShouldMasterRetry(Item{Kind: KindUsageCritical, State: StateOpen, FirstSeenAt: at(0)}, at(1*time.Minute), pol) {
		t.Fatalf("usage_critical is not a fast-retry kind")
	}
}

func TestMasterLiveAndAbsent(t *testing.T) {
	pol := DefaultPolicy() // MasterStaleAfter = 3m
	if !MasterLive(at(0), at(2*time.Minute), pol) {
		t.Fatalf("a 2m-old heartbeat should be live")
	}
	if MasterLive(at(0), at(4*time.Minute), pol) {
		t.Fatalf("a 4m-old heartbeat should be stale")
	}
	if MasterLive(time.Time{}, at(0), pol) {
		t.Fatalf("a zero heartbeat is never live")
	}

	items := []Item{{Kind: KindUsageCritical, State: StateOpen, FirstSeenAt: at(0)}}
	// live master: no alarm even though the item is aged.
	if ShouldRaiseMasterAbsent(items, at(11*time.Minute), at(12*time.Minute), pol) {
		t.Fatalf("a live master should suppress the master_absent alarm")
	}
	// dead master + an item past its window: alarm.
	if !ShouldRaiseMasterAbsent(items, at(0), at(12*time.Minute), pol) {
		t.Fatalf("dead master + an aged master-first item should raise master_absent")
	}
	// dead master (heartbeat 4m old, past the 3m stale bound) + a human-immediate item
	// (fresh): the alarm fires without waiting on any per-kind window.
	hi := []Item{{Kind: KindAuthDead, State: StateOpen, FirstSeenAt: at(4 * time.Minute)}}
	if !ShouldRaiseMasterAbsent(hi, at(0), at(4*time.Minute), pol) {
		t.Fatalf("dead master + a human-immediate item should raise master_absent at once")
	}
	// the alarm never feeds on its own kind.
	self := []Item{{Kind: KindMasterAbsent, State: StateOpen, FirstSeenAt: at(0)}}
	if ShouldRaiseMasterAbsent(self, at(0), at(30*time.Minute), pol) {
		t.Fatalf("master_absent items must not re-raise master_absent")
	}
}

func TestAckExpired(t *testing.T) {
	item := Item{State: StateAwaitingAck, AwaitingSince: at(0)}
	if AckExpired(item, at(5*time.Minute), 6*time.Minute) {
		t.Fatalf("within T_ack: not expired")
	}
	if !AckExpired(item, at(7*time.Minute), 6*time.Minute) {
		t.Fatalf("past T_ack: expired")
	}
	// only awaiting_ack items can ack-expire.
	if AckExpired(Item{State: StateLeased, AwaitingSince: at(0)}, at(999*time.Minute), 6*time.Minute) {
		t.Fatalf("a non-awaiting_ack item never ack-expires")
	}
	if AckExpired(Item{State: StateAwaitingAck}, at(999*time.Minute), 6*time.Minute) {
		t.Fatalf("a zero AwaitingSince never ack-expires")
	}
}

func TestValidKind(t *testing.T) {
	for k := range knownKinds {
		if !ValidKind(string(k)) {
			t.Fatalf("known kind %q rejected by ValidKind", k)
		}
	}
	for _, bad := range []string{"", "bogus", "GOAL_PAUSED", "goal_paused", "drop table"} {
		if ValidKind(bad) {
			t.Fatalf("ValidKind accepted a non-kind %q", bad)
		}
	}
}
