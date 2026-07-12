package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/samhotchkiss/flowbee/internal/epicspec"
)

// EpicRun is one row of the epics table (0026_epics.sql, epic-lane Phase 2). Named
// `EpicRun` (not `Epic`) deliberately — see the migration's comment — to stay
// unambiguous from the pre-existing, unrelated F4 "epic" (store.SeedEpic/EpicIssue).
type EpicRun struct {
	ID       string // slug parsed off the filename (epics/YYYY-MM-DD-<slug>.md)
	Repo     string
	FilePath string
	Title    string
	Scope    []string
	Host     string
	Branch   string
	TmuxName string // "epic-<slug>" — also the linked goal_sessions.id (same string)
	Agent    string
	State    string // pending|launching|running|blocked|achieved|abandoned|done

	StatusUpdatedAt   string // raw "Updated:" text off the agent's own ## Status
	StatusCurrentStep int
	StatusStepsTotal  int
	StatusStateDetail string // raw "State:" text (distinct from the State field above)
	StatusChecklist   []epicspec.ChecklistItem
	StatusBlockers    string

	CreatedAt  string
	LaunchedAt string
	FinishedAt string
	UpdatedAt  string
}

var (
	ErrEpicRunNotFound = errors.New("epic not found")
	ErrEpicRunExists   = errors.New("epic already registered")
)

// AddEpicRun registers a new epic at state='launching' (`flowbee epic start`'s
// first write, AFTER the scope/host/quota gates pass — see cmd/flowbee/epic.go).
// Starting at 'launching' rather than 'running' means a crash between this insert
// and the tmux session actually coming up leaves a VISIBLE half-launched row
// instead of nothing; runEpicStart's own error path calls DeleteEpicRun to roll
// this back cleanly on any preflight/launch failure, so in steady state a row only
// ever reaches 'launching' for the few seconds a launch is actually in flight.
func (s *Store) AddEpicRun(ctx context.Context, e EpicRun, now time.Time) error {
	if e.ID == "" {
		return errors.New("epic id is required")
	}
	ts := now.Format(rfc3339)
	_, err := s.DB.ExecContext(ctx, `
		INSERT INTO epics
		    (id, repo, file_path, title, scope_json, host, branch, tmux_name, agent,
		     state, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, 'launching', ?, ?)`,
		e.ID, e.Repo, e.FilePath, e.Title, marshalStrings(e.Scope), e.Host, e.Branch,
		e.TmuxName, e.Agent, ts, ts)
	if err != nil {
		if isUniqueConstraintErr(err) {
			return ErrEpicRunExists
		}
		return fmt.Errorf("add epic %q: %w", e.ID, err)
	}
	return nil
}

// DeleteEpicRun hard-deletes an epic row — ONLY used to roll back a launch that
// failed after AddEpicRun but before the tmux session was confirmed up (see its
// doc). Never used on a real (launched) epic; `flowbee epic abandon` marks
// state='abandoned' instead of deleting, so the history stays queryable.
func (s *Store) DeleteEpicRun(ctx context.Context, id string) error {
	_, err := s.DB.ExecContext(ctx, `DELETE FROM epics WHERE id = ?`, id)
	return err
}

// MarkEpicLaunched flips a 'launching' epic to 'running' once the tmux session is
// confirmed up and the goal has been sent (the LAST step of runEpicStart, after
// which `flowbee epic status` is expected to show it — see author-epic/SKILL.md
// "don't consider the epic launched until step 3 confirms it").
func (s *Store) MarkEpicLaunched(ctx context.Context, id string, now time.Time) error {
	ts := now.Format(rfc3339)
	res, err := s.DB.ExecContext(ctx,
		`UPDATE epics SET state = 'running', launched_at = ?, updated_at = ? WHERE id = ?`,
		ts, ts, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrEpicRunNotFound
	}
	return nil
}

// GetEpicRun returns one epic by id. ErrEpicRunNotFound if absent.
func (s *Store) GetEpicRun(ctx context.Context, id string) (EpicRun, error) {
	return scanEpicRun(s.DB.QueryRowContext(ctx, epicRunSelect+` WHERE id = ?`, id))
}

const epicRunSelect = `
	SELECT id, repo, file_path, title, scope_json, host, branch, tmux_name, agent, state,
	       status_updated_at, status_current_step, status_steps_total, status_state_detail,
	       status_checklist_json, status_blockers,
	       created_at, launched_at, finished_at, updated_at
	  FROM epics`

