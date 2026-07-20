// Package capacitycollector gathers identity-bound live provider observations into
// complete atomic capacity generations. It is deliberately routing-blind: the
// collector can report facts and durable backoff, but only the store/router decides
// whether work may launch.
package capacitycollector

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/samhotchkiss/flowbee/internal/acctprobe"
	"github.com/samhotchkiss/flowbee/internal/capacity"
	"github.com/samhotchkiss/flowbee/internal/store"
)

const AdapterVersion = "flowbee-capacity-collector/v2.1"

type Config struct {
	ProbeTimeout        time.Duration
	HostConcurrency     int
	ProviderConcurrency int
	BaseBackoff         time.Duration
	MaxBackoff          time.Duration
	MaxFutureSkew       time.Duration
}

func (c Config) normalized() Config {
	if c.ProbeTimeout <= 0 {
		c.ProbeTimeout = 20 * time.Second
	}
	if c.HostConcurrency < 1 {
		c.HostConcurrency = 4
	}
	if c.ProviderConcurrency < 1 {
		c.ProviderConcurrency = 2
	}
	if c.BaseBackoff <= 0 {
		c.BaseBackoff = 15 * time.Second
	}
	if c.MaxBackoff <= 0 {
		c.MaxBackoff = 15 * time.Minute
	}
	if c.MaxFutureSkew <= 0 {
		c.MaxFutureSkew = 30 * time.Second
	}
	return c
}

// Seat is the operator-owned expectation supplied to the host collector. None of
// these identity values may be selected or rewritten by the collector.
type Seat struct {
	ID, HostID, Provider, ConfigHome              string
	ExpectedAccountKey, ExpectedCredentialLineage string
	ExpectedOrgFingerprint                        string
	// Local is set by the operator-owned seat registry when the config home is
	// physically resident in this process's host namespace. It is not inferred
	// from HostID, hostname, CWD, or a report body.
	Local bool
}

type Identity struct {
	ID, HostID    string
	Authenticated bool
}

type ProbeResult struct {
	Result         *acctprobe.Result
	RawSHA256      string
	AdapterVersion string
}

type LiveProbe interface {
	ProbeLive(context.Context, Seat) (ProbeResult, error)
}

// AcctProbe is the production local-host adapter. It calls only live entry points;
// cache fallback is intentionally not part of this path.
type AcctProbe struct{ Prober *acctprobe.Prober }

func (p AcctProbe) ProbeLive(ctx context.Context, seat Seat) (ProbeResult, error) {
	probe := p.Prober
	if probe == nil {
		probe = acctprobe.New()
	}
	var result *acctprobe.Result
	var err error
	switch seat.Provider {
	case "codex":
		result, err = probe.ProbeCodexLive(ctx, seat.ConfigHome)
	case "grok":
		result, err = probe.ProbeGrokLive(ctx, seat.ConfigHome)
	case "claude":
		result, err = probe.ProbeClaudeLive(ctx, seat.ConfigHome, seat.ExpectedOrgFingerprint)
	default:
		return ProbeResult{}, fmt.Errorf("unsupported provider %q", seat.Provider)
	}
	if err != nil {
		return ProbeResult{}, err
	}
	return ProbeResult{Result: result, AdapterVersion: AdapterVersion}, nil
}

type BackoffState struct {
	RetryAt  time.Time
	Failures int
	Reason   string
}

type BackoffStore interface {
	Get(context.Context, string, string) (BackoffState, error)
	Failure(context.Context, string, string, string, time.Time, time.Time, time.Duration, time.Duration) (BackoffState, error)
	Success(context.Context, string, string, time.Time) error
}

type Service struct {
	Probe   LiveProbe
	Backoff BackoffStore
	Config  Config

	mu   sync.Mutex
	keys map[string]chan struct{}
}

type GenerationCommitter interface {
	CommitCapacityGeneration(context.Context, store.CapacityGeneration, time.Time) error
}

