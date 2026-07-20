package api

import (
	"errors"
	"net/http"

	"github.com/samhotchkiss/flowbee/internal/auth"
	"github.com/samhotchkiss/flowbee/internal/store"
)

type workIntentEpicContractBody struct {
	ProjectID             string                       `json:"project_id"`
	IntentVersion         int                          `json:"intent_version"`
	ExpectedStateVersion  int                          `json:"expected_state_version"`
	SourceArtifactSHA256  string                       `json:"source_artifact_sha256"`
	ContractVersion       int                          `json:"contract_version"`
	ContractRef           string                       `json:"contract_ref"`
	ContractSHA256        string                       `json:"contract_sha256"`
	Contract              store.WorkIntentEpicContract `json:"contract"`
	OrchestratorBindingID string                       `json:"orchestrator_binding_id"`
	SubmissionKey         string                       `json:"submission_key"`
}

type workIntentEpicContractView struct {
	ID                    string `json:"id"`
	ProjectID             string `json:"project_id"`
	WorkIntentID          string `json:"work_intent_id"`
	SourceArtifactSHA256  string `json:"source_artifact_sha256"`
	IntentVersion         int    `json:"intent_version"`
	ContractVersion       int    `json:"contract_version"`
	ContractRef           string `json:"contract_ref"`
	ContractSHA256        string `json:"contract_sha256"`
	OrchestratorBindingID string `json:"orchestrator_binding_id"`
	SubmissionKey         string `json:"submission_key"`
	State                 string `json:"state"`
	AdmittedEpicID        string `json:"admitted_epic_id,omitempty"`
}

func viewWorkIntentEpicContract(row store.PreparedWorkIntentEpicContract) workIntentEpicContractView {
	return workIntentEpicContractView{
		ID: row.ID, ProjectID: row.ProjectID, WorkIntentID: row.WorkIntentID,
		SourceArtifactSHA256: row.SourceArtifactSHA256, IntentVersion: row.IntentVersion,
		ContractVersion: row.ContractVersion, ContractRef: row.ContractRef,
		ContractSHA256: row.ContractSHA256, OrchestratorBindingID: row.OrchestratorBindingID,
		SubmissionKey: row.SubmissionKey, State: row.State, AdmittedEpicID: row.AdmittedEpicID,
	}
}

func (s *Server) workIntentEpicContract(w http.ResponseWriter, r *http.Request) {
	key := r.Header.Get("Idempotency-Key")
	if key == "" {
		http.Error(w, "Idempotency-Key is required", http.StatusBadRequest)
		return
	}
	var body workIntentEpicContractBody
	if err := decodeBoundedJSON(r, &body); err != nil {
		http.Error(w, "invalid epic contract", http.StatusBadRequest)
		return
	}
	if body.SubmissionKey == "" || key != body.SubmissionKey {
		http.Error(w, "Idempotency-Key must equal the work-intent submission key", http.StatusConflict)
		return
	}
	if claims, ok := auth.CredentialClaimsFrom(r); ok {
		if claims.WorkerRole != store.DriverOrchestratorRole || body.ProjectID != claims.ProjectID {
			http.Error(w, "forbidden: actor credential is scoped to another project or role", http.StatusForbidden)
			return
		}
		binding, err := s.store.ActiveDriverSessionBinding(r.Context(), claims.ProjectID,
			claims.Identity, store.DriverOrchestratorRole)
		if err != nil || binding.BindingID == "" || body.OrchestratorBindingID != binding.BindingID {
			http.Error(w, "forbidden: orchestrator binding is not current for this credential", http.StatusForbidden)
			return
		}
	}
	row, err := s.store.RecordWorkIntentEpicContract(r.Context(),
		store.RecordWorkIntentEpicContractInput{
			ProjectID: body.ProjectID, WorkIntentID: r.PathValue("id"),
			IntentVersion: body.IntentVersion, ExpectedStateVersion: body.ExpectedStateVersion,
			SourceArtifactSHA256: body.SourceArtifactSHA256,
			ContractVersion:      body.ContractVersion, ContractRef: body.ContractRef,
			ContractSHA256: body.ContractSHA256, Contract: body.Contract,
			OrchestratorBindingID: body.OrchestratorBindingID, SubmissionKey: body.SubmissionKey,
		}, s.clock.Now())
	if err != nil {
		switch {
		case errors.Is(err, store.ErrWorkIntentNotFound):
			http.Error(w, "work intent not found", http.StatusNotFound)
		case errors.Is(err, store.ErrWorkIntentContractFenced):
			http.Error(w, err.Error(), http.StatusPreconditionFailed)
		case errors.Is(err, store.ErrWorkIntentContractInvalid):
			http.Error(w, err.Error(), http.StatusUnprocessableEntity)
		default:
			http.Error(w, "epic contract submission failed", http.StatusInternalServerError)
		}
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{
		"schema_version": "flowbee.work-intent-epic-contract/v1",
		"epic_contract":  viewWorkIntentEpicContract(row),
	})
}
