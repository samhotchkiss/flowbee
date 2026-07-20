package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/samhotchkiss/flowbee/internal/workintent"
)

var (
	ErrWorkIntentContractInvalid = errors.New("work intent epic contract is invalid")
	ErrWorkIntentContractFenced  = errors.New("work intent epic contract is stale or unauthorized")
)

var epicContractSlugPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

// WorkIntentEpicContract is the closed, deterministic handoff from a project's
// Orchestrator to Flowbee. Flowbee chooses the branch and opaque epic identity;
// issue references remain metadata and never become independently owned work.
type WorkIntentEpicContract struct {
	Slug         string   `json:"slug"`
	Title        string   `json:"title"`
	Repositories []string `json:"repositories"`
	DeliveryRepo string   `json:"delivery_repo"`
	SpecPath     string   `json:"spec_path"`
	Scope        []string `json:"scope"`
	IssueRefs    []string `json:"issue_refs,omitempty"`
	Acceptance   []string `json:"acceptance"`
}

type PreparedWorkIntentEpicContract struct {
	ID, ProjectID, WorkIntentID, SourceArtifactSHA256 string
	IntentVersion, ContractVersion                    int
	ContractRef, ContractSHA256, ContractJSON         string
	OrchestratorBindingID, SubmissionKey, State       string
	AdmittedEpicID                                    string
	CreatedAt, AdmittedAt                             time.Time
}

type RecordWorkIntentEpicContractInput struct {
	ProjectID, WorkIntentID, SourceArtifactSHA256 string
	IntentVersion, ExpectedStateVersion           int
	ContractVersion                               int
	ContractRef, ContractSHA256                   string
	Contract                                      WorkIntentEpicContract
	OrchestratorBindingID, SubmissionKey          string
}

type WorkIntentAdmissionReconcileResult struct {
	Scanned, Admitted, Held int
}

func canonicalWorkIntentEpicContract(contract WorkIntentEpicContract) ([]byte, error) {
	if !epicContractSlugPattern.MatchString(contract.Slug) || strings.TrimSpace(contract.Title) == "" || len(contract.Title) > 200 {
		return nil, fmt.Errorf("%w: slug or title", ErrWorkIntentContractInvalid)
	}
	if len(contract.Repositories) == 0 || len(contract.Repositories) > 32 || contract.DeliveryRepo == "" {
		return nil, fmt.Errorf("%w: repository set", ErrWorkIntentContractInvalid)
	}
	repos := append([]string(nil), contract.Repositories...)
	sort.Strings(repos)
	for i, repo := range repos {
		if strings.TrimSpace(repo) != repo || repo == "" || strings.ContainsAny(repo, "\r\n\x00") ||
			i > 0 && repo == repos[i-1] {
			return nil, fmt.Errorf("%w: repository identity", ErrWorkIntentContractInvalid)
		}
	}
	deliveryFound := false
	for _, repo := range repos {
		if repo == contract.DeliveryRepo {
			deliveryFound = true
		}
	}
	if !deliveryFound {
		return nil, fmt.Errorf("%w: delivery repository is outside repository set", ErrWorkIntentContractInvalid)
	}
	cleanPath := path.Clean(contract.SpecPath)
	if cleanPath != contract.SpecPath || strings.HasPrefix(cleanPath, "../") || path.IsAbs(cleanPath) ||
		!strings.HasPrefix(cleanPath, "epics/") || !strings.HasSuffix(cleanPath, ".md") {
		return nil, fmt.Errorf("%w: spec path", ErrWorkIntentContractInvalid)
	}
	if len(contract.Scope) == 0 || len(contract.Scope) > 256 || len(contract.Acceptance) == 0 || len(contract.Acceptance) > 256 {
		return nil, fmt.Errorf("%w: scope or acceptance is empty/oversized", ErrWorkIntentContractInvalid)
	}
	for _, values := range [][]string{contract.Scope, contract.IssueRefs, contract.Acceptance} {
		for _, value := range values {
			if strings.TrimSpace(value) == "" || len(value) > 1_000 || strings.ContainsRune(value, '\x00') {
				return nil, fmt.Errorf("%w: blank or oversized contract item", ErrWorkIntentContractInvalid)
			}
		}
	}
	contract.Repositories = repos
	return json.Marshal(contract)
}

func WorkIntentEpicContractSHA256(contract WorkIntentEpicContract) (string, error) {
	payload, err := canonicalWorkIntentEpicContract(contract)
	if err != nil {
		return "", err
	}
	hash := sha256.Sum256(payload)
	return "sha256:" + hex.EncodeToString(hash[:]), nil
}

