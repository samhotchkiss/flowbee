package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/samhotchkiss/flowbee/internal/ulid"
	"github.com/samhotchkiss/flowbee/internal/workintent"
)

var (
	ErrWorkIntentNotFound         = errors.New("work intent not found")
	ErrWorkIntentFenced           = errors.New("work intent version or artifact fence is stale")
	ErrWorkIntentRouteUnavailable = errors.New("work intent exact Driver route is unavailable")
)

type WorkIntent struct {
	ID, ProjectID, SourceConversationID, SourceMessageID string
	SourceMessageVersion                                 int
	InteractorIncarnationID, Title, Summary              string
	ArtifactRef, ArtifactSHA256                          string
	IntentVersion, StateVersion, Priority                int
	DefinitionComplete                                   bool
	DefinitionEvidenceJSON, DependencyRefsJSON           string
	State                                                workintent.State
	OwnerActorID, RouteTo, OrchestratorRegistration      string
	DeliveryActionID, RouteLeaseID                       string
	RouteEpoch, RouteAttempts                            int
	RouteDueAt, RouteAcknowledgedAt                      time.Time
	EpicContractRef, EpicContractSHA256                  string
	SubmissionIdempotencyKey, AdmittedEpicID             string
	HoldKind, HoldReason                                 string
	NextRetryAt                                          time.Time
	SupersededBy, CancellationReason                     string
	CreatedAt, UpdatedAt                                 time.Time
}

type CreateWorkIntentInput struct {
	ID, ProjectID, SourceConversationID, SourceMessageID string
	SourceMessageVersion                                 int
	InteractorIncarnationID, Title, Summary              string
	ArtifactRef, ArtifactSHA256                          string
	IntentVersion, Priority                              int
	DefinitionComplete                                   bool
	DefinitionEvidence, DependencyRefs                   json.RawMessage
	OwnerActorID, OrchestratorRegistration               string
	RequiredDecisionIDs                                  []string
}

type WorkIntentReconcileResult struct {
	Scanned, Advanced, ActionsCreated, Held int
}