func scanEpicRun(row rowScanner) (EpicRun, error) {
	var e EpicRun
	var scopeJSON, checklistJSON string
	err := row.Scan(&e.ID, &e.Repo, &e.FilePath, &e.Title, &scopeJSON, &e.Host, &e.Branch,
		&e.TmuxName, &e.Agent, &e.State,
		&e.StatusUpdatedAt, &e.StatusCurrentStep, &e.StatusStepsTotal, &e.StatusStateDetail,
		&checklistJSON, &e.StatusBlockers,
		&e.CreatedAt, &e.LaunchedAt, &e.FinishedAt, &e.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return EpicRun{}, ErrEpicRunNotFound
	}
	if err != nil {
		return EpicRun{}, err
	}
	e.Scope = unmarshalStrings(scopeJSON)
	e.StatusChecklist = unmarshalChecklist(checklistJSON)
	return e, nil
}

// ListEpicRuns returns every registered epic ordered by id (`flowbee epic status`,
// full history including terminal states).
func (s *Store) ListEpicRuns(ctx context.Context) ([]EpicRun, error) {
	return queryEpicRuns(ctx, s.DB, epicRunSelect+` ORDER BY id`)
}

// epicActiveStatesSQL is the "still in flight" IN-clause: what the scope/host
// launch gates and the status-ingestion tick both consider ACTIVE. 'pending' is
// excluded (nothing has reserved anything yet — see the migration comment, no
// current command produces it); 'achieved'/'abandoned'/'done' are excluded as
// terminal. A single constant so ListActiveEpicRuns and HostActiveEpic can never
// drift out of sync on what "active" means.
const epicActiveStatesSQL = `('launching','running','blocked')`

// ListActiveEpicRuns returns every in-flight epic (any repo, any host) — the set
// the launch-time scope/host gates check against, and the set the ~2-minute
// ingestion tick re-reads status for. Once an epic reaches achieved/abandoned/done
// it drops out of this list and is simply never ingested again (see UpsertEpicStatus's
// doc for why that's sufficient to make those states terminal in practice).
func (s *Store) ListActiveEpicRuns(ctx context.Context) ([]EpicRun, error) {
	return queryEpicRuns(ctx, s.DB,
		epicRunSelect+` WHERE state IN `+epicActiveStatesSQL+` ORDER BY id`)
}

func queryEpicRuns(ctx context.Context, db *sql.DB, query string, args ...any) ([]EpicRun, error) {
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []EpicRun
	for rows.Next() {
		e, err := scanEpicRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// HostActiveEpic returns the active epic currently holding host (ok=true), or
// ok=false if the host is free — the one-box-one-epic occupancy check
// `flowbee epic start` runs before launching onto a host.
func (s *Store) HostActiveEpic(ctx context.Context, host string) (EpicRun, bool, error) {
	e, err := scanEpicRun(s.DB.QueryRowContext(ctx,
		epicRunSelect+` WHERE host = ? AND state IN `+epicActiveStatesSQL+` LIMIT 1`, host))
	if errors.Is(err, ErrEpicRunNotFound) {
		return EpicRun{}, false, nil
	}
	if err != nil {
		return EpicRun{}, false, err
	}
	return e, true, nil
}

// UpsertEpicStatus folds one status-ingestion pass into the epics row: refreshes
// the status_* fields from a leniently-parsed ## Status block and advances the
// epics lifecycle STATE per nextEpicState's narrow mapping — state is NOT a mirror
// of the raw agent-reported text (0026 migration comment), it only ever advances
// off it. It also consults the LINKED goal_sessions row (id == epics.tmux_name,
// the "epic-<slug>" convention both share) for the watchdog's independently
// observed StateAchieved signal (task brief point 2's "(a) the goal-session
// watchdog's session state") — an agent that reaches the goal without ever writing
// State: done to its own ## Status still surfaces as achieved here. Callers
// (the ~2-minute ingestion tick) are expected to call this ONLY for currently
// ACTIVE epics (ListActiveEpicRuns); once state reaches done/achieved this method
// is simply not invoked again for that id — see ListActiveEpicRuns's doc for why
// that omission alone is what makes those terminal, with no extra guard needed here.
func (s *Store) UpsertEpicStatus(ctx context.Context, id string, sb epicspec.StatusBlock, now time.Time) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		var tmuxName, curState string
		err := tx.QueryRowContext(ctx, `SELECT tmux_name, state FROM epics WHERE id = ?`, id).
			Scan(&tmuxName, &curState)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrEpicRunNotFound
		}
		if err != nil {
			return err
		}
		newState := nextEpicState(curState, sb.State)
		if tmuxName != "" {
			var sessState string
			// best-effort join: a missing/unreadable goal_sessions row must never
			// fail the whole status ingest (sql.ErrNoRows or any scan issue is
			// silently treated as "no achieved signal this pass").
			if serr := tx.QueryRowContext(ctx, `SELECT state FROM goal_sessions WHERE id = ?`, tmuxName).
				Scan(&sessState); serr == nil && sessState == "achieved" && newState != "done" {
				newState = "achieved"
			}
		}
		ts := now.Format(rfc3339)
		becameTerminal := (newState == "done" || newState == "achieved") &&
			curState != "done" && curState != "achieved"
		checklistJSON := marshalChecklist(sb.Checklist)
		if becameTerminal {
			_, err = tx.ExecContext(ctx, `
				UPDATE epics SET updated_at = ?, status_updated_at = ?, status_current_step = ?,
				    status_steps_total = ?, status_state_detail = ?, status_checklist_json = ?,
				    status_blockers = ?, state = ?, finished_at = ?
				 WHERE id = ?`,
				ts, sb.UpdatedRaw, sb.CurrentStep, sb.StepsTotal, sb.State, checklistJSON,
				sb.Blockers, newState, ts, id)
		} else {
			_, err = tx.ExecContext(ctx, `
				UPDATE epics SET updated_at = ?, status_updated_at = ?, status_current_step = ?,
				    status_steps_total = ?, status_state_detail = ?, status_checklist_json = ?,
				    status_blockers = ?, state = ?
				 WHERE id = ?`,
				ts, sb.UpdatedRaw, sb.CurrentStep, sb.StepsTotal, sb.State, checklistJSON,
				sb.Blockers, newState, id)
		}
		return err
	})
}

