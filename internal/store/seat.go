package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"time"
)

// A SEAT (0028_epic_capacity.sql, plan §15.13) is a place an account is ALREADY
// logged in and usable: (account, box, agent family, config dir/env). The launch gate
// provisions an epic session onto a READY seat by injecting its env
// (CLAUDE_CONFIG_DIR / CODEX_HOME + FLOWBEE_ACCOUNT) at tmux-session creation. Flowbee
// NEVER logs in — the human authenticates once per box; auth_dead routes re-login back
// to the human. This file is the seat registry's store seam.

// Seat health values (plan §15.13a). The staggered capacity ticker probes each seat
// over ssh and sets one of these; the launch gate selects only SeatReady seats.
const (
	SeatReady         = "ready"          // logged in, account has weekly headroom
	SeatLimitCritical = "limit_critical" // account weekly-critical: do not place new work
	SeatAuthDead      = "auth_dead"      // login revoked/absent: needs human re-login
	SeatUnreachable   = "unreachable"    // box/probe unreachable (or never probed)
)

// Agent families a seat may run.
const (
	seatFamilyClaude = "claude"
	seatFamilyCodex  = "codex"
	// seatFamilyGrok is xAI's grok. Like claude it uses a single config-dir env, so it
	// REUSES the config_dir column for its GROK_HOME (no new seat column / migration —
	// the family columns are free-text). The seatID folds the family in, so a grok seat
	// and a claude seat with the same dir string on one box never collide.
	seatFamilyGrok = "grok"
)

// Seat is one seats row.
type Seat struct {
	ID          string
	Box         string // registered epic_hosts.name / ssh destination; '' = control-plane box
	AgentFamily string // claude | codex | grok
	AccountKey  string // account_windows.account_key ('' until resolved by a probe)
	ConfigDir   string // CLAUDE_CONFIG_DIR (claude) / GROK_HOME (grok); '' for codex
	CodexHome   string // CODEX_HOME (codex seats; '' for claude/grok)
	ExtraEnv    map[string]string
	// MaxConcurrent is how many ACTIVE epics this exact authenticated seat may hold
	// at once (0031 migration). DEFAULT 1 preserves the original conservative rule
	// (claude/grok seats stay 1-wide); an operator raises it when a seat can run more.
	// AddEpicRun's launch gate reads it as the cap for its count-then-insert. A zero
	// value read off a row that predates the column, or a caller that leaves it unset,
	// is normalized to 1 at the enforcement seam — never a cap of 0 (which would refuse
	// every launch onto the seat).
	MaxConcurrent int
	Enabled       bool
	Health        string
	HealthDetail  string
	LastProbeAt   string // RFC3339; '' = never probed
	CreatedAt     string
	UpdatedAt     string
}

// Ident returns the seat's dir identity — codex_home for a codex seat, else config_dir
// (CLAUDE_CONFIG_DIR for claude, GROK_HOME for grok) — the value the
// UNIQUE(box, config_dir, codex_home) constraint keys on.
func (s Seat) Ident() string {
	if s.AgentFamily == seatFamilyCodex {
		return s.CodexHome
	}
	return s.ConfigDir
}

var (
	ErrSeatNotFound = errors.New("seat not found")
	ErrSeatExists   = errors.New("seat already registered")
)

// envKeyRe bounds an extra-env variable NAME to the POSIX env charset — the value is
// separately argv-safe-validated. A stray name/value that could split argv or inject an
// ssh/shell token at launch is rejected at REGISTRATION (defense in depth: the launch
// ladder also builds from a closed template).
var envKeyRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// AddSeat registers a new seat (`flowbee seat add` / `flowbee seat discover`). NOT an
// upsert — re-registering the same box+dir is almost always an operator mistake and
// would silently rebind where an epic launches, so it fails loud (ErrSeatExists), the
// same posture AddEpicHost/AddGoalSession take. The box/config_dir/codex_home are
// validated argv-safe (they flow into the launch ladder's ssh/tmux/env argv — the F6
// AddEpicHost gate posture) and the family must be claude|codex with EXACTLY the
// matching dir set. The seat id is a deterministic "<box>|<ident>" composite.
func (s *Store) AddSeat(ctx context.Context, seat Seat, now time.Time) error {
	if err := validateSeat(&seat); err != nil {
		return err
	}
	seat.ID = seat.ComposeID()
	ts := now.Format(rfc3339)
	envJSON, err := marshalEnv(seat.ExtraEnv)
	if err != nil {
		return err
	}
	health := seat.Health
	if health == "" {
		health = SeatUnreachable // never probed yet
	}
	// an unset (0) or negative MaxConcurrent means "the safe default" — one epic at a
	// time — never a cap of 0 that would refuse every launch onto the seat.
	maxConc := seat.MaxConcurrent
	if maxConc < 1 {
		maxConc = 1
	}
	_, err = s.DB.ExecContext(ctx, `
		INSERT INTO seats
		    (id, box, agent_family, account_key, config_dir, codex_home, extra_env_json,
		     max_concurrent, enabled, health, health_detail, last_probe_at, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, 1, ?, ?, ?, ?, ?)`,
		seat.ID, seat.Box, seat.AgentFamily, seat.AccountKey, seat.ConfigDir, seat.CodexHome,
		envJSON, maxConc, health, seat.HealthDetail, seat.LastProbeAt, ts, ts)
	if err != nil {
		if isUniqueConstraintErr(err) {
			return ErrSeatExists
		}
		return fmt.Errorf("add seat %q: %w", seat.ID, err)
	}
	return nil
}