func (s *Store) CreateWorkIntent(ctx context.Context, in CreateWorkIntentInput, now time.Time) (WorkIntent, error) {
	if in.ID == "" {
		in.ID = "intent-" + ulid.New()
	}
	if in.ProjectID == "" || in.SourceMessageID == "" || in.SourceMessageVersion < 1 ||
		in.InteractorIncarnationID == "" || in.Title == "" || in.ArtifactRef == "" ||
		in.IntentVersion < 1 || !validSHA256(in.ArtifactSHA256) || in.OwnerActorID == "" {
		return WorkIntent{}, errors.New("work intent requires project, source/version, interactor incarnation, title, artifact/version/hash, and owner")
	}
	if len(in.Title) > 200 || len(in.Summary) > 4_000 {
		return WorkIntent{}, errors.New("work intent title or summary exceeds its bound")
	}
	if in.Priority == 0 {
		in.Priority = 3
	}
	if in.Priority < 1 || in.Priority > 5 {
		return WorkIntent{}, errors.New("work intent priority must be between 1 and 5")
	}
	definitionEvidence, err := normalizedJSON(in.DefinitionEvidence, "[]")
	if err != nil {
		return WorkIntent{}, fmt.Errorf("definition evidence: %w", err)
	}
	dependencies, err := normalizedJSON(in.DependencyRefs, "[]")
	if err != nil {
		return WorkIntent{}, fmt.Errorf("dependency references: %w", err)
	}
	decisions := append([]string(nil), in.RequiredDecisionIDs...)
	sort.Strings(decisions)
	for i, id := range decisions {
		if id == "" || i > 0 && id == decisions[i-1] {
			return WorkIntent{}, errors.New("required decisions contain an empty or duplicate id")
		}
	}
	err = s.tx(ctx, func(tx *sql.Tx) error {
		var project int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM projects WHERE id=? AND state='active'`, in.ProjectID).Scan(&project); err != nil {
			return err
		}
		if project != 1 {
			return errors.New("work intent project is missing or inactive")
		}
		var existingID, existingConversation, existingInteractor, existingTitle, existingSummary string
		var existingRef, existingHash, existingEvidence, existingDependencies, existingOwner, existingOrchestrator string
		var existingVersion, existingComplete, existingPriority int
		existingErr := tx.QueryRowContext(ctx, `SELECT id,source_conversation_id,
			interactor_incarnation_id,title,summary,artifact_ref,intent_version,artifact_sha256,
			definition_complete,definition_evidence_json,dependency_refs_json,priority,
			owner_actor_id,orchestrator_registration FROM work_intents
			WHERE project_id=? AND source_message_id=? AND source_message_version=?`, in.ProjectID,
			in.SourceMessageID, in.SourceMessageVersion).Scan(&existingID, &existingConversation,
			&existingInteractor, &existingTitle, &existingSummary, &existingRef, &existingVersion,
			&existingHash, &existingComplete, &existingEvidence, &existingDependencies,
			&existingPriority, &existingOwner, &existingOrchestrator)
		if existingErr == nil {
			if existingConversation != in.SourceConversationID || existingInteractor != in.InteractorIncarnationID || existingTitle != in.Title ||
				existingSummary != in.Summary || existingRef != in.ArtifactRef ||
				existingVersion != in.IntentVersion || existingHash != in.ArtifactSHA256 ||
				existingComplete != b2i(in.DefinitionComplete) || existingEvidence != definitionEvidence ||
				existingDependencies != dependencies || existingPriority != in.Priority ||
				existingOwner != in.OwnerActorID || existingOrchestrator != in.OrchestratorRegistration {
				return errors.New("work intent source-message idempotency conflict")
			}
			rows, err := tx.QueryContext(ctx, `SELECT decision_id FROM work_intent_decisions
				WHERE work_intent_id=? ORDER BY decision_id`, existingID)
			if err != nil {
				return err
			}
			var existingDecisions []string
			for rows.Next() {
				var decisionID string
				if err := rows.Scan(&decisionID); err != nil {
					rows.Close()
					return err
				}
				existingDecisions = append(existingDecisions, decisionID)
			}
			if err := rows.Close(); err != nil {
				return err
			}
			if len(existingDecisions) != len(decisions) {
				return errors.New("work intent source-message idempotency conflict")
			}
			for i := range decisions {
				if existingDecisions[i] != decisions[i] {
					return errors.New("work intent source-message idempotency conflict")
				}
			}
			in.ID = existingID
			return nil
		}
		if !errors.Is(existingErr, sql.ErrNoRows) {
			return existingErr
		}
		admissionKey, err := workintent.AdmissionKey(workintent.Intent{
			ID: in.ID, ProjectID: in.ProjectID, Version: in.IntentVersion,
		})
		if err != nil {
			return err
		}
		nowText := now.UTC().Format(rfc3339)
		_, err = tx.ExecContext(ctx, `INSERT INTO work_intents
			(id,project_id,source_conversation_id,source_message_id,source_message_version,
			 interactor_incarnation_id,title,summary,artifact_ref,intent_version,artifact_sha256,
			 definition_complete,definition_evidence_json,dependency_refs_json,state,state_version,
			 priority,owner_actor_id,route_to,orchestrator_registration,submission_idempotency_key,
			 created_at,updated_at)
			VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,'captured',1,?,?,'orchestrator',?,?,?,?)`,
			in.ID, in.ProjectID, in.SourceConversationID, in.SourceMessageID, in.SourceMessageVersion,
			in.InteractorIncarnationID, in.Title, in.Summary, in.ArtifactRef, in.IntentVersion,
			in.ArtifactSHA256, b2i(in.DefinitionComplete), definitionEvidence, dependencies,
			in.Priority, in.OwnerActorID, in.OrchestratorRegistration, admissionKey, nowText, nowText)
		if err != nil {
			return err
		}
		for _, decisionID := range decisions {
			var projectID, sha string
			var version int
			if err := tx.QueryRowContext(ctx, `SELECT project_id,subject_version,subject_sha256
				FROM decision_requests WHERE id=?`, decisionID).Scan(&projectID, &version, &sha); err != nil {
				return fmt.Errorf("required decision %s: %w", decisionID, err)
			}
			if projectID != in.ProjectID || version != in.IntentVersion || sha != in.ArtifactSHA256 {
				return fmt.Errorf("required decision %s is not bound to the exact intent artifact", decisionID)
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO work_intent_decisions
				(work_intent_id,decision_id,subject_version,subject_sha256,required,created_at)
				VALUES (?,?,?,?,1,?)`, in.ID, decisionID, version, sha, nowText); err != nil {
				return err
			}
		}
		payload, _ := json.Marshal(map[string]any{"work_intent_id": in.ID, "intent_version": in.IntentVersion,
			"artifact_sha256": in.ArtifactSHA256, "required_decisions": decisions})
		return appendDecisionControlEventTx(ctx, tx, in.ProjectID, "", "work_intent_captured",
			"", string(workintent.StateCaptured), 1, "interactor", in.OwnerActorID, string(payload), now)
	})
	if err != nil {
		return WorkIntent{}, err
	}
	return s.GetWorkIntent(ctx, in.ProjectID, in.ID)
}