// nextEpicState maps a raw agent-reported "State:" word to the epics lifecycle
// state, per epics/INSTRUCTIONS.md's documented vocabulary (building|blocked|done,
// plus the author template's initial "pending"). Unrecognized or empty text
// leaves the CURRENT lifecycle state unchanged (fail toward "no transition" —
// same "degrade to inert" posture the Phase 1 watchdog's own status parser uses)
// rather than guessing or resetting to something misleading.
func nextEpicState(cur, raw string) string {
	r := strings.ToLower(strings.TrimSpace(raw))
	switch {
	case r == "":
		return cur
	case strings.Contains(r, "done"):
		return "done"
	case strings.Contains(r, "blocked"):
		return "blocked"
	case strings.Contains(r, "building") || strings.Contains(r, "pursuing") ||
		strings.Contains(r, "working") || strings.Contains(r, "running"):
		return "running"
	default:
		return cur
	}
}

// AbandonEpicRun marks an epic abandoned and releases both reservations it was
// holding: the scope/host occupancy (immediate — ListActiveEpicRuns excludes
// 'abandoned') and the linked goal_sessions watch (disabled in the SAME tx, direct
// SQL rather than SetGoalSessionEnabled, so the two writes commit atomically). Per
// the task brief this deliberately does NOT kill the tmux session — that's an
// operator decision the CLI output calls out explicitly (cmd/flowbee/epic.go).
func (s *Store) AbandonEpicRun(ctx context.Context, id string, now time.Time) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		var tmuxName string
		err := tx.QueryRowContext(ctx, `SELECT tmux_name FROM epics WHERE id = ?`, id).Scan(&tmuxName)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrEpicRunNotFound
		}
		if err != nil {
			return err
		}
		ts := now.Format(rfc3339)
		if _, err := tx.ExecContext(ctx,
			`UPDATE epics SET state = 'abandoned', finished_at = ?, updated_at = ? WHERE id = ?`,
			ts, ts, id); err != nil {
			return err
		}
		if tmuxName != "" {
			// best-effort: a goal_sessions row that's already gone (0 rows affected)
			// is not an error here — abandon must still succeed.
			if _, err := tx.ExecContext(ctx,
				`UPDATE goal_sessions SET enabled = 0, updated_at = ? WHERE id = ?`,
				ts, tmuxName); err != nil {
				return err
			}
		}
		return nil
	})
}

func marshalChecklist(items []epicspec.ChecklistItem) string {
	if len(items) == 0 {
		return "[]"
	}
	b, _ := json.Marshal(items)
	return string(b)
}

func unmarshalChecklist(s string) []epicspec.ChecklistItem {
	if s == "" || s == "[]" {
		return nil
	}
	var out []epicspec.ChecklistItem
	_ = json.Unmarshal([]byte(s), &out)
	return out
}