// validateSeat enforces the family/dir/argv-safety invariants. It mutates seat only to
// trim the family to its canonical lowercase token.
func validateSeat(seat *Seat) error {
	switch seat.AgentFamily {
	case seatFamilyClaude:
		if seat.ConfigDir == "" {
			return errors.New("a claude seat requires config_dir (CLAUDE_CONFIG_DIR)")
		}
		if seat.CodexHome != "" {
			return errors.New("a claude seat must not set codex_home")
		}
	case seatFamilyCodex:
		if seat.CodexHome == "" {
			return errors.New("a codex seat requires codex_home (CODEX_HOME)")
		}
		if seat.ConfigDir != "" {
			return errors.New("a codex seat must not set config_dir")
		}
	case seatFamilyGrok:
		// grok reuses config_dir for its GROK_HOME (no dedicated column).
		if seat.ConfigDir == "" {
			return errors.New("a grok seat requires config_dir (GROK_HOME)")
		}
		if seat.CodexHome != "" {
			return errors.New("a grok seat must not set codex_home")
		}
	default:
		return fmt.Errorf("seat agent_family %q must be %q, %q, or %q", seat.AgentFamily, seatFamilyClaude, seatFamilyCodex, seatFamilyGrok)
	}
	if err := validateArgvSafe("seat box", seat.Box); err != nil {
		return err
	}
	if err := validateArgvSafe("seat config_dir", seat.ConfigDir); err != nil {
		return err
	}
	if err := validateArgvSafe("seat codex_home", seat.CodexHome); err != nil {
		return err
	}
	for k, v := range seat.ExtraEnv {
		if !envKeyRe.MatchString(k) {
			return fmt.Errorf("seat extra-env key %q is not a valid env var name", k)
		}
		if err := validateArgvSafe("seat extra-env value for "+k, v); err != nil {
			return err
		}
	}
	return nil
}

// seatID composes the deterministic seat id from box, family, and dir-ident. Family is
// folded in so a claude seat whose config_dir string happens to EQUAL a codex seat's
// codex_home on the same box does not collide on the id (the UNIQUE(box,config_dir,
// codex_home) constraint would allow both rows). A '|' separator is safe: box/ident are
// argv-safe (no whitespace/control) and family is a closed {claude,codex,grok} token, so
// the join is injective.
func seatID(box, family, ident string) string {
	return box + "|" + family + "|" + ident
}

// ComposeID is the exported deterministic seat id for this seat's identity
// (box, family, dir-ident) — the same "<box>|<family>|<ident>" AddSeat mints. Callers
// that need to ADDRESS an already-registered seat by its identity (the `flowbee seat
// set-max-concurrent` CLI, which has the box/family/dir flags but not the raw id) use
// this so the id-composition rule lives in exactly one place.
func (s Seat) ComposeID() string {
	return seatID(s.Box, s.AgentFamily, s.Ident())
}

func marshalEnv(env map[string]string) (string, error) {
	if len(env) == 0 {
		return "{}", nil
	}
	b, err := json.Marshal(env)
	if err != nil {
		return "", fmt.Errorf("marshal seat env: %w", err)
	}
	return string(b), nil
}

func unmarshalEnv(s string) map[string]string {
	if s == "" || s == "{}" {
		return nil
	}
	out := map[string]string{}
	_ = json.Unmarshal([]byte(s), &out)
	return out
}

const seatSelect = `
	SELECT id, box, agent_family, account_key, config_dir, codex_home, extra_env_json,
	       max_concurrent, enabled, health, health_detail, last_probe_at, created_at, updated_at
	  FROM seats`

