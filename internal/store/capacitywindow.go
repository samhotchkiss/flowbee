package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"math"
	"time"

	"github.com/samhotchkiss/flowbee/internal/acctprobe"
	"github.com/samhotchkiss/flowbee/internal/capacity"
	"github.com/samhotchkiss/flowbee/internal/epicdigest"
)

// account_windows is the epic-lane Phase 6 capacity table (0028_epic_capacity.sql):
// REAL server usage percentages per ACCOUNT, folded from acctprobe.Result by the
// consolidated capacity ticker. This file is the store seam for that fold plus the
// read models the digest/summary and dashboard consume. See the migration comment
// for the schema rationale (§4.2, §12.14, §15.13).

// unknownPct is the -1 sentinel a window percentage carries when it is UNKNOWN (an
// absent window is never a real 0% — acctprobe's own "absent ≠ zero" invariant).
const unknownPct = -1.0

// ScopedWindow is one per-model weekly sub-limit stored in account_windows.
// weekly_scoped_json (Claude weekly_scoped / a Codex model-scoped bucket).
type ScopedWindow struct {
	Scope    string  `json:"scope"`
	Percent  float64 `json:"percent"`
	Severity string  `json:"severity"`
	ResetsAt string  `json:"resets_at,omitempty"`
}

// AccountWindow is one account_windows row (a read model). Percentages carry -1 for
// UNKNOWN; ProbeStale is the §12.14 flag (true = the reading is old/untrustworthy for
// gating). It joins 1:1 to worker_accounts on AccountKey == account_id.
type AccountWindow struct {
	AccountKey   string
	Provider     string
	Email        string
	ModelFamily  string
	SessionPct   float64 // -1 = unknown
	WeeklyPct    float64 // -1 = unknown
	WeeklyScoped []ScopedWindow
	// Windows is acctprobe's FULL per-window set carried verbatim (plan §2.1 + §15.16) —
	// the public `windows[]` the digest/capacity-strip emit. Never nil.
	Windows         []epicdigest.Window
	Severity        string // normal | critical
	ResetsSessionAt string // RFC3339; '' = unknown
	ResetsWeeklyAt  string // RFC3339; '' = unknown
	TrustState      string // acctprobe.TrustState
	ProbeStale      bool
	FetchedAtMs     int64
	ReportedAt      string
}

// Routable reports whether this stored reading is capacity-schedulable — the same
// gate acctprobe.TrustState.Routable() draws, re-derived off the persisted string so
// callers need not re-probe.
func (a AccountWindow) Routable() bool {
	return acctprobe.TrustState(a.TrustState).Routable()
}

// CriticalNonStale reports whether this account is critically capped AND the reading
// is trustworthy — the exact §12.14 predicate the usage_critical producer and the
// on-task rollup gate on (a critical reading that is probe_stale is SUPPRESSED, never
// fired as a phantom critical off a flaky-ssh reading).
func (a AccountWindow) CriticalNonStale() bool {
	return a.Severity == string(acctprobe.SeverityCritical) && !a.ProbeStale
}

