package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/samhotchkiss/flowbee/internal/capacity"
)

type CapacitySeatObservation struct {
	ObservationID, SeatID, HostID, Provider, AccountKey string
	CredentialLineage, CollectorID, Source, TrustState  string
	IntegrityState, LiveUnavailableReason, RawSHA256    string
	AdapterVersion                                      string
	BillingPeriodActive, RateLimited                    bool
	Windows                                             []capacity.RouteWindow
	FetchedAt, RetryAt                                  time.Time
}

type CapacityGeneration struct {
	ID              string
	ExpectedSeatIDs []string
	StartedAt       time.Time
	Observations    []CapacitySeatObservation
}

type CapacitySeatIdentity struct {
	SeatID, HostID, AccountKey, CredentialLineage string
	ReservePct                                    float64
	AccountMaximum                                int
}

// distinctReviewerCapacityTx proves that a newly admitted epic can eventually
// satisfy its independent-review obligation. It deliberately reads the same
// active-generation predicate as review claim and excludes the builder family
// before evaluating capacity: same-family headroom is never a fallback.
//
// This is an admission check, not a reservation. Review capacity can change
// while the build runs, so dispatch still re-evaluates and leases atomically.
// The admission invariant is narrower: never create work when the presently
// configured, fresh fleet has no viable independent-review path at all.
func distinctReviewerCapacityTx(ctx context.Context, tx *sql.Tx, projectID, builderFamily string,
	now time.Time, freshFor time.Duration) (capacity.RouteDecision, error) {
	decision, _, err := distinctReviewerFamilyTx(ctx, tx, projectID, builderFamily, now, freshFor)
	return decision, err
}

// distinctReviewerFamilyTx returns the family whose fresh, project-bound seat
// made the admission proof. The choice is persisted in the epic worker plan so
// later launch/recovery never has to re-infer anti-affinity from mutable fleet
// inventory.
func distinctReviewerFamilyTx(ctx context.Context, tx *sql.Tx, projectID, builderFamily string,
	now time.Time, freshFor time.Duration) (capacity.RouteDecision, string, error) {
	// Capacity is useful only when the project has an active reviewer route to
	// the exact seat. A healthy seat elsewhere in the fleet is inventory, not an
	// admission proof for this project. Keep the binding/seat authority checks in
	// this transaction so a concurrent rebind cannot false-green admission.
	rows, err := tx.QueryContext(ctx, `SELECT DISTINCT s.id,s.agent_family
		FROM driver_session_bindings b
		JOIN seats s ON s.id=b.seat_id
		WHERE b.project_id=? AND b.role=? AND b.state='active' AND b.seat_id<>''
		  AND b.lifecycle_ownership='driver_managed'
		  AND b.tmux_server_domain_id<>'' AND b.tmux_server_instance_id<>''
		  AND s.enabled=1 AND s.agent_family<>?
		  AND s.expected_host_id<>'' AND s.expected_host_id=b.host_id
		  AND s.expected_account_key<>'' AND s.expected_credential_lineage<>''
		  AND b.provider=s.agent_family
		ORDER BY s.agent_family,s.id`, projectID, DriverReviewerRole, builderFamily)
	if err != nil {
		return capacity.RouteDecision{}, "", err
	}
	type candidate struct{ seatID, family string }
	var candidates []candidate
	for rows.Next() {
		var item candidate
		if err := rows.Scan(&item.seatID, &item.family); err != nil {
			rows.Close()
			return capacity.RouteDecision{}, "", err
		}
		candidates = append(candidates, item)
	}
	if err := rows.Close(); err != nil {
		return capacity.RouteDecision{}, "", err
	}
	if len(candidates) == 0 {
		return capacity.RouteDecision{Reasons: []string{
			"no active project-bound review seat from a distinct family",
		}}, "", nil
	}
	var reasons []string
	for _, candidate := range candidates {
		decision, err := capacityRouteForSeatQuery(ctx, tx, candidate.seatID, now, freshFor)
		if err != nil {
			return capacity.RouteDecision{}, "", err
		}
		if decision.Routable {
			return capacity.RouteDecision{Routable: true}, candidate.family, nil
		}
		reasons = append(reasons, candidate.family+"/"+candidate.seatID+"="+
			strings.Join(decision.Reasons, ","))
	}
	return capacity.RouteDecision{Reasons: reasons}, "", nil
}