func scanSeat(row rowScanner) (Seat, error) {
	var seat Seat
	var enabled int
	var envJSON string
	err := row.Scan(&seat.ID, &seat.Box, &seat.AgentFamily, &seat.AccountKey, &seat.ConfigDir,
		&seat.CodexHome, &envJSON, &seat.MaxConcurrent, &enabled, &seat.Health, &seat.HealthDetail,
		&seat.LastProbeAt, &seat.CreatedAt, &seat.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Seat{}, ErrSeatNotFound
	}
	if err != nil {
		return Seat{}, err
	}
	seat.Enabled = enabled != 0
	seat.ExtraEnv = unmarshalEnv(envJSON)
	return seat, nil
}

// GetSeat returns one seat by id. ErrSeatNotFound if absent.
func (s *Store) GetSeat(ctx context.Context, id string) (Seat, error) {
	return scanSeat(s.DB.QueryRowContext(ctx, seatSelect+` WHERE id = ?`, id))
}

// ListSeats returns every registered seat ordered by id (`flowbee seat list`).
func (s *Store) ListSeats(ctx context.Context) ([]Seat, error) {
	return querySeats(ctx, s.DB, seatSelect+` ORDER BY id`)
}

// ListReadySeats returns enabled, health=ready seats for an agent family — the launch
// gate's candidate set (plan §15.13c). Health already encodes account headroom (the
// seat ticker flips a weekly-critical account's seats to limit_critical), so a ready
// seat is launch-eligible; the caller layers the free-box / anti-collocation join on
// top. Ordered by id for a deterministic pick.
func (s *Store) ListReadySeats(ctx context.Context, agentFamily string) ([]Seat, error) {
	return querySeats(ctx, s.DB,
		seatSelect+` WHERE agent_family = ? AND enabled = 1 AND health = ? ORDER BY id`,
		agentFamily, SeatReady)
}

func querySeats(ctx context.Context, db *sql.DB, query string, args ...any) ([]Seat, error) {
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Seat{}
	for rows.Next() {
		seat, err := scanSeat(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, seat)
	}
	return out, rows.Err()
}

// UpdateSeatHealth records a seat probe's outcome (the staggered capacity ticker,
// plan §15.13d). health must be one of the closed set; detail is free text; probedAt is
// the probe instant. ErrSeatNotFound if the seat is gone.
func (s *Store) UpdateSeatHealth(ctx context.Context, id, health, detail string, probedAt time.Time) error {
	if !validSeatHealth(health) {
		return fmt.Errorf("invalid seat health %q", health)
	}
	ts := probedAt.Format(rfc3339)
	res, err := s.DB.ExecContext(ctx, `
		UPDATE seats SET health = ?, health_detail = ?, last_probe_at = ?, updated_at = ?
		 WHERE id = ?`, health, detail, ts, ts, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrSeatNotFound
	}
	return nil
}

// SetSeatAccountKey binds/refreshes a seat's resolved account_key (a probe resolving the
// accountUuid/account_id after registration). ErrSeatNotFound if the seat is gone.
func (s *Store) SetSeatAccountKey(ctx context.Context, id, accountKey string, now time.Time) error {
	ts := now.Format(rfc3339)
	res, err := s.DB.ExecContext(ctx,
		`UPDATE seats SET account_key = ?, updated_at = ? WHERE id = ?`, accountKey, ts, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrSeatNotFound
	}
	return nil
}

// SetSeatMaxConcurrent updates a seat's concurrent-epic cap (`flowbee seat
// set-max-concurrent`) — the operational write that turns a codex seat into a 2-wide seat
// after `flowbee seat discover` registered it at the default 1, without re-adding it. A
// cap below 1 is rejected (a seat that can hold zero epics is a `flowbee seat rm`, not a
// cap). ErrSeatNotFound if the seat is gone. It never touches an ACTIVE epic, so lowering
// the cap below the current occupancy simply stops NEW launches onto the box until the
// running epics finish (the launch gate is a count-at-launch, not a live constraint).
func (s *Store) SetSeatMaxConcurrent(ctx context.Context, id string, maxConcurrent int, now time.Time) error {
	if maxConcurrent < 1 {
		return fmt.Errorf("max_concurrent must be >= 1 (got %d) — to retire a seat use `flowbee seat rm`", maxConcurrent)
	}
	ts := now.Format(rfc3339)
	res, err := s.DB.ExecContext(ctx,
		`UPDATE seats SET max_concurrent = ?, updated_at = ? WHERE id = ?`, maxConcurrent, ts, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrSeatNotFound
	}
	return nil
}

func validSeatHealth(h string) bool {
	switch h {
	case SeatReady, SeatLimitCritical, SeatAuthDead, SeatUnreachable:
		return true
	}
	return false
}
