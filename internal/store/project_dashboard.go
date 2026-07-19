package store

import (
	"context"
	"database/sql"
	"sort"
	"strings"
	"time"

	"github.com/samhotchkiss/flowbee/internal/job"
	"github.com/samhotchkiss/flowbee/internal/scheduler"
)

// ProjectDashboardRow is the global portfolio read model. It deliberately folds
// only durable Flowbee facts: project policy, physical builder residency, and
// blocking attention/epic state. The web layer never guesses ownership from a
// repository name or a session label.
type ProjectDashboardRow struct {
	Project       PortfolioProject         `json:"project"`
	Interactor    ProjectActorHealth       `json:"interactor"`
	Orchestrator  ProjectActorHealth       `json:"orchestrator"`
	Capacity      ProjectCapacity          `json:"capacity"`
	Scheduler     []ProjectSchedulerMetric `json:"scheduler"`
	ActiveEpics   int                      `json:"active_epics"`
	ParkedEpics   int                      `json:"parked_epics"`
	NeedsYou      int                      `json:"needs_you"`
	OldestBlocker string                   `json:"oldest_blocker,omitempty"`
	BlockerKind   string                   `json:"blocker_kind,omitempty"`
	BlockedSince  time.Time                `json:"blocked_since,omitempty"`
}

// ProjectCapacity is the current physical/service allocation attributable to a
// project. It is intentionally an allocation, not a quota estimate: every slot
// comes from a live builder residency or an unended lease in the durable ledger.
type ProjectCapacity struct {
	Allocated  int `json:"allocated"`
	Build      int `json:"build"`
	Review     int `json:"review"`
	SpecAuthor int `json:"spec_author"`
	SpecReview int `json:"spec_review"`
}

// ProjectSchedulerMetric is a deterministic, audit-backed fairness view for one
// capability pool. Shares use integer basis points so API/UI clients never have
// to infer rounding. Service is lifetime scheduler turns; weight share is the
// currently configured share among active projects and is labelled separately.
// Starvation is evaluated by EvaluateProjectDashboardStarvation with an injected
// clock and bound using the same max(oldest eligible,last served) base as the
// scheduler's age fence.
type ProjectSchedulerMetric struct {
	Pool                             string    `json:"pool"`
	Allocated                        int       `json:"allocated"`
	ServiceTurns                     int64     `json:"service_turns"`
	PoolServiceTurns                 int64     `json:"pool_service_turns"`
	ServiceShareBasisPoints          int       `json:"service_share_basis_points"`
	ConfiguredWeightShareBasisPoints int       `json:"configured_weight_share_basis_points"`
	Eligible                         int       `json:"eligible"`
	OldestEligibleAt                 time.Time `json:"oldest_eligible_at,omitempty"`
	LastServedAt                     time.Time `json:"last_served_at,omitempty"`
	StarvationDueAt                  time.Time `json:"starvation_due_at,omitempty"`
	EligibleWaitSeconds              int64     `json:"eligible_wait_seconds"`
	StarvationBoundSeconds           int64     `json:"starvation_bound_seconds"`
	Starved                          bool      `json:"starved"`
}

const ProjectStarvationBound = 15 * time.Minute

// ProjectActorHealth makes the logical actor registration and the exact active
// Driver incarnation independently visible. A registered actor without an
// active binding is not "healthy": product delivery to it is durably held.
type ProjectActorHealth struct {
	Role               string    `json:"role"`
	Status             string    `json:"status"`
	ActorID            string    `json:"actor_id,omitempty"`
	RouteState         string    `json:"route_state,omitempty"`
	BindingID          string    `json:"binding_id,omitempty"`
	BindingEpoch       int64     `json:"binding_epoch,omitempty"`
	HostID             string    `json:"host_id,omitempty"`
	StoreID            string    `json:"store_id,omitempty"`
	SessionID          string    `json:"session_id,omitempty"`
	PaneInstance       string    `json:"pane_instance_id,omitempty"`
	AgentRunID         string    `json:"agent_run_id,omitempty"`
	ObservedAt         time.Time `json:"observed_at,omitempty"`
	LifecycleState     string    `json:"lifecycle_state,omitempty"`
	LifecycleOperation string    `json:"lifecycle_operation,omitempty"`
	LifecycleDueAt     time.Time `json:"lifecycle_due_at,omitempty"`
	HoldKind           string    `json:"hold_kind,omitempty"`
	HoldReason         string    `json:"hold_reason,omitempty"`
}

