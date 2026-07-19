package capacitycollector

import (
	"context"
	"database/sql"
	"encoding/json"
)

type SeatSource interface {
	EnabledCapacitySeats(context.Context) ([]Seat, error)
}

// SQLSeatSource reads the operator-owned expectations and config-home identity
// needed by a collector. It never reads or returns provider credentials.
type SQLSeatSource struct{ DB *sql.DB }

func (s SQLSeatSource) EnabledCapacitySeats(ctx context.Context) ([]Seat, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT id,expected_host_id,agent_family,
		CASE WHEN agent_family='codex' THEN codex_home ELSE config_dir END,
		expected_account_key,expected_credential_lineage,extra_env_json,(box='')
		FROM seats WHERE enabled=1 ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Seat
	for rows.Next() {
		var seat Seat
		var extraEnv string
		var local int
		if err := rows.Scan(&seat.ID, &seat.HostID, &seat.Provider, &seat.ConfigHome,
			&seat.ExpectedAccountKey, &seat.ExpectedCredentialLineage, &extraEnv, &local); err != nil {
			return nil, err
		}
		seat.Local = local == 1
		// Claude's org fingerprint is non-secret identity evidence. Reuse the
		// operator-owned seat env map until a dedicated schema field is added.
		seat.ExpectedOrgFingerprint = expectedOrgFingerprint(extraEnv)
		out = append(out, seat)
	}
	return out, rows.Err()
}

func expectedOrgFingerprint(raw string) string {
	// Keep the parser private and allowlisted: no arbitrary launch environment is
	// exposed to the provider adapter.
	var object map[string]string
	if json.Unmarshal([]byte(raw), &object) != nil {
		return ""
	}
	return object["FLOWBEE_EXPECTED_ORG_FINGERPRINT"]
}