// HostClient is the narrow authenticated collector RPC boundary. A production
// transport authenticates Identity from the peer/host registration rather than
// accepting it from the JSON body. Local Service implements the same contract for
// deterministic tests and single-host deployments.
type HostClient interface {
	Collect(context.Context, string, Identity, []Seat, time.Time) (store.CapacityGeneration, error)
}

// FleetService gathers all host reports before one atomic commit. Per-host reports
// must never advance the active-generation pointer independently: doing so would
// make every other host disappear from routing until its own report arrived.
type FleetService struct {
	Hosts       map[string]HostClient
	Collectors  map[string]Identity
	Committer   GenerationCommitter
	Concurrency int
}

func (f FleetService) CollectAndCommit(ctx context.Context, generationID string, seats []Seat, now time.Time) (store.CapacityGeneration, error) {
	if generationID == "" || now.IsZero() || f.Committer == nil {
		return store.CapacityGeneration{}, errors.New("fleet capacity generation id, time, and committer are required")
	}
	groups := map[string][]Seat{}
	seen := map[string]bool{}
	seatHosts := map[string]string{}
	for _, seat := range seats {
		if seat.ID == "" || seen[seat.ID] {
			return store.CapacityGeneration{}, fmt.Errorf("empty or duplicate fleet seat %q", seat.ID)
		}
		seen[seat.ID] = true
		seatHosts[seat.ID] = seat.HostID
		groups[seat.HostID] = append(groups[seat.HostID], seat)
	}
	if len(groups) == 0 {
		return store.CapacityGeneration{}, errors.New("fleet capacity generation requires seats")
	}
	limit := f.Concurrency
	if limit < 1 {
		limit = 4
	}
	sem := make(chan struct{}, limit)
	type hostResult struct {
		host       string
		generation store.CapacityGeneration
		err        error
	}
	resultCh := make(chan hostResult, len(groups))
	for host, hostSeats := range groups {
		go func(host string, hostSeats []Seat) {
			if err := acquire(ctx, sem); err != nil {
				resultCh <- hostResult{host: host, err: err}
				return
			}
			defer func() { <-sem }()
			client, identity := f.Hosts[host], f.Collectors[host]
			if client == nil || identity.ID == "" || identity.HostID != host || !identity.Authenticated {
				resultCh <- hostResult{host: host, err: errors.New("registered authenticated host collector is unavailable")}
				return
			}
			generation, err := client.Collect(ctx, generationID, identity, hostSeats, now)
			resultCh <- hostResult{host: host, generation: generation, err: err}
		}(host, hostSeats)
	}
	merged := store.CapacityGeneration{ID: generationID, StartedAt: now.UTC()}
	for range groups {
		result := <-resultCh
		if result.err != nil {
			return store.CapacityGeneration{}, fmt.Errorf("host collector %s: %w", result.host, result.err)
		}
		if result.generation.ID != generationID || !result.generation.StartedAt.Equal(now.UTC()) {
			return store.CapacityGeneration{}, fmt.Errorf("host collector %s returned a mismatched generation fence", result.host)
		}
		merged.ExpectedSeatIDs = append(merged.ExpectedSeatIDs, result.generation.ExpectedSeatIDs...)
		merged.Observations = append(merged.Observations, result.generation.Observations...)
	}
	sort.Strings(merged.ExpectedSeatIDs)
	if len(merged.ExpectedSeatIDs) != len(seats) || len(merged.Observations) != len(seats) {
		return store.CapacityGeneration{}, errors.New("fleet collector returned an incomplete generation")
	}
	for i, seatID := range merged.ExpectedSeatIDs {
		if !seen[seatID] || i > 0 && seatID == merged.ExpectedSeatIDs[i-1] {
			return store.CapacityGeneration{}, errors.New("fleet collector returned an unexpected or duplicate seat")
		}
	}
	observed := map[string]bool{}
	for _, observation := range merged.Observations {
		if observation.SeatID == "" || observed[observation.SeatID] || !seen[observation.SeatID] || observation.HostID != seatHosts[observation.SeatID] {
			return store.CapacityGeneration{}, errors.New("fleet collector returned an unexpected, duplicate, or wrong-host observation")
		}
		observed[observation.SeatID] = true
	}
	if err := f.Committer.CommitCapacityGeneration(ctx, merged, now); err != nil {
		return store.CapacityGeneration{}, err
	}
	return merged, nil
}

