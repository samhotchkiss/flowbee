package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/samhotchkiss/flowbee/internal/ellie/maintenance"
)

type EllieMaintenanceCheck struct {
	StoreID      string
	SweepType    maintenance.SweepType
	Candidate    maintenance.Candidate
	ResultStatus maintenance.ResultStatus
	CheckedAt    time.Time
	SweepRunID   string
}

func (s *Store) MaintenanceCheckCompleted(ctx context.Context, storeID string, sweep maintenance.SweepType, candidate maintenance.Candidate) (bool, error) {
	if storeID == "" {
		return false, errors.New("maintenance check store_id is required")
	}
	if !maintenance.ValidSweepType(sweep) {
		return false, fmt.Errorf("unknown maintenance sweep type %q", sweep)
	}
	var hashes string
	err := s.DB.QueryRowContext(ctx, `
		SELECT candidate_content_hashes
		  FROM ellie_maintenance_checks
		 WHERE store_id = ? AND sweep_type = ? AND candidate_key = ?`,
		storeID, string(sweep), candidate.Key).Scan(&hashes)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read ellie maintenance check: %w", err)
	}
	var completed []maintenance.Member
	if err := json.Unmarshal([]byte(hashes), &completed); err != nil {
		return false, fmt.Errorf("decode ellie maintenance check hashes: %w", err)
	}
	return maintenance.ContentHashesMatch(candidate, completed), nil
}

func (s *Store) RecordEllieMaintenanceCheck(ctx context.Context, check EllieMaintenanceCheck) (bool, error) {
	if check.StoreID == "" {
		return false, errors.New("maintenance check store_id is required")
	}
	if !maintenance.ValidSweepType(check.SweepType) {
		return false, fmt.Errorf("unknown maintenance sweep type %q", check.SweepType)
	}
	if check.Candidate.Key == "" {
		return false, errors.New("maintenance check candidate key is required")
	}
	if check.CheckedAt.IsZero() {
		return false, errors.New("maintenance check checked_at is required")
	}
	if !maintenance.IsCompletedStatus(check.ResultStatus) {
		return false, nil
	}
	members := make([]string, 0, len(check.Candidate.Members))
	for _, m := range check.Candidate.Members {
		members = append(members, m.ID)
	}
	memberJSON, err := json.Marshal(members)
	if err != nil {
		return false, fmt.Errorf("encode ellie maintenance members: %w", err)
	}
	hashJSON, err := json.Marshal(check.Candidate.Members)
	if err != nil {
		return false, fmt.Errorf("encode ellie maintenance hashes: %w", err)
	}
	checkedAt := check.CheckedAt.UTC().Format(time.RFC3339Nano)
	_, err = s.DB.ExecContext(ctx, `
		INSERT INTO ellie_maintenance_checks (
		    store_id, sweep_type, candidate_kind, candidate_key, candidate_members,
		    candidate_content_hashes, result_status, checked_at, sweep_run_id,
		    created_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, datetime('now'), datetime('now'))
		ON CONFLICT (store_id, sweep_type, candidate_key) DO UPDATE SET
		    candidate_kind = excluded.candidate_kind,
		    candidate_members = excluded.candidate_members,
		    candidate_content_hashes = excluded.candidate_content_hashes,
		    result_status = excluded.result_status,
		    checked_at = excluded.checked_at,
		    sweep_run_id = excluded.sweep_run_id,
		    updated_at = datetime('now')`,
		check.StoreID,
		string(check.SweepType),
		string(check.Candidate.Kind),
		check.Candidate.Key,
		string(memberJSON),
		string(hashJSON),
		string(check.ResultStatus),
		checkedAt,
		check.SweepRunID)
	if err != nil {
		return false, fmt.Errorf("record ellie maintenance check: %w", err)
	}
	return true, nil
}

func (s *Store) RecordMaintenanceCheck(ctx context.Context, check maintenance.CheckRecord) (bool, error) {
	return s.RecordEllieMaintenanceCheck(ctx, EllieMaintenanceCheck{
		StoreID:      check.StoreID,
		SweepType:    check.SweepType,
		Candidate:    check.Candidate,
		ResultStatus: check.ResultStatus,
		CheckedAt:    check.CheckedAt,
		SweepRunID:   check.SweepRunID,
	})
}
