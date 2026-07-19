package api

import (
	"encoding/json"
	"net/http"
	"strings"
)

// epicEffectRecovery is the authenticated, typed human-clear edge for an
// exhausted v2 effect. It cannot manufacture an action or select a mutable
// target: the exact epic, immutable head, and known recovery code must resolve
// to an existing dead-lettered ledger row.
func (s *Server) epicEffectRecovery(w http.ResponseWriter, r *http.Request) {
	epicID := strings.TrimSpace(r.PathValue("id"))
	var body struct {
		HeadSHA      string `json:"head_sha"`
		RecoveryCode string `json:"recovery_code"`
	}
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		http.Error(w, "invalid effect recovery request: "+err.Error(), http.StatusBadRequest)
		return
	}
	granted, err := s.store.GrantEpicActionRecoveryBudget(r.Context(), epicID,
		strings.TrimSpace(body.HeadSHA), strings.TrimSpace(body.RecoveryCode), s.clock.Now())
	if err != nil {
		http.Error(w, "effect recovery: "+err.Error(), http.StatusBadRequest)
		return
	}
	if !granted {
		http.Error(w, "no matching dead-lettered effect for exact epic/head/recovery code", http.StatusConflict)
		return
	}
	s.broker.Publish(s.epicNudge(r.Context(), epicID, "effect_recovery_budget_granted"))
	writeJSON(w, http.StatusOK, map[string]any{
		"epic_id": epicID, "head_sha": body.HeadSHA, "recovery_code": body.RecoveryCode, "granted": true,
	})
}
