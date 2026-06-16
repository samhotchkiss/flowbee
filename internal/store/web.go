package store

import (
	"context"
	"time"

	"github.com/samhotchkiss/flowbee/internal/history"
	"github.com/samhotchkiss/flowbee/internal/job"
)

// web.go holds the read-models the F12 web UI (internal/web) renders: the rich
// board cards (per-card stage timer + needs-you/backlog lanes), the per-stage
// ENTERED/LEFT absolute times the detail drawer shows, and the composed job
// detail (job facts + stage timings + the build-history timeline). Every method
// here is a pure read over persisted facts — the ledger stays canonical and the
// markdown/history fold is reused (history.Fold), never re-derived.

// BoardCard is one card on the F12 board view (build-list §G). It carries the
// fields the board lane + per-card stage timer + ⚠ needs-you marker need. The
// timer reads UpdatedAt (when the card last entered its current stage projection)
// against `now` at render time (gray -> amber -> red).
type BoardCard struct {
	JobID         string    `json:"job_id"`
	Kind          string    `json:"kind"`
	Stage         string    `json:"stage"`
	State         string    `json:"state"`
	Role          string    `json:"role"`
	// Repo is the F9 repo-scope handle (the repos.id this card's job belongs to).
	// Empty is the legacy single-repo default. The board's repo filter chips are the
	// distinct non-empty values, and ?repo=<id> keeps only the matching cards.
	Repo          string    `json:"repo,omitempty"`
	Identity      string    `json:"bound_identity"`
	IssueNumber   int       `json:"issue_number,omitempty"`
	EpicID        string    `json:"epic_id,omitempty"`
	IsEpic        bool      `json:"is_epic"`
	Title         string    `json:"title"`
	Priority      int       `json:"priority"`
	NeedsFullSpec bool      `json:"needs_full_spec"`
	LeaseEpoch    int       `json:"lease_epoch"`
	StageEntered  time.Time `json:"stage_entered"`
	// StageAgeS is seconds spent in the current stage at snapshot time (now -
	// StageEntered). The UI maps it to the gray->amber->red per-card timer; surfaced
	// here too so a non-JS client still sees the age.
	StageAgeS int `json:"stage_age_s"`
}