func (s *Store) RecordWorkIntentEpicContract(ctx context.Context,
	in RecordWorkIntentEpicContractInput, now time.Time) (PreparedWorkIntentEpicContract, error) {
	if in.ProjectID == "" || in.WorkIntentID == "" || in.IntentVersion < 1 ||
		in.ExpectedStateVersion < 1 || in.ContractVersion < 1 || in.ContractRef == "" ||
		!validSHA256(in.SourceArtifactSHA256) || !validSHA256(in.ContractSHA256) ||
		in.OrchestratorBindingID == "" || in.SubmissionKey == "" {
		return PreparedWorkIntentEpicContract{}, ErrWorkIntentContractInvalid
	}
	payload, err := canonicalWorkIntentEpicContract(in.Contract)
	if err != nil {
		return PreparedWorkIntentEpicContract{}, err
	}
	hash := sha256.Sum256(payload)
	if in.ContractSHA256 != "sha256:"+hex.EncodeToString(hash[:]) {
		return PreparedWorkIntentEpicContract{}, fmt.Errorf("%w: contract hash does not match canonical payload", ErrWorkIntentContractInvalid)
	}
	wantKey, err := workintent.AdmissionKey(workintent.Intent{ID: in.WorkIntentID,
		ProjectID: in.ProjectID, Version: in.IntentVersion})
	if err != nil || in.SubmissionKey != wantKey {
		return PreparedWorkIntentEpicContract{}, fmt.Errorf("%w: submission key", ErrWorkIntentContractFenced)
	}
	contractID := "intent-contract-" + stableID(in.ProjectID+":"+in.SubmissionKey)
	err = s.tx(ctx, func(tx *sql.Tx) error {
		var state, artifactHash, registration string
		var intentVersion, stateVersion int
		if err := tx.QueryRowContext(ctx, `SELECT state,intent_version,state_version,
			artifact_sha256,orchestrator_registration FROM work_intents
			WHERE project_id=? AND id=?`, in.ProjectID, in.WorkIntentID).
			Scan(&state, &intentVersion, &stateVersion, &artifactHash, &registration); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrWorkIntentNotFound
			}
			return err
		}
		if state != string(workintent.StateOrchestrating) || intentVersion != in.IntentVersion ||
			stateVersion != in.ExpectedStateVersion || artifactHash != in.SourceArtifactSHA256 {
			// An exact lost-ack replay may arrive after the projection advanced.
			var existingSourceHash, existingRef, existingHash, existingBinding, existingKey string
			var existingContractVersion int
			err := tx.QueryRowContext(ctx, `SELECT contract_sha256,orchestrator_binding_id,
				submission_key,source_artifact_sha256,contract_version,contract_ref
				FROM work_intent_epic_contracts WHERE project_id=? AND work_intent_id=?
				AND intent_version=?`, in.ProjectID, in.WorkIntentID, in.IntentVersion).
				Scan(&existingHash, &existingBinding, &existingKey, &existingSourceHash,
					&existingContractVersion, &existingRef)
			if err == nil && existingHash == in.ContractSHA256 &&
				existingBinding == in.OrchestratorBindingID && existingKey == in.SubmissionKey &&
				existingSourceHash == in.SourceArtifactSHA256 &&
				existingContractVersion == in.ContractVersion && existingRef == in.ContractRef {
				return nil
			}
			return ErrWorkIntentContractFenced
		}
		var bindingProject, workerIdentity, role, bindingState string
		if err := tx.QueryRowContext(ctx, `SELECT project_id,worker_identity,role,state
			FROM driver_session_bindings WHERE binding_id=?`, in.OrchestratorBindingID).
			Scan(&bindingProject, &workerIdentity, &role, &bindingState); err != nil {
			return ErrWorkIntentContractFenced
		}
		if bindingProject != in.ProjectID || workerIdentity != registration ||
			role != DriverOrchestratorRole || bindingState != "active" {
			return ErrWorkIntentContractFenced
		}
		stamp := now.UTC().Format(rfc3339)
		_, err := tx.ExecContext(ctx, `INSERT INTO work_intent_epic_contracts
			(id,project_id,work_intent_id,intent_version,source_artifact_sha256,
			 contract_version,contract_ref,contract_sha256,contract_json,
			 orchestrator_binding_id,submission_key,state,created_at)
			VALUES (?,?,?,?,?,?,?,?,?,?,?,'prepared',?)`, contractID, in.ProjectID,
			in.WorkIntentID, in.IntentVersion, in.SourceArtifactSHA256, in.ContractVersion,
			in.ContractRef, in.ContractSHA256, string(payload), in.OrchestratorBindingID,
			in.SubmissionKey, stamp)
		if err != nil {
			return err
		}
		res, err := tx.ExecContext(ctx, `UPDATE work_intents SET state='submitting',
			state_version=state_version+1,epic_contract_ref=?,epic_contract_sha256=?,
			hold_kind='',hold_reason='',route_due_at=?,updated_at=? WHERE project_id=? AND id=?
			AND state='orchestrating' AND state_version=?`, in.ContractRef, in.ContractSHA256,
			now.Add(10*time.Minute).UTC().Format(rfc3339), stamp, in.ProjectID,
			in.WorkIntentID, in.ExpectedStateVersion)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return ErrWorkIntentContractFenced
		}
		return appendDecisionControlEventTx(ctx, tx, in.ProjectID, "",
			"work_intent_epic_contract_prepared", string(workintent.StateOrchestrating),
			string(workintent.StateSubmitting), in.ExpectedStateVersion+1, "orchestrator",
			workerIdentity, string(payload), now)
	})
	if err != nil {
		return PreparedWorkIntentEpicContract{}, err
	}
	return s.GetPreparedWorkIntentEpicContract(ctx, in.ProjectID, in.WorkIntentID, in.IntentVersion)
}

