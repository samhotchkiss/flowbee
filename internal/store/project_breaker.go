package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/samhotchkiss/flowbee/internal/lease"
)

var (
	ErrProjectBreakerNotFound = errors.New("project circuit breaker not found")
	ErrProjectBreakerInput    = errors.New("invalid project circuit breaker input")
)

type ProjectBreaker struct {
	ProjectID            string
	RepoID               string
	State                string
	StateVersion         int
	FailureKind          string
	Reason               string
	FailureCount         int
	OpenedAt             time.Time
	ProbeDueAt           time.Time
	ProbeOwner           string
	ProbeEpoch           int
	ProbeLeaseExpiresAt  time.Time
	LastRecoveryFact     string
	CreatedAt, UpdatedAt time.Time
}

type ProjectBreakerFailure struct {
	ProjectID   string
	RepoID      string
	Kind        string
	Reason      string
	RetryAfter  time.Duration
	EvidenceRef string
}

type ProjectBreakerProbe struct {
	ProjectID      string
	RepoID         string
	Owner          string
	Epoch          int
	StateVersion   int
	LeaseExpiresAt time.Time
}

type ProjectBreakerRecoveryFact struct {
	Kind        string
	EvidenceRef string
	ObservedAt  time.Time
}

type ProjectBreakerDecision struct {
	Allowed      bool
	BlockedScope string
	BlockedID    string
	Reason       string
	StateVersion int
}

type ProjectBreakerOverride struct {
	ProjectID       string
	RepoID          string
	Action          string // open | probe_now; closing always requires a recovery fact.
	ExpectedVersion int
	ActorID         string
	IdempotencyKey  string
	Reason          string
	FailureKind     string
	ProbeAfter      time.Duration
}

type ProjectBreakerEvent struct {
	Seq, StateVersion, ProbeEpoch int
	ProjectID, RepoID             string
	Kind, FromState, ToState      string
	ActorKind, ActorID            string
	Reason, EvidenceRef           string
	CreatedAt                     time.Time
}

var projectBreakerFailureKinds = map[string]bool{
	"ci_outage": true, "github_error": true, "merge_incident": true,
	"action_failure": true, "policy_violation": true,
}

func validateProjectBreakerScopeTx(ctx context.Context, tx *sql.Tx, projectID, repoID string) error {
	if !projectIDPattern.MatchString(projectID) {
		return ErrProjectBreakerInput
	}
	var exists int
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM projects WHERE id=?)`, projectID).Scan(&exists); err != nil {
		return err
	}
	if exists != 1 {
		return ErrProjectNotFound
	}
	if repoID == "" {
		return nil
	}
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM project_repos WHERE project_id=? AND repo_id=?)`, projectID, repoID).Scan(&exists); err != nil {
		return err
	}
	if exists != 1 {
		return ErrProjectBreakerInput
	}
	return nil
}