func (s *Service) Collect(ctx context.Context, generationID string, collector Identity, seats []Seat, now time.Time) (store.CapacityGeneration, error) {
	if generationID == "" || now.IsZero() {
		return store.CapacityGeneration{}, errors.New("capacity generation id and start time are required")
	}
	if collector.ID == "" || collector.HostID == "" || !collector.Authenticated {
		return store.CapacityGeneration{}, errors.New("authenticated collector identity and host are required")
	}
	if s.Probe == nil || s.Backoff == nil {
		return store.CapacityGeneration{}, errors.New("capacity collector probe and durable backoff store are required")
	}
	if len(seats) == 0 {
		return store.CapacityGeneration{}, errors.New("capacity collector requires at least one seat")
	}
	ordered := append([]Seat(nil), seats...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].ID < ordered[j].ID })
	seenID, seenHome := map[string]bool{}, map[string]string{}
	for _, seat := range ordered {
		if seat.ID == "" || seat.HostID == "" || seat.Provider == "" || seat.ConfigHome == "" || seat.ExpectedAccountKey == "" || seat.ExpectedCredentialLineage == "" {
			return store.CapacityGeneration{}, fmt.Errorf("seat %q has incomplete identity expectation", seat.ID)
		}
		if seat.HostID != collector.HostID {
			return store.CapacityGeneration{}, fmt.Errorf("seat %s host %s does not match authenticated collector host %s", seat.ID, seat.HostID, collector.HostID)
		}
		if seenID[seat.ID] {
			return store.CapacityGeneration{}, fmt.Errorf("duplicate seat %s", seat.ID)
		}
		seenID[seat.ID] = true
		homeKey := seat.HostID + "\x00" + seat.Provider + "\x00" + seat.ConfigHome
		if prior := seenHome[homeKey]; prior != "" {
			return store.CapacityGeneration{}, fmt.Errorf("canonical config home %s is registered by both %s and %s", seat.ConfigHome, prior, seat.ID)
		}
		seenHome[homeKey] = seat.ID
		switch seat.Provider {
		case "claude", "codex", "grok":
		default:
			return store.CapacityGeneration{}, fmt.Errorf("seat %s has unsupported provider %s", seat.ID, seat.Provider)
		}
	}

	cfg := s.Config.normalized()
	hostSem := make(chan struct{}, cfg.HostConcurrency)
	providerSem := map[string]chan struct{}{}
	for _, seat := range ordered {
		if providerSem[seat.Provider] == nil {
			providerSem[seat.Provider] = make(chan struct{}, cfg.ProviderConcurrency)
		}
	}
	observations := make([]store.CapacitySeatObservation, len(ordered))
	errCh := make(chan error, len(ordered))
	var wg sync.WaitGroup
	for i, seat := range ordered {
		wg.Add(1)
		go func(i int, seat Seat) {
			defer wg.Done()
			obs, err := s.collectSeat(ctx, cfg, generationID, collector, seat, now, hostSem, providerSem[seat.Provider])
			if err != nil {
				errCh <- fmt.Errorf("seat %s: %w", seat.ID, err)
				return
			}
			observations[i] = obs
		}(i, seat)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		if err != nil {
			return store.CapacityGeneration{}, err
		}
	}
	expected := make([]string, len(ordered))
	for i := range ordered {
		expected[i] = ordered[i].ID
	}
	return store.CapacityGeneration{ID: generationID, ExpectedSeatIDs: expected, StartedAt: now.UTC(), Observations: observations}, nil
}

