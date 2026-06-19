package store

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/samhotchkiss/flowbee/internal/capacity"
	"github.com/samhotchkiss/flowbee/internal/job"
)

// ErrNoCapacity means a box has no free per-model SLOT (its advertised concurrency
// for the model is full) OR no per-model ACCOUNT below its ceiling (every account
// is at/over ceiling or rate-limited). Either way the box must WAIT — the lease
// loop treats it like ErrLostRace (the job stays leasable for another box, and the
// no_eligible_worker alarm can eventually fire, §C capacity alarm).
var ErrNoCapacity = errors.New("no per-model capacity (slot full or all accounts at ceiling)")

// activeLeaseStatesClause is the SQL IN-list of states that hold a live lease (one per
// running agent). Counting a worker's jobs in these states gives its in-flight slot
// usage. DERIVED from the canonical job.ActiveLeaseStates so it can never drift — a
// previous hand-copied literal omitted resolving_conflict, so a box running a
// conflict_resolver was invisible to the gate and could overcommit (run advertised+N
// agents). resolving_conflict and the worker-less merge states are now counted exactly
// as the canonical set defines them.
var activeLeaseStatesClause = job.ActiveLeaseStatesSQL()

// SetWorkerModelSlots advertises a box's PER-MODEL concurrency (§C "box =
// multi-model, multi-slot"): a set of (model_family -> max_slots) replacing the
// single max_concurrent_leases. Idempotent upsert per (worker, model). The box
// sends this on registration; the lease claim gates dispatch on it.
func (s *Store) SetWorkerModelSlots(ctx context.Context, workerID string, slots map[string]int, weight int, now time.Time) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		if weight < 1 {
			weight = 1
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE workers SET distribution_weight = ? WHERE worker_id = ?`, weight, workerID); err != nil {
			return err
		}
		for model, max := range slots {
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO worker_model_slots (worker_id, model_family, max_slots, updated_at)
				VALUES (?, ?, ?, ?)
				ON CONFLICT (worker_id, model_family) DO UPDATE SET
				    max_slots  = excluded.max_slots,
				    updated_at = excluded.updated_at`,
				workerID, model, max, now.Format(rfc3339)); err != nil {
				return err
			}
		}
		return nil
	})
}

// AccountSpec describes one named per-model account to enroll/update (§C
// "accounts"). PreferenceRank orders the rollover chain (lower = preferred).
type AccountSpec struct {
	AccountID      string
	ModelFamily    string
	CeilingPct     int
	PreferenceRank int
}