// RecordProjectBreakerFailure opens (or refreshes) exactly one project/repo
// breaker. It never mutates another scope, and every write is ledgered in the
// same transaction.
func (s *Store) RecordProjectBreakerFailure(ctx context.Context, in ProjectBreakerFailure, now time.Time) (ProjectBreaker, error) {
	in.ProjectID, in.RepoID = strings.TrimSpace(in.ProjectID), strings.TrimSpace(in.RepoID)
	in.Kind, in.Reason, in.EvidenceRef = strings.TrimSpace(in.Kind), strings.TrimSpace(in.Reason), strings.TrimSpace(in.EvidenceRef)
	if !projectBreakerFailureKinds[in.Kind] || in.Reason == "" || in.RetryAfter <= 0 {
		return ProjectBreaker{}, ErrProjectBreakerInput
	}
	now, due := now.UTC(), now.UTC().Add(in.RetryAfter)
	err := s.tx(ctx, func(tx *sql.Tx) error {
		if err := validateProjectBreakerScopeTx(ctx, tx, in.ProjectID, in.RepoID); err != nil {
			return err
		}
		prior, err := getProjectBreakerTx(ctx, tx, in.ProjectID, in.RepoID)
		if err != nil && !errors.Is(err, ErrProjectBreakerNotFound) {
			return err
		}
		from, version, failures, opened, created := "closed", 1, 1, now, now
		if err == nil {
			from, version, failures, created = prior.State, prior.StateVersion+1, prior.FailureCount+1, prior.CreatedAt
			if !prior.OpenedAt.IsZero() && prior.State != "closed" {
				opened = prior.OpenedAt
			}
		}
		stamp := now.Format(rfc3339)
		_, err = tx.ExecContext(ctx, `INSERT INTO project_circuit_breakers
			(project_id,repo_id,state,state_version,failure_kind,reason,failure_count,opened_at,probe_due_at,
			 probe_owner,probe_epoch,probe_lease_expires_at,last_recovery_fact,created_at,updated_at)
			VALUES (?,?,'open',?,?,?,?,?,?,'',0,'','',?,?)
			ON CONFLICT(project_id,repo_id) DO UPDATE SET state='open',state_version=excluded.state_version,
			 failure_kind=excluded.failure_kind,reason=excluded.reason,failure_count=excluded.failure_count,
			 opened_at=excluded.opened_at,probe_due_at=excluded.probe_due_at,probe_owner='',
			 probe_lease_expires_at='',last_recovery_fact='',updated_at=excluded.updated_at`,
			in.ProjectID, in.RepoID, version, in.Kind, in.Reason, failures, opened.Format(rfc3339),
			due.Format(rfc3339), created.Format(rfc3339), stamp)
		if err != nil {
			return err
		}
		return appendProjectBreakerEventTx(ctx, tx, in.ProjectID, in.RepoID, "failure_recorded", from, "open",
			version, 0, "reconciler", "flowbee", in.Reason, in.EvidenceRef, now)
	})
	if err != nil {
		return ProjectBreaker{}, err
	}
	return s.GetProjectBreaker(ctx, in.ProjectID, in.RepoID)
}

// ReconcileDueProjectBreakerProbes claims a bounded set of due probes. Expired
// half-open claims are reclaimed with a new epoch, which makes process restart
// recovery deterministic and fences the dead claimant.
func (s *Store) ReconcileDueProjectBreakerProbes(ctx context.Context, owner string, now time.Time, ttl time.Duration, budget int) ([]ProjectBreakerProbe, error) {
	owner = strings.TrimSpace(owner)
	if owner == "" || ttl <= 0 || budget < 1 || budget > 1000 {
		return nil, ErrProjectBreakerInput
	}
	now = now.UTC()
	var claimed []ProjectBreakerProbe
	err := s.tx(ctx, func(tx *sql.Tx) error {
		stamp := now.Format(rfc3339)
		rows, err := tx.QueryContext(ctx, `SELECT project_id,repo_id,state
			FROM project_circuit_breakers
			WHERE (state='open' AND probe_due_at<>'' AND julianday(probe_due_at)<=julianday(?))
			   OR (state='half_open' AND probe_lease_expires_at<>'' AND julianday(probe_lease_expires_at)<=julianday(?))
			ORDER BY CASE state WHEN 'half_open' THEN 0 ELSE 1 END,
			         CASE state WHEN 'half_open' THEN probe_lease_expires_at ELSE probe_due_at END,
			         project_id,repo_id LIMIT ?`, stamp, stamp, budget)
		if err != nil {
			return err
		}
		type candidate struct{ project, repo, state string }
		var candidates []candidate
		for rows.Next() {
			var c candidate
			if err := rows.Scan(&c.project, &c.repo, &c.state); err != nil {
				rows.Close()
				return err
			}
			candidates = append(candidates, c)
		}
		if err := rows.Close(); err != nil {
			return err
		}
		for _, c := range candidates {
			var version, epoch int
			if err := tx.QueryRowContext(ctx, `SELECT state_version,probe_epoch FROM project_circuit_breakers
				WHERE project_id=? AND repo_id=?`, c.project, c.repo).Scan(&version, &epoch); err != nil {
				return err
			}
			version, epoch = version+1, epoch+1
			expires := now.Add(ttl)
			res, err := tx.ExecContext(ctx, `UPDATE project_circuit_breakers SET state='half_open',state_version=?,
				probe_owner=?,probe_epoch=?,probe_lease_expires_at=?,updated_at=?
				WHERE project_id=? AND repo_id=? AND state=?`, version, owner, epoch, expires.Format(rfc3339),
				now.Format(rfc3339), c.project, c.repo, c.state)
			if err != nil {
				return err
			}
			if n, _ := res.RowsAffected(); n != 1 {
				return lease.ErrStaleEpoch
			}
			if err := appendProjectBreakerEventTx(ctx, tx, c.project, c.repo, "probe_claimed", c.state, "half_open",
				version, epoch, "reconciler", owner, "", "", now); err != nil {
				return err
			}
			claimed = append(claimed, ProjectBreakerProbe{ProjectID: c.project, RepoID: c.repo, Owner: owner,
				Epoch: epoch, StateVersion: version, LeaseExpiresAt: expires})
		}
		return nil
	})
	return claimed, err
}