func (s *Service) collectSeat(ctx context.Context, cfg Config, generationID string, collector Identity, seat Seat, now time.Time, hostSem, providerSem chan struct{}) (store.CapacitySeatObservation, error) {
	base := store.CapacitySeatObservation{ObservationID: observationID(generationID, collector.ID, seat.ID), SeatID: seat.ID, HostID: seat.HostID, Provider: seat.Provider, CollectorID: collector.ID, Source: "unavailable", TrustState: "held", IntegrityState: "held", AdapterVersion: AdapterVersion}
	key := seat.Provider + "\x00" + seat.ExpectedAccountKey
	release, err := s.acquireKey(ctx, key)
	if err != nil {
		return base, err
	}
	defer release()
	for _, scope := range [][2]string{{"provider", seat.Provider}, {"account", key}} {
		state, err := s.Backoff.Get(ctx, scope[0], scope[1])
		if err != nil {
			return base, err
		}
		if state.RetryAt.After(now) {
			base.LiveUnavailableReason = "backoff:" + state.Reason
			base.RetryAt = state.RetryAt
			return base, nil
		}
	}
	if err := acquire(ctx, hostSem); err != nil {
		return base, err
	}
	defer func() { <-hostSem }()
	if err := acquire(ctx, providerSem); err != nil {
		return base, err
	}
	defer func() { <-providerSem }()
	probeCtx, cancel := context.WithTimeout(ctx, cfg.ProbeTimeout)
	defer cancel()
	report, err := s.Probe.ProbeLive(probeCtx, seat)
	if err != nil {
		reason, retryAt := classifyProbeError(err)
		base.LiveUnavailableReason, base.RetryAt = reason, retryAt
		if _, berr := s.Backoff.Failure(ctx, "account", key, reason, now, retryAt, cfg.BaseBackoff, cfg.MaxBackoff); berr != nil {
			return base, berr
		}
		if providerFailure(reason) {
			if _, berr := s.Backoff.Failure(ctx, "provider", seat.Provider, reason, now, retryAt, cfg.BaseBackoff, cfg.MaxBackoff); berr != nil {
				return base, berr
			}
		}
		return base, nil
	}
	if report.Result == nil {
		return base, errors.New("live probe returned nil result without error")
	}
	if err := s.Backoff.Success(ctx, "account", key, now); err != nil {
		return base, err
	}
	if err := s.Backoff.Success(ctx, "provider", seat.Provider, now); err != nil {
		return base, err
	}
	return normalize(report, generationID, collector, seat, now, cfg.MaxFutureSkew), nil
}

