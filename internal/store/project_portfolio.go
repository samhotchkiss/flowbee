package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

var (
	ErrProjectNotFound        = errors.New("project not found")
	ErrProjectConflict        = errors.New("project identity conflict")
	ErrProjectCommandConflict = errors.New("project idempotency key is bound to another command")
)

var projectIDPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

type PortfolioProject struct {
	ID              string    `json:"id"`
	Name            string    `json:"name"`
	State           string    `json:"state"`
	PauseReason     string    `json:"pause_reason,omitempty"`
	StateVersion    int       `json:"state_version"`
	Priority        int       `json:"priority"`
	SchedulerWeight int       `json:"scheduler_weight"`
	ConcurrencyCap  int       `json:"concurrency_cap"`
	ArchivedAt      time.Time `json:"archived_at,omitempty"`
	CreatedAt       time.Time `json:"created_at"`
}

type ProjectActorRoute struct {
	ProjectID    string    `json:"project_id"`
	Role         string    `json:"role"`
	ActorID      string    `json:"actor_id"`
	State        string    `json:"state"`
	StateVersion int       `json:"state_version"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

const projectCreateCommandScope = "portfolio"

func normalizePortfolioProject(project PortfolioProject) (PortfolioProject, error) {
	project.ID, project.Name = strings.TrimSpace(project.ID), strings.TrimSpace(project.Name)
	if !projectIDPattern.MatchString(project.ID) || project.Name == "" || len(project.Name) > 200 {
		return PortfolioProject{}, ErrProjectConflict
	}
	if project.State == "" {
		project.State = "active"
	}
	if project.State != "active" && project.State != "paused" && project.State != "archived" {
		return PortfolioProject{}, ErrProjectConflict
	}
	if project.Priority == 0 {
		project.Priority = 100
	}
	if project.SchedulerWeight == 0 {
		project.SchedulerWeight = 1
	}
	if project.Priority < 1 || project.SchedulerWeight < 1 || project.SchedulerWeight > 1000 || project.ConcurrencyCap < 0 {
		return PortfolioProject{}, ErrProjectConflict
	}
	return project, nil
}

func projectCommandPayloadHash(value any) (string, error) {
	payload, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:]), nil
}

// ensureProjectCommandTx binds one idempotency key to one normalized command
// payload. It must run in the same transaction as the domain mutation. replay
// is true only when the durable binding already exists and matches exactly.
func ensureProjectCommandTx(ctx context.Context, tx *sql.Tx, scopeID, key, kind, payloadHash, resourceRef, stamp string) (replay bool, err error) {
	scopeID, key = strings.TrimSpace(scopeID), strings.TrimSpace(key)
	if scopeID == "" || key == "" || len(key) > 200 || payloadHash == "" || resourceRef == "" {
		return false, ErrProjectConflict
	}
	var gotKind, gotHash, gotRef string
	err = tx.QueryRowContext(ctx, `SELECT kind,payload_sha256,resource_ref FROM project_commands
		WHERE scope_id=? AND idempotency_key=?`, scopeID, key).Scan(&gotKind, &gotHash, &gotRef)
	if err == nil {
		if gotKind != kind || gotHash != payloadHash || gotRef != resourceRef {
			return false, ErrProjectCommandConflict
		}
		return true, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return false, err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO project_commands
		(scope_id,idempotency_key,kind,payload_sha256,resource_ref,created_at)
		VALUES (?,?,?,?,?,?)`, scopeID, key, kind, payloadHash, resourceRef, stamp)
	return false, err
}