func (s *Store) GetPreparedWorkIntentEpicContract(ctx context.Context, projectID, intentID string,
	intentVersion int) (PreparedWorkIntentEpicContract, error) {
	return scanPreparedWorkIntentEpicContract(s.DB.QueryRowContext(ctx, `SELECT id,project_id,
		work_intent_id,intent_version,source_artifact_sha256,contract_version,contract_ref,
		contract_sha256,contract_json,orchestrator_binding_id,submission_key,state,
		COALESCE(admitted_epic_id,''),created_at,admitted_at FROM work_intent_epic_contracts
		WHERE project_id=? AND work_intent_id=? AND intent_version=?`, projectID, intentID, intentVersion))
}

func scanPreparedWorkIntentEpicContract(row interface{ Scan(...any) error }) (PreparedWorkIntentEpicContract, error) {
	var out PreparedWorkIntentEpicContract
	var created, admitted string
	err := row.Scan(&out.ID, &out.ProjectID, &out.WorkIntentID, &out.IntentVersion,
		&out.SourceArtifactSHA256, &out.ContractVersion, &out.ContractRef,
		&out.ContractSHA256, &out.ContractJSON, &out.OrchestratorBindingID,
		&out.SubmissionKey, &out.State, &out.AdmittedEpicID, &created, &admitted)
	if err != nil {
		return out, err
	}
	out.CreatedAt, out.AdmittedAt = parseOptionalTime(created), parseOptionalTime(admitted)
	return out, nil
}

func (s *Store) ReconcileWorkIntentAdmissions(ctx context.Context, now time.Time) (WorkIntentAdmissionReconcileResult, error) {
	var out WorkIntentAdmissionReconcileResult
	// A prepared contract is a durable admission obligation, but the owning work
	// intent remains the authority for whether that obligation may run. In
	// particular, an operator can pause after contract preparation but before this
	// reconciler executes. Joining the exact intent incarnation prevents a restart
	// from admitting through that hold (and prevents terminal intent rows from
	// becoming poison entries that are retried forever).
	rows, err := s.DB.QueryContext(ctx, `SELECT c.id FROM work_intent_epic_contracts c
		JOIN work_intents w ON w.project_id=c.project_id AND w.id=c.work_intent_id
			AND w.intent_version=c.intent_version
		WHERE c.state='prepared' AND w.state='submitting' AND w.hold_kind<>'paused'
		ORDER BY c.created_at,c.id`)
	if err != nil {
		return out, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return out, err
		}
		ids = append(ids, id)
	}
	if err := rows.Close(); err != nil {
		return out, err
	}
	for _, id := range ids {
		out.Scanned++
		err := s.admitPreparedWorkIntentContract(ctx, id, now)
		if err == nil {
			out.Admitted++
			continue
		}
		if errors.Is(err, ErrEpicScopeOverlap) || errors.Is(err, ErrEpicRunExists) ||
			errors.Is(err, ErrEpicAdmissionConflict) ||
			errors.Is(err, ErrEpicDistinctReviewerUnavailable) {
			if holdErr := s.holdPreparedWorkIntentAdmission(ctx, id, err.Error(), now); holdErr != nil {
				return out, holdErr
			}
			out.Held++
			continue
		}
		return out, err
	}
	return out, nil
}

