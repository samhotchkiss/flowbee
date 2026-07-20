package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/samhotchkiss/flowbee/internal/epicspec"
)

// EpicRun is one row of the epics table (0026_epics.sql, epic-lane Phase 2). Named
// `EpicRun` (not `Epic`) deliberately — see the migration's comment — to stay
// unambiguous from the pre-existing, unrelated F4 "epic" (store.SeedEpic/EpicIssue).
type EpicRun struct {
	ID            string // slug parsed off the filename (epics/YYYY-MM-DD-<slug>.md)
	ProjectID     string
	Slug          string
	AdmissionKey  string
	WorkIntentID  string
	IntentVersion int
	ContractHash  string
	// Repositories is the explicit product scope. DeliveryRepo is its exactly-one
	// Git delivery member; Repo remains the compatibility projection of that value.
	Repositories      []string `json:"repositories"`
	DeliveryRepo      string   `json:"delivery_repo"`
	RepositorySetMode string   `json:"repository_set_mode"`
	Repo              string
	FilePath          string
	Title             string
	Scope             []string
	Host              string
	Branch            string
	TmuxName          string // "epic-<slug>" — also the linked goal_sessions.id (same string)
	Agent             string
	State             string // pending|launching|running|blocked|achieved|abandoned|done
	DeliveryState     string
	DeliveryCIState   string
	ReviewJobID       string
	// WorkerBootstrapMaterials is explicit, authoritative launch context supplied
	// before admission. It is deliberately not persisted on epics; its exact bytes
	// are committed into each immutable epic_worker_sessions bootstrap manifest.
	WorkerBootstrapMaterials *EpicWorkerBootstrapMaterials `json:"-"`

	StatusUpdatedAt   string // raw "Updated:" text off the agent's own ## Status
	StatusCurrentStep int
	StatusStepsTotal  int
	StatusStateDetail string // raw "State:" text (distinct from the State field above)
	StatusChecklist   []epicspec.ChecklistItem
	StatusBlockers    string

	// ── epic-lane Phase 6 (0028_epic_capacity.sql): account/seat binding + disk-derived
	// runtime facts. AccountKey/SeatID/BuilderModelFamily are BOUND at launch by the
	// gate; ContextPct/PaneState/AuthState/LastCommitAt are written each supervision pass.
	AccountKey         string
	SeatID             string
	BuilderModelFamily string
	ContextPct         float64 // remaining-context %; -1 = unknown (ctxprobe, §12.4)
	PaneState          string  // last tmuxio.Classify (§12.1)
	AuthState          string  // '' | ok | auth_dead (§12.4/§12.13)
	LastCommitAt       string  // RFC3339 of the newest epic-branch commit
	ExplainerPath      string  // per-epic visual explainer file on the branch (§15.14)

	CreatedAt  string
	LaunchedAt string
	FinishedAt string
	UpdatedAt  string
}

var (
	ErrEpicRunNotFound = errors.New("epic not found")
	ErrEpicRunExists   = errors.New("epic already registered")
	// ErrEpicHostBusy / ErrEpicScopeOverlap are the launch-gate refusals AddEpicRun
	// enforces ATOMICALLY with its insert (review m6): the CLI's own pre-checks in
	// runEpicStart give fast feedback before the expensive ssh preflight, but two
	// concurrent `epic start`s that both passed those reads could otherwise both
	// insert (TOCTOU double-book) — the tx here is the authority, the CLI checks
	// are a courtesy. Both are wrapped with detail via fmt.Errorf("%w: ...").
	// ErrEpicHostBusy keeps its historical name for callers, but capacity is now
	// keyed by the selected seat. Distinct seats on one host can run concurrently;
	// the host is only the physical placement/spread dimension.
	ErrEpicHostBusy          = errors.New("seat is at its concurrent-epic cap")
	ErrEpicScopeOverlap      = errors.New("scope overlaps an active epic")
	ErrEpicAdmissionConflict = errors.New("epic admission contract conflicts with existing admission")
	// ErrEpicDistinctReviewerUnavailable is the fail-closed v2 admission gate:
	// admitting a build without a fresh, routable reviewer from another model
	// family would create a pipeline that can never satisfy its durable review
	// obligation. Callers with durable source state (notably work intents) turn
	// this error into a visible admission hold after this transaction rolls back.
	ErrEpicDistinctReviewerUnavailable = errors.New("no distinct routable review family at admission")
	ErrEpicRepositorySetInvalid        = errors.New("epic repository set is invalid")
)