func (s *Store) CreatePortfolioProject(ctx context.Context, project PortfolioProject, now time.Time) (PortfolioProject, error) {
	var err error
	project, err = normalizePortfolioProject(project)
	if err != nil {
		return PortfolioProject{}, err
	}
	stamp := now.UTC().Format(rfc3339)
	_, err = s.DB.ExecContext(ctx, `INSERT INTO projects
		(id,name,state,state_version,priority,scheduler_weight,concurrency_cap,pause_reason,archived_at,created_at,updated_at)
		VALUES (?,?,?,1,?,?,?,'','',?,?)`, project.ID, project.Name, project.State,
		project.Priority, project.SchedulerWeight, project.ConcurrencyCap, stamp, stamp)
	if err != nil {
		if !isUniqueConstraintErr(err) {
			return PortfolioProject{}, err
		}
		existing, getErr := s.GetPortfolioProject(ctx, project.ID)
		if getErr != nil {
			return PortfolioProject{}, getErr
		}
		if existing.Name != project.Name || existing.State != project.State || existing.Priority != project.Priority ||
			existing.SchedulerWeight != project.SchedulerWeight || existing.ConcurrencyCap != project.ConcurrencyCap {
			return PortfolioProject{}, ErrProjectConflict
		}
		return existing, nil
	}
	return s.GetPortfolioProject(ctx, project.ID)
}