func normalize(report ProbeResult, generationID string, collector Identity, seat Seat, now time.Time, futureSkew time.Duration) store.CapacitySeatObservation {
	r := report.Result
	lineage := r.Identity.LineageDigest
	if lineage == "" {
		lineage = r.Identity.CredentialDigest
	}
	obs := store.CapacitySeatObservation{ObservationID: observationID(generationID, collector.ID, seat.ID), SeatID: seat.ID, HostID: seat.HostID, Provider: string(r.Identity.Provider), AccountKey: r.Identity.AccountKey, CredentialLineage: lineage, CollectorID: collector.ID, Source: normalizedSource(r.Source), TrustState: string(r.TrustState), IntegrityState: "verified", LiveUnavailableReason: string(r.LiveUnavailableReason), RawSHA256: report.RawSHA256, AdapterVersion: report.AdapterVersion, RateLimited: r.Usage.RateLimited, FetchedAt: r.CapturedAt.UTC(), RetryAt: r.RetryAt.UTC()}
	if obs.AdapterVersion == "" {
		obs.AdapterVersion = AdapterVersion
	}
	if obs.RawSHA256 == "" {
		obs.RawSHA256 = sanitizedHash(r)
	}
	for _, win := range r.Usage.Windows {
		obs.Windows = append(obs.Windows, capacity.RouteWindow{Kind: string(win.Kind), Scope: win.Scope, Display: win.Display, Applicable: true, Known: true, Percent: win.Percent, ResetAt: win.ResetsAt.UTC()})
	}
	if obs.Provider != seat.Provider || obs.AccountKey != seat.ExpectedAccountKey {
		obs.IntegrityState = "identity_mismatch"
	}
	if obs.CredentialLineage != seat.ExpectedCredentialLineage {
		obs.IntegrityState = "credential_lineage_mismatch"
	}
	if r.TrustState != acctprobe.TrustVerified || !r.Identity.Verified {
		obs.IntegrityState = "unverified"
	}
	if obs.Source != "live_app_server" && obs.Source != "live_billing" {
		obs.IntegrityState = "live_source_required"
	}
	if obs.FetchedAt.IsZero() || obs.FetchedAt.After(now.Add(futureSkew)) {
		obs.IntegrityState = "capture_clock_invalid"
	}
	contractOK, billingActive := requiredWindows(seat.Provider, obs.Windows, obs.FetchedAt, now)
	obs.BillingPeriodActive = billingActive
	if !contractOK {
		obs.IntegrityState = "required_window_missing_or_invalid"
	}
	return obs
}

func requiredWindows(provider string, windows []capacity.RouteWindow, fetchedAt, now time.Time) (bool, bool) {
	required := false
	billingActive := false
	for _, win := range windows {
		if !win.Applicable || !win.Known || win.Percent < 0 || win.Percent > 100 {
			continue
		}
		resetValid := !win.ResetAt.IsZero() && win.ResetAt.After(fetchedAt) && win.ResetAt.After(now)
		switch provider {
		case "codex", "claude":
			if win.Kind == "weekly" || win.Kind == "weekly_all" {
				required = required || resetValid
			}
		case "grok":
			if win.Kind == "weekly" || win.Kind == "weekly_all" || win.Kind == "monthly" {
				required = required || resetValid
				billingActive = billingActive || resetValid
			}
		}
	}
	return required, billingActive
}

func normalizedSource(source string) string {
	switch source {
	case "codex_app_server":
		return "live_app_server"
	case "grok_billing_api", "anthropic_usage_api":
		return "live_billing"
	case "":
		return "unavailable"
	default:
		if strings.Contains(source, "rollout") {
			return "display_only"
		}
		return "cache"
	}
}

func classifyProbeError(err error) (string, time.Time) {
	var hold *acctprobe.HoldError
	if errors.As(err, &hold) {
		return string(hold.Reason), hold.RetryAt.UTC()
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "probe_timeout", time.Time{}
	}
	return "live_unavailable", time.Time{}
}

func providerFailure(reason string) bool {
	switch reason {
	case "app_server_unavailable", "app_server_protocol", "probe_timeout", "live_unavailable":
		return true
	default:
		return false
	}
}

func observationID(generationID, collectorID, seatID string) string {
	h := sha256.Sum256([]byte(generationID + "\x00" + collectorID + "\x00" + seatID))
	return "capobs-" + hex.EncodeToString(h[:16])
}

func sanitizedHash(result *acctprobe.Result) string {
	b, _ := json.Marshal(result)
	h := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(h[:])
}

func acquire(ctx context.Context, sem chan struct{}) error {
	select {
	case sem <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *Service) acquireKey(ctx context.Context, key string) (func(), error) {
	s.mu.Lock()
	if s.keys == nil {
		s.keys = map[string]chan struct{}{}
	}
	ch := s.keys[key]
	if ch == nil {
		ch = make(chan struct{}, 1)
		s.keys[key] = ch
	}
	s.mu.Unlock()
	select {
	case ch <- struct{}{}:
		return func() { <-ch }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