func (s *Store) admitPreparedWorkIntentContract(ctx context.Context, contractID string, now time.Time) error {
	var prepared PreparedWorkIntentEpicContract
	var slug, title, repo, specPath, scopeJSON string
	err := s.DB.QueryRowContext(ctx, `SELECT c.id,c.project_id,c.work_intent_id,c.intent_version,
		c.source_artifact_sha256,c.contract_version,c.contract_ref,c.contract_sha256,
		c.contract_json,c.orchestrator_binding_id,c.submission_key,c.state,
		COALESCE(c.admitted_epic_id,''),c.created_at,c.admitted_at,
		json_extract(c.contract_json,'$.slug'),json_extract(c.contract_json,'$.title'),
		json_extract(c.contract_json,'$.delivery_repo'),json_extract(c.contract_json,'$.spec_path'),
		json_extract(c.contract_json,'$.scope') FROM work_intent_epic_contracts c WHERE c.id=?`,
		contractID).Scan(&prepared.ID, &prepared.ProjectID, &prepared.WorkIntentID,
		&prepared.IntentVersion, &prepared.SourceArtifactSHA256, &prepared.ContractVersion,
		&prepared.ContractRef, &prepared.ContractSHA256, &prepared.ContractJSON,
		&prepared.OrchestratorBindingID, &prepared.SubmissionKey, &prepared.State,
		&prepared.AdmittedEpicID, new(string), new(string), &slug, &title, &repo, &specPath, &scopeJSON)
	if err != nil {
		return err
	}
	if prepared.State != "prepared" {
		return nil
	}
	var scope []string
	if err := json.Unmarshal([]byte(scopeJSON), &scope); err != nil {
		return err
	}
	epicID := "epic-" + stableID(prepared.ProjectID+":"+prepared.SubmissionKey)
	return s.AddEpicRun(ctx, EpicRun{
		ID: epicID, ProjectID: prepared.ProjectID, Slug: slug,
		AdmissionKey: prepared.SubmissionKey, WorkIntentID: prepared.WorkIntentID,
		IntentVersion: prepared.IntentVersion, ContractHash: prepared.ContractSHA256,
		Repo: repo, FilePath: specPath, Title: title, Scope: scope,
		Branch: epicBranchForProject(prepared.ProjectID, slug),
	}, 1, now)
}

// epicBranchForProject preserves the single-project branch contract while
// preventing two non-default projects with the same human slug from sharing
// Git authority. Project IDs and slugs are already constrained to a conservative
// lowercase path-safe alphabet before this helper is reached.
func epicBranchForProject(projectID, slug string) string {
	if projectID == "" || projectID == "default" {
		return "epic/" + slug
	}
	return "epic/" + projectID + "/" + slug
}

// epicSessionNameForProject is a display/compatibility name only; Driver's
// stable identity tuple remains the routing authority. Keeping the historical
// default-project spelling avoids breaking existing operators while the
// project component prevents same-slug sessions from colliding on a shared
// host during the Phase-2 migration window.
func epicSessionNameForProject(projectID, slug string) string {
	if projectID == "" || projectID == "default" {
		return "epic-" + slug
	}
	return "epic-" + projectID + "-" + slug
}

func (s *Store) holdPreparedWorkIntentAdmission(ctx context.Context, contractID, detail string, now time.Time) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		var intentID, projectID string
		var version int
		if err := tx.QueryRowContext(ctx, `SELECT w.id,w.project_id,w.state_version
			FROM work_intents w JOIN work_intent_epic_contracts c ON c.work_intent_id=w.id
			WHERE c.id=?`, contractID).Scan(&intentID, &projectID, &version); err != nil {
			return err
		}
		stamp := now.UTC().Format(rfc3339)
		if _, err := tx.ExecContext(ctx, `UPDATE work_intents SET hold_kind='epic_admission_blocked',
			hold_reason=?,state_version=state_version+1,route_due_at='',updated_at=?
			WHERE id=? AND state='submitting' AND state_version=?`, detail, stamp, intentID, version); err != nil {
			return err
		}
		dedup := "work_intent_epic_admission_blocked:" + intentID
		payload, _ := json.Marshal(map[string]string{"work_intent_id": intentID, "reason": detail})
		return ensureControlAlertTx(ctx, tx, projectID, "", "work_intent_epic_admission_blocked", dedup, string(payload), now)
	})
}
