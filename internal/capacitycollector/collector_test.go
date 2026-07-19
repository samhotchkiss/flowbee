package capacitycollector

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/samhotchkiss/flowbee/internal/acctprobe"
	"github.com/samhotchkiss/flowbee/internal/store"
)

type fixture struct {
	Provider          string    `json:"provider"`
	AccountKey        string    `json:"account_key"`
	CredentialLineage string    `json:"credential_lineage"`
	Source            string    `json:"source"`
	CapturedAt        time.Time `json:"captured_at"`
	Windows           []struct {
		Kind, Scope, Display string
		Percent              float64
		ResetAt              time.Time `json:"reset_at"`
		WindowMinutes        int       `json:"window_minutes"`
	}
}

func fixtureResult(t *testing.T, name string) *acctprobe.Result {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	var f fixture
	if err := json.Unmarshal(b, &f); err != nil {
		t.Fatal(err)
	}
	r := &acctprobe.Result{Identity: acctprobe.Identity{Provider: acctprobe.Provider(f.Provider), AccountKey: f.AccountKey, LineageDigest: f.CredentialLineage, Verified: true}, TrustState: acctprobe.TrustVerified, CapturedAt: f.CapturedAt, Source: f.Source}
	for _, w := range f.Windows {
		r.Usage.Windows = append(r.Usage.Windows, acctprobe.LimitWindow{Kind: acctprobe.WindowKind(w.Kind), Scope: w.Scope, Display: w.Display, Percent: w.Percent, ResetsAt: w.ResetAt, WindowMinutes: w.WindowMinutes, Active: true})
	}
	return r
}

type fakeProbe struct {
	mu          sync.Mutex
	results     map[string]*acctprobe.Result
	errs        map[string]error
	active, max int32
	delay       time.Duration
}