// CompleteProjectBreakerProbe is epoch/owner/lease fenced. A successful probe
// may close a breaker only with a durable, mechanical recovery fact; a human or
// send receipt alone is not sufficient recovery evidence.
func (s *Store) CompleteProjectBreakerProbe(ctx context.Context, probe ProjectBreakerProbe, recovered bool,
	fact ProjectBreakerRecoveryFact, failureReason string, retryAfter time.Duration, now time.Time) (ProjectBreaker, error) {
	now = now.UTC()
	if probe.ProjectID == "" || probe.Owner == "" || probe.Epoch < 1 {
		return ProjectBreaker{}, ErrProjectBreakerInput
	}
	if recovered {
		fact.Kind, fact.EvidenceRef = strings.TrimSpace(fact.Kind), strings.TrimSpace(fact.EvidenceRef)
		if fact.Kind == "" || fact.EvidenceRef == "" || fact.ObservedAt.IsZero() || fact.ObservedAt.After(now) {
			return ProjectBreaker{}, ErrProjectBreakerInput
		}
	} else if strings.TrimSpace(failureReason) == "" || retryAfter <= 0 {
		return ProjectBreaker{}, ErrProjectBreakerInput
	}
	err := s.tx(ctx, func(tx *sql.Tx) error {
		current, err := getProjectBreakerTx(ctx, tx, probe.ProjectID, probe.RepoID)
		if err != nil {
			return err
		}
		if current.State != "half_open" || current.ProbeOwner != probe.Owner || current.ProbeEpoch != probe.Epoch ||
			current.ProbeLeaseExpiresAt.IsZero() || !now.Before(current.ProbeLeaseExpiresAt) {
			return lease.ErrStaleEpoch
		}
		version := current.StateVersion + 1
		if recovered {
			res, err := tx.ExecContext(ctx, `UPDATE project_circuit_breakers SET state='closed',state_version=?,
				failure_kind='',reason='',failure_count=0,opened_at='',probe_due_at='',probe_owner='',
				probe_lease_expires_at='',last_recovery_fact=?,updated_at=?
				WHERE project_id=? AND repo_id=? AND state='half_open' AND probe_owner=? AND probe_epoch=?`,
				version, fact.EvidenceRef, now.Format(rfc3339), probe.ProjectID, probe.RepoID, probe.Owner, probe.Epoch)
			if err != nil {
				return err
			}
			if n, _ := res.RowsAffected(); n != 1 {
				return lease.ErrStaleEpoch
			}
			return appendProjectBreakerEventTx(ctx, tx, probe.ProjectID, probe.RepoID, "probe_recovered", "half_open", "closed",
				version, probe.Epoch, "reconciler", probe.Owner, fact.Kind, fact.EvidenceRef, now)
		}
		res, err := tx.ExecContext(ctx, `UPDATE project_circuit_breakers SET state='open',state_version=?,reason=?,
			failure_count=failure_count+1,probe_due_at=?,probe_owner='',probe_lease_expires_at='',updated_at=?
			WHERE project_id=? AND repo_id=? AND state='half_open' AND probe_owner=? AND probe_epoch=?`,
			version, strings.TrimSpace(failureReason), now.Add(retryAfter).Format(rfc3339), now.Format(rfc3339),
			probe.ProjectID, probe.RepoID, probe.Owner, probe.Epoch)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return lease.ErrStaleEpoch
		}
		return appendProjectBreakerEventTx(ctx, tx, probe.ProjectID, probe.RepoID, "probe_failed", "half_open", "open",
			version, probe.Epoch, "reconciler", probe.Owner, failureReason, "", now)
	})
	if err != nil {
		return ProjectBreaker{}, err
	}
	return s.GetProjectBreaker(ctx, probe.ProjectID, probe.RepoID)
}