func (s *Store) GetWorkIntent(ctx context.Context, projectID, id string) (WorkIntent, error) {
	return scanWorkIntent(s.DB.QueryRowContext(ctx, workIntentSelect+` WHERE project_id=? AND id=?`, projectID, id))
}

func (s *Store) ListWorkIntents(ctx context.Context, projectID string) ([]WorkIntent, error) {
	query, args := workIntentSelect+` WHERE project_id=? ORDER BY priority,created_at,id`, []any{projectID}
	if projectID == "" {
		query, args = workIntentSelect+` ORDER BY priority,project_id,created_at,id`, nil
	}
	rows, err := s.DB.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []WorkIntent
	for rows.Next() {
		item, err := scanWorkIntent(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

func (s *Store) SetWorkIntentDefinition(ctx context.Context, projectID, id string, expectedStateVersion,
	intentVersion int, artifactSHA string, complete bool, evidence json.RawMessage, actor string, now time.Time) error {
	if projectID == "" || id == "" || expectedStateVersion < 1 || intentVersion < 1 ||
		!validSHA256(artifactSHA) || actor == "" {
		return ErrWorkIntentFenced
	}
	normalized, err := normalizedJSON(evidence, "[]")
	if err != nil {
		return err
	}
	return s.tx(ctx, func(tx *sql.Tx) error {
		var state string
		res, err := tx.ExecContext(ctx, `UPDATE work_intents SET definition_complete=?,
			definition_evidence_json=?,state_version=state_version+1,updated_at=? WHERE project_id=? AND id=? AND state_version=?
			AND intent_version=? AND artifact_sha256=? AND state IN ('captured','defining','awaiting_decision')`,
			b2i(complete), normalized, now.UTC().Format(rfc3339), projectID, id, expectedStateVersion,
			intentVersion, artifactSHA)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return ErrWorkIntentFenced
		}
		_ = tx.QueryRowContext(ctx, `SELECT state FROM work_intents WHERE id=?`, id).Scan(&state)
		payload, _ := json.Marshal(map[string]any{"work_intent_id": id, "definition_complete": complete,
			"artifact_sha256": artifactSHA})
		return appendDecisionControlEventTx(ctx, tx, projectID, "", "work_intent_definition_updated",
			state, state, expectedStateVersion+1, "interactor", actor, string(payload), now)
	})
}

func (s *Store) RegisterWorkIntentOrchestrator(ctx context.Context, projectID, id string,
	expectedStateVersion int, registration, actor string, now time.Time) error {
	if projectID == "" || id == "" || expectedStateVersion < 1 || registration == "" || actor == "" {
		return ErrWorkIntentFenced
	}
	return s.tx(ctx, func(tx *sql.Tx) error {
		var state string
		if err := tx.QueryRowContext(ctx, `SELECT state FROM work_intents WHERE project_id=? AND id=?`,
			projectID, id).Scan(&state); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrWorkIntentNotFound
			}
			return err
		}
		res, err := tx.ExecContext(ctx, `UPDATE work_intents SET orchestrator_registration=?,
			state_version=state_version+1,hold_kind='',hold_reason='',route_due_at='',updated_at=?
			WHERE project_id=? AND id=? AND state_version=? AND state NOT IN ('admitted','cancelled','superseded')`,
			registration, now.UTC().Format(rfc3339), projectID, id, expectedStateVersion)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return ErrWorkIntentFenced
		}
		if _, err := tx.ExecContext(ctx, `UPDATE attention_items SET state='resolved',
			resolution='orchestrator_registered',resolved_at=?,updated_at=? WHERE dedup_key=?
			AND state IN ('open','leased','delivering','awaiting_ack')`, now.UTC().Format(rfc3339),
			now.UTC().Format(rfc3339), "work_intent_promotion_stalled:"+id); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE control_alerts SET state='acknowledged',
			acknowledged_at=?,updated_at=? WHERE dedup_key=? AND state IN ('pending','delivering')`,
			now.UTC().Format(rfc3339), now.UTC().Format(rfc3339),
			"work_intent_promotion_stalled:"+id); err != nil {
			return err
		}
		payload, _ := json.Marshal(map[string]string{"work_intent_id": id, "orchestrator_registration": registration})
		return appendDecisionControlEventTx(ctx, tx, projectID, "", "work_intent_orchestrator_registered",
			state, state, expectedStateVersion+1, "orchestrator", actor, string(payload), now)
	})
}

func (s *Store) ReconcileWorkIntents(ctx context.Context, now time.Time, ackTimeout time.Duration) (WorkIntentReconcileResult, error) {
	var out WorkIntentReconcileResult
	if ackTimeout <= 0 {
		ackTimeout = 10 * time.Minute
	}
	err := s.tx(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, workIntentSelect+` WHERE state NOT IN ('admitted','cancelled','superseded') ORDER BY created_at,id`)
		if err != nil {
			return err
		}
		var intents []WorkIntent
		for rows.Next() {
			item, err := scanWorkIntent(rows)
			if err != nil {
				rows.Close()
				return err
			}
			intents = append(intents, item)
		}
		if err := rows.Close(); err != nil {
			return err
		}
		for _, item := range intents {
			out.Scanned++
			if item.HoldKind == "paused" {
				continue
			}
			gates, err := workIntentGatesTx(ctx, tx, item)
			if err != nil {
				return err
			}
			core := workintent.Intent{
				ID: item.ID, ProjectID: item.ProjectID, Version: item.IntentVersion,
				StateVersion: item.StateVersion, ArtifactSHA256: item.ArtifactSHA256,
				State: item.State, DefinitionComplete: item.DefinitionComplete, Gates: gates,
				OrchestratorRegistration: item.OrchestratorRegistration,
				OrchestratorActionID:     item.DeliveryActionID, AdmittedEpicID: item.AdmittedEpicID,
			}
			transition, err := workintent.Advance(core)
			if err != nil {
				return fmt.Errorf("advance work intent %s: %w", item.ID, err)
			}
			to := transition.To
			if item.State == workintent.StateDefining && item.DefinitionComplete && !allGatesAccepted(gates) {
				to = workintent.StateAwaitingDecision
			}
			if item.State == workintent.StateCaptured {
				to = workintent.StateDefining
			}
			if to == workintent.StateReadyForOrchestrator && item.OrchestratorRegistration == "" {
				if err := holdWorkIntentRouteTx(ctx, tx, item, now, ackTimeout); err != nil {
					return err
				}
				out.Held++
				continue
			}
			actionID := item.DeliveryActionID
			if to == workintent.StateReadyForOrchestrator && item.OrchestratorRegistration != "" {
				if !s.HasDriverControlOrigin() {
					if err := holdWorkIntentDeliveryTx(ctx, tx, item, now, ackTimeout,
						ErrDriverControlOriginUnavailable.Error()); err != nil {
						return err
					}
					out.Held++
					continue
				}
				created, id, err := ensureWorkIntentDeliveryActionTx(ctx, tx, item, now)
				if errors.Is(err, ErrWorkIntentRouteUnavailable) {
					if err := holdWorkIntentDeliveryTx(ctx, tx, item, now, ackTimeout, err.Error()); err != nil {
						return err
					}
					out.Held++
					continue
				}
				if err != nil {
					return err
				}
				actionID = id
				if created {
					out.ActionsCreated++
				}
			}
			if to == item.State && actionID == item.DeliveryActionID {
				continue
			}
			nextVersion := item.StateVersion + 1
			res, err := tx.ExecContext(ctx, `UPDATE work_intents SET state=?,state_version=?,
				delivery_action_id=?,hold_kind='',hold_reason='',route_due_at=?,updated_at=?
				WHERE id=? AND state=? AND state_version=?`, string(to), nextVersion, actionID,
				formatOptionalTime(now.Add(ackTimeout)), now.UTC().Format(rfc3339), item.ID,
				string(item.State), item.StateVersion)
			if err != nil {
				return err
			}
			if n, _ := res.RowsAffected(); n != 1 {
				continue
			}
			payload, _ := json.Marshal(map[string]any{"work_intent_id": item.ID,
				"reason": transition.Reason, "delivery_action_id": actionID})
			if err := appendDecisionControlEventTx(ctx, tx, item.ProjectID, "", "work_intent_advanced",
				string(item.State), string(to), nextVersion, "reconciler", "work_intent_promotion",
				string(payload), now); err != nil {
				return err
			}
			out.Advanced++
		}
		return nil
	})
	return out, err
}

func (s *Store) PauseWorkIntent(ctx context.Context, projectID, id string, expectedVersion int, actor, reason string, now time.Time) error {
	if reason == "" {
		reason = "paused by operator"
	}
	return s.transitionWorkIntentHold(ctx, projectID, id, expectedVersion, actor, "paused", reason, now)
}

func (s *Store) ResumeWorkIntent(ctx context.Context, projectID, id string, expectedVersion int, actor string, now time.Time) error {
	return s.transitionWorkIntentHold(ctx, projectID, id, expectedVersion, actor, "", "", now)
}

func (s *Store) transitionWorkIntentHold(ctx context.Context, projectID, id string, expectedVersion int, actor, hold, reason string, now time.Time) error {
	if projectID == "" || id == "" || expectedVersion < 1 || actor == "" {
		return ErrWorkIntentFenced
	}
	return s.tx(ctx, func(tx *sql.Tx) error {
		var state, currentHold string
		if err := tx.QueryRowContext(ctx, `SELECT state,hold_kind FROM work_intents
			WHERE project_id=? AND id=?`, projectID, id).Scan(&state, &currentHold); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrWorkIntentNotFound
			}
			return err
		}
		if hold == "" && currentHold != "paused" {
			return ErrWorkIntentFenced
		}
		res, err := tx.ExecContext(ctx, `UPDATE work_intents SET hold_kind=?,hold_reason=?,
			state_version=state_version+1,updated_at=? WHERE project_id=? AND id=? AND state_version=?
			AND state NOT IN ('admitted','cancelled','superseded')`, hold, reason,
			now.UTC().Format(rfc3339), projectID, id, expectedVersion)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return ErrWorkIntentFenced
		}
		kind := "work_intent_paused"
		if hold == "" {
			kind = "work_intent_resumed"
		}
		payload, _ := json.Marshal(map[string]string{"work_intent_id": id, "reason": reason})
		return appendDecisionControlEventTx(ctx, tx, projectID, "", kind, state, state,
			expectedVersion+1, "human", actor, string(payload), now)
	})
}

func (s *Store) CancelWorkIntent(ctx context.Context, projectID, id string, expectedVersion int, actor, reason string, now time.Time) error {
	if projectID == "" || id == "" || expectedVersion < 1 || actor == "" || reason == "" {
		return ErrWorkIntentFenced
	}
	return s.tx(ctx, func(tx *sql.Tx) error {
		var from string
		if err := tx.QueryRowContext(ctx, `SELECT state FROM work_intents WHERE project_id=? AND id=?`,
			projectID, id).Scan(&from); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrWorkIntentNotFound
			}
			return err
		}
		res, err := tx.ExecContext(ctx, `UPDATE work_intents SET state='cancelled',
			state_version=state_version+1,cancellation_reason=?,hold_kind='',hold_reason='',updated_at=?
			WHERE project_id=? AND id=? AND state_version=?
			AND state NOT IN ('admitted','cancelled','superseded')`, reason, now.UTC().Format(rfc3339),
			projectID, id, expectedVersion)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return ErrWorkIntentFenced
		}
		if _, err := tx.ExecContext(ctx, `UPDATE work_intent_actions SET state='dead_letter',
			last_error='work_intent_cancelled',updated_at=? WHERE work_intent_id=?
			AND state IN ('pending','claimed','uncertain')`, now.UTC().Format(rfc3339), id); err != nil {
			return err
		}
		payload, _ := json.Marshal(map[string]string{"work_intent_id": id, "reason": reason})
		return appendDecisionControlEventTx(ctx, tx, projectID, "", "work_intent_cancelled",
			from, string(workintent.StateCancelled), expectedVersion+1, "human", actor, string(payload), now)
	})
}

func workIntentGatesTx(ctx context.Context, tx *sql.Tx, item WorkIntent) ([]workintent.Gate, error) {
	rows, err := tx.QueryContext(ctx, `SELECT l.decision_id,l.subject_version,l.subject_sha256,
		d.state FROM work_intent_decisions l JOIN decision_requests d ON d.id=l.decision_id
		WHERE l.work_intent_id=? AND l.required=1 ORDER BY l.decision_id`, item.ID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []workintent.Gate
	for rows.Next() {
		var gate workintent.Gate
		var state string
		if err := rows.Scan(&gate.DecisionID, &gate.SubjectVersion, &gate.SubjectSHA256, &state); err != nil {
			return nil, err
		}
		gate.Resolved = state == "approved" || state == "answered"
		gate.Accepted = gate.Resolved
		out = append(out, gate)
	}
	return out, rows.Err()
}

func allGatesAccepted(gates []workintent.Gate) bool {
	for _, gate := range gates {
		if !gate.Resolved || !gate.Accepted {
			return false
		}
	}
	return true
}

func ensureWorkIntentDeliveryActionTx(ctx context.Context, tx *sql.Tx, item WorkIntent, now time.Time) (bool, string, error) {
	recipient, err := activeDriverSessionBindingTx(ctx, tx, item.ProjectID,
		item.OrchestratorRegistration, DriverOrchestratorRole)
	if errors.Is(err, sql.ErrNoRows) {
		return false, "", fmt.Errorf("%w: paired Orchestrator session is not bound", ErrWorkIntentRouteUnavailable)
	}
	if err != nil {
		return false, "", err
	}
	var baselineSeq, uncertaintyEpoch uint64
	var instanceState string
	err = tx.QueryRowContext(ctx, `SELECT c.high_store_seq,c.uncertainty_epoch,i.state
		FROM driver_observation_cursors c JOIN driver_instances i
		  ON i.instance_ref=c.instance_ref AND i.store_id=c.store_id
		WHERE c.store_id=? AND c.active=1 AND i.host_id=?`, recipient.StoreID,
		recipient.HostID).Scan(&baselineSeq, &uncertaintyEpoch, &instanceState)
	if errors.Is(err, sql.ErrNoRows) || err == nil && instanceState != "live" {
		return false, "", fmt.Errorf("%w: Orchestrator observation store is not live", ErrWorkIntentRouteUnavailable)
	}
	if err != nil {
		return false, "", err
	}
	dedup := fmt.Sprintf("work-intent:%s:v%d:deliver:%s:%s", item.ID, item.IntentVersion,
		DriverControlIdentity, recipient.BindingID)
	actionID := "intent-action-" + stableID(dedup)
	payload, _ := json.Marshal(map[string]any{
		"work_intent_id": item.ID, "project_id": item.ProjectID, "intent_version": item.IntentVersion,
		"artifact_ref": item.ArtifactRef, "artifact_sha256": item.ArtifactSHA256,
		"title": item.Title, "summary": item.Summary,
	})
	hash := sha256.Sum256(payload)
	payloadHash := "sha256:" + hex.EncodeToString(hash[:])
	grantID := stableUUID("driver-work-intent-grant/v1", dedup)
	result, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO work_intent_actions
		(id,project_id,work_intent_id,intent_version,kind,state,dedup_key,payload_json,payload_sha256,
		 target_actor_id,target_incarnation,sender_binding_id,sender_principal_id,evidence_baseline_store_seq,
		 evidence_baseline_uncertainty_epoch,grant_id,created_at,updated_at)
		VALUES (?,?,?,?,'deliver_to_orchestrator','pending',?,?,?,?,?,?,?,?,?,?,?,?)`, actionID, item.ProjectID,
		item.ID, item.IntentVersion, dedup, string(payload), payloadHash, item.OrchestratorRegistration,
		recipient.BindingID, "", DriverControlIdentity, baselineSeq, uncertaintyEpoch, grantID,
		now.UTC().Format(rfc3339), now.UTC().Format(rfc3339))
	if err != nil {
		return false, "", err
	}
	created, _ := result.RowsAffected()
	var gotHash, gotTarget, gotSender, gotPrincipal, gotGrant string
	if err := tx.QueryRowContext(ctx, `SELECT payload_sha256,target_incarnation,
		sender_binding_id,sender_principal_id,grant_id FROM work_intent_actions WHERE dedup_key=?`, dedup).
		Scan(&gotHash, &gotTarget, &gotSender, &gotPrincipal, &gotGrant); err != nil {
		return false, "", err
	}
	if gotHash != payloadHash || gotTarget != recipient.BindingID || gotSender != "" ||
		gotPrincipal != DriverControlIdentity || gotGrant != grantID {
		return false, "", errors.New("work intent delivery action idempotency conflict")
	}
	if _, err := tx.ExecContext(ctx, `UPDATE control_alerts SET state='acknowledged',
		acknowledged_at=?,updated_at=? WHERE dedup_key=? AND state IN ('pending','delivering')`,
		now.UTC().Format(rfc3339), now.UTC().Format(rfc3339),
		"work_intent_delivery_route_stale:"+item.ID); err != nil {
		return false, "", err
	}
	return created == 1, actionID, nil
}

