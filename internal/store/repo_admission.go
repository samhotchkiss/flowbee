package store

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/samhotchkiss/flowbee/internal/ledger"
)

var (
	ErrRepoAdmissionUnmapped   = errors.New("repository has no active project owner")
	ErrRepoAdmissionAmbiguous  = errors.New("repository has multiple active project owners")
	ErrRepoAdmissionWrongOwner = errors.New("repository is owned by another project")
)

type RepoAdmissionRoutingError struct {
	RepoID     string
	Kind       error
	Candidates []string
}

func (e *RepoAdmissionRoutingError) Error() string {
	return fmt.Sprintf("repo admission routing %q: %v (active projects=%v)", e.RepoID, e.Kind, e.Candidates)
}

func (e *RepoAdmissionRoutingError) Unwrap() error { return e.Kind }

type RepoAdmissionHold struct {
	RepoID, State, Reason string
	CandidateProjects     []string
	Occurrences           int
	FirstSeenAt           time.Time
	LastSeenAt            time.Time
	ResolvedAt            time.Time
}

// ResolveRepoAdmissionProject is the legacy repo-origin compatibility resolver.
// Native v2 admission already carries a project id and instead proves active
// membership with assertProjectRepoMembershipTx. A repository may legitimately
// belong to multiple projects; only an origin that lacks project authority must
// fail closed on that ambiguity.
func (s *Store) ResolveRepoAdmissionProject(ctx context.Context, repoID string) (string, error) {
	return resolveRepoAdmissionProjectDB(ctx, s.DB, strings.TrimSpace(repoID))
}

// ResolveRepoAdmissionProjectAndHold is the control-plane wiring probe. A bad
// route is committed as a durable portfolio hold before the typed error returns;
// a repaired route resolves the prior hold.
func (s *Store) ResolveRepoAdmissionProjectAndHold(ctx context.Context, repoID string, now time.Time) (string, error) {
	repoID = strings.TrimSpace(repoID)
	var projectID string
	var routeErr error
	err := s.tx(ctx, func(tx *sql.Tx) error {
		projectID, routeErr = resolveRepoAdmissionProjectTx(ctx, tx, repoID)
		if routeErr != nil {
			return upsertRepoAdmissionHoldTx(ctx, tx, repoID, routeErr, now)
		}
		return resolveRepoAdmissionHoldTx(ctx, tx, repoID, now)
	})
	if err != nil {
		return "", err
	}
	return projectID, routeErr
}

func resolveRepoAdmissionProjectDB(ctx context.Context, db *sql.DB, repoID string) (string, error) {
	rows, err := db.QueryContext(ctx, `SELECT pr.project_id
		FROM project_repos pr
		JOIN projects p ON p.id=pr.project_id
		JOIN repos r ON r.id=pr.repo_id
		WHERE pr.repo_id=? AND pr.state='active' AND p.state='active' AND r.active=1
		ORDER BY pr.project_id`, repoID)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	var candidates []string
	for rows.Next() {
		var projectID string
		if err := rows.Scan(&projectID); err != nil {
			return "", err
		}
		candidates = append(candidates, projectID)
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	return chooseRepoAdmissionProject(repoID, candidates)
}

func resolveRepoAdmissionProjectTx(ctx context.Context, tx *sql.Tx, repoID string) (string, error) {
	rows, err := tx.QueryContext(ctx, `SELECT pr.project_id
		FROM project_repos pr
		JOIN projects p ON p.id=pr.project_id
		JOIN repos r ON r.id=pr.repo_id
		WHERE pr.repo_id=? AND pr.state='active' AND p.state='active' AND r.active=1
		ORDER BY pr.project_id`, repoID)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	var candidates []string
	for rows.Next() {
		var projectID string
		if err := rows.Scan(&projectID); err != nil {
			return "", err
		}
		candidates = append(candidates, projectID)
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	return chooseRepoAdmissionProject(repoID, candidates)
}

func chooseRepoAdmissionProject(repoID string, candidates []string) (string, error) {
	switch len(candidates) {
	case 1:
		return candidates[0], nil
	case 0:
		return "", &RepoAdmissionRoutingError{RepoID: repoID, Kind: ErrRepoAdmissionUnmapped}
	default:
		return "", &RepoAdmissionRoutingError{RepoID: repoID, Kind: ErrRepoAdmissionAmbiguous,
			Candidates: append([]string(nil), candidates...)}
	}
}

func assertLegacyRepoAdmissionProjectTx(ctx context.Context, tx *sql.Tx, projectID, repoID string) error {
	owner, err := resolveRepoAdmissionProjectTx(ctx, tx, repoID)
	if err != nil {
		return err
	}
	if owner != projectID {
		return &RepoAdmissionRoutingError{RepoID: repoID, Kind: ErrRepoAdmissionWrongOwner, Candidates: []string{owner}}
	}
	return nil
}

func assertProjectRepoMembershipTx(ctx context.Context, tx *sql.Tx, projectID, repoID string) error {
	var active int
	err := tx.QueryRowContext(ctx, `SELECT EXISTS(
		SELECT 1 FROM project_repos pr
		JOIN projects p ON p.id=pr.project_id
		JOIN repos r ON r.id=pr.repo_id
		WHERE pr.project_id=? AND pr.repo_id=? AND pr.state='active' AND p.state='active' AND r.active=1
	)`, projectID, repoID).Scan(&active)
	if err != nil {
		return err
	}
	if active != 1 {
		return &RepoAdmissionRoutingError{RepoID: repoID, Kind: ErrRepoAdmissionWrongOwner, Candidates: []string{projectID}}
	}
	return nil
}

func (s *Store) AssertProjectRepoMembership(ctx context.Context, projectID, repoID string) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		return assertProjectRepoMembershipTx(ctx, tx, strings.TrimSpace(projectID), strings.TrimSpace(repoID))
	})
}

