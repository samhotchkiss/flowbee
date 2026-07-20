package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

const workerAuthRuntimePostureKey = "runtime_worker_auth_posture_v1"

// WorkerAuthRuntimePosture is a non-secret heartbeat from the process that owns
// the writer lock. Offline activation tooling compares its effective-config
// fingerprint with the invoking process, instead of assuming shell variables are
// the variables the running service actually received.
type WorkerAuthRuntimePosture struct {
	Fingerprint string    `json:"fingerprint"`
	PID         int       `json:"pid"`
	UpdatedAt   time.Time `json:"updated_at"`
}

func (s *Store) RecordWorkerAuthRuntimePosture(ctx context.Context, posture WorkerAuthRuntimePosture) error {
	posture.UpdatedAt = posture.UpdatedAt.UTC()
	payload, err := json.Marshal(posture)
	if err != nil {
		return err
	}
	_, err = s.DB.ExecContext(ctx, `INSERT INTO flowbee_meta(key,value) VALUES(?,?)
		ON CONFLICT(key) DO UPDATE SET value=excluded.value`, workerAuthRuntimePostureKey, string(payload))
	return err
}

func (s *Store) WorkerAuthRuntimePosture(ctx context.Context) (WorkerAuthRuntimePosture, error) {
	var payload string
	if err := s.DB.QueryRowContext(ctx, `SELECT value FROM flowbee_meta WHERE key=?`,
		workerAuthRuntimePostureKey).Scan(&payload); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return WorkerAuthRuntimePosture{}, sql.ErrNoRows
		}
		return WorkerAuthRuntimePosture{}, err
	}
	var posture WorkerAuthRuntimePosture
	if err := json.Unmarshal([]byte(payload), &posture); err != nil {
		return WorkerAuthRuntimePosture{}, err
	}
	return posture, nil
}
