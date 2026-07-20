package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// metaEpicDedicatedWorkersV2 is written in the same transaction that backfills
// every live Phase-1 epic. A process restart therefore cannot observe the
// feature as authoritative while an older epic is still missing its two worker
// obligations.
const metaEpicDedicatedWorkersV2 = "runtime_epic_dedicated_workers_v2"

// SetDurableEpicDedicatedWorkersV2 atomically activates the dedicated-worker
// model. Enabling is deliberately more than a boolean write: it first plants
// and validates exactly one builder and one distinct-family reviewer contract,
// including their credential-install obligations, for every non-terminal epic.
// Repeated and concurrent calls are idempotent because SQLite serializes this
// transaction and the worker ledgers have (epic_id, role) primary keys.
func (s *Store) SetDurableEpicDedicatedWorkersV2(ctx context.Context, enabled bool, now time.Time) error {
	s.epicWorkerActivationMu.Lock()
	defer s.epicWorkerActivationMu.Unlock()
	if !enabled {
		return s.tx(ctx, func(tx *sql.Tx) error {
			var obligations int
			if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM epic_worker_sessions`).Scan(&obligations); err != nil {
				return err
			}
			if obligations != 0 {
				return fmt.Errorf("dedicated epic workers are one-way after materialization: %d obligations remain", obligations)
			}
			_, err := tx.ExecContext(ctx, `INSERT INTO flowbee_meta(key,value) VALUES (?, '0')
				ON CONFLICT(key) DO UPDATE SET value=excluded.value`, metaEpicDedicatedWorkersV2)
			return err
		})
	}
	// Resolve every candidate's authoritative context before the activation
	// transaction. The transaction re-reads the candidate set; an epic admitted in
	// between is therefore a visible fail-closed retry, never a prose/path fallback.
	activationMaterials := make(map[string]*EpicWorkerBootstrapMaterials)
	var materialCandidates []EpicRun
	materialRows, err := s.DB.QueryContext(ctx, `SELECT e.id,e.project_id,e.slug,e.repo,e.file_path,e.title,
		e.scope_json,e.branch,e.builder_model_family,e.contract_hash,e.work_intent_id,e.intent_version FROM epics e
		JOIN epic_deliveries d ON d.epic_id=e.id
		WHERE d.state NOT IN ('complete','abandoned') ORDER BY e.project_id,e.id`)
	if err != nil {
		return err
	}
	for materialRows.Next() {
		var e EpicRun
		var scopeJSON string
		if err := materialRows.Scan(&e.ID, &e.ProjectID, &e.Slug, &e.Repo, &e.FilePath,
			&e.Title, &scopeJSON, &e.Branch, &e.BuilderModelFamily, &e.ContractHash,
			&e.WorkIntentID, &e.IntentVersion); err != nil {
			materialRows.Close()
			return err
		}
		e.Scope = unmarshalStrings(scopeJSON)
		materialCandidates = append(materialCandidates, e)
	}
	if err := materialRows.Close(); err != nil {
		return err
	}
	for _, e := range materialCandidates {
		material, err := s.resolveEpicWorkerBootstrapMaterials(ctx, e)
		if err != nil {
			return fmt.Errorf("resolve dedicated-worker activation context for epic %s: %w", e.ID, err)
		}
		activationMaterials[e.ID] = material
	}
	return s.tx(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `SELECT e.id,e.project_id,e.slug,e.repo,e.file_path,e.title,
			e.scope_json,e.branch,e.builder_model_family,d.builder_model_family,d.reviewer_model_family
			FROM epics e JOIN epic_deliveries d ON d.epic_id=e.id
			WHERE d.state NOT IN ('complete','abandoned') ORDER BY e.project_id,e.id`)
		if err != nil {
			return err
		}
		type candidate struct {
			epic                       EpicRun
			epicFamily, deliveryFamily string
			reviewerFamily             string
		}
		var candidates []candidate
		for rows.Next() {
			var c candidate
			var scopeJSON string
			if err := rows.Scan(&c.epic.ID, &c.epic.ProjectID, &c.epic.Slug, &c.epic.Repo,
				&c.epic.FilePath, &c.epic.Title, &scopeJSON, &c.epic.Branch,
				&c.epicFamily, &c.deliveryFamily, &c.reviewerFamily); err != nil {
				rows.Close()
				return err
			}
			c.epic.Scope = unmarshalStrings(scopeJSON)
			c.epicFamily = firstNonemptyFamily(c.epicFamily)
			c.deliveryFamily = firstNonemptyFamily(c.deliveryFamily)
			c.reviewerFamily = firstNonemptyFamily(c.reviewerFamily)
			if c.epicFamily != "" && c.deliveryFamily != "" && c.epicFamily != c.deliveryFamily {
				rows.Close()
				return fmt.Errorf("epic %s has ambiguous builder family authority: epic=%s delivery=%s; dedicated-worker activation requires explicit migration",
					c.epic.ID, c.epicFamily, c.deliveryFamily)
			}
			c.epic.BuilderModelFamily = firstNonemptyFamily(c.deliveryFamily, c.epicFamily)
			if c.epic.BuilderModelFamily == "" {
				rows.Close()
				return fmt.Errorf("epic %s has no authoritative builder family; dedicated-worker activation requires explicit migration", c.epic.ID)
			}
			candidates = append(candidates, c)
		}
		if err := rows.Close(); err != nil {
			return err
		}

		for _, c := range candidates {
			material := activationMaterials[c.epic.ID]
			if material == nil {
				return fmt.Errorf("epic %s has no pre-transaction authoritative worker context; retry activation", c.epic.ID)
			}
			c.epic.WorkerBootstrapMaterials = material
			reviewerSelectedFromCapacity := false
			if c.reviewerFamily == "" {
				decision, family, err := distinctReviewerFamilyTx(ctx, tx, c.epic.ProjectID,
					c.epic.BuilderModelFamily, now, 5*time.Minute)
				if err != nil {
					return err
				}
				if !decision.Routable || family == "" {
					return fmt.Errorf("epic %s has no authoritative distinct reviewer family; dedicated-worker activation requires explicit migration or fresh capacity proof: %s",
						c.epic.ID, strings.Join(decision.Reasons, "; "))
				}
				c.reviewerFamily = family
				reviewerSelectedFromCapacity = true
			}
			if c.reviewerFamily == c.epic.BuilderModelFamily {
				return fmt.Errorf("epic %s reviewer family %s is not distinct from its builder; dedicated-worker activation requires explicit migration",
					c.epic.ID, c.reviewerFamily)
			}
			// Old rows may predate the builder-family columns. Persist the family
			// used by the immutable worker contract so later routing cannot infer a
			// different answer.
			if _, err := tx.ExecContext(ctx, `UPDATE epics SET builder_model_family=?
				WHERE id=? AND builder_model_family=''`, c.epic.BuilderModelFamily, c.epic.ID); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `UPDATE epic_deliveries SET builder_model_family=?
				WHERE epic_id=? AND builder_model_family=''`, c.epic.BuilderModelFamily, c.epic.ID); err != nil {
				return err
			}
			if reviewerSelectedFromCapacity {
				res, err := tx.ExecContext(ctx, `UPDATE epic_deliveries SET reviewer_model_family=?
					WHERE epic_id=? AND reviewer_model_family=''`, c.reviewerFamily, c.epic.ID)
				if err != nil {
					return err
				}
				if n, err := res.RowsAffected(); err != nil || n != 1 {
					if err != nil {
						return err
					}
					return fmt.Errorf("epic %s reviewer family authority changed during activation", c.epic.ID)
				}
			}

			var existing int
			if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM epic_worker_sessions
				WHERE epic_id=?`, c.epic.ID).Scan(&existing); err != nil {
				return err
			}
			switch existing {
			case 0:
				if err := insertEpicWorkerSessionsTx(ctx, tx, c.epic, c.reviewerFamily, now); err != nil {
					return fmt.Errorf("backfill dedicated workers for epic %s: %w", c.epic.ID, err)
				}
			case 2:
				// A concurrently admitted v2 epic already has its full plan. Validate
				// it below rather than attempting a duplicate INSERT.
			default:
				return fmt.Errorf("epic %s has partial dedicated worker plan: %d/2", c.epic.ID, existing)
			}
			if err := validateExactlyTwoEpicWorkerPlansTx(ctx, tx, c.epic.ProjectID,
				c.epic.ID, c.epic.BuilderModelFamily); err != nil {
				return err
			}
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO flowbee_meta(key,value) VALUES (?, '1')
			ON CONFLICT(key) DO UPDATE SET value=excluded.value`, metaEpicDedicatedWorkersV2)
		return err
	})
}

// DurableEpicDedicatedWorkersV2 reports the transactionally activated worker
// boundary. Corrupt values fail closed as enabled and return an error.
func (s *Store) DurableEpicDedicatedWorkersV2(ctx context.Context) (bool, error) {
	var value string
	err := s.DB.QueryRowContext(ctx, `SELECT value FROM flowbee_meta WHERE key=?`,
		metaEpicDedicatedWorkersV2).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	switch value {
	case "0":
		return false, nil
	case "1":
		return true, nil
	default:
		return true, errors.New("invalid durable epic-dedicated-workers v2 activation value")
	}
}

func dedicatedEpicWorkersEnabledTx(ctx context.Context, s *Store, tx *sql.Tx) (bool, error) {
	if s.EnableEpicDedicatedWorkersV2 {
		return true, nil
	}
	var value string
	err := tx.QueryRowContext(ctx, `SELECT value FROM flowbee_meta WHERE key=?`,
		metaEpicDedicatedWorkersV2).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if value == "1" {
		return true, nil
	}
	if value == "0" {
		return false, nil
	}
	return true, errors.New("invalid durable epic-dedicated-workers v2 activation value")
}

func firstNonemptyFamily(values ...string) string {
	for _, value := range values {
		if value = strings.ToLower(strings.TrimSpace(value)); value != "" {
			return value
		}
	}
	return ""
}

// validateExactlyTwoEpicWorkerPlansTx is the pre-effect invariant used by both
// builder and reviewer action commits. It checks the immutable session contract
// and the separate credential material obligation; a missing SQL row is never
// interpreted as a legacy/no-op success after activation.
func validateExactlyTwoEpicWorkerPlansTx(ctx context.Context, tx *sql.Tx, projectID,
	epicID, expectedBuilderFamily string) error {
	rows, err := tx.QueryContext(ctx, `SELECT w.worker_role,w.model_family,w.worker_identity,
		w.flowbee_identity,w.lifecycle_key,w.display_name,w.bootstrap_payload,w.bootstrap_sha256,
		c.project_id,c.worker_role,c.flowbee_identity,c.install_ref
		FROM epic_worker_sessions w
		LEFT JOIN epic_worker_credentials c ON c.epic_id=w.epic_id AND c.worker_role=w.worker_role
		WHERE w.epic_id=? AND w.project_id=? ORDER BY w.worker_role`, epicID, projectID)
	if err != nil {
		return err
	}
	defer rows.Close()
	type plan struct{ family string }
	plans := map[string]plan{}
	for rows.Next() {
		var role, family, workerIdentity, flowbeeIdentity, lifecycleKey, displayName string
		var payload, payloadHash string
		var credentialProject, credentialRole, credentialIdentity, installRef sql.NullString
		if err := rows.Scan(&role, &family, &workerIdentity, &flowbeeIdentity, &lifecycleKey,
			&displayName, &payload, &payloadHash, &credentialProject, &credentialRole,
			&credentialIdentity, &installRef); err != nil {
			return err
		}
		if _, exists := plans[role]; exists || (role != "builder" && role != "reviewer") {
			return fmt.Errorf("epic %s dedicated worker roles are not exactly builder+reviewer", epicID)
		}
		if family == "" || workerIdentity == "" || flowbeeIdentity == "" || lifecycleKey == "" ||
			displayName == "" || payload == "" || payloadHash == "" {
			return fmt.Errorf("epic %s %s worker plan is incomplete", epicID, role)
		}
		var manifest epicWorkerBootstrap
		if err := json.Unmarshal([]byte(payload), &manifest); err != nil || manifest.Format != EpicWorkerBootstrapFormat ||
			manifest.ProjectID != projectID || manifest.EpicID != epicID || manifest.Role != role ||
			manifest.Family != family || manifest.FlowbeeWorkerIdentity != flowbeeIdentity ||
			manifest.CredentialInstallRef == "" || manifest.EpicSpecGoalFormat != EpicWorkerGoalFormat ||
			manifest.EpicSpecGoalUTF8 == "" || manifest.EpicSpecGoalSHA256 != sha256String(manifest.EpicSpecGoalUTF8) ||
			!validSHA256Text(manifest.AdmissionContractSHA256) ||
			manifest.SourceArtifactSHA256 != manifest.EpicSpecGoalSHA256 ||
			manifest.RoleCharter == "" || manifest.RoleCharterSHA256 != sha256String(manifest.RoleCharter) ||
			manifest.DisciplineKind != role || manifest.DisciplineUTF8 == "" ||
			manifest.DisciplineSHA256 != sha256String(manifest.DisciplineUTF8) ||
			len(manifest.ReferenceDocuments) == 0 ||
			manifest.ReferenceManifestSHA256 != epicWorkerReferenceManifestHash(manifest.ReferenceDocuments) {
			return fmt.Errorf("epic %s %s worker bootstrap does not match its immutable plan", epicID, role)
		}
		for _, doc := range manifest.ReferenceDocuments {
			if !validEpicWorkerReference(doc.Reference) || !validEpicWorkerReferenceFormat(doc.Format) ||
				!validSHA256Text(doc.SHA256) {
				return fmt.Errorf("epic %s %s worker reference material is incomplete or corrupt", epicID, role)
			}
		}
		h := sha256.Sum256([]byte(payload))
		if payloadHash != "sha256:"+hex.EncodeToString(h[:]) {
			return fmt.Errorf("epic %s %s worker bootstrap hash mismatch", epicID, role)
		}
		if !credentialProject.Valid || credentialProject.String != projectID ||
			!credentialRole.Valid || credentialRole.String != role ||
			!credentialIdentity.Valid || credentialIdentity.String != flowbeeIdentity ||
			!installRef.Valid || installRef.String != manifest.CredentialInstallRef {
			return fmt.Errorf("epic %s %s worker credential material is absent or mismatched", epicID, role)
		}
		plans[role] = plan{family: family}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if len(plans) != 2 || plans["builder"].family == "" || plans["reviewer"].family == "" {
		return fmt.Errorf("epic %s requires exactly two valid dedicated worker plans; found %d", epicID, len(plans))
	}
	if plans["builder"].family == plans["reviewer"].family {
		return fmt.Errorf("epic %s dedicated reviewer is not distinct from builder family %s",
			epicID, plans["builder"].family)
	}
	if expectedBuilderFamily != "" && plans["builder"].family != expectedBuilderFamily {
		return fmt.Errorf("epic %s builder plan family %s differs from admitted family %s",
			epicID, plans["builder"].family, expectedBuilderFamily)
	}
	var deliveryBuilder, deliveryReviewer string
	if err := tx.QueryRowContext(ctx, `SELECT builder_model_family,reviewer_model_family
		FROM epic_deliveries WHERE epic_id=? AND project_id=?`, epicID, projectID).
		Scan(&deliveryBuilder, &deliveryReviewer); err != nil {
		return fmt.Errorf("epic %s delivery authority: %w", epicID, err)
	}
	if deliveryBuilder = firstNonemptyFamily(deliveryBuilder); deliveryBuilder != "" &&
		deliveryBuilder != plans["builder"].family {
		return fmt.Errorf("epic %s delivery builder family %s differs from worker plan %s",
			epicID, deliveryBuilder, plans["builder"].family)
	}
	if deliveryReviewer = firstNonemptyFamily(deliveryReviewer); deliveryReviewer != "" &&
		deliveryReviewer != plans["reviewer"].family {
		return fmt.Errorf("epic %s delivery reviewer family %s differs from worker plan %s",
			epicID, deliveryReviewer, plans["reviewer"].family)
	}
	return nil
}

func requireExactlyTwoEpicWorkerPlansTx(ctx context.Context, s *Store, tx *sql.Tx,
	projectID, epicID, expectedBuilderFamily string) error {
	enabled, err := dedicatedEpicWorkersEnabledTx(ctx, s, tx)
	if err != nil {
		return err
	}
	if !enabled {
		return nil
	}
	return validateExactlyTwoEpicWorkerPlansTx(ctx, tx, projectID, epicID, expectedBuilderFamily)
}