// BindCapacitySeatIdentity establishes the operator-approved provider identity
// and credential lineage. A collector can observe these values but cannot choose
// or silently rewrite the expectation.
func (s *Store) BindCapacitySeatIdentity(ctx context.Context, binding CapacitySeatIdentity, now time.Time) error {
	if binding.SeatID == "" || binding.HostID == "" || binding.AccountKey == "" || binding.CredentialLineage == "" {
		return errors.New("capacity seat binding requires seat, host, account, and credential lineage")
	}
	if binding.ReservePct < 0 || binding.ReservePct > 100 || binding.AccountMaximum < 0 {
		return errors.New("capacity seat binding has invalid reserve or account concurrency")
	}
	res, err := s.DB.ExecContext(ctx, `UPDATE seats SET expected_host_id=?,expected_account_key=?,
		expected_credential_lineage=?,capacity_reserve_pct=?,account_max_concurrent=?,updated_at=?
		WHERE id=?`, binding.HostID, binding.AccountKey, binding.CredentialLineage,
		binding.ReservePct, binding.AccountMaximum, now.UTC().Format(rfc3339), binding.SeatID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return ErrSeatNotFound
	}
	return nil
}

// CommitCapacityGeneration persists every seat result, folds account-global truth
// once, and advances the active pointer atomically. A partial generation can never
// make a seat routable.
func (s *Store) CommitCapacityGeneration(ctx context.Context, generation CapacityGeneration, now time.Time) error {
	if generation.ID == "" || generation.StartedAt.IsZero() || len(generation.ExpectedSeatIDs) == 0 {
		return errors.New("capacity generation identity and expected seats are required")
	}
	canonical, hash, err := canonicalCapacityGeneration(generation)
	if err != nil {
		return err
	}
	_ = canonical
	return s.tx(ctx, func(tx *sql.Tx) error {
		var priorHash, priorState string
		err := tx.QueryRowContext(ctx, `SELECT input_sha256,state FROM capacity_generations WHERE generation_id=?`, generation.ID).
			Scan(&priorHash, &priorState)
		if err == nil {
			if priorHash == hash && priorState == "active" {
				return nil
			}
			return fmt.Errorf("capacity generation %s idempotency conflict", generation.ID)
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}

		expected := append([]string(nil), generation.ExpectedSeatIDs...)
		sort.Strings(expected)
		for i, seatID := range expected {
			if seatID == "" || i > 0 && seatID == expected[i-1] {
				return errors.New("capacity generation has empty or duplicate expected seat")
			}
		}
		bySeat := make(map[string]CapacitySeatObservation, len(generation.Observations))
		for _, observation := range generation.Observations {
			if observation.ObservationID == "" || observation.SeatID == "" || observation.CollectorID == "" {
				return errors.New("capacity observation identity is incomplete")
			}
			if _, duplicate := bySeat[observation.SeatID]; duplicate {
				return fmt.Errorf("duplicate capacity observation for seat %s", observation.SeatID)
			}
			bySeat[observation.SeatID] = observation
		}
		if len(bySeat) != len(expected) {
			return fmt.Errorf("capacity generation observed %d of %d expected seats", len(bySeat), len(expected))
		}
		for _, seatID := range expected {
			if _, ok := bySeat[seatID]; !ok {
				return fmt.Errorf("capacity generation missing expected seat %s", seatID)
			}
		}

		stamp := now.UTC().Format(rfc3339)
		if _, err := tx.ExecContext(ctx, `INSERT INTO capacity_generations
			(generation_id,state,expected_seats,observed_seats,input_sha256,started_at,created_at)
			VALUES (?,'gathering',?,?,?,?,?)`, generation.ID, len(expected), len(bySeat), hash,
			generation.StartedAt.UTC().Format(rfc3339), stamp); err != nil {
			return err
		}

		type acceptedObservation struct {
			CapacitySeatObservation
			windowsJSON string
		}
		accounts := map[string][]acceptedObservation{}
		for _, seatID := range expected {
			observation := bySeat[seatID]
			var registeredProvider, expectedHost, expectedAccount, expectedLineage string
			var enabled int
			if err := tx.QueryRowContext(ctx, `SELECT agent_family,enabled,expected_host_id,
				expected_account_key,expected_credential_lineage FROM seats WHERE id=?`, seatID).
				Scan(&registeredProvider, &enabled, &expectedHost, &expectedAccount, &expectedLineage); err != nil {
				return err
			}
			windowsJSON, err := json.Marshal(observation.Windows)
			if err != nil {
				return err
			}
			identityMatch := expectedHost != "" && expectedAccount != "" &&
				observation.HostID == expectedHost && observation.AccountKey == expectedAccount &&
				observation.Provider == registeredProvider
			lineageMatch := expectedLineage != "" && observation.CredentialLineage == expectedLineage
			live := observation.Source == "live_app_server" || observation.Source == "live_billing"
			accepted := enabled == 1 && identityMatch && lineageMatch && live &&
				observation.TrustState == "verified" && observation.IntegrityState == "verified" &&
				!observation.FetchedAt.IsZero()
			reasons := []string{}
			if enabled != 1 {
				reasons = append(reasons, "seat_disabled")
			}
			if !identityMatch {
				reasons = append(reasons, "identity_mismatch")
			}
			if !lineageMatch {
				reasons = append(reasons, "credential_lineage_mismatch")
			}
			if !live {
				reasons = append(reasons, "live_source_required")
			}
			if observation.TrustState != "verified" || observation.IntegrityState != "verified" {
				reasons = append(reasons, "unverified")
			}
			if observation.FetchedAt.IsZero() {
				reasons = append(reasons, "capture_time_missing")
			}
			rejection := strings.Join(reasons, ",")
			_, err = tx.ExecContext(ctx, `INSERT INTO account_usage_observations
				(observation_id,generation_id,seat_id,host_id,provider,account_key,
				 expected_account_key,credential_lineage,expected_lineage,collector_id,source,
				 trust_state,integrity_state,identity_match,lineage_match,billing_period_active,
				 windows_json,rate_limited,fetched_at,persisted_at,retry_at,live_unavailable_reason,
				 raw_response_sha256,accepted,rejection_reason,adapter_version)
				VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, observation.ObservationID,
				generation.ID, seatID, observation.HostID, observation.Provider, observation.AccountKey,
				expectedAccount, observation.CredentialLineage, expectedLineage, observation.CollectorID,
				observation.Source, observation.TrustState, observation.IntegrityState, b2i(identityMatch),
				b2i(lineageMatch), b2i(observation.BillingPeriodActive), string(windowsJSON),
				b2i(observation.RateLimited), formatCapacityTime(observation.FetchedAt), stamp,
				formatCapacityTime(observation.RetryAt), observation.LiveUnavailableReason,
				observation.RawSHA256, b2i(accepted), rejection, observation.AdapterVersion)
			if err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO capacity_seat_projection
				(seat_id,generation_id,observation_id,host_id,provider,account_key,source,trust_state,
				 integrity_state,identity_match,lineage_match,billing_period_active,windows_json,
				 rate_limited,fetched_at,persisted_at,live_unavailable_reason)
				VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
				ON CONFLICT(seat_id) DO UPDATE SET generation_id=excluded.generation_id,
				 observation_id=excluded.observation_id,host_id=excluded.host_id,provider=excluded.provider,
				 account_key=excluded.account_key,source=excluded.source,trust_state=excluded.trust_state,
				 integrity_state=excluded.integrity_state,identity_match=excluded.identity_match,
				 lineage_match=excluded.lineage_match,billing_period_active=excluded.billing_period_active,
				 windows_json=excluded.windows_json,rate_limited=excluded.rate_limited,
				 fetched_at=excluded.fetched_at,persisted_at=excluded.persisted_at,
				 live_unavailable_reason=excluded.live_unavailable_reason`, seatID, generation.ID,
				observation.ObservationID, observation.HostID, observation.Provider, observation.AccountKey,
				observation.Source, observation.TrustState, observation.IntegrityState, b2i(identityMatch),
				b2i(lineageMatch), b2i(observation.BillingPeriodActive), string(windowsJSON),
				b2i(observation.RateLimited), formatCapacityTime(observation.FetchedAt), stamp,
				observation.LiveUnavailableReason); err != nil {
				return err
			}
			// Seat health is part of the same generation projection. Leaving the
			// legacy value in place would make a freshly verified seat remain
			// unreachable (or a drifted seat remain ready) even though v2 routing
			// had atomically advanced to this observation. Quota/rate decisions stay
			// in CapacityRouteForSeat; health here only reflects whether this exact
			// host/account/credential incarnation produced acceptable live truth.
			health, healthDetail := capacitySeatHealth(accepted, enabled == 1,
				identityMatch, lineageMatch, observation, rejection)
			if _, err := tx.ExecContext(ctx, `UPDATE seats SET health=?,health_detail=?,
				last_probe_at=?,updated_at=? WHERE id=?`, health, healthDetail, stamp, stamp,
				seatID); err != nil {
				return err
			}
			if accepted {
				key := observation.Provider + "\x00" + observation.AccountKey
				accounts[key] = append(accounts[key], acceptedObservation{observation, string(windowsJSON)})
			}
		}

		for _, candidates := range accounts {
			sort.Slice(candidates, func(i, j int) bool {
				if !candidates[i].FetchedAt.Equal(candidates[j].FetchedAt) {
					return candidates[i].FetchedAt.After(candidates[j].FetchedAt)
				}
				return candidates[i].SeatID < candidates[j].SeatID
			})
			winner := candidates[0]
			if _, err := tx.ExecContext(ctx, `INSERT INTO capacity_account_projection
				(provider,account_key,generation_id,source_observation_id,trust_state,windows_json,
				 rate_limited,fetched_at,persisted_at) VALUES (?,?,?,?,?,?,?,?,?)
				ON CONFLICT(provider,account_key) DO UPDATE SET generation_id=excluded.generation_id,
				 source_observation_id=excluded.source_observation_id,trust_state=excluded.trust_state,
				 windows_json=excluded.windows_json,rate_limited=excluded.rate_limited,
				 fetched_at=excluded.fetched_at,persisted_at=excluded.persisted_at`, winner.Provider,
				winner.AccountKey, generation.ID, winner.ObservationID, winner.TrustState,
				winner.windowsJSON, b2i(winner.RateLimited), winner.FetchedAt.UTC().Format(rfc3339), stamp); err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx, `UPDATE capacity_generations SET state='active',committed_at=?
			WHERE generation_id=? AND state='gathering'`, stamp, generation.ID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO capacity_active_generation(singleton,generation_id,activated_at)
			VALUES (1,?,?) ON CONFLICT(singleton) DO UPDATE SET generation_id=excluded.generation_id,
			activated_at=excluded.activated_at`, generation.ID, stamp); err != nil {
			return err
		}
		// Capacity refusals are observations of the prior active generation, not
		// permanent operator decisions. Re-arm their still-pending review jobs when
		// a new complete generation becomes authoritative; the next claimant is
		// evaluated against this generation and may create a new deduped hold if it
		// remains ineligible. No lease or Driver action exists during either state.
		if err := rearmReviewCapacityHoldsTx(ctx, tx, generation.ID, now); err != nil {
			return err
		}
		payload, _ := json.Marshal(map[string]any{"generation_id": generation.ID,
			"expected_seats": len(expected), "accepted_accounts": len(accounts)})
		return appendGlobalControlEventTx(ctx, tx, "capacity_generation_activated", string(payload), now)
	})
}

func capacitySeatHealth(accepted, enabled, identityMatch, lineageMatch bool,
	observation CapacitySeatObservation, rejection string) (string, string) {
	if accepted {
		return SeatReady, "verified " + observation.Source
	}
	detail := rejection
	if observation.LiveUnavailableReason != "" {
		if detail != "" {
			detail += ": "
		}
		detail += observation.LiveUnavailableReason
	}
	if detail == "" {
		detail = "capacity observation held"
	}
	if !enabled {
		return SeatUnreachable, detail
	}
	if !identityMatch || !lineageMatch || capacityAuthFailure(observation.LiveUnavailableReason) {
		return SeatAuthDead, detail
	}
	return SeatUnreachable, detail
}

func capacityAuthFailure(reason string) bool {
	switch reason {
	case "token_expired", "token_rejected", "credentials_missing", "identity_missing", "app_server_auth":
		return true
	default:
		return false
	}
}

func rearmReviewCapacityHoldsTx(ctx context.Context, tx *sql.Tx, generationID string, now time.Time) error {
	rows, err := tx.QueryContext(ctx, `SELECT epic_id,project_id,state_version
		FROM epic_deliveries WHERE state='review_queued'
		  AND hold_kind='review_capacity_unavailable' AND review_job_id<>''`)
	if err != nil {
		return err
	}
	type held struct {
		epicID, projectID string
		version           int
	}
	var heldRows []held
	for rows.Next() {
		var item held
		if err := rows.Scan(&item.epicID, &item.projectID, &item.version); err != nil {
			rows.Close()
			return err
		}
		heldRows = append(heldRows, item)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	nowText := now.UTC().Format(rfc3339)
	for _, item := range heldRows {
		res, err := tx.ExecContext(ctx, `UPDATE epic_deliveries SET state_version=state_version+1,
			hold_kind='',hold_reason='',return_state='',last_error='',state_due_at=?,updated_at=?
			WHERE epic_id=? AND state='review_queued' AND hold_kind='review_capacity_unavailable'
			  AND state_version=?`, now.Add(10*time.Minute).UTC().Format(rfc3339), nowText,
			item.epicID, item.version)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			continue
		}
		if _, err := tx.ExecContext(ctx, `UPDATE attention_items SET state='resolved',
			resolution='capacity_generation_rearmed',resolved_at=?,updated_at=?
			WHERE epic_id=? AND kind='review_capacity_exhausted'
			  AND state IN ('open','leased','delivering','awaiting_ack')`, nowText, nowText,
			item.epicID); err != nil {
			return err
		}
		payload, _ := json.Marshal(map[string]string{"generation_id": generationID})
		if err := appendEpicControlEventTx(ctx, tx, item.projectID, item.epicID,
			"review_capacity_rearmed", "review_queued", "review_queued", item.version+1,
			"capacity_reconciler", string(payload), now); err != nil {
			return err
		}
	}
	return nil
}

func canonicalCapacityGeneration(g CapacityGeneration) ([]byte, string, error) {
	copyGeneration := g
	copyGeneration.ExpectedSeatIDs = append([]string(nil), g.ExpectedSeatIDs...)
	copyGeneration.Observations = append([]CapacitySeatObservation(nil), g.Observations...)
	sort.Strings(copyGeneration.ExpectedSeatIDs)
	sort.Slice(copyGeneration.Observations, func(i, j int) bool {
		return copyGeneration.Observations[i].SeatID < copyGeneration.Observations[j].SeatID
	})
	for i := range copyGeneration.Observations {
		copyGeneration.Observations[i].Windows = append([]capacity.RouteWindow(nil), copyGeneration.Observations[i].Windows...)
		sort.Slice(copyGeneration.Observations[i].Windows, func(a, b int) bool {
			left, right := copyGeneration.Observations[i].Windows[a], copyGeneration.Observations[i].Windows[b]
			if left.Kind != right.Kind {
				return left.Kind < right.Kind
			}
			return left.Scope < right.Scope
		})
	}
	b, err := json.Marshal(copyGeneration)
	if err != nil {
		return nil, "", err
	}
	h := sha256.Sum256(b)
	return b, "sha256:" + hex.EncodeToString(h[:]), nil
}

func formatCapacityTime(value time.Time) string {
	if value.IsZero() {
		return ""
	}
	return value.UTC().Format(rfc3339)
}

// CapacityRouteForSeat reads only one complete active generation and evaluates
// freshness at read time. A failed generation or stale projection never falls
// back to worker_accounts.usage_pct.
func (s *Store) CapacityRouteForSeat(ctx context.Context, seatID string, now time.Time, freshFor time.Duration) (capacity.RouteDecision, error) {
	return capacityRouteForSeatQueryExcludingEpic(ctx, s.DB, seatID, "", now, freshFor)
}

type capacityQueryer interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func capacityRouteForSeatQuery(ctx context.Context, q capacityQueryer, seatID string, now time.Time, freshFor time.Duration) (capacity.RouteDecision, error) {
	return capacityRouteForSeatQueryExcludingEpic(ctx, q, seatID, "", now, freshFor)
}

// capacityRouteForSeatQueryExcludingEpic is used only for an already-committed
// compute lease's pre-mutation recheck. The named epic occupies the seat/account
// by design and must not make its own exact lease appear over capacity.
func capacityRouteForSeatQueryExcludingEpic(ctx context.Context, q capacityQueryer, seatID,
	excludedEpicID string, now time.Time, freshFor time.Duration) (capacity.RouteDecision, error) {
	var o capacity.RouteObservation
	var enabled, identityMatch, lineageMatch, billingActive, seatRate, accountRate int
	var seatHealth, seatFetched, accountFetched, seatWindows, accountWindows string
	var reserve float64
	err := q.QueryRowContext(ctx, `SELECT sp.provider,sp.account_key,sp.seat_id,sp.host_id,
		se.enabled,se.health,sp.source,sp.trust_state,
		(sp.identity_match=1 AND sp.host_id=se.expected_host_id
		 AND sp.account_key=se.expected_account_key AND sp.provider=se.agent_family),
		(sp.lineage_match=1 AND obs.credential_lineage=se.expected_credential_lineage),
		sp.fetched_at,ap.fetched_at,ap.trust_state,sp.rate_limited,ap.rate_limited,
		sp.billing_period_active,se.max_concurrent,se.account_max_concurrent,se.capacity_reserve_pct,
		sp.windows_json,ap.windows_json
		FROM capacity_active_generation ag
		JOIN capacity_seat_projection sp ON sp.seat_id=? AND sp.generation_id=ag.generation_id
		JOIN seats se ON se.id=sp.seat_id
		JOIN account_usage_observations obs ON obs.observation_id=sp.observation_id
		 AND obs.generation_id=sp.generation_id AND obs.seat_id=sp.seat_id
		JOIN capacity_account_projection ap ON ap.provider=sp.provider AND ap.account_key=sp.account_key
		 AND ap.generation_id=ag.generation_id WHERE ag.singleton=1`, seatID).
		Scan(&o.Provider, &o.AccountKey, &o.SeatID, &o.HostID, &enabled, &seatHealth,
			&o.Source, &o.TrustState, &identityMatch, &lineageMatch, &seatFetched,
			&accountFetched, &o.AccountTrustState, &seatRate, &accountRate, &billingActive,
			&o.SeatMaximum, &o.AccountMaximum, &reserve, &seatWindows, &accountWindows)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return capacity.RouteDecision{Reasons: []string{capacity.HoldLiveSourceRequired}}, nil
		}
		return capacity.RouteDecision{}, err
	}
	// The account projection is the coalesced quota authority. Seat windows remain
	// historical/display evidence and must never weaken it.
	if err := json.Unmarshal([]byte(accountWindows), &o.Windows); err != nil {
		return capacity.RouteDecision{}, err
	}
	_ = seatWindows
	o.Enabled, o.SeatReady = enabled == 1, seatHealth == SeatReady
	o.IdentityMatches, o.CredentialLineageMatches = identityMatch == 1, lineageMatch == 1
	o.RateLimited = seatRate == 1 || accountRate == 1
	o.BillingPeriodActive = billingActive == 1
	o.FetchedAt, _ = time.Parse(rfc3339, seatFetched)
	o.AccountFetchedAt, _ = time.Parse(rfc3339, accountFetched)
	_ = q.QueryRowContext(ctx, `SELECT COUNT(*) FROM epics WHERE seat_id=? AND id<>?
		AND state IN `+epicActiveStatesSQL, seatID, excludedEpicID).Scan(&o.SeatActive)
	_ = q.QueryRowContext(ctx, `SELECT COUNT(*) FROM epics WHERE account_key=? AND id<>?
		AND state IN `+epicActiveStatesSQL, o.AccountKey, excludedEpicID).Scan(&o.AccountActive)
	var reviewerSeatActive, reviewerAccountActive int
	_ = q.QueryRowContext(ctx, `SELECT COUNT(*) FROM epic_worker_sessions WHERE seat_id=?
		AND worker_role='reviewer' AND epic_id<>? AND state IN ('ensure_pending','active','stop_pending','held')`,
		seatID, excludedEpicID).Scan(&reviewerSeatActive)
	_ = q.QueryRowContext(ctx, `SELECT COUNT(*) FROM epic_worker_sessions w JOIN seats s ON s.id=w.seat_id
		WHERE s.expected_account_key=? AND w.worker_role='reviewer' AND w.epic_id<>?
		AND w.state IN ('ensure_pending','active','stop_pending','held')`, o.AccountKey,
		excludedEpicID).Scan(&reviewerAccountActive)
	o.SeatActive += reviewerSeatActive
	o.AccountActive += reviewerAccountActive
	return capacity.DecideRoute(now, o, capacity.RoutePolicy{FreshFor: freshFor, ReservePct: reserve}), nil
}

// markReviewCapacityHoldTx makes a fail-closed routing refusal visible without
// consuming the review job. A later complete capacity generation can make the
// same pending job claimable; no lease or Driver action exists while this hold
// is active.
func markReviewCapacityHoldTx(ctx context.Context, tx *sql.Tx, projectID, epicID, jobID, seatID, detail string, now time.Time) error {
	var version int
	if err := tx.QueryRowContext(ctx, `SELECT state_version FROM epic_deliveries
		WHERE epic_id=? AND state='review_queued' AND review_job_id=?`, epicID, jobID).Scan(&version); err != nil {
		return err
	}
	nowText := now.UTC().Format(rfc3339)
	res, err := tx.ExecContext(ctx, `UPDATE epic_deliveries SET
		state_version=state_version+1,hold_kind='review_capacity_unavailable',hold_reason=?,
		return_state='review_queued',alert_pending=1,last_error=?,state_due_at=?,updated_at=?
		WHERE epic_id=? AND state='review_queued' AND review_job_id=? AND state_version=?`,
		detail, detail, now.Add(10*time.Minute).UTC().Format(rfc3339), nowText,
		epicID, jobID, version)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return fmt.Errorf("review capacity hold lost delivery race for %s", epicID)
	}

	dedupHash := sha256.Sum256([]byte(epicID + "\x00" + jobID + "\x00" + seatID))
	dedup := "review_capacity_unavailable:" + epicID + ":" + hex.EncodeToString(dedupHash[:8])
	attentionID := "review-capacity-unavailable-" + hex.EncodeToString(dedupHash[:12])
	evidence, _ := json.Marshal(map[string]string{
		"job_id": jobID, "seat_id": seatID, "reason": detail,
	})
	insertResult, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO attention_items
		(id,kind,epic_id,repo,priority,state,dedup_key,blocking,leased_by,item_epoch,
		 lease_expires_at,awaiting_since,delivery_key,evidence_json,detail,resolution,verdict,
		 occurrences,first_seen_at,last_seen_at,resolved_at,created_at,updated_at)
		VALUES (?,'review_capacity_exhausted',?,'',10,'open',?,1,'',0,'','','',?,?,'','',1,?,?,'',?,?)`,
		attentionID, epicID, dedup, string(evidence), detail, nowText, nowText, nowText, nowText)
	if err != nil {
		return err
	}
	inserted, _ := insertResult.RowsAffected()
	occurrenceDelta := 1
	if inserted == 1 {
		occurrenceDelta = 0
	}
	if _, err := tx.ExecContext(ctx, `UPDATE attention_items SET
		occurrences=occurrences+?,last_seen_at=?,detail=?,evidence_json=?,
		state=CASE WHEN state='resolved' THEN 'open' ELSE state END,resolved_at='',updated_at=?
		WHERE dedup_key=?`, occurrenceDelta, nowText, detail, string(evidence), nowText, dedup); err != nil {
		return err
	}
	payload, _ := json.Marshal(map[string]string{
		"epic_id": epicID, "job_id": jobID, "seat_id": seatID, "reason": detail,
	})
	if err := ensureControlAlertTx(ctx, tx, projectID, epicID, "capacity_pool_exhausted", dedup, string(payload), now); err != nil {
		return err
	}
	return appendEpicControlEventTx(ctx, tx, projectID, epicID, "review_capacity_held",
		"review_queued", "review_queued", version+1, "scheduler", string(payload), now)
}