// CreatePortfolioProjectCommand atomically binds the portfolio-scoped command
// key and creates the project. A lost response can be replayed with the same
// normalized payload; any changed payload under the key is rejected.
func (s *Store) CreatePortfolioProjectCommand(ctx context.Context, project PortfolioProject, key string, now time.Time) (PortfolioProject, error) {
	var err error
	project, err = normalizePortfolioProject(project)
	if err != nil {
		return PortfolioProject{}, err
	}
	hash, err := projectCommandPayloadHash(struct {
		ID, Name, State                           string
		Priority, SchedulerWeight, ConcurrencyCap int
	}{project.ID, project.Name, project.State, project.Priority, project.SchedulerWeight, project.ConcurrencyCap})
	if err != nil {
		return PortfolioProject{}, err
	}
	stamp := now.UTC().Format(rfc3339)
	err = s.tx(ctx, func(tx *sql.Tx) error {
		replay, err := ensureProjectCommandTx(ctx, tx, projectCreateCommandScope, key, "create", hash, project.ID, stamp)
		if err != nil || replay {
			return err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO projects
			(id,name,state,state_version,priority,scheduler_weight,concurrency_cap,pause_reason,archived_at,created_at,updated_at)
			VALUES (?,?,?,1,?,?,?,'','',?,?)`, project.ID, project.Name, project.State,
			project.Priority, project.SchedulerWeight, project.ConcurrencyCap, stamp, stamp)
		if err == nil || !isUniqueConstraintErr(err) {
			return err
		}
		existing, getErr := scanPortfolioProject(tx.QueryRowContext(ctx, `SELECT id,name,state,state_version,priority,
			scheduler_weight,concurrency_cap,pause_reason,archived_at,created_at FROM projects WHERE id=?`, project.ID))
		if getErr != nil {
			return getErr
		}
		if existing.Name != project.Name || existing.State != project.State || existing.Priority != project.Priority ||
			existing.SchedulerWeight != project.SchedulerWeight || existing.ConcurrencyCap != project.ConcurrencyCap {
			return ErrProjectConflict
		}
		return nil
	})
	if err != nil {
		return PortfolioProject{}, err
	}
	return s.GetPortfolioProject(ctx, project.ID)
}

func (s *Store) GetPortfolioProject(ctx context.Context, id string) (PortfolioProject, error) {
	return scanPortfolioProject(s.DB.QueryRowContext(ctx, `SELECT id,name,state,state_version,priority,
		scheduler_weight,concurrency_cap,pause_reason,archived_at,created_at FROM projects WHERE id=?`, id))
}

func (s *Store) ListPortfolioProjects(ctx context.Context) ([]PortfolioProject, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT id,name,state,state_version,priority,
		scheduler_weight,concurrency_cap,pause_reason,archived_at,created_at
		FROM projects ORDER BY priority,id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PortfolioProject
	for rows.Next() {
		item, err := scanPortfolioProject(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

type portfolioProjectScanner interface{ Scan(...any) error }

func scanPortfolioProject(row portfolioProjectScanner) (PortfolioProject, error) {
	var out PortfolioProject
	var archived, created string
	err := row.Scan(&out.ID, &out.Name, &out.State, &out.StateVersion, &out.Priority,
		&out.SchedulerWeight, &out.ConcurrencyCap, &out.PauseReason, &archived, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return out, ErrProjectNotFound
	}
	out.ArchivedAt, out.CreatedAt = parseOptionalTime(archived), parseOptionalTime(created)
	return out, err
}

func (s *Store) SetPortfolioProjectState(ctx context.Context, id, state, reason string, expectedVersion int, now time.Time) (PortfolioProject, error) {
	if expectedVersion < 1 || (state != "active" && state != "paused" && state != "archived") ||
		state != "active" && strings.TrimSpace(reason) == "" {
		return PortfolioProject{}, ErrProjectConflict
	}
	stamp, archived := now.UTC().Format(rfc3339), ""
	if state == "archived" {
		archived = stamp
	}
	res, err := s.DB.ExecContext(ctx, `UPDATE projects SET state=?,state_version=state_version+1,
		pause_reason=?,archived_at=?,updated_at=? WHERE id=? AND state_version=?`, state,
		strings.TrimSpace(reason), archived, stamp, id, expectedVersion)
	if err != nil {
		return PortfolioProject{}, err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return PortfolioProject{}, ErrProjectConflict
	}
	return s.GetPortfolioProject(ctx, id)
}

func (s *Store) SetPortfolioProjectStateCommand(ctx context.Context, id, state, reason string, expectedVersion int, key string, now time.Time) (PortfolioProject, error) {
	id, reason = strings.TrimSpace(id), strings.TrimSpace(reason)
	if expectedVersion < 1 || (state != "active" && state != "paused" && state != "archived") ||
		state != "active" && reason == "" {
		return PortfolioProject{}, ErrProjectConflict
	}
	hash, err := projectCommandPayloadHash(struct {
		ProjectID       string
		State           string
		Reason          string
		ExpectedVersion int
	}{id, state, reason, expectedVersion})
	if err != nil {
		return PortfolioProject{}, err
	}
	stamp, archived := now.UTC().Format(rfc3339), ""
	if state == "archived" {
		archived = stamp
	}
	err = s.tx(ctx, func(tx *sql.Tx) error {
		replay, err := ensureProjectCommandTx(ctx, tx, id, key, "state", hash, id, stamp)
		if err != nil || replay {
			return err
		}
		res, err := tx.ExecContext(ctx, `UPDATE projects SET state=?,state_version=state_version+1,
			pause_reason=?,archived_at=?,updated_at=? WHERE id=? AND state_version=?`, state,
			reason, archived, stamp, id, expectedVersion)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return ErrProjectConflict
		}
		return nil
	})
	if err != nil {
		return PortfolioProject{}, err
	}
	return s.GetPortfolioProject(ctx, id)
}

func (s *Store) AddProjectRepo(ctx context.Context, projectID, repoID string, now time.Time) error {
	stamp := now.UTC().Format(rfc3339)
	_, err := s.DB.ExecContext(ctx, `INSERT INTO project_repos(project_id,repo_id,state,created_at,updated_at)
		VALUES (?,?,'active',?,?) ON CONFLICT(project_id,repo_id) DO UPDATE SET
		state='active',updated_at=excluded.updated_at`, projectID, repoID, stamp, stamp)
	return err
}

func (s *Store) AddProjectRepoCommand(ctx context.Context, projectID, repoID, key string, now time.Time) error {
	projectID, repoID = strings.TrimSpace(projectID), strings.TrimSpace(repoID)
	if projectID == "" || repoID == "" {
		return ErrProjectConflict
	}
	hash, err := projectCommandPayloadHash(struct{ ProjectID, RepoID string }{projectID, repoID})
	if err != nil {
		return err
	}
	stamp := now.UTC().Format(rfc3339)
	return s.tx(ctx, func(tx *sql.Tx) error {
		replay, err := ensureProjectCommandTx(ctx, tx, projectID, key, "repo", hash, projectID+":"+repoID, stamp)
		if err != nil || replay {
			return err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO project_repos(project_id,repo_id,state,created_at,updated_at)
			VALUES (?,?,'active',?,?) ON CONFLICT(project_id,repo_id) DO UPDATE SET
			state='active',updated_at=excluded.updated_at`, projectID, repoID, stamp, stamp)
		return err
	})
}