// OverrideProjectBreaker is an optimistic-version-fenced operator control.
// It can open a hold or accelerate its next probe, but it cannot force-close:
// closing requires CompleteProjectBreakerProbe with a recovery fact.
func (s *Store) OverrideProjectBreaker(ctx context.Context, in ProjectBreakerOverride, now time.Time) (ProjectBreaker, error) {
	in.ProjectID, in.RepoID, in.ActorID, in.Reason = strings.TrimSpace(in.ProjectID), strings.TrimSpace(in.RepoID), strings.TrimSpace(in.ActorID), strings.TrimSpace(in.Reason)
	in.IdempotencyKey = strings.TrimSpace(in.IdempotencyKey)
	if in.ExpectedVersion < 0 || in.ActorID == "" || in.Reason == "" || (in.Action != "open" && in.Action != "probe_now") {
		return ProjectBreaker{}, ErrProjectBreakerInput
	}
	if in.Action == "open" && in.FailureKind == "" {
		in.FailureKind = "policy_violation"
	}
	if (in.Action == "open" && !projectBreakerFailureKinds[in.FailureKind]) ||
		(in.Action == "probe_now" && in.FailureKind != "") || in.ProbeAfter < 0 {
		return ProjectBreaker{}, ErrProjectBreakerInput
	}
	var commandHash string
	if in.IdempotencyKey != "" {
		var err error
		commandHash, err = projectCommandPayloadHash(struct {
			ProjectID, RepoID, Action, ActorID, Reason, FailureKind string
			ExpectedVersion                                         int
			ProbeAfterNanos                                         int64
		}{in.ProjectID, in.RepoID, in.Action, in.ActorID, in.Reason, in.FailureKind,
			in.ExpectedVersion, int64(in.ProbeAfter)})
		if err != nil {
			return ProjectBreaker{}, err
		}
	}
	now = now.UTC()
	err := s.tx(ctx, func(tx *sql.Tx) error {
		if err := validateProjectBreakerScopeTx(ctx, tx, in.ProjectID, in.RepoID); err != nil {
			return err
		}
		if in.IdempotencyKey != "" {
			replay, err := ensureProjectBreakerCommandTx(ctx, tx, in, commandHash, now.Format(rfc3339))
			if err != nil || replay {
				return err
			}
		}
		current, err := getProjectBreakerTx(ctx, tx, in.ProjectID, in.RepoID)
		if errors.Is(err, ErrProjectBreakerNotFound) {
			if in.Action != "open" || in.ExpectedVersion != 0 {
				return lease.ErrStaleEpoch
			}
			stamp := now.Format(rfc3339)
			_, err = tx.ExecContext(ctx, `INSERT INTO project_circuit_breakers
				(project_id,repo_id,state,state_version,failure_kind,reason,failure_count,opened_at,probe_due_at,
				 probe_owner,probe_epoch,probe_lease_expires_at,last_recovery_fact,created_at,updated_at)
				VALUES (?,?,'open',1,?,?,0,?,?,'',0,'','',?,?)`, in.ProjectID, in.RepoID, in.FailureKind,
				in.Reason, stamp, now.Add(in.ProbeAfter).Format(rfc3339), stamp, stamp)
			if err != nil {
				return err
			}
			return appendProjectBreakerEventTx(ctx, tx, in.ProjectID, in.RepoID, "operator_opened", "closed", "open",
				1, 0, "human", in.ActorID, in.Reason, "", now)
		}
		if err != nil {
			return err
		}
		if current.StateVersion != in.ExpectedVersion || current.State == "half_open" {
			return lease.ErrStaleEpoch
		}
		if in.Action == "probe_now" && current.State != "open" {
			return ErrProjectBreakerInput
		}
		version, kind, failureKind := current.StateVersion+1, "operator_opened", in.FailureKind
		if in.Action == "probe_now" {
			kind = "operator_probe_requested"
			failureKind = current.FailureKind
		}
		res, err := tx.ExecContext(ctx, `UPDATE project_circuit_breakers SET state='open',state_version=?,
			failure_kind=?,reason=?,opened_at=CASE WHEN opened_at='' THEN ? ELSE opened_at END,
			probe_due_at=?,probe_owner='',probe_lease_expires_at='',last_recovery_fact='',updated_at=?
			WHERE project_id=? AND repo_id=? AND state_version=?`, version, failureKind, in.Reason,
			now.Format(rfc3339), now.Add(in.ProbeAfter).Format(rfc3339), now.Format(rfc3339),
			in.ProjectID, in.RepoID, in.ExpectedVersion)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return lease.ErrStaleEpoch
		}
		return appendProjectBreakerEventTx(ctx, tx, in.ProjectID, in.RepoID, kind, current.State, "open",
			version, current.ProbeEpoch, "human", in.ActorID, in.Reason, "", now)
	})
	if err != nil {
		return ProjectBreaker{}, err
	}
	return s.GetProjectBreaker(ctx, in.ProjectID, in.RepoID)
}