func holdWorkIntentDeliveryTx(ctx context.Context, tx *sql.Tx, item WorkIntent, now time.Time,
	retry time.Duration, detail string) error {
	if item.HoldKind == "orchestrator_session_unbound" && item.HoldReason == detail &&
		item.RouteDueAt.After(now) {
		return nil
	}
	res, err := tx.ExecContext(ctx, `UPDATE work_intents SET state='ready_for_orchestrator',
		state_version=state_version+1,hold_kind='orchestrator_session_unbound',hold_reason=?,
		route_due_at=?,updated_at=? WHERE id=? AND state_version=?`, detail,
		now.Add(retry).UTC().Format(rfc3339), now.UTC().Format(rfc3339), item.ID, item.StateVersion)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return nil
	}
	dedup := "work_intent_promotion_stalled:" + item.ID
	_, err = tx.ExecContext(ctx, `INSERT OR IGNORE INTO attention_items
		(id,kind,epic_id,repo,priority,state,dedup_key,blocking,evidence_json,detail,
		 occurrences,first_seen_at,last_seen_at,created_at,updated_at)
		VALUES (?,'work_intent_promotion_stalled','','',10,'open',?,1,
		 json_object('work_intent_id',?,'project_id',?),?,1,?,?,?,?)`,
		"work-intent-stalled-"+stableID(dedup), dedup, item.ID, item.ProjectID, detail,
		now.UTC().Format(rfc3339), now.UTC().Format(rfc3339), now.UTC().Format(rfc3339), now.UTC().Format(rfc3339))
	if err != nil {
		return err
	}
	payload, _ := json.Marshal(map[string]string{"work_intent_id": item.ID, "reason": detail})
	return ensureControlAlertTx(ctx, tx, item.ProjectID, "", "work_intent_promotion_stalled", dedup, string(payload), now)
}