func (s *Store) ProjectRepoIDs(ctx context.Context, projectID string, onlyActive bool) ([]string, error) {
	query := `SELECT repo_id FROM project_repos WHERE project_id=?`
	if onlyActive {
		query += ` AND state='active'`
	}
	query += ` ORDER BY repo_id`
	rows, err := s.DB.QueryContext(ctx, query, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

func (s *Store) RegisterProjectActor(ctx context.Context, route ProjectActorRoute, now time.Time) (ProjectActorRoute, error) {
	if route.ProjectID == "" || (route.Role != DriverInteractorRole && route.Role != DriverOrchestratorRole) ||
		strings.TrimSpace(route.ActorID) == "" {
		return ProjectActorRoute{}, ErrProjectConflict
	}
	stamp := now.UTC().Format(rfc3339)
	err := s.tx(ctx, func(tx *sql.Tx) error { return registerProjectActorTx(ctx, tx, route, stamp) })
	if err != nil {
		return ProjectActorRoute{}, err
	}
	return s.GetProjectActor(ctx, route.ProjectID, route.Role)
}

func registerProjectActorTx(ctx context.Context, tx *sql.Tx, route ProjectActorRoute, stamp string) error {
	var actorID, state string
	err := tx.QueryRowContext(ctx, `SELECT actor_id,state FROM project_actor_routes
		WHERE project_id=? AND role=?`, route.ProjectID, route.Role).Scan(&actorID, &state)
	if err == nil && actorID == route.ActorID && state == "active" {
		return nil
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if errors.Is(err, sql.ErrNoRows) {
		_, err = tx.ExecContext(ctx, `INSERT INTO project_actor_routes
			(project_id,role,actor_id,state,state_version,created_at,updated_at)
			VALUES (?,?,?,'active',1,?,?)`, route.ProjectID, route.Role, route.ActorID, stamp, stamp)
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE project_actor_routes SET actor_id=?,state='active',
		state_version=state_version+1,updated_at=? WHERE project_id=? AND role=?`,
		route.ActorID, stamp, route.ProjectID, route.Role)
	return err
}

func (s *Store) RegisterProjectActorCommand(ctx context.Context, route ProjectActorRoute, key string, now time.Time) (ProjectActorRoute, error) {
	route.ProjectID, route.Role, route.ActorID = strings.TrimSpace(route.ProjectID), strings.TrimSpace(route.Role), strings.TrimSpace(route.ActorID)
	if route.ProjectID == "" || (route.Role != DriverInteractorRole && route.Role != DriverOrchestratorRole) || route.ActorID == "" {
		return ProjectActorRoute{}, ErrProjectConflict
	}
	hash, err := projectCommandPayloadHash(struct{ ProjectID, Role, ActorID string }{route.ProjectID, route.Role, route.ActorID})
	if err != nil {
		return ProjectActorRoute{}, err
	}
	stamp := now.UTC().Format(rfc3339)
	err = s.tx(ctx, func(tx *sql.Tx) error {
		replay, err := ensureProjectCommandTx(ctx, tx, route.ProjectID, key, "actor", hash,
			route.ProjectID+":"+route.Role, stamp)
		if err != nil || replay {
			return err
		}
		return registerProjectActorTx(ctx, tx, route, stamp)
	})
	if err != nil {
		return ProjectActorRoute{}, err
	}
	return s.GetProjectActor(ctx, route.ProjectID, route.Role)
}

func (s *Store) GetProjectActor(ctx context.Context, projectID, role string) (ProjectActorRoute, error) {
	var out ProjectActorRoute
	var created, updated string
	err := s.DB.QueryRowContext(ctx, `SELECT project_id,role,actor_id,state,state_version,created_at,updated_at
		FROM project_actor_routes WHERE project_id=? AND role=?`, projectID, role).
		Scan(&out.ProjectID, &out.Role, &out.ActorID, &out.State, &out.StateVersion, &created, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return out, ErrProjectNotFound
	}
	out.CreatedAt, out.UpdatedAt = parseOptionalTime(created), parseOptionalTime(updated)
	return out, err
}

func (p PortfolioProject) ValidateForAdmission(repositories []string, deliveryRepo string) error {
	if p.State != "active" {
		return fmt.Errorf("project %s is %s", p.ID, p.State)
	}
	if len(repositories) == 0 || deliveryRepo == "" {
		return ErrProjectConflict
	}
	return nil
}