func ensureProjectBreakerCommandTx(ctx context.Context, tx *sql.Tx, in ProjectBreakerOverride, payloadHash, stamp string) (bool, error) {
	var action, gotHash, repoID string
	err := tx.QueryRowContext(ctx, `SELECT action,payload_sha256,repo_id
		FROM project_circuit_breaker_commands WHERE project_id=? AND idempotency_key=?`,
		in.ProjectID, in.IdempotencyKey).Scan(&action, &gotHash, &repoID)
	if err == nil {
		if action != in.Action || gotHash != payloadHash || repoID != in.RepoID {
			return false, ErrProjectCommandConflict
		}
		return true, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return false, err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO project_circuit_breaker_commands
		(project_id,idempotency_key,action,payload_sha256,repo_id,created_at) VALUES (?,?,?,?,?,?)`,
		in.ProjectID, in.IdempotencyKey, in.Action, payloadHash, in.RepoID, stamp)
	return false, err
}

func (s *Store) GetProjectBreaker(ctx context.Context, projectID, repoID string) (ProjectBreaker, error) {
	return scanProjectBreaker(s.DB.QueryRowContext(ctx, projectBreakerSelect+` WHERE project_id=? AND repo_id=?`, projectID, repoID))
}

// ListProjectBreakers returns every project- and repository-scoped breaker for
// one exact project. It never widens a project-scoped API read into a portfolio
// read; callers must request each authorized project separately.
func (s *Store) ListProjectBreakers(ctx context.Context, projectID string) ([]ProjectBreaker, error) {
	projectID = strings.TrimSpace(projectID)
	if !projectIDPattern.MatchString(projectID) {
		return nil, ErrProjectBreakerInput
	}
	rows, err := s.DB.QueryContext(ctx, projectBreakerSelect+`
		WHERE project_id=? ORDER BY CASE WHEN repo_id='' THEN 0 ELSE 1 END, repo_id`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ProjectBreaker
	for rows.Next() {
		item, err := scanProjectBreaker(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

func getProjectBreakerTx(ctx context.Context, tx *sql.Tx, projectID, repoID string) (ProjectBreaker, error) {
	return scanProjectBreaker(tx.QueryRowContext(ctx, projectBreakerSelect+` WHERE project_id=? AND repo_id=?`, projectID, repoID))
}

const projectBreakerSelect = `SELECT project_id,repo_id,state,state_version,failure_kind,reason,failure_count,
	opened_at,probe_due_at,probe_owner,probe_epoch,probe_lease_expires_at,last_recovery_fact,created_at,updated_at
	FROM project_circuit_breakers`

type projectBreakerScanner interface{ Scan(...any) error }

func scanProjectBreaker(row projectBreakerScanner) (ProjectBreaker, error) {
	var out ProjectBreaker
	var opened, due, expires, created, updated string
	err := row.Scan(&out.ProjectID, &out.RepoID, &out.State, &out.StateVersion, &out.FailureKind, &out.Reason,
		&out.FailureCount, &opened, &due, &out.ProbeOwner, &out.ProbeEpoch, &expires, &out.LastRecoveryFact, &created, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return out, ErrProjectBreakerNotFound
	}
	out.OpenedAt, out.ProbeDueAt, out.ProbeLeaseExpiresAt = parseOptionalTime(opened), parseOptionalTime(due), parseOptionalTime(expires)
	out.CreatedAt, out.UpdatedAt = parseOptionalTime(created), parseOptionalTime(updated)
	return out, err
}

// ProjectBreakerDisposition folds project and repository scopes without any
// global state. Absence means closed. Project scope takes precedence; otherwise
// only the selected repository can be held.
func (s *Store) ProjectBreakerDisposition(ctx context.Context, projectID, repoID string) (ProjectBreakerDecision, error) {
	for _, scope := range []struct{ repo, label string }{{"", "project"}, {repoID, "repository"}} {
		if scope.label == "repository" && scope.repo == "" {
			continue
		}
		breaker, err := s.GetProjectBreaker(ctx, projectID, scope.repo)
		if errors.Is(err, ErrProjectBreakerNotFound) {
			continue
		}
		if err != nil {
			return ProjectBreakerDecision{}, err
		}
		if breaker.State != "closed" {
			id := projectID
			if scope.repo != "" {
				id = scope.repo
			}
			return ProjectBreakerDecision{Allowed: false, BlockedScope: scope.label, BlockedID: id,
				Reason: breaker.Reason, StateVersion: breaker.StateVersion}, nil
		}
	}
	return ProjectBreakerDecision{Allowed: true}, nil
}

// ProjectBreakerBlockedJobIDs is the shared-scheduler projection used at the
// one global lease choke point. It covers every role candidate source: a
// project breaker holds all of the project's jobs, while a repository breaker
// holds only jobs bound to that repository. Absence/closed is runnable.
func (s *Store) ProjectBreakerBlockedJobIDs(ctx context.Context) (map[string]ProjectBreakerDecision, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT j.id,b.project_id,b.repo_id,b.reason,b.state_version
		FROM jobs j JOIN project_circuit_breakers b ON b.project_id=j.project_id
		 AND b.state<>'closed' AND (b.repo_id='' OR b.repo_id=j.repo)
		ORDER BY j.id,CASE WHEN b.repo_id='' THEN 0 ELSE 1 END`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]ProjectBreakerDecision{}
	for rows.Next() {
		var jobID, projectID, repoID, reason string
		var version int
		if err := rows.Scan(&jobID, &projectID, &repoID, &reason, &version); err != nil {
			return nil, err
		}
		// Project scope wins because rows are ordered project-before-repository.
		if _, exists := out[jobID]; exists {
			continue
		}
		scope, id := "project", projectID
		if repoID != "" {
			scope, id = "repository", repoID
		}
		out[jobID] = ProjectBreakerDecision{Allowed: false, BlockedScope: scope,
			BlockedID: id, Reason: reason, StateVersion: version}
	}
	return out, rows.Err()
}