const (
	ProjectActorReady        = "ready"
	ProjectActorRouteAbsent  = "route_absent"
	ProjectActorUnregistered = "unregistered"
	ProjectActorPaused       = "paused"
)

// ProjectDashboard returns one row for every project, including paused and
// archived projects. ActiveEpics means a physically resident builder; ParkedEpics
// means retained affinity with no occupied pane. The two counts are intentionally
// disjoint so the dashboard cannot imply that parked work consumes capacity.
func (s *Store) ProjectDashboard(ctx context.Context) ([]ProjectDashboardRow, error) {
	projects, err := s.ListPortfolioProjects(ctx)
	if err != nil {
		return nil, err
	}
	byID := make(map[string]*ProjectDashboardRow, len(projects))
	rows := make([]ProjectDashboardRow, len(projects))
	for i := range projects {
		rows[i].Project = projects[i]
		rows[i].Interactor = ProjectActorHealth{Role: DriverInteractorRole, Status: ProjectActorUnregistered}
		rows[i].Orchestrator = ProjectActorHealth{Role: DriverOrchestratorRole, Status: ProjectActorUnregistered}
		byID[projects[i].ID] = &rows[i]
	}

	actors, err := s.DB.QueryContext(ctx, `SELECT r.project_id,r.role,r.actor_id,r.state,
		COALESCE(b.binding_id,''),COALESCE(b.binding_epoch,0),COALESCE(b.host_id,''),
		COALESCE(b.store_id,''),COALESCE(b.session_id,''),COALESCE(b.pane_instance_id,''),
		COALESCE(b.agent_run_id,''),COALESCE(b.observed_at,''),
		COALESCE(l.state,''),COALESCE(l.desired_operation,''),COALESCE(l.state_due_at,''),
		COALESCE(l.hold_kind,''),COALESCE(l.hold_reason,'')
		FROM project_actor_routes r LEFT JOIN driver_session_bindings b
		  ON b.project_id=r.project_id AND b.worker_identity=r.actor_id
		 AND b.role=r.role AND b.state='active'
		 AND EXISTS (
		   SELECT 1 FROM driver_instances i
		   JOIN driver_session_projections p
		     ON p.store_id=b.store_id AND p.session_id=b.session_id
		    AND p.pane_instance_id=b.pane_instance_id
		    AND p.agent_run_id=b.agent_run_id
		    AND p.tmux_server_instance_id=b.tmux_server_instance_id
		    AND p.lifecycle<>'ended'
		   WHERE i.store_id=b.store_id AND i.host_id=b.host_id AND i.state='live'
		 )
		LEFT JOIN project_actor_lifecycles l
		  ON l.project_id=r.project_id AND l.role=r.role AND l.actor_id=r.actor_id
		ORDER BY r.project_id,r.role`)
	if err != nil {
		return nil, err
	}
	for actors.Next() {
		var projectID, observed, lifecycleDue string
		var health ProjectActorHealth
		if err := actors.Scan(&projectID, &health.Role, &health.ActorID, &health.RouteState,
			&health.BindingID, &health.BindingEpoch, &health.HostID, &health.StoreID,
			&health.SessionID, &health.PaneInstance, &health.AgentRunID, &observed,
			&health.LifecycleState, &health.LifecycleOperation, &lifecycleDue,
			&health.HoldKind, &health.HoldReason); err != nil {
			actors.Close()
			return nil, err
		}
		health.ObservedAt = parseDashboardTime(observed)
		health.LifecycleDueAt = parseDashboardTime(lifecycleDue)
		switch {
		case health.RouteState != "active":
			health.Status = ProjectActorPaused
		case health.LifecycleState != "" && health.LifecycleState != "active":
			health.Status = health.LifecycleState
		case health.BindingID == "":
			health.Status = ProjectActorRouteAbsent
		default:
			health.Status = ProjectActorReady
		}
		if row := byID[projectID]; row != nil {
			if health.Role == DriverInteractorRole {
				row.Interactor = health
			} else if health.Role == DriverOrchestratorRole {
				row.Orchestrator = health
			}
		}
	}
	if err := actors.Err(); err != nil {
		actors.Close()
		return nil, err
	}
	if err := actors.Close(); err != nil {
		return nil, err
	}

	counts, err := s.DB.QueryContext(ctx, `SELECT project_id,
		SUM(CASE WHEN builder_affinity_state='active' THEN 1 ELSE 0 END),
		SUM(CASE WHEN builder_affinity_state='parked' AND state NOT IN ('complete','abandoned') THEN 1 ELSE 0 END)
		FROM epic_deliveries GROUP BY project_id`)
	if err != nil {
		return nil, err
	}
	for counts.Next() {
		var projectID string
		var active, parked sql.NullInt64
		if err := counts.Scan(&projectID, &active, &parked); err != nil {
			counts.Close()
			return nil, err
		}
		if row := byID[projectID]; row != nil {
			row.ActiveEpics, row.ParkedEpics = int(active.Int64), int(parked.Int64)
		}
	}
	if err := counts.Close(); err != nil {
		return nil, err
	}

	needs, err := s.DB.QueryContext(ctx, `SELECT project_id,COUNT(*) FROM decision_requests
		WHERE state IN ('open','viewed') GROUP BY project_id`)
	if err != nil {
		return nil, err
	}
	for needs.Next() {
		var projectID string
		var count int
		if err := needs.Scan(&projectID, &count); err != nil {
			needs.Close()
			return nil, err
		}
		if row := byID[projectID]; row != nil {
			row.NeedsYou = count
		}
	}
	if err := needs.Close(); err != nil {
		return nil, err
	}

	// Blocking attention is authoritative when present. A legacy epic blocked
	// before an attention item was materialized remains visible as the fallback.
	blockers, err := s.DB.QueryContext(ctx, `
		SELECT project_id,kind,detail,seen_at FROM (
			SELECT COALESCE(NULLIF(a.project_id,''),e.project_id,'default') AS project_id,
			       a.kind AS kind,a.detail AS detail,a.first_seen_at AS seen_at,0 AS source_rank
			  FROM attention_items a LEFT JOIN epics e ON e.id=a.epic_id
			 WHERE a.blocking=1 AND a.state IN ('open','leased','awaiting_ack')
			UNION ALL
			SELECT project_id,'epic_blocked',
			       CASE WHEN status_blockers<>'' THEN status_blockers ELSE title END,
			       updated_at,1
			  FROM epics WHERE state='blocked'
		) ORDER BY seen_at,source_rank,project_id`)
	if err != nil {
		return nil, err
	}
	defer blockers.Close()
	for blockers.Next() {
		var projectID, kind, detail, seen string
		if err := blockers.Scan(&projectID, &kind, &detail, &seen); err != nil {
			return nil, err
		}
		row := byID[projectID]
		if row == nil || !row.BlockedSince.IsZero() {
			continue
		}
		row.BlockerKind = kind
		row.OldestBlocker = strings.TrimSpace(detail)
		row.BlockedSince = parseDashboardTime(seen)
	}
	if err := blockers.Err(); err != nil {
		return nil, err
	}

	if err := s.foldProjectSchedulerDashboard(ctx, byID); err != nil {
		return nil, err
	}

	// ListPortfolioProjects already orders priority/id. Keep that stable even if
	// its implementation changes: this is the operator's global triage order.
	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].Project.Priority != rows[j].Project.Priority {
			return rows[i].Project.Priority < rows[j].Project.Priority
		}
		return rows[i].Project.ID < rows[j].Project.ID
	})
	return rows, nil
}