func normalizeEpicRepositorySet(e *EpicRun) (explicit bool, err error) {
	explicit = len(e.Repositories) > 0 || strings.TrimSpace(e.DeliveryRepo) != ""
	delivery := strings.TrimSpace(e.DeliveryRepo)
	legacyRepo := strings.TrimSpace(e.Repo)
	if delivery == "" {
		delivery = legacyRepo
	}
	if legacyRepo != "" && delivery != legacyRepo {
		return explicit, fmt.Errorf("%w: legacy repo %q differs from delivery repo %q",
			ErrEpicRepositorySetInvalid, legacyRepo, delivery)
	}
	repos := append([]string(nil), e.Repositories...)
	if len(repos) == 0 {
		repos = []string{delivery}
	}
	for i := range repos {
		repos[i] = strings.TrimSpace(repos[i])
		if repos[i] == "" && !(e.ProjectID == "" || e.ProjectID == "default") {
			return explicit, fmt.Errorf("%w: empty repository", ErrEpicRepositorySetInvalid)
		}
	}
	sort.Strings(repos)
	deliveryCount := 0
	for i, repoID := range repos {
		if i > 0 && repoID == repos[i-1] {
			return explicit, fmt.Errorf("%w: duplicate repository %q", ErrEpicRepositorySetInvalid, repoID)
		}
		if repoID == delivery {
			deliveryCount++
		}
	}
	if deliveryCount != 1 {
		return explicit, fmt.Errorf("%w: delivery repository %q must occur exactly once", ErrEpicRepositorySetInvalid, delivery)
	}
	e.Repositories, e.DeliveryRepo, e.Repo = repos, delivery, delivery
	if explicit {
		e.RepositorySetMode = "explicit"
	} else {
		e.RepositorySetMode = "legacy"
	}
	return explicit, nil
}

func epicRepositorySetTx(ctx context.Context, tx *sql.Tx, epicID string) ([]string, string, error) {
	rows, err := tx.QueryContext(ctx, `SELECT repo_id,is_delivery FROM epic_repositories
		WHERE epic_id=? ORDER BY repo_id`, epicID)
	if err != nil {
		return nil, "", err
	}
	defer rows.Close()
	var repos []string
	var delivery string
	for rows.Next() {
		var repoID string
		var isDelivery int
		if err := rows.Scan(&repoID, &isDelivery); err != nil {
			return nil, "", err
		}
		repos = append(repos, repoID)
		if isDelivery == 1 {
			if delivery != "" {
				return nil, "", fmt.Errorf("%w: multiple delivery repositories", ErrEpicRepositorySetInvalid)
			}
			delivery = repoID
		}
	}
	if err := rows.Err(); err != nil {
		return nil, "", err
	}
	if len(repos) == 0 || delivery == "" && !(len(repos) == 1 && repos[0] == "") {
		return nil, "", fmt.Errorf("%w: incomplete durable repository set", ErrEpicRepositorySetInvalid)
	}
	return repos, delivery, nil
}

func equalStringSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// AddEpicRun registers a new epic at state='launching' — `flowbee epic start`'s
// one durable write before the tmux launch. The per-seat CONCURRENCY-CAP gate and the
// shared-repository scope-overlap gate run INSIDE the same transaction as the insert (review
// m6): the store pins MaxOpenConns(1), so the count-then-insert is serialized against
// any concurrent start and can never over-book a seat or double-book a scope.
//
// When SeatID is present (the production launch path), the authoritative seat row is
// loaded inside this transaction. Its max_concurrent is the cap, and its box/account/
// family become the epic's binding. The caller-supplied seatCap and placement fields
// cannot weaken or stale that decision. Occupancy includes the exact seat plus legacy
// active rows on its box that predate seat binding. This lets two distinct cap-1 seats on
// one host run simultaneously without allowing either seat to be double-booked.
//
// Legacy/unbound callers retain the historical host-based gate so old imports and tests
// remain readable. An empty host has no identity and therefore cannot be capacity-gated;
// real local launches are safe because the production path always supplies a registered
// SeatID. A legacy cap < 1 normalizes to 1.
//
// Starting at 'launching' rather than 'running' means a crash between this insert
// and the tmux session actually coming up leaves a VISIBLE half-launched row
// instead of nothing; runEpicStart's own error path calls DeleteEpicRun to roll
// this back cleanly on any preflight/launch failure, so in steady state a row only
// ever reaches 'launching' for the few seconds a launch is actually in flight.
func (s *Store) AddEpicRun(ctx context.Context, e EpicRun, seatCap int, now time.Time) error {
	s.epicWorkerActivationMu.Lock()
	defer s.epicWorkerActivationMu.Unlock()
	if e.ID == "" {
		return errors.New("epic id is required")
	}
	if seatCap < 1 {
		seatCap = 1
	}
	ts := now.Format(rfc3339)
	if e.ProjectID == "" {
		e.ProjectID = "default"
	}
	explicitRepositorySet, err := normalizeEpicRepositorySet(&e)
	if err != nil {
		return err
	}
	if e.Slug == "" {
		e.Slug = e.ID // legacy callers use the historical ID-as-slug model.
	}
	// Resolve external material before opening the admission transaction. A stable
	// admission-key replay does not need the source bytes again: the immutable
	// committed worker contract is already authoritative and is validated in-tx.
	dedicatedWorkers := s.EnableEpicDedicatedWorkersV2
	if !dedicatedWorkers {
		var durableErr error
		dedicatedWorkers, durableErr = s.DurableEpicDedicatedWorkersV2(ctx)
		if durableErr != nil {
			return durableErr
		}
	}
	if dedicatedWorkers {
		existingReplay := false
		if e.AdmissionKey != "" {
			var n int
			if err := s.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM epics WHERE project_id=? AND admission_key=?`,
				e.ProjectID, e.AdmissionKey).Scan(&n); err != nil {
				return err
			}
			existingReplay = n > 0
		}
		if !existingReplay {
			material, err := s.resolveEpicWorkerBootstrapMaterials(ctx, e)
			if err != nil {
				return err
			}
			e.WorkerBootstrapMaterials = material
		}
	}
	return s.tx(ctx, func(tx *sql.Tx) error {
		reviewerFamily := ""
		dedicatedWorkers, err := dedicatedEpicWorkersEnabledTx(ctx, s, tx)
		if err != nil {
			return err
		}
		// Stable admission keys make lost acknowledgements safe. A retry with the
		// same contract returns success; a changed contract is an explicit conflict.
		if e.AdmissionKey != "" {
			var existingID, existingHash, existingIntent string
			var existingVersion int
			err := tx.QueryRowContext(ctx, `SELECT id,contract_hash,work_intent_id,intent_version
				FROM epics WHERE project_id = ? AND admission_key = ?`, e.ProjectID, e.AdmissionKey).
				Scan(&existingID, &existingHash, &existingIntent, &existingVersion)
			if err == nil {
				if existingHash != e.ContractHash || e.WorkIntentID != "" &&
					(existingIntent != e.WorkIntentID || existingVersion != e.IntentVersion) {
					return fmt.Errorf("%w: project=%s admission_key=%s", ErrEpicAdmissionConflict, e.ProjectID, e.AdmissionKey)
				}
				existingRepos, existingDelivery, repoErr := epicRepositorySetTx(ctx, tx, existingID)
				if repoErr != nil {
					return repoErr
				}
				if existingDelivery != e.DeliveryRepo || !equalStringSet(existingRepos, e.Repositories) {
					return fmt.Errorf("%w: project=%s admission_key=%s repository set changed",
						ErrEpicAdmissionConflict, e.ProjectID, e.AdmissionKey)
				}
				e.ID = existingID
				return nil
			}
			if !errors.Is(err, sql.ErrNoRows) {
				return err
			}
		}
		// Native Phase-2 admission carries project authority. Prove that the
		// requested delivery repository is an active member of that exact project
		// before creating any workflow state; a repository may belong to several
		// projects, so this is membership—not global ownership inference. Only a
		// v2-off legacy caller may retain the historical unregistered-repo behavior;
		// "default" is a compatibility identity, not an authorization bypass.
		strictMembership := explicitRepositorySet || e.ProjectID != "default"
		if !strictMembership && s.EnableEpicReviewHandoffV2 {
			var activeProjects int
			if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM projects WHERE state!='archived'`).Scan(&activeProjects); err != nil {
				return err
			}
			strictMembership = activeProjects > 1
		}
		if strictMembership {
			for _, repoID := range e.Repositories {
				if err := assertProjectRepoMembershipTx(ctx, tx, e.ProjectID, repoID); err != nil {
					return err
				}
			}
		}
		var active int
		capacityKey, capacityValue := "host", e.Host
		if e.SeatID != "" {
			var canonical Seat
			err := tx.QueryRowContext(ctx, `
				SELECT box, account_key, agent_family, max_concurrent
				  FROM seats
				 WHERE id = ?`, e.SeatID).Scan(
				&canonical.Box, &canonical.AccountKey, &canonical.AgentFamily, &canonical.MaxConcurrent)
			if errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("%w: %q", ErrSeatNotFound, e.SeatID)
			}
			if err != nil {
				return err
			}
			e.Host = canonical.Box
			e.AccountKey = canonical.AccountKey
			e.BuilderModelFamily = canonical.AgentFamily
			seatCap = canonical.MaxConcurrent
			if seatCap < 1 {
				seatCap = 1
			}
			capacityKey, capacityValue = "seat", e.SeatID
		}
		if s.EnableCapacityV2 {
			// Automated work-intent admission intentionally does not choose a
			// builder seat. Its shipped builder lane is Codex; a direct/legacy
			// caller that already selected a seat was canonicalized above. Keep
			// this choice durable so later anti-affinity never has to infer it.
			if e.BuilderModelFamily == "" {
				e.BuilderModelFamily = "codex"
			}
			decision, family, err := distinctReviewerFamilyTx(ctx, tx, e.ProjectID, e.BuilderModelFamily, now, 5*time.Minute)
			if err != nil {
				return err
			}
			if !decision.Routable {
				detail := strings.Join(decision.Reasons, "; ")
				if detail == "" {
					detail = "no distinct review seat passed the v2 capacity gate"
				}
				return fmt.Errorf("%w: builder_family=%s: %s",
					ErrEpicDistinctReviewerUnavailable, e.BuilderModelFamily, detail)
			}
			reviewerFamily = family
		} else if dedicatedWorkers {
			// Tests and explicitly capacity-disabled local development still get a
			// deterministic anti-affine worker plan. Production activation enables
			// capacity v2 and persists the actually proven family above.
			if e.BuilderModelFamily == "" {
				e.BuilderModelFamily = "codex"
			}
			reviewerFamily = "grok"
			if e.BuilderModelFamily == "grok" {
				reviewerFamily = "codex"
			}
		}
		if e.SeatID != "" {
			if err := tx.QueryRowContext(ctx, `
				SELECT COUNT(*)
				  FROM epics
				 WHERE (seat_id = ? OR (seat_id = '' AND host = ?))
				   AND state IN `+epicActiveStatesSQL, e.SeatID, e.Host).Scan(&active); err != nil {
				return err
			}
		} else if e.Host != "" {
			if err := tx.QueryRowContext(ctx,
				`SELECT COUNT(*) FROM epics WHERE host = ? AND state IN `+epicActiveStatesSQL,
				e.Host).Scan(&active); err != nil {
				return err
			}
		}
		if e.SeatID != "" || e.Host != "" {
			if active >= seatCap {
				return fmt.Errorf("%w: %s %q already runs %d active epic(s) (cap %d)",
					ErrEpicHostBusy, capacityKey, capacityValue, active, seatCap)
			}
		}
		type activeScope struct {
			id    string
			scope []string
		}
		for _, repoID := range e.Repositories {
			scopeQuery := `SELECT e.id,e.scope_json FROM epics e
				JOIN epic_repositories er ON er.epic_id=e.id
				WHERE er.repo_id=? AND e.state IN ` + epicActiveStatesSQL
			if s.EnableEpicReviewHandoffV2 {
				// Physical compute is released when the builder is positively parked,
				// but repository/scope affinity survives until merge/abandon cleanup.
				scopeQuery = `SELECT e.id,e.scope_json FROM epics e
					JOIN epic_repositories er ON er.epic_id=e.id
					JOIN epic_deliveries d ON d.epic_id=e.id
					WHERE er.repo_id=? AND d.state NOT IN ('complete','abandoned')`
			}
			rows, err := tx.QueryContext(ctx, scopeQuery, repoID)
			if err != nil {
				return err
			}
			var others []activeScope
			for rows.Next() {
				var id, scopeJSON string
				if err := rows.Scan(&id, &scopeJSON); err != nil {
					rows.Close()
					return err
				}
				others = append(others, activeScope{id: id, scope: unmarshalStrings(scopeJSON)})
			}
			if err := rows.Close(); err != nil {
				return err
			}
			for _, o := range others {
				if overlaps, ga, gb := epicspec.ScopeOverlap(e.Scope, o.scope); overlaps {
					return fmt.Errorf("%w: %q overlaps epic %q's %q in repo %q",
						ErrEpicScopeOverlap, ga, o.id, gb, repoID)
				}
			}
		}
		_, err = tx.ExecContext(ctx, `
			INSERT INTO epics
			    (id, repo, file_path, title, scope_json, host, branch, tmux_name, agent,
			     state, account_key, seat_id, builder_model_family, project_id, slug, admission_key,
			     work_intent_id, intent_version, contract_hash, repository_set_mode,
			     repository_set_finalized, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, 'launching', ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 0, ?, ?)`,
			e.ID, e.Repo, e.FilePath, e.Title, marshalStrings(e.Scope), e.Host, e.Branch,
			e.TmuxName, e.Agent, e.AccountKey, e.SeatID, e.BuilderModelFamily, e.ProjectID, e.Slug,
			e.AdmissionKey, e.WorkIntentID, e.IntentVersion, e.ContractHash, e.RepositorySetMode, ts, ts)
		if err != nil {
			if isUniqueConstraintErr(err) {
				return ErrEpicRunExists
			}
			return fmt.Errorf("add epic %q: %w", e.ID, err)
		}
		for _, repoID := range e.Repositories {
			if repoID == e.DeliveryRepo {
				continue // the migration trigger created the exactly-one delivery row.
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO epic_repositories
				(epic_id,project_id,repo_id,is_delivery,membership_validated,created_at)
				VALUES (?,?,?,0,1,?)`, e.ID, e.ProjectID, repoID, ts); err != nil {
				return fmt.Errorf("add epic repository %q: %w", repoID, err)
			}
		}
		res, err := tx.ExecContext(ctx, `UPDATE epics SET repository_set_finalized=1
			WHERE id=? AND repository_set_finalized=0`, e.ID)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return fmt.Errorf("%w: repository set finalization", ErrEpicRepositorySetInvalid)
		}
		// The review obligation is admission-time durable state, in the same tx.
		_, err = tx.ExecContext(ctx, `INSERT INTO epic_deliveries
			(epic_id, project_id, delivery_repo, branch, state, review_required,
			 builder_model_family, builder_affinity_state, head_sha, base_sha,
			 state_entered_at,state_due_at,fact_progress_at,created_at,updated_at)
			VALUES (?, ?, ?, ?, 'admitted', 1, ?, 'pending', '', '', ?, ?, ?, ?, ?)`,
			e.ID, e.ProjectID, e.Repo, e.Branch, e.BuilderModelFamily, ts,
			now.Add(10*time.Minute).Format(rfc3339), ts, ts, ts)
		if err != nil && !isUniqueConstraintErr(err) {
			return err
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO epic_artifacts
			(epic_id, project_id, repo, branch, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?)`, e.ID, e.ProjectID, e.Repo, e.Branch, ts, ts); err != nil && !isUniqueConstraintErr(err) {
			return err
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO control_events
			(project_id, epic_id, kind, from_state, to_state, state_version, epic_seq, actor_kind, payload_json, created_at)
			VALUES (?, ?, 'epic_admitted', '', 'admitted', 0, 1, 'flowbee', '{}', ?)`, e.ProjectID, e.ID, ts); err != nil {
			return err
		}
		if dedicatedWorkers {
			if err := insertEpicWorkerSessionsTx(ctx, tx, e, reviewerFamily, now); err != nil {
				return err
			}
		}
		if e.WorkIntentID != "" {
			res, err := tx.ExecContext(ctx, `UPDATE work_intents SET state='admitted',
				state_version=state_version+1,admitted_epic_id=?,route_due_at='',hold_kind='',
				hold_reason='',updated_at=? WHERE project_id=? AND id=? AND intent_version=?
				AND submission_idempotency_key=? AND epic_contract_sha256=? AND state='submitting'
				AND (admitted_epic_id IS NULL OR admitted_epic_id='')`, e.ID, ts, e.ProjectID,
				e.WorkIntentID, e.IntentVersion, e.AdmissionKey, e.ContractHash)
			if err != nil {
				return err
			}
			if n, _ := res.RowsAffected(); n != 1 {
				return fmt.Errorf("%w: work intent admission projection", ErrEpicAdmissionConflict)
			}
			res, err = tx.ExecContext(ctx, `UPDATE work_intent_epic_contracts SET state='admitted',
				admitted_epic_id=?,admitted_at=? WHERE project_id=? AND work_intent_id=?
				AND intent_version=? AND submission_key=? AND contract_sha256=? AND state='prepared'`,
				e.ID, ts, e.ProjectID, e.WorkIntentID, e.IntentVersion, e.AdmissionKey, e.ContractHash)
			if err != nil {
				return err
			}
			if n, _ := res.RowsAffected(); n != 1 {
				return fmt.Errorf("%w: prepared contract projection", ErrEpicAdmissionConflict)
			}
			var intentStateVersion int
			if err := tx.QueryRowContext(ctx, `SELECT state_version FROM work_intents WHERE id=?`,
				e.WorkIntentID).Scan(&intentStateVersion); err != nil {
				return err
			}
			payload, _ := json.Marshal(map[string]any{"work_intent_id": e.WorkIntentID,
				"epic_id": e.ID, "intent_version": e.IntentVersion, "contract_sha256": e.ContractHash})
			if err := appendDecisionControlEventTx(ctx, tx, e.ProjectID, "",
				"work_intent_epic_admitted", "submitting", "admitted", intentStateVersion,
				"flowbee", "admission_reconciler", string(payload), now); err != nil {
				return err
			}
		}
		return nil
	})
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
	return s.tx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx,
			`UPDATE epics SET state = 'running', launched_at = ?, updated_at = ? WHERE id = ?`,
			ts, ts, id)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrEpicRunNotFound
		}
		var projectID, state string
		var version int
		if err := tx.QueryRowContext(ctx, `SELECT project_id,state,state_version FROM epic_deliveries WHERE epic_id=?`, id).
			Scan(&projectID, &state, &version); err != nil {
			return err
		}
		if state != "admitted" {
			return nil // an early artifact observation is authoritative; never regress it.
		}
		if _, err := tx.ExecContext(ctx, `UPDATE epic_deliveries SET state='building',
			state_version=state_version+1,builder_affinity_state='active',state_entered_at=?,
			state_due_at=?,fact_progress_at=?,updated_at=? WHERE epic_id=? AND state_version=?`,
			ts, now.Add(2*time.Hour).Format(rfc3339), ts, ts, id, version); err != nil {
			return err
		}
		return appendEpicControlEventTx(ctx, tx, projectID, id, "builder_launched", state,
			"building", version+1, "flowbee", "{}", now)
	})
}

// GetEpicRun returns one epic by id. ErrEpicRunNotFound if absent.
func (s *Store) GetEpicRun(ctx context.Context, id string) (EpicRun, error) {
	return scanEpicRun(s.DB.QueryRowContext(ctx, epicRunSelect+` WHERE id = ?`, id))
}

// GetEpicRunByProjectSlug resolves the human-facing slug without allowing it to
// become durable identity. Phase 2 may reuse the same slug in another project.
func (s *Store) GetEpicRunByProjectSlug(ctx context.Context, projectID, slug string) (EpicRun, error) {
	if projectID == "" {
		projectID = "default"
	}
	return scanEpicRun(s.DB.QueryRowContext(ctx, epicRunSelect+` WHERE project_id = ? AND slug = ?`, projectID, slug))
}

const epicRunSelect = `
	SELECT id, repo, file_path, title, scope_json, host, branch, tmux_name, agent, state,
	       project_id, slug, admission_key, work_intent_id, intent_version, contract_hash,
	       repository_set_mode,
	       COALESCE((SELECT json_group_array(repo_id) FROM
	         (SELECT repo_id FROM epic_repositories er WHERE er.epic_id=epics.id ORDER BY repo_id)),'[]'),
	       COALESCE((SELECT repo_id FROM epic_repositories er
	                  WHERE er.epic_id=epics.id AND er.is_delivery=1),repo),
	       COALESCE((SELECT state FROM epic_deliveries d WHERE d.epic_id=epics.id),''),
	       COALESCE((SELECT ci_state FROM epic_deliveries d WHERE d.epic_id=epics.id),''),
	       COALESCE((SELECT review_job_id FROM epic_deliveries d WHERE d.epic_id=epics.id),''),
	       status_updated_at, status_current_step, status_steps_total, status_state_detail,
	       status_checklist_json, status_blockers,
	       account_key, seat_id, builder_model_family, context_pct, pane_state, auth_state,
	       last_commit_at, explainer_path,
	       created_at, launched_at, finished_at, updated_at
	  FROM epics`

func scanEpicRun(row rowScanner) (EpicRun, error) {
	var e EpicRun
	var scopeJSON, checklistJSON, repositoriesJSON string
	err := row.Scan(&e.ID, &e.Repo, &e.FilePath, &e.Title, &scopeJSON, &e.Host, &e.Branch,
		&e.TmuxName, &e.Agent, &e.State,
		&e.ProjectID, &e.Slug, &e.AdmissionKey, &e.WorkIntentID, &e.IntentVersion, &e.ContractHash,
		&e.RepositorySetMode, &repositoriesJSON, &e.DeliveryRepo,
		&e.DeliveryState, &e.DeliveryCIState, &e.ReviewJobID,
		&e.StatusUpdatedAt, &e.StatusCurrentStep, &e.StatusStepsTotal, &e.StatusStateDetail,
		&checklistJSON, &e.StatusBlockers,
		&e.AccountKey, &e.SeatID, &e.BuilderModelFamily, &e.ContextPct, &e.PaneState, &e.AuthState,
		&e.LastCommitAt, &e.ExplainerPath,
		&e.CreatedAt, &e.LaunchedAt, &e.FinishedAt, &e.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return EpicRun{}, ErrEpicRunNotFound
	}
	if err != nil {
		return EpicRun{}, err
	}
	e.Scope = unmarshalStrings(scopeJSON)
	e.Repositories = unmarshalStrings(repositoriesJSON)
	if e.DeliveryRepo == "" && e.Repo != "" {
		e.DeliveryRepo = e.Repo
	}
	e.StatusChecklist = unmarshalChecklist(checklistJSON)
	return e, nil
}

// ListEpicRuns returns every registered epic ordered by id (`flowbee epic status`,
// full history including terminal states).
func (s *Store) ListEpicRuns(ctx context.Context) ([]EpicRun, error) {
	return queryEpicRuns(ctx, s.DB, epicRunSelect+` ORDER BY id`)
}

// ListEpicRunsForProject is the Phase-2 project workspace projection. Project
// ownership is matched from durable project_id; repository names and branch
// prefixes are never used as an authorization or namespace proxy.
func (s *Store) ListEpicRunsForProject(ctx context.Context, projectID string) ([]EpicRun, error) {
	if strings.TrimSpace(projectID) == "" {
		return nil, ErrProjectNotFound
	}
	return queryEpicRuns(ctx, s.DB, epicRunSelect+` WHERE project_id=? ORDER BY id`, projectID)
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
	out := make([]EpicRun, 0)
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

		// an EMPTY parse (missing/garbage ## Status — sb.IsEmpty) must not clobber
		// the last-good status_* fields with zero values (review m4; the 0026
		// migration comment promises "these columns simply keep their prior values
		// when a pass can't parse"). The lifecycle state machine above still ran —
		// an empty raw State is a no-transition, but the linked session's
		// independently-observed 'achieved' can still legitimately fire — so only
		// the status_* column writes are conditional, never the state advance.
		statusCols, statusArgs := "", []any{}
		if !sb.IsEmpty() {
			statusCols = `, status_updated_at = ?, status_current_step = ?,
			    status_steps_total = ?, status_state_detail = ?, status_checklist_json = ?,
			    status_blockers = ?`
			statusArgs = []any{sb.UpdatedRaw, sb.CurrentStep, sb.StepsTotal, sb.State,
				marshalChecklist(sb.Checklist), sb.Blockers}
		}
		projectedEpicState := newState
		if s.EnableEpicReviewHandoffV2 && becameTerminal {
			// The builder claim is not physical absence. Keep the legacy row active
			// until the durable Driver Stop receipt is projected by I.5.
			projectedEpicState = curState
		}
		finishedCol := ""
		if becameTerminal && !s.EnableEpicReviewHandoffV2 {
			finishedCol = `, finished_at = ?`
		}
		args := append([]any{ts}, statusArgs...)
		args = append(args, projectedEpicState)
		if becameTerminal && !s.EnableEpicReviewHandoffV2 {
			args = append(args, ts)
		}
		args = append(args, id)
		_, err = tx.ExecContext(ctx,
			`UPDATE epics SET updated_at = ?`+statusCols+`, state = ?`+finishedCol+` WHERE id = ?`,
			args...)
		if err != nil {
			return err
		}
		var projectID, deliveryState string
		var stateVersion int
		if err := tx.QueryRowContext(ctx, `SELECT project_id,state,state_version FROM epic_deliveries WHERE epic_id=?`, id).
			Scan(&projectID, &deliveryState, &stateVersion); err != nil {
			return err
		}
		if becameTerminal && (deliveryState == "admitted" || deliveryState == "building") {
			affinity := "parked"
			if s.EnableEpicReviewHandoffV2 {
				if err := ensureBuilderParkActionTx(ctx, tx, projectID, id, newState, now); err != nil {
					return err
				}
				affinity = "active"
			}
			if _, err := tx.ExecContext(ctx, `UPDATE epic_deliveries SET state='awaiting_artifact',
				state_version=state_version+1,builder_affinity_state=?,state_entered_at=?,
				state_due_at=?,fact_progress_at=?,updated_at=? WHERE epic_id=? AND state_version=?`,
				affinity, ts, now.Add(10*time.Minute).Format(rfc3339), ts, ts, id, stateVersion); err != nil {
				return err
			}
			return appendEpicControlEventTx(ctx, tx, projectID, id, "builder_completed", deliveryState,
				"awaiting_artifact", stateVersion+1, "artifact_ingest", "{}", now)
		}
		// Status progress is not authority for GitHub/CI, but it is an observed
		// builder fact and keeps the building-state progress clock honest.
		if !sb.IsEmpty() && deliveryState == "building" {
			_, err = tx.ExecContext(ctx, `UPDATE epic_deliveries SET fact_progress_at=?,updated_at=? WHERE epic_id=?`, ts, ts, id)
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
//
// EVERY match is EXACT (whole token, after trim+casefold) — never a substring
// Contains. The lesson (review MAJOR M3): "done" is the one terminal,
// irreversible, reservation-RELEASING transition, and a Contains("done") match
// fired on "abandoned" (and would on "undone"): a RUNNING epic flipped terminal,
// finished_at got set, the row dropped out of epicActiveStatesSQL, and its
// scope+host reservation was released while the agent was still mutating the tree
// — exactly the multi-day merge collision the reservation exists to prevent. The
// non-terminal matches are exact too: they cost nothing extra, and an agent that
// invents vocabulary outside the documented set should read as "no transition",
// not as whichever documented word its invention happens to contain.
func nextEpicState(cur, raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "done":
		return "done"
	case "blocked":
		return "blocked"
	case "building", "pursuing", "working", "running":
		return "running"
	default: // "", "pending", "abandoned", "undone", any invention: no transition
		return cur
	}
}

// AbandonEpicRun marks an epic abandoned and releases both reservations it was
// holding: the scope/host occupancy (immediate — ListActiveEpicRuns excludes
// 'abandoned') and the linked goal_sessions watch (disabled in the SAME tx, direct
// SQL rather than SetGoalSessionEnabled, so the two writes commit atomically). Per
// this store method does not perform process I/O itself: its transition caller MUST first
// confirm the row's exact registered tmux session is stopped. Keeping that ordering at
// the command boundary prevents releasing these reservations around a live agent.
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
		var projectID, deliveryState string
		var stateVersion int
		if err := tx.QueryRowContext(ctx, `SELECT project_id,state,state_version FROM epic_deliveries WHERE epic_id=?`, id).
			Scan(&projectID, &deliveryState, &stateVersion); err != nil {
			return err
		}
		if deliveryState != "complete" && deliveryState != "abandoned" {
			if _, err := tx.ExecContext(ctx, `UPDATE epic_deliveries SET state='abandoned',
				state_version=state_version+1,review_required=0,builder_affinity_state='abandoned',
				reviewer_identity='',reviewer_model_family='',hold_kind='',hold_reason='',
				state_entered_at=?,state_due_at='',fact_progress_at=?,updated_at=?
				WHERE epic_id=? AND state_version=?`, ts, ts, ts, id, stateVersion); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `UPDATE epic_actions SET state='cancelled_superseded',
				last_error='epic_abandoned',claim_owner='',claim_deadline_at='',updated_at=?
				WHERE epic_id=? AND state IN ('pending','delivering','verifying')`, ts, id); err != nil {
				return err
			}
			if err := appendEpicControlEventTx(ctx, tx, projectID, id, "epic_abandoned", deliveryState,
				"abandoned", stateVersion+1, "human", "{}", now); err != nil {
				return err
			}
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