// UpsertAccounts enrolls/updates the named per-model accounts (the rollover chain).
// Usage state (usage_pct / rate_limited) is preserved across an upsert — only the
// configuration (ceiling, rank) is replaced — because usage is reported separately
// and shared across boxes on the same login. Idempotent.
func (s *Store) UpsertAccounts(ctx context.Context, specs []AccountSpec, now time.Time) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		for _, a := range specs {
			ceiling := a.CeilingPct
			if ceiling == 0 {
				ceiling = 90 // the shipped default ceiling (§C "e.g. 90%")
			}
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO worker_accounts
				    (account_id, model_family, ceiling_pct, preference_rank, updated_at)
				VALUES (?, ?, ?, ?, ?)
				ON CONFLICT (account_id) DO UPDATE SET
				    model_family    = excluded.model_family,
				    ceiling_pct     = excluded.ceiling_pct,
				    preference_rank = excluded.preference_rank,
				    updated_at      = excluded.updated_at`,
				a.AccountID, a.ModelFamily, ceiling, a.PreferenceRank, now.Format(rfc3339)); err != nil {
				return err
			}
		}
		return nil
	})
}

// RecordUsage folds per-account usage reports into the shared account buckets (the
// POST /v1/workers/usage path, §C). Usage is PER ACCOUNT (shared across boxes on
// the same login), so a report keyed by account_id updates the canonical bucket
// the ceiling gate reads. A 429 report pins the account to a cool-down; a normal
// sub-ceiling report clears a prior 429. Returns the accounts that are now AT/OVER
// ceiling (a capacity-alarm surface). PURE fold via capacity.FoldUsage.
func (s *Store) RecordUsage(ctx context.Context, reports []capacity.UsageReport, now time.Time) ([]string, error) {
	var atCeiling []string
	err := s.tx(ctx, func(tx *sql.Tx) error {
		for _, r := range reports {
			prior, err := loadAccountTx(ctx, tx, r.AccountID)
			if err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					// auto-enroll an unknown account on first report so usage is never
					// silently dropped; the box's register call normally enrolls it first.
					prior = capacity.Account{
						AccountID: r.AccountID, ModelFamily: r.ModelFamily, CeilingPct: 90,
					}
					if _, ierr := tx.ExecContext(ctx, `
						INSERT INTO worker_accounts (account_id, model_family, ceiling_pct, preference_rank, updated_at)
						VALUES (?, ?, ?, 0, ?)`,
						r.AccountID, r.ModelFamily, 90, now.Format(rfc3339)); ierr != nil {
						return ierr
					}
				} else {
					return err
				}
			}
			pct, rl := capacity.FoldUsage(prior, r)
			if _, err := tx.ExecContext(ctx, `
				UPDATE worker_accounts
				   SET usage_pct = ?, rate_limited = ?, reported_at = ?, updated_at = ?
				 WHERE account_id = ?`,
				pct, boolToInt(rl), now.Format(rfc3339), now.Format(rfc3339), r.AccountID); err != nil {
				return err
			}
			folded := prior
			folded.UsagePct = pct
			folded.RateLimited = rl
			if folded.AtCeiling() {
				atCeiling = append(atCeiling, r.AccountID)
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return atCeiling, nil
}

// AccountsForModel returns every account for a model_family as capacity.Account
// values, ordered by the rollover chain (preference_rank, then account_id). The
// selector folds these into a dispatch choice.
func (s *Store) AccountsForModel(ctx context.Context, modelFamily string) ([]capacity.Account, error) {
	rows, err := s.DB.QueryContext(ctx, `
		SELECT account_id, model_family, ceiling_pct, preference_rank, usage_pct, rate_limited
		  FROM worker_accounts WHERE model_family = ?
		 ORDER BY preference_rank ASC, account_id ASC`, modelFamily)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []capacity.Account
	for rows.Next() {
		a, err := scanAccount(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// SelectAccountForModel runs the ceiling-gated rollover selection (capacity.
// SelectAccount) over the model's accounts. ok=false means every account is
// at/over ceiling — the dispatch must wait. Read-only; the lease claim re-reads
// inside its tx for the atomic decision.
func (s *Store) SelectAccountForModel(ctx context.Context, modelFamily string) (capacity.Account, bool, error) {
	accts, err := s.AccountsForModel(ctx, modelFamily)
	if err != nil {
		return capacity.Account{}, false, err
	}
	a, ok := capacity.SelectAccount(accts, modelFamily)
	return a, ok, nil
}

// ── tx-scoped helpers used by the atomic lease claim ──

// selectAccountTx re-reads the model's accounts and runs the rollover selection
// INSIDE the claim tx (serialized via MaxOpenConns=1), so the ceiling decision is
// atomic with the slot count and the state flip. Returns ErrNoCapacity if no
// account is below ceiling. modelFamily == "" means the job carries no model gate
// (legacy / accountless): selection is skipped and the empty account is returned.
func selectAccountTx(ctx context.Context, tx *sql.Tx, modelFamily string) (capacity.Account, error) {
	if modelFamily == "" {
		return capacity.Account{}, nil
	}
	rows, err := tx.QueryContext(ctx, `
		SELECT account_id, model_family, ceiling_pct, preference_rank, usage_pct, rate_limited
		  FROM worker_accounts WHERE model_family = ?
		 ORDER BY preference_rank ASC, account_id ASC`, modelFamily)
	if err != nil {
		return capacity.Account{}, err
	}
	var accts []capacity.Account
	for rows.Next() {
		a, serr := scanAccount(rows)
		if serr != nil {
			rows.Close()
			return capacity.Account{}, serr
		}
		accts = append(accts, a)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return capacity.Account{}, err
	}
	// no accounts enrolled for this model => no account gate (accountless dispatch).
	if len(accts) == 0 {
		return capacity.Account{}, nil
	}
	a, ok := capacity.SelectAccount(accts, modelFamily)
	if !ok {
		return capacity.Account{}, ErrNoCapacity
	}
	return a, nil
}

// modelSlotGateTx enforces the box's PER-MODEL concurrency at claim time: the box
// must have a free slot for modelFamily — its advertised max_slots must exceed the
// leases it currently holds for that model. Returns ErrNoCapacity when the slot
// budget is full. workerID may be empty: the box is then resolved from its unique
// identity (workers.identity is UNIQUE). identity == "" or modelFamily == "" skips
// the gate (legacy registration that advertised no per-model slots — back-compat).
func modelSlotGateTx(ctx context.Context, tx *sql.Tx, workerID, identity, modelFamily string) error {
	if identity == "" || modelFamily == "" {
		return nil
	}
	if workerID == "" {
		// resolve the box from its identity (the lease carries identity, not worker_id).
		err := tx.QueryRowContext(ctx,
			`SELECT worker_id FROM workers WHERE identity = ?`, identity).Scan(&workerID)
		if errors.Is(err, sql.ErrNoRows) {
			return nil // unregistered identity: no advertised slots to gate on.
		}
		if err != nil {
			return err
		}
	}
	var maxSlots int
	err := tx.QueryRowContext(ctx,
		`SELECT max_slots FROM worker_model_slots WHERE worker_id = ? AND model_family = ?`,
		workerID, modelFamily).Scan(&maxSlots)
	if errors.Is(err, sql.ErrNoRows) {
		// the box advertised no per-model slots (legacy single-slot worker): don't gate.
		return nil
	}
	if err != nil {
		return err
	}
	// count this box's in-flight leases for the model. A box is identified by its
	// bound identity on the job (the worker leases AS its identity); model_family
	// scopes the slot bucket so claude and codex slots are independent.
	var active int
	if err := tx.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM jobs
		 WHERE bound_identity = ? AND bound_model_family = ?
		   AND state IN `+activeLeaseStatesClause, identity, modelFamily).Scan(&active); err != nil {
		return err
	}
	if !capacity.HasFreeSlot(maxSlots, active) {
		return ErrNoCapacity
	}
	return nil
}

