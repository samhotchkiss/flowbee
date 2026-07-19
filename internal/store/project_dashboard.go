package store

import (
	"context"
	"database/sql"
	"sort"
	"strings"
	"time"
)

// ProjectDashboardRow is the global portfolio read model. It deliberately folds
// only durable Flowbee facts: project policy, physical builder residency, and
// blocking attention/epic state. The web layer never guesses ownership from a
// repository name or a session label.
type ProjectDashboardRow struct {
	Project       PortfolioProject
	ActiveEpics   int
	ParkedEpics   int
	NeedsYou      int
	OldestBlocker string
	BlockerKind   string
	BlockedSince  time.Time
}

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
		byID[projects[i].ID] = &rows[i]
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

func parseDashboardTime(value string) time.Time {
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02 15:04:05"} {
		if parsed, err := time.Parse(layout, strings.TrimSpace(value)); err == nil {
			return parsed.UTC()
		}
	}
	return time.Time{}
}