func (s *Store) ListProjectBreakerEvents(ctx context.Context, projectID, repoID string) ([]ProjectBreakerEvent, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT seq,project_id,repo_id,kind,from_state,to_state,state_version,
		probe_epoch,actor_kind,actor_id,reason,evidence_ref,created_at FROM project_circuit_breaker_events
		WHERE project_id=? AND repo_id=? ORDER BY seq`, projectID, repoID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ProjectBreakerEvent
	for rows.Next() {
		var item ProjectBreakerEvent
		var created string
		if err := rows.Scan(&item.Seq, &item.ProjectID, &item.RepoID, &item.Kind, &item.FromState, &item.ToState,
			&item.StateVersion, &item.ProbeEpoch, &item.ActorKind, &item.ActorID, &item.Reason, &item.EvidenceRef, &created); err != nil {
			return nil, err
		}
		item.CreatedAt = parseOptionalTime(created)
		out = append(out, item)
	}
	return out, rows.Err()
}

func appendProjectBreakerEventTx(ctx context.Context, tx *sql.Tx, projectID, repoID, kind, from, to string,
	version, epoch int, actorKind, actorID, reason, evidence string, now time.Time) error {
	stamp := now.UTC().Format(rfc3339)
	if _, err := tx.ExecContext(ctx, `INSERT INTO project_circuit_breaker_events
		(project_id,repo_id,kind,from_state,to_state,state_version,probe_epoch,actor_kind,actor_id,reason,evidence_ref,created_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`, projectID, repoID, kind, from, to, version, epoch, actorKind, actorID, reason, evidence, stamp); err != nil {
		return err
	}
	payload, _ := json.Marshal(map[string]any{"project_id": projectID, "repo_id": repoID, "reason": reason,
		"evidence_ref": evidence, "probe_epoch": epoch})
	if _, err := tx.ExecContext(ctx, `INSERT INTO control_events
		(project_id,epic_id,kind,from_state,to_state,state_version,epic_seq,actor_kind,actor_id,payload_json,created_at)
		VALUES (?,'',?,?,?,?,0,?,?,?,?)`, projectID, "project_breaker_"+kind, from, to, version, actorKind, actorID, string(payload), stamp); err != nil {
		return err
	}
	dedup := "project_breaker:" + projectID + ":" + repoID
	if to == "closed" {
		_, err := tx.ExecContext(ctx, `UPDATE attention_items SET state='resolved',resolution='recovery_fact_observed',
			resolved_at=?,updated_at=? WHERE dedup_key=? AND state IN ('open','leased','delivering','awaiting_ack')`, stamp, stamp, dedup)
		return err
	}
	detail := fmt.Sprintf("%s breaker %s", map[bool]string{true: "repository", false: "project"}[repoID != ""], to)
	attentionID := "project-breaker-" + stableID(dedup+"\x00"+stamp)
	inserted, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO attention_items
		(id,kind,epic_id,repo,priority,state,dedup_key,blocking,evidence_json,detail,occurrences,first_seen_at,last_seen_at,created_at,updated_at,project_id)
		VALUES (?,'project_breaker_open','',?,10,'open',?,1,?,?,1,?,?,?,?,?)`, attentionID, repoID, dedup,
		string(payload), detail+": "+reason, stamp, stamp, stamp, stamp, projectID)
	if err != nil {
		return err
	}
	n, _ := inserted.RowsAffected()
	delta := 1
	if n == 1 {
		delta = 0
	}
	_, err = tx.ExecContext(ctx, `UPDATE attention_items SET occurrences=occurrences+?,last_seen_at=?,
		evidence_json=?,detail=?,updated_at=? WHERE dedup_key=? AND state IN ('open','leased','delivering','awaiting_ack')`,
		delta, stamp, string(payload), detail+": "+reason, stamp, dedup)
	return err
}