func loadAccountTx(ctx context.Context, tx *sql.Tx, accountID string) (capacity.Account, error) {
	return scanAccount(tx.QueryRowContext(ctx, `
		SELECT account_id, model_family, ceiling_pct, preference_rank, usage_pct, rate_limited
		  FROM worker_accounts WHERE account_id = ?`, accountID))
}

func scanAccount(row rowScanner) (capacity.Account, error) {
	var a capacity.Account
	var rl int
	if err := row.Scan(&a.AccountID, &a.ModelFamily, &a.CeilingPct, &a.PreferenceRank, &a.UsagePct, &rl); err != nil {
		return capacity.Account{}, err
	}
	a.RateLimited = rl != 0
	return a, nil
}

// AccountUsageRow is one account's reported usage for the fleet view / capacity
// dashboard (§G fleet view: account + usage gauge with ceiling line + rollover).
type AccountUsageRow struct {
	AccountID      string `json:"account_id"`
	ModelFamily    string `json:"model_family"`
	CeilingPct     int    `json:"ceiling_pct"`
	PreferenceRank int    `json:"preference_rank"`
	UsagePct       int    `json:"usage_pct"`
	RateLimited    bool   `json:"rate_limited"`
	AtCeiling      bool   `json:"at_ceiling"`
}

// AllAccountUsage returns every account's usage gauge (the §G fleet view source):
// per-account usage with its ceiling line and whether it is currently gated out.
func (s *Store) AllAccountUsage(ctx context.Context) ([]AccountUsageRow, error) {
	rows, err := s.DB.QueryContext(ctx, `
		SELECT account_id, model_family, ceiling_pct, preference_rank, usage_pct, rate_limited
		  FROM worker_accounts ORDER BY model_family ASC, preference_rank ASC, account_id ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AccountUsageRow
	for rows.Next() {
		a, err := scanAccount(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, AccountUsageRow{
			AccountID: a.AccountID, ModelFamily: a.ModelFamily,
			CeilingPct: a.CeilingPct, PreferenceRank: a.PreferenceRank,
			UsagePct: a.UsagePct, RateLimited: a.RateLimited, AtCeiling: a.AtCeiling(),
		})
	}
	return out, rows.Err()
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