func (p *fakeProbe) ProbeLive(ctx context.Context, seat Seat) (ProbeResult, error) {
	n := atomic.AddInt32(&p.active, 1)
	for {
		old := atomic.LoadInt32(&p.max)
		if n <= old || atomic.CompareAndSwapInt32(&p.max, old, n) {
			break
		}
	}
	defer atomic.AddInt32(&p.active, -1)
	if p.delay > 0 {
		select {
		case <-time.After(p.delay):
		case <-ctx.Done():
			return ProbeResult{}, ctx.Err()
		}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.errs[seat.ID]; err != nil {
		return ProbeResult{}, err
	}
	return ProbeResult{Result: p.results[seat.ID], RawSHA256: "sha256:fixture", AdapterVersion: "fixture/v1"}, nil
}

type memBackoff struct {
	mu     sync.Mutex
	states map[string]BackoffState
}

func (m *memBackoff) Get(_ context.Context, kind, key string) (BackoffState, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.states[kind+"\x00"+key], nil
}
func (m *memBackoff) Failure(_ context.Context, kind, key, reason string, now, retryAt time.Time, base, max time.Duration) (BackoffState, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.states == nil {
		m.states = map[string]BackoffState{}
	}
	k := kind + "\x00" + key
	v := m.states[k]
	v.Failures++
	v.Reason = reason
	v.RetryAt = now.Add(base)
	if retryAt.After(v.RetryAt) {
		v.RetryAt = retryAt
	}
	m.states[k] = v
	return v, nil
}
func (m *memBackoff) Success(_ context.Context, kind, key string, _ time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.states != nil {
		delete(m.states, kind+"\x00"+key)
	}
	return nil
}

func TestCollectProducesCompleteStrictAtomicGenerationFromCapturedFixtures(t *testing.T) {
	now := time.Date(2026, 7, 19, 16, 0, 1, 0, time.UTC)
	probe := &fakeProbe{results: map[string]*acctprobe.Result{"codex-seat": fixtureResult(t, "codex_live_weekly.json"), "grok-seat": fixtureResult(t, "grok_live_monthly_absent_percent.json")}, errs: map[string]error{}}
	svc := &Service{Probe: probe, Backoff: &memBackoff{}, Config: Config{ProbeTimeout: time.Second}}
	seats := []Seat{{ID: "codex-seat", HostID: "host-a", Provider: "codex", ConfigHome: "/home/codex", ExpectedAccountKey: "codex-account-1", ExpectedCredentialLineage: "codex-lineage-1"}, {ID: "grok-seat", HostID: "host-a", Provider: "grok", ConfigHome: "/home/grok", ExpectedAccountKey: "grok-account-1", ExpectedCredentialLineage: "grok-lineage-1"}}
	gen, err := svc.Collect(context.Background(), "generation-1", Identity{ID: "collector-a", HostID: "host-a", Authenticated: true}, seats, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(gen.ExpectedSeatIDs) != 2 || len(gen.Observations) != 2 {
		t.Fatalf("incomplete generation: %+v", gen)
	}
	bySeat := map[string]store.CapacitySeatObservation{}
	for _, o := range gen.Observations {
		bySeat[o.SeatID] = o
	}
	codex, grok := bySeat["codex-seat"], bySeat["grok-seat"]
	if codex.Source != "live_app_server" || codex.TrustState != "verified" || codex.IntegrityState != "verified" || codex.CredentialLineage != "codex-lineage-1" {
		t.Fatalf("codex observation: %+v", codex)
	}
	if len(codex.Windows) != 2 || codex.Windows[1].Scope != "codex_bengalfox" || codex.Windows[1].Display != "Spark" {
		t.Fatalf("stable scoped limit lost: %+v", codex.Windows)
	}
	if grok.Source != "live_billing" || !grok.BillingPeriodActive || len(grok.Windows) != 1 || grok.Windows[0].Kind != "monthly" || grok.Windows[0].Percent != 0 {
		t.Fatalf("grok monthly absent-percent contract: %+v", grok)
	}
}

func TestCollectIncludesHeldObservationAndDurablyBacksOff(t *testing.T) {
	now := time.Date(2026, 7, 19, 16, 0, 0, 0, time.UTC)
	calls := &fakeProbe{results: map[string]*acctprobe.Result{}, errs: map[string]error{"s": &acctprobe.HoldError{Reason: acctprobe.ReasonThrottled, RetryAt: now.Add(2 * time.Minute)}}}
	back := &memBackoff{}
	svc := &Service{Probe: calls, Backoff: back, Config: Config{BaseBackoff: time.Second}}
	seat := Seat{ID: "s", HostID: "h", Provider: "codex", ConfigHome: "/c", ExpectedAccountKey: "a", ExpectedCredentialLineage: "l"}
	gen, err := svc.Collect(context.Background(), "g1", Identity{ID: "c", HostID: "h", Authenticated: true}, []Seat{seat}, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(gen.Observations) != 1 || gen.Observations[0].LiveUnavailableReason != "throttled" || !gen.Observations[0].RetryAt.Equal(now.Add(2*time.Minute)) {
		t.Fatalf("held observation missing: %+v", gen)
	}
	calls.errs["s"] = errors.New("must not run during backoff")
	gen, err = svc.Collect(context.Background(), "g2", Identity{ID: "c", HostID: "h", Authenticated: true}, []Seat{seat}, now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if got := gen.Observations[0].LiveUnavailableReason; got != "backoff:throttled" {
		t.Fatalf("got %q", got)
	}
}

func TestCollectSerializesSameProviderAccountAndBoundsProviderConcurrency(t *testing.T) {
	now := time.Date(2026, 7, 19, 16, 0, 1, 0, time.UTC)
	base := fixtureResult(t, "codex_live_weekly.json")
	probe := &fakeProbe{results: map[string]*acctprobe.Result{}, errs: map[string]error{}, delay: 20 * time.Millisecond}
	var seats []Seat
	for i, id := range []string{"s1", "s2", "s3"} {
		r := *base
		r.Identity.LineageDigest = "lineage-" + id
		probe.results[id] = &r
		seats = append(seats, Seat{ID: id, HostID: "h", Provider: "codex", ConfigHome: "/" + id, ExpectedAccountKey: "codex-account-1", ExpectedCredentialLineage: "lineage-" + id})
		_ = i
	}
	svc := &Service{Probe: probe, Backoff: &memBackoff{}, Config: Config{ProviderConcurrency: 3, HostConcurrency: 3, ProbeTimeout: time.Second}}
	if _, err := svc.Collect(context.Background(), "g", Identity{ID: "c", HostID: "h", Authenticated: true}, seats, now); err != nil {
		t.Fatal(err)
	}
	if got := atomic.LoadInt32(&probe.max); got != 1 {
		t.Fatalf("same provider account live probes overlapped: max=%d", got)
	}
}

func TestCollectBoundsProviderConcurrencyAcrossDistinctAccounts(t *testing.T) {
	now := time.Date(2026, 7, 19, 16, 0, 1, 0, time.UTC)
	base := fixtureResult(t, "codex_live_weekly.json")
	probe := &fakeProbe{results: map[string]*acctprobe.Result{}, errs: map[string]error{}, delay: 30 * time.Millisecond}
	var seats []Seat
	for _, id := range []string{"s1", "s2", "s3", "s4"} {
		r := *base
		r.Identity.AccountKey = "account-" + id
		r.Identity.LineageDigest = "lineage-" + id
		probe.results[id] = &r
		seats = append(seats, Seat{ID: id, HostID: "h", Provider: "codex", ConfigHome: "/" + id, ExpectedAccountKey: "account-" + id, ExpectedCredentialLineage: "lineage-" + id})
	}
	svc := &Service{Probe: probe, Backoff: &memBackoff{}, Config: Config{ProviderConcurrency: 2, HostConcurrency: 4, ProbeTimeout: time.Second}}
	if _, err := svc.Collect(context.Background(), "g", Identity{ID: "c", HostID: "h", Authenticated: true}, seats, now); err != nil {
		t.Fatal(err)
	}
	if got := atomic.LoadInt32(&probe.max); got != 2 {
		t.Fatalf("provider concurrency max=%d want exactly configured bound 2", got)
	}
}

func TestNormalizeHoldsMissingRequiredWindowFutureClockAndIdentityDrift(t *testing.T) {
	now := time.Date(2026, 7, 19, 16, 0, 1, 0, time.UTC)
	base := fixtureResult(t, "codex_live_weekly.json")
	seat := Seat{ID: "s", HostID: "h", Provider: "codex", ConfigHome: "/c", ExpectedAccountKey: "codex-account-1", ExpectedCredentialLineage: "codex-lineage-1"}
	missing := *base
	missing.Usage.Windows = acctprobe.Windows{{Kind: acctprobe.KindSession, Percent: 2, ResetsAt: now.Add(time.Hour), Active: true}}
	if got := normalize(ProbeResult{Result: &missing}, "g1", Identity{ID: "c", HostID: "h", Authenticated: true}, seat, now, 30*time.Second); got.IntegrityState != "required_window_missing_or_invalid" {
		t.Fatalf("missing weekly window accepted: %+v", got)
	}
	future := *base
	future.CapturedAt = now.Add(time.Minute)
	if got := normalize(ProbeResult{Result: &future}, "g2", Identity{ID: "c", HostID: "h", Authenticated: true}, seat, now, 30*time.Second); got.IntegrityState != "capture_clock_invalid" {
		t.Fatalf("future capture accepted: %+v", got)
	}
	drift := *base
	drift.Identity.AccountKey = "other"
	if got := normalize(ProbeResult{Result: &drift}, "g3", Identity{ID: "c", HostID: "h", Authenticated: true}, seat, now, 30*time.Second); got.IntegrityState != "identity_mismatch" {
		t.Fatalf("identity drift accepted: %+v", got)
	}
}

func TestCollectRejectsUnauthenticatedWrongHostAndDuplicateCanonicalHome(t *testing.T) {
	svc := &Service{Probe: &fakeProbe{}, Backoff: &memBackoff{}}
	now := time.Now()
	seat := Seat{ID: "s", HostID: "h", Provider: "codex", ConfigHome: "/c", ExpectedAccountKey: "a", ExpectedCredentialLineage: "l"}
	if _, err := svc.Collect(context.Background(), "g", Identity{ID: "c", HostID: "h"}, []Seat{seat}, now); err == nil {
		t.Fatal("unauthenticated collector accepted")
	}
	if _, err := svc.Collect(context.Background(), "g", Identity{ID: "c", HostID: "other", Authenticated: true}, []Seat{seat}, now); err == nil {
		t.Fatal("wrong-host collector accepted")
	}
	dup := seat
	dup.ID = "s2"
	if _, err := svc.Collect(context.Background(), "g", Identity{ID: "c", HostID: "h", Authenticated: true}, []Seat{seat, dup}, now); err == nil {
		t.Fatal("duplicate canonical home accepted")
	}
}

type fakeCommitter struct {
	mu    sync.Mutex
	calls int
	last  store.CapacityGeneration
	err   error
}

func (f *fakeCommitter) CommitCapacityGeneration(_ context.Context, generation store.CapacityGeneration, _ time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.last = generation
	return f.err
}

type errorHostClient struct{ err error }

func (e errorHostClient) Collect(context.Context, string, Identity, []Seat, time.Time) (store.CapacityGeneration, error) {
	return store.CapacityGeneration{}, e.err
}

func TestFleetServiceCommitsAllHostsOnceAndNeverPublishesPartial(t *testing.T) {
	now := time.Date(2026, 7, 19, 16, 0, 1, 0, time.UTC)
	codexSeat := Seat{ID: "codex-seat", HostID: "host-a", Provider: "codex", ConfigHome: "/codex", ExpectedAccountKey: "codex-account-1", ExpectedCredentialLineage: "codex-lineage-1"}
	grokSeat := Seat{ID: "grok-seat", HostID: "host-b", Provider: "grok", ConfigHome: "/grok", ExpectedAccountKey: "grok-account-1", ExpectedCredentialLineage: "grok-lineage-1"}
	serviceA := &Service{Probe: &fakeProbe{results: map[string]*acctprobe.Result{"codex-seat": fixtureResult(t, "codex_live_weekly.json")}, errs: map[string]error{}}, Backoff: &memBackoff{}}
	serviceB := &Service{Probe: &fakeProbe{results: map[string]*acctprobe.Result{"grok-seat": fixtureResult(t, "grok_live_monthly_absent_percent.json")}, errs: map[string]error{}}, Backoff: &memBackoff{}}
	sink := &fakeCommitter{}
	fleet := FleetService{Hosts: map[string]HostClient{"host-a": serviceA, "host-b": serviceB}, Collectors: map[string]Identity{"host-a": {ID: "collector-a", HostID: "host-a", Authenticated: true}, "host-b": {ID: "collector-b", HostID: "host-b", Authenticated: true}}, Committer: sink}
	got, err := fleet.CollectAndCommit(context.Background(), "fleet-generation", []Seat{grokSeat, codexSeat}, now)
	if err != nil {
		t.Fatal(err)
	}
	if sink.calls != 1 || len(got.Observations) != 2 || len(sink.last.ExpectedSeatIDs) != 2 {
		t.Fatalf("atomic fleet commit calls=%d got=%+v last=%+v", sink.calls, got, sink.last)
	}

	sink.calls = 0
	fleet.Hosts["host-b"] = errorHostClient{err: errors.New("host unavailable")}
	if _, err := fleet.CollectAndCommit(context.Background(), "failed-generation", []Seat{codexSeat, grokSeat}, now); err == nil {
		t.Fatal("partial fleet generation unexpectedly committed")
	}
	if sink.calls != 0 {
		t.Fatalf("partial host results reached committer %d times", sink.calls)
	}
}