func holdWorkIntentRouteTx(ctx context.Context, tx *sql.Tx, item WorkIntent, now time.Time, retry time.Duration) error {
	detail := "defined work intent has no paired Orchestrator registration"
	if item.HoldKind == "orchestrator_route_missing" && item.HoldReason == detail && item.RouteDueAt.After(now) {
		return nil
	}
	res, err := tx.ExecContext(ctx, `UPDATE work_intents SET state='ready_for_orchestrator',
		state_version=state_version+1,hold_kind='orchestrator_route_missing',hold_reason=?,
		route_due_at=?,updated_at=? WHERE id=? AND state_version=?`, detail,
		now.Add(retry).UTC().Format(rfc3339), now.UTC().Format(rfc3339), item.ID, item.StateVersion)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return nil
	}
	dedup := "work_intent_promotion_stalled:" + item.ID
	_, err = tx.ExecContext(ctx, `INSERT OR IGNORE INTO attention_items
		(id,kind,epic_id,repo,priority,state,dedup_key,blocking,evidence_json,detail,
		 occurrences,first_seen_at,last_seen_at,created_at,updated_at)
		VALUES (?,'work_intent_promotion_stalled','','',10,'open',?,1,
		 json_object('work_intent_id',?,'project_id',?),?,1,?,?,?,?)`,
		"work-intent-stalled-"+stableID(dedup), dedup, item.ID, item.ProjectID, detail,
		now.UTC().Format(rfc3339), now.UTC().Format(rfc3339), now.UTC().Format(rfc3339), now.UTC().Format(rfc3339))
	if err != nil {
		return err
	}
	payload, _ := json.Marshal(map[string]string{"work_intent_id": item.ID, "reason": detail})
	return ensureControlAlertTx(ctx, tx, item.ProjectID, "", "work_intent_promotion_stalled", dedup, string(payload), now)
}