// EvaluateProjectDashboardStarvation completes the time-relative portion of the
// portfolio projection. Keeping the clock outside SQL makes API responses, HTML,
// and tests agree exactly at a supplied instant.
func EvaluateProjectDashboardStarvation(rows []ProjectDashboardRow, now time.Time, bound time.Duration) {
	if bound <= 0 {
		bound = ProjectStarvationBound
	}
	now = now.UTC()
	for i := range rows {
		for j := range rows[i].Scheduler {
			metric := &rows[i].Scheduler[j]
			metric.StarvationBoundSeconds = int64(bound / time.Second)
			metric.Starved = false
			metric.EligibleWaitSeconds = 0
			metric.StarvationDueAt = time.Time{}
			if metric.Eligible == 0 || metric.OldestEligibleAt.IsZero() {
				continue
			}
			base := metric.OldestEligibleAt
			if metric.LastServedAt.After(base) {
				base = metric.LastServedAt
			}
			metric.StarvationDueAt = base.Add(bound)
			if now.After(base) {
				metric.EligibleWaitSeconds = int64(now.Sub(base) / time.Second)
			}
			metric.Starved = !now.Before(metric.StarvationDueAt)
		}
	}
}

func (s *Store) foldProjectSchedulerDashboard(ctx context.Context, byID map[string]*ProjectDashboardRow) error {
	activeWeight := 0
	for _, row := range byID {
		if row.Project.State == "active" {
			activeWeight += row.Project.SchedulerWeight
		}
	}

	// Every project always exposes the two production capacity pools, including
	// zeroes. This makes "no service" distinguishable from an omitted metric.
	metrics := make(map[string]map[string]*ProjectSchedulerMetric, len(byID))
	for projectID, row := range byID {
		metrics[projectID] = map[string]*ProjectSchedulerMetric{}
		for _, pool := range []string{scheduler.PoolBuild, scheduler.PoolReview} {
			metric := &ProjectSchedulerMetric{Pool: pool}
			if row.Project.State == "active" && activeWeight > 0 {
				metric.ConfiguredWeightShareBasisPoints = ratioBasisPoints(int64(row.Project.SchedulerWeight), int64(activeWeight))
			}
			metrics[projectID][pool] = metric
		}
	}
	ensureMetric := func(projectID, pool string) *ProjectSchedulerMetric {
		row := byID[projectID]
		if row == nil {
			return nil
		}
		if metrics[projectID][pool] == nil {
			metric := &ProjectSchedulerMetric{Pool: pool}
			if row.Project.State == "active" && activeWeight > 0 {
				metric.ConfiguredWeightShareBasisPoints = ratioBasisPoints(int64(row.Project.SchedulerWeight), int64(activeWeight))
			}
			metrics[projectID][pool] = metric
		}
		return metrics[projectID][pool]
	}

	// Builder residency is Flowbee v2's physical build allocation. Legacy and
	// review/spec workers are represented by unended job leases below.
	for projectID, row := range byID {
		row.Capacity.Build = row.ActiveEpics
		row.Capacity.Allocated = row.ActiveEpics
		ensureMetric(projectID, scheduler.PoolBuild).Allocated = row.ActiveEpics
	}
	leaseRows, err := s.DB.QueryContext(ctx, `SELECT j.project_id,j.role,COUNT(*)
		FROM leases l JOIN jobs j ON j.id=l.job_id
		WHERE l.ended_at IS NULL GROUP BY j.project_id,j.role`)
	if err != nil {
		return err
	}
	for leaseRows.Next() {
		var projectID, role string
		var count int
		if err := leaseRows.Scan(&projectID, &role, &count); err != nil {
			leaseRows.Close()
			return err
		}
		row := byID[projectID]
		if row == nil {
			continue
		}
		pool := scheduler.PoolBuild
		switch job.Role(role) {
		case job.RoleCodeReviewer:
			pool, row.Capacity.Review = scheduler.PoolReview, row.Capacity.Review+count
		case job.RoleSpecAuthor:
			pool, row.Capacity.SpecAuthor = scheduler.PoolSpecAuthor, row.Capacity.SpecAuthor+count
		case job.RoleSpecReviewer:
			pool, row.Capacity.SpecReview = scheduler.PoolSpecReview, row.Capacity.SpecReview+count
		default:
			row.Capacity.Build += count
		}
		row.Capacity.Allocated += count
		ensureMetric(projectID, pool).Allocated += count
	}
	if err := leaseRows.Close(); err != nil {
		return err
	}

	poolTotals := map[string]int64{}
	turnRows, err := s.DB.QueryContext(ctx, `SELECT pool,project_id,COUNT(*)
		FROM project_scheduler_turns GROUP BY pool,project_id`)
	if err != nil {
		return err
	}
	for turnRows.Next() {
		var pool, projectID string
		var turns int64
		if err := turnRows.Scan(&pool, &projectID, &turns); err != nil {
			turnRows.Close()
			return err
		}
		poolTotals[pool] += turns
		if metric := ensureMetric(projectID, pool); metric != nil {
			metric.ServiceTurns = turns
		}
	}
	if err := turnRows.Close(); err != nil {
		return err
	}
	stateRows, err := s.DB.QueryContext(ctx, `SELECT pool,project_id,last_served_at
		FROM project_scheduler_state WHERE last_served_at<>''`)
	if err != nil {
		return err
	}
	for stateRows.Next() {
		var pool, projectID, served string
		if err := stateRows.Scan(&pool, &projectID, &served); err != nil {
			stateRows.Close()
			return err
		}
		if metric := ensureMetric(projectID, pool); metric != nil {
			metric.LastServedAt = parseDashboardTime(served)
		}
	}
	if err := stateRows.Close(); err != nil {
		return err
	}

	build, err := s.ReadyCandidates(ctx)
	if err != nil {
		return err
	}
	review, err := s.ReviewPendingCandidates(ctx)
	if err != nil {
		return err
	}
	for _, candidate := range append(build, review...) {
		row := byID[candidate.ProjectID]
		if row == nil || row.Project.State != "active" ||
			(row.Project.ConcurrencyCap > 0 && row.Capacity.Allocated >= row.Project.ConcurrencyCap) {
			continue
		}
		metric := ensureMetric(candidate.ProjectID, candidate.Pool)
		metric.Eligible++
		if metric.OldestEligibleAt.IsZero() || candidate.EnqueuedAt.Before(metric.OldestEligibleAt) {
			metric.OldestEligibleAt = candidate.EnqueuedAt
		}
	}

	for projectID, row := range byID {
		pools := make([]string, 0, len(metrics[projectID]))
		for pool := range metrics[projectID] {
			pools = append(pools, pool)
		}
		sort.Strings(pools)
		row.Scheduler = make([]ProjectSchedulerMetric, 0, len(pools))
		for _, pool := range pools {
			metric := metrics[projectID][pool]
			metric.PoolServiceTurns = poolTotals[pool]
			metric.ServiceShareBasisPoints = ratioBasisPoints(metric.ServiceTurns, metric.PoolServiceTurns)
			row.Scheduler = append(row.Scheduler, *metric)
		}
	}
	return nil
}

func ratioBasisPoints(part, total int64) int {
	if part <= 0 || total <= 0 {
		return 0
	}
	return int((part*10_000 + total/2) / total)
}

func parseDashboardTime(value string) time.Time {
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02 15:04:05"} {
		if parsed, err := time.Parse(layout, strings.TrimSpace(value)); err == nil {
			return parsed.UTC()
		}
	}
	return time.Time{}
}