// BoardCards returns every tracked job as a rich board card (F12 board view).
// Backlog + needs_design jobs are included (they own the Backlog and ⚠ Needs-you
// lanes); terminal/quiescent jobs are excluded so the board shows the live flow.
// StageAgeS is computed against now so the per-card stage timer renders without a
// second query. Ordered by recency so the newest movement floats up.
func (s *Store) BoardCards(ctx context.Context, now time.Time) ([]BoardCard, error) {
	// StageEntered is the absolute time the job ENTERED its current state — the
	// latest event whose to_state equals the current state — folded from the ledger
	// (deterministic: it is the event CreatedAt Flowbee recorded, not the wall-clock
	// updated_at). The per-card timer measures now - StageEntered against the
	// gray->amber->red thresholds. Falls back to updated_at when no such event exists.
	rows, err := s.DB.QueryContext(ctx, `
		SELECT j.id, j.kind, j.stage, j.state, j.role, COALESCE(j.bound_identity,''),
		       COALESCE(j.issue_number,0), COALESCE(j.epic_id,''), COALESCE(j.is_epic,0),
		       COALESCE(j.task_text,''), COALESCE(j.spec_text,''),
		       j.priority, COALESCE(j.needs_full_spec,0), j.lease_epoch,
		       COALESCE(j.repo,''),
		       COALESCE((SELECT MAX(e.created_at) FROM job_events e
		                  WHERE e.job_id = j.id AND e.to_state = j.state), j.updated_at)
		  FROM jobs j
		 WHERE j.state NOT IN ('quiescent','cancelled')
		 ORDER BY j.updated_at DESC, j.id ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []BoardCard
	for rows.Next() {
		var c BoardCard
		var isEpic, needs int
		var task, spec, entered string
		if err := rows.Scan(&c.JobID, &c.Kind, &c.Stage, &c.State, &c.Role, &c.Identity,
			&c.IssueNumber, &c.EpicID, &isEpic, &task, &spec,
			&c.Priority, &needs, &c.LeaseEpoch, &c.Repo, &entered); err != nil {
			return nil, err
		}
		c.IsEpic = isEpic != 0
		c.NeedsFullSpec = needs != 0
		c.Title = cardTitle(task, spec, c.JobID)
		if ts, perr := time.Parse(rfc3339, entered); perr == nil {
			c.StageEntered = ts
			age := now.Sub(ts)
			if age < 0 {
				age = 0
			}
			c.StageAgeS = int(age / time.Second)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// cardTitle derives a short human title for a card: first non-empty line of the
// task text, else the spec text, else the job id.
func cardTitle(task, spec, jobID string) string {
	for _, s := range []string{task, spec} {
		if line := firstNonEmptyLine(s); line != "" {
			return line
		}
	}
	return jobID
}

func firstNonEmptyLine(s string) string {
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			line := trimSpace(s[start:i])
			if line != "" {
				return line
			}
			start = i + 1
		}
	}
	return trimSpace(s[start:])
}

func trimSpace(s string) string {
	for len(s) > 0 && (s[0] == ' ' || s[0] == '\t' || s[0] == '\r' || s[0] == '\n') {
		s = s[1:]
	}
	for len(s) > 0 {
		c := s[len(s)-1]
		if c == ' ' || c == '\t' || c == '\r' || c == '\n' {
			s = s[:len(s)-1]
			continue
		}
		break
	}
	return s
}

// StageTiming is one row of a job's per-stage ENTERED/LEFT absolute times (F12
// detail drawer). It is folded from the ledger's state transitions: a stage is
// ENTERED when an event's to_state names it and LEFT when a later event's
// from_state names it (Left is zero while the job still occupies the stage).
type StageTiming struct {
	Stage   string    `json:"stage"`
	Entered time.Time `json:"entered"`
	Left    time.Time `json:"left,omitempty"`
	// DurationS is Left-Entered in seconds, or (asOf - Entered) while still open.
	DurationS int  `json:"duration_s"`
	Open      bool `json:"open"`
}

// JobStageTimings folds a job's event ledger into per-stage ENTERED/LEFT absolute
// times (F12 detail drawer: "per-stage ENTERED/LEFT absolute times"). Pure read
// over job_events: each transition's from_state closes the prior stage span and
// to_state opens the next. asOf bounds the still-open final span's duration. The
// returned slice is in transition order (the job's lifecycle path).
func (s *Store) JobStageTimings(ctx context.Context, jobID string, asOf time.Time) ([]StageTiming, error) {
	events, err := s.LoadEvents(ctx, jobID)
	if err != nil {
		return nil, err
	}
	var out []StageTiming
	openIdx := -1 // index into out of the currently-open span, or -1
	for _, e := range events {
		// close the open span when a transition leaves it.
		if openIdx >= 0 && e.FromState != "" && string(e.FromState) == out[openIdx].Stage && !e.CreatedAt.IsZero() {
			out[openIdx].Left = e.CreatedAt
			out[openIdx].Open = false
			out[openIdx].DurationS = spanSeconds(out[openIdx].Entered, e.CreatedAt)
			openIdx = -1
		}
		// open a new span when a transition enters a (different) state.
		if e.ToState != "" && !e.CreatedAt.IsZero() {
			if openIdx >= 0 && out[openIdx].Stage == string(e.ToState) {
				continue // same state (e.g. an in-place event): keep the span open.
			}
			if openIdx >= 0 {
				// an entry without a matching from_state close: close the prior span here.
				out[openIdx].Left = e.CreatedAt
				out[openIdx].Open = false
				out[openIdx].DurationS = spanSeconds(out[openIdx].Entered, e.CreatedAt)
			}
			out = append(out, StageTiming{Stage: string(e.ToState), Entered: e.CreatedAt, Open: true})
			openIdx = len(out) - 1
		}
	}
	if openIdx >= 0 {
		out[openIdx].DurationS = spanSeconds(out[openIdx].Entered, asOf)
	}
	return out, nil
}

func spanSeconds(a, b time.Time) int {
	d := b.Sub(a)
	if d < 0 {
		return 0
	}
	return int(d / time.Second)
}

// JobDetail is the F12 detail-drawer payload: the job's card facts, its per-stage
// ENTERED/LEFT timings, and the full build-history timeline (the same fold the
// post-merge archive writes, build-list §F). Composed for one card so the drawer
// can click card-to-card without dimming the board.
type JobDetail struct {
	Card    BoardCard     `json:"card"`
	Timings []StageTiming `json:"stage_timings"`
	History history.Card  `json:"history"`
}

// JobDetail composes the detail-drawer payload for one job (F12). It reuses the
// canonical history fold (history.Fold via HistoryCardForJob) for the build-history
// timeline + the stage-timing fold, so the drawer is a pure read-model.
func (s *Store) JobDetail(ctx context.Context, jobID string, now time.Time) (JobDetail, error) {
	j, _, err := s.loadJob(ctx, jobID)
	if err != nil {
		return JobDetail{}, err
	}
	timings, err := s.JobStageTimings(ctx, jobID, now)
	if err != nil {
		return JobDetail{}, err
	}
	card, err := s.HistoryCardForJob(ctx, jobID)
	if err != nil {
		return JobDetail{}, err
	}
	bc := BoardCard{
		JobID: j.ID, Kind: string(j.Kind), Stage: j.Stage, State: string(j.State),
		Role: string(j.Role), Identity: j.BoundIdentity, IssueNumber: j.IssueNum,
		EpicID: j.EpicID, IsEpic: j.IsEpic, Title: cardTitle(j.TaskText, j.SpecText, j.ID),
		Priority: j.Priority, LeaseEpoch: j.LeaseEpoch, Repo: j.Repo,
	}
	return JobDetail{Card: bc, Timings: timings, History: card}, nil
}

// loadJob loads a job outside a transaction (a thin read wrapper used by the web
// detail read-model). It reuses the canonical jobSelect projection.
func (s *Store) loadJob(ctx context.Context, jobID string) (job.Job, int, error) {
	j, err := scanJob(s.DB.QueryRowContext(ctx, jobSelect+` WHERE id = ?`, jobID))
	if err != nil {
		return job.Job{}, 0, err
	}
	return j, j.JobSeq, nil
}