const workIntentSelect = `SELECT id,project_id,source_conversation_id,source_message_id,
	source_message_version,interactor_incarnation_id,title,summary,artifact_ref,intent_version,
	artifact_sha256,definition_complete,definition_evidence_json,dependency_refs_json,state,
	state_version,priority,owner_actor_id,route_to,orchestrator_registration,delivery_action_id,
	route_lease_id,route_epoch,route_attempts,route_due_at,route_acknowledged_at,epic_contract_ref,
	epic_contract_sha256,submission_idempotency_key,COALESCE(admitted_epic_id,''),hold_kind,
	hold_reason,next_retry_at,COALESCE(superseded_by,''),cancellation_reason,created_at,updated_at
	FROM work_intents`

func scanWorkIntent(row decisionRowScanner) (WorkIntent, error) {
	var out WorkIntent
	var complete int
	var state, routeDue, routeAck, nextRetry, created, updated string
	err := row.Scan(&out.ID, &out.ProjectID, &out.SourceConversationID, &out.SourceMessageID,
		&out.SourceMessageVersion, &out.InteractorIncarnationID, &out.Title, &out.Summary,
		&out.ArtifactRef, &out.IntentVersion, &out.ArtifactSHA256, &complete,
		&out.DefinitionEvidenceJSON, &out.DependencyRefsJSON, &state, &out.StateVersion,
		&out.Priority, &out.OwnerActorID, &out.RouteTo, &out.OrchestratorRegistration,
		&out.DeliveryActionID, &out.RouteLeaseID, &out.RouteEpoch, &out.RouteAttempts,
		&routeDue, &routeAck, &out.EpicContractRef, &out.EpicContractSHA256,
		&out.SubmissionIdempotencyKey, &out.AdmittedEpicID, &out.HoldKind, &out.HoldReason,
		&nextRetry, &out.SupersededBy, &out.CancellationReason, &created, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return WorkIntent{}, ErrWorkIntentNotFound
	}
	if err != nil {
		return WorkIntent{}, err
	}
	out.DefinitionComplete = complete == 1
	out.State = workintent.State(state)
	out.RouteDueAt, out.RouteAcknowledgedAt = parseOptionalTime(routeDue), parseOptionalTime(routeAck)
	out.NextRetryAt, out.CreatedAt, out.UpdatedAt = parseOptionalTime(nextRetry), parseOptionalTime(created), parseOptionalTime(updated)
	return out, nil
}