// UpsertAccountLimits folds one acctprobe.Result into account_windows (plan §4.2),
// and — for a routable reading only — keeps worker_accounts.usage_pct in sync as
// max(session,weekly) so the legacy ceiling gate and capacity.SelectAccount keep
// working unchanged. It RESPECTS acctprobe's trust semantics:
//
//   - a ROUTABLE result (Verified / VerifiedLocal) writes the full verified
//     percentages, severity, resets, and clears probe_stale; it also folds
//     worker_accounts (auto-enrolling the account if absent, mirroring RecordUsage).
//   - a NON-ROUTABLE result (Stale / DisplayOnly / Held) updates ONLY trust_state,
//     probe_stale, reported_at (and back-fills identity fields that were still empty)
//     and NEVER overwrites the last verified percentages — and never touches
//     worker_accounts (a non-routable reading folded via capacity.FoldUsage would
//     report 0%/false and could clear a prior 429 pin; UsageReport's ok=false guard
//     is the structural precedent).
//
// Both writes commit in ONE serialized tx. now is the fold instant (passed in).
func (s *Store) UpsertAccountLimits(ctx context.Context, res acctprobe.Result, now time.Time) error {
	key := res.Identity.AccountKey
	if key == "" {
		return errors.New("acctprobe result has no account key (cannot key account_windows)")
	}
	provider := string(res.Identity.Provider)
	model := provider // the capacity model_family IS the provider (claude/codex)
	ts := now.Format(rfc3339)
	fetchedMs := int64(0)
	if !res.CapturedAt.IsZero() {
		fetchedMs = res.CapturedAt.UnixMilli()
	}
	trust := string(res.TrustState)

	if !res.Routable() {
		// Non-routable: touch only trust/staleness/identity, never the percentages.
		return s.tx(ctx, func(tx *sql.Tx) error {
			_, err := tx.ExecContext(ctx, `
				INSERT INTO account_windows
				    (account_key, provider, email, model_family, trust_state, probe_stale,
				     fetched_at_ms, reported_at)
				VALUES (?, ?, ?, ?, ?, 1, ?, ?)
				ON CONFLICT(account_key) DO UPDATE SET
				    trust_state   = excluded.trust_state,
				    probe_stale   = 1,
				    fetched_at_ms = excluded.fetched_at_ms,
				    reported_at   = excluded.reported_at,
				    -- back-fill identity only where it was still empty; never clobber a
				    -- previously verified value with a weaker reading's blanks.
				    provider      = CASE WHEN account_windows.provider = '' THEN excluded.provider ELSE account_windows.provider END,
				    email         = CASE WHEN account_windows.email = '' THEN excluded.email ELSE account_windows.email END,
				    model_family  = CASE WHEN account_windows.model_family = '' THEN excluded.model_family ELSE account_windows.model_family END`,
				key, provider, res.Identity.Email, model, trust, fetchedMs, ts)
			return err
		})
	}

	// Routable: write the full verified reading.
	sessionPct, sessionResets := pickWindowPct(res.Usage.Windows, acctprobe.KindSession)
	weeklyPct, weeklyResets := pickWindowPct(res.Usage.Windows, acctprobe.KindWeeklyAll)
	severity := string(acctprobe.SeverityNormal)
	if res.Usage.Windows.Critical() {
		severity = string(acctprobe.SeverityCritical)
	}
	scopedJSON := marshalScopedWindows(res.Usage.Windows)
	windowsJSON := marshalWindows(res.Usage.Windows)

	return s.tx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO account_windows
			    (account_key, provider, email, model_family, session_pct, weekly_pct,
			     weekly_scoped_json, windows_json, severity, resets_session_at, resets_weekly_at,
			     trust_state, probe_stale, fetched_at_ms, reported_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 0, ?, ?)
			ON CONFLICT(account_key) DO UPDATE SET
			    provider           = excluded.provider,
			    email              = excluded.email,
			    model_family       = excluded.model_family,
			    session_pct        = excluded.session_pct,
			    weekly_pct         = excluded.weekly_pct,
			    weekly_scoped_json = excluded.weekly_scoped_json,
			    windows_json       = excluded.windows_json,
			    severity           = excluded.severity,
			    resets_session_at  = excluded.resets_session_at,
			    resets_weekly_at   = excluded.resets_weekly_at,
			    trust_state        = excluded.trust_state,
			    probe_stale        = 0,
			    fetched_at_ms      = excluded.fetched_at_ms,
			    reported_at        = excluded.reported_at`,
			key, provider, res.Identity.Email, model, sessionPct, weeklyPct,
			scopedJSON, windowsJSON, severity, sessionResets, weeklyResets, trust, fetchedMs, ts); err != nil {
			return err
		}
		return syncWorkerAccountUsageTx(ctx, tx, key, model, sessionPct, weeklyPct, res.Usage.RateLimited, ts)
	})
}

// marshalWindows carries acctprobe's FULL window set verbatim into the public
// windows[] shape (kind/percent/severity/resets_at/scope) — the passthrough the digest
// and capacity strip serve (plan §2.1 + §15.16). ResetsAt is RFC3339 (” when unknown).
func marshalWindows(w acctprobe.Windows) string {
	out := digestWindows(w)
	if len(out) == 0 {
		return "[]"
	}
	b, _ := json.Marshal(out)
	return string(b)
}

// digestWindows maps acctprobe windows to the public epicdigest.Window shape.
func digestWindows(w acctprobe.Windows) []epicdigest.Window {
	out := make([]epicdigest.Window, 0, len(w))
	for _, win := range w {
		resets := ""
		if !win.ResetsAt.IsZero() {
			resets = win.ResetsAt.UTC().Format(rfc3339)
		}
		out = append(out, epicdigest.Window{
			Kind:     string(win.Kind),
			Percent:  win.Percent,
			Severity: string(win.Severity),
			ResetsAt: resets,
			Scope:    win.Scope,
		})
	}
	return out
}

func unmarshalWindows(s string) []epicdigest.Window {
	if s == "" || s == "[]" {
		return []epicdigest.Window{}
	}
	var out []epicdigest.Window
	if json.Unmarshal([]byte(s), &out) != nil || out == nil {
		return []epicdigest.Window{}
	}
	return out
}

// syncWorkerAccountUsageTx keeps worker_accounts.usage_pct in sync as
// max(session,weekly) (plan §4.2) so the legacy ceiling gate + capacity.SelectAccount
// keep working. It folds through capacity.FoldUsage (so a rate_limited reading pins,
// and a fresh sub-ceiling reading clears a prior 429), auto-enrolling an account that
// is not yet in worker_accounts — mirroring RecordUsage's first-report auto-enroll so
// a seat's account is selectable without a separate enroll step.
func syncWorkerAccountUsageTx(ctx context.Context, tx *sql.Tx, accountKey, modelFamily string, sessionPct, weeklyPct float64, rateLimited bool, ts string) error {
	pct := maxKnownPct(sessionPct, weeklyPct)
	report := capacity.UsageReport{
		AccountID:   accountKey,
		ModelFamily: modelFamily,
		UsagePct:    int(math.Ceil(pct)),
		RateLimited: rateLimited,
	}
	prior, err := loadAccountTx(ctx, tx, accountKey)
	if errors.Is(err, sql.ErrNoRows) {
		// auto-enroll at the shipped default ceiling so the account is selectable.
		if _, ierr := tx.ExecContext(ctx, `
			INSERT INTO worker_accounts (account_id, model_family, ceiling_pct, preference_rank, updated_at)
			VALUES (?, ?, 90, 0, ?)`, accountKey, modelFamily, ts); ierr != nil {
			return ierr
		}
		prior = capacity.Account{AccountID: accountKey, ModelFamily: modelFamily, CeilingPct: 90}
	} else if err != nil {
		return err
	}
	foldedPct, rl := capacity.FoldUsage(prior, report)
	_, err = tx.ExecContext(ctx, `
		UPDATE worker_accounts
		   SET usage_pct = ?, rate_limited = ?, reported_at = ?, updated_at = ?
		 WHERE account_id = ?`,
		foldedPct, boolToInt(rl), ts, ts, accountKey)
	return err
}

// maxKnownPct returns the larger of two percentages treating -1 (unknown) as absent;
// if BOTH are unknown it returns 0 (a routable reading always carries at least one
// window, so this fallback is defensive only).
func maxKnownPct(a, b float64) float64 {
	best := 0.0
	if a >= 0 {
		best = a
	}
	if b >= 0 && b > best {
		best = b
	}
	return best
}

// pickWindowPct returns the highest-percent window of a kind and its reset timestamp
// (RFC3339, ” if unknown). Percentage is -1 when no such window exists.
func pickWindowPct(w acctprobe.Windows, kind acctprobe.WindowKind) (float64, string) {
	best := unknownPct
	resets := ""
	for _, win := range w {
		if win.Kind != kind {
			continue
		}
		if best == unknownPct || win.Percent > best {
			best = win.Percent
			if !win.ResetsAt.IsZero() {
				resets = win.ResetsAt.UTC().Format(rfc3339)
			} else {
				resets = ""
			}
		}
	}
	return best, resets
}

func marshalScopedWindows(w acctprobe.Windows) string {
	var out []ScopedWindow
	for _, win := range w {
		if win.Kind != acctprobe.KindWeeklyScoped {
			continue
		}
		sw := ScopedWindow{Scope: win.Scope, Percent: win.Percent, Severity: string(win.Severity)}
		if !win.ResetsAt.IsZero() {
			sw.ResetsAt = win.ResetsAt.UTC().Format(rfc3339)
		}
		out = append(out, sw)
	}
	if len(out) == 0 {
		return "[]"
	}
	b, _ := json.Marshal(out)
	return string(b)
}

func unmarshalScopedWindows(s string) []ScopedWindow {
	if s == "" || s == "[]" {
		return nil
	}
	var out []ScopedWindow
	_ = json.Unmarshal([]byte(s), &out)
	return out
}

const accountWindowSelect = `
	SELECT account_key, provider, email, model_family, session_pct, weekly_pct,
	       weekly_scoped_json, windows_json, severity, resets_session_at, resets_weekly_at,
	       trust_state, probe_stale, fetched_at_ms, reported_at
	  FROM account_windows`

func scanAccountWindow(row rowScanner) (AccountWindow, error) {
	var a AccountWindow
	var scopedJSON, windowsJSON string
	var stale int
	err := row.Scan(&a.AccountKey, &a.Provider, &a.Email, &a.ModelFamily, &a.SessionPct,
		&a.WeeklyPct, &scopedJSON, &windowsJSON, &a.Severity, &a.ResetsSessionAt, &a.ResetsWeeklyAt,
		&a.TrustState, &stale, &a.FetchedAtMs, &a.ReportedAt)
	if err != nil {
		return AccountWindow{}, err
	}
	a.WeeklyScoped = unmarshalScopedWindows(scopedJSON)
	a.Windows = unmarshalWindows(windowsJSON)
	a.ProbeStale = stale != 0
	return a, nil
}

// GetAccountWindow returns one account's window row. ok=false when the account has no
// reading yet (never probed).
func (s *Store) GetAccountWindow(ctx context.Context, accountKey string) (AccountWindow, bool, error) {
	a, err := scanAccountWindow(s.DB.QueryRowContext(ctx, accountWindowSelect+` WHERE account_key = ?`, accountKey))
	if errors.Is(err, sql.ErrNoRows) {
		return AccountWindow{}, false, nil
	}
	if err != nil {
		return AccountWindow{}, false, err
	}
	return a, true, nil
}

// ListAccountWindows returns every account's window reading ordered by account_key
// (stable ordering per the §15.16d JSON contract). Never nil — an empty registry is
// an empty slice so a `[]` serializes, never a null.
func (s *Store) ListAccountWindows(ctx context.Context) ([]AccountWindow, error) {
	rows, err := s.DB.QueryContext(ctx, accountWindowSelect+` ORDER BY account_key`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []AccountWindow{}
	for rows.Next() {
		a, err := scanAccountWindow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}
