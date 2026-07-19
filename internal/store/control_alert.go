package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"time"
)

func stableID(value string) string {
	h := sha256.Sum256([]byte(value))
	return hex.EncodeToString(h[:12])
}

func ensureControlAlertTx(ctx context.Context, tx *sql.Tx, projectID, epicID, kind, dedup, payload string, now time.Time) error {
	id := "alert-" + stableID(dedup)
	var epicRef any
	if epicID != "" {
		epicRef = epicID
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO control_alerts
		(id,project_id,epic_id,kind,dedup_key,payload_json,state,created_at,updated_at)
		VALUES (?,?,?,?,?,?,'pending',?,?)`, id, projectID, epicRef, kind, dedup, payload,
		now.UTC().Format(rfc3339), now.UTC().Format(rfc3339))
	if err == nil || isUniqueConstraintErr(err) {
		return nil
	}
	return err
}