func upsertRepoAdmissionHoldTx(ctx context.Context, tx *sql.Tx, repoID string, routeErr error, now time.Time) error {
	var routing *RepoAdmissionRoutingError
	if !errors.As(routeErr, &routing) {
		return routeErr
	}
	candidates, _ := json.Marshal(routing.Candidates)
	stamp := now.UTC().Format(rfc3339)
	_, err := tx.ExecContext(ctx, `INSERT INTO repo_admission_holds
		(repo_id,state,reason,candidate_projects_json,occurrences,first_seen_at,last_seen_at,resolved_at)
		VALUES (?,'pending',?,?,1,?,?,'')
		ON CONFLICT(repo_id) DO UPDATE SET state='pending',reason=excluded.reason,
		candidate_projects_json=excluded.candidate_projects_json,
		occurrences=repo_admission_holds.occurrences+1,last_seen_at=excluded.last_seen_at,resolved_at=''`,
		repoID, routing.Kind.Error(), string(candidates), stamp, stamp)
	if err != nil {
		return err
	}

	// A routing error must be visible outside this table. Publish it into the
	// durable attention queue owned by the portfolio/default control project. This
	// does not route the work to default: the admission remains held and no job is
	// inserted. It gives the dashboard and supervisor plane an actionable signal.
	dedupKey := repoAdmissionAttentionDedup(repoID)
	item := AttentionItem{
		ID:        repoAdmissionAttentionID(repoID),
		ProjectID: "default",
		Kind:      "repo_admission_routing_hold",
		Repo:      repoID,
		Priority:  2,
		DedupKey:  dedupKey,
		Blocking:  true,
		Evidence: map[string]string{
			"repo_id":            repoID,
			"reason":             routing.Kind.Error(),
			"candidate_projects": string(candidates),
		},
		Detail: "GitHub admission held: repository does not resolve to exactly one active project; no default was selected",
	}
	evidenceJSON := marshalEvidence(item.Evidence)
	if existingID, found, findErr := activeItemIDByDedupTx(ctx, tx, "default", dedupKey); findErr != nil {
		return findErr
	} else if found {
		return refreshActiveAttentionTx(ctx, tx, existingID, item, evidenceJSON, stamp)
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO attention_items
		(id,project_id,kind,epic_id,repo,priority,state,dedup_key,blocking,leased_by,item_epoch,
		 lease_expires_at,awaiting_since,delivery_key,evidence_json,detail,resolution,verdict,
		 occurrences,first_seen_at,last_seen_at,resolved_at,created_at,updated_at)
		VALUES (?,? ,?,'',?,?,'open',?,?, '',0,'','','',?,?,'','',1,?,?,'',?,?)`,
		item.ID, item.ProjectID, item.Kind, item.Repo, item.Priority, item.DedupKey, b2i(item.Blocking),
		evidenceJSON, item.Detail, stamp, stamp, stamp, stamp)
	if err != nil {
		return err
	}
	return appendEpicLedger(ctx, tx, ledgerKeyFor("", item.ID), ledger.KindAttentionOpened,
		"system", 0, item.ID, item.Kind, now)
}

func resolveRepoAdmissionHoldTx(ctx context.Context, tx *sql.Tx, repoID string, now time.Time) error {
	stamp := now.UTC().Format(rfc3339)
	_, err := tx.ExecContext(ctx, `UPDATE repo_admission_holds SET state='resolved',resolved_at=?,last_seen_at=?
		WHERE repo_id=? AND state='pending'`, stamp, stamp, repoID)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE attention_items
		SET state='resolved',resolution='cleared',resolved_at=?,leased_by='',lease_expires_at='',updated_at=?
		WHERE project_id='default' AND dedup_key=? AND state IN `+attentionActiveStatesSQL,
		stamp, stamp, repoAdmissionAttentionDedup(repoID))
	return err
}

func repoAdmissionAttentionDedup(repoID string) string { return "repo_admission_routing:" + repoID }

func repoAdmissionAttentionID(repoID string) string {
	return "repo-admission-" + base64.RawURLEncoding.EncodeToString([]byte(repoID))
}

func (s *Store) GetRepoAdmissionHold(ctx context.Context, repoID string) (RepoAdmissionHold, error) {
	var out RepoAdmissionHold
	var candidatesJSON, first, last, resolved string
	err := s.DB.QueryRowContext(ctx, `SELECT repo_id,state,reason,candidate_projects_json,
		occurrences,first_seen_at,last_seen_at,resolved_at FROM repo_admission_holds WHERE repo_id=?`, repoID).
		Scan(&out.RepoID, &out.State, &out.Reason, &candidatesJSON, &out.Occurrences, &first, &last, &resolved)
	if err != nil {
		return RepoAdmissionHold{}, err
	}
	_ = json.Unmarshal([]byte(candidatesJSON), &out.CandidateProjects)
	out.FirstSeenAt, out.LastSeenAt, out.ResolvedAt = parseOptionalTime(first), parseOptionalTime(last), parseOptionalTime(resolved)
	return out, nil
}
