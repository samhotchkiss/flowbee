package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"strings"
	"time"
)

var (
	ErrHumanLoginInvalid = errors.New("human login token is invalid")
	ErrHumanLoginExpired = errors.New("human login token is expired")
	ErrHumanLoginUsed    = errors.New("human login token was already used")
)

type HumanLogin struct {
	Identity  string
	SessionID string
	ExpiresAt time.Time
}

// CreateHumanLoginToken stores only a domain-separated SHA-256 digest of the
// one-time bearer. The raw token exists only in the CLI output/browser URL
// fragment and therefore cannot be recovered from a database snapshot.
func (s *Store) CreateHumanLoginToken(ctx context.Context, rawToken, identity, sessionID string, expiresAt, now time.Time) error {
	rawToken, identity, sessionID = strings.TrimSpace(rawToken), strings.TrimSpace(identity), strings.TrimSpace(sessionID)
	if len(rawToken) < 32 || identity == "" || sessionID == "" || !expiresAt.After(now) || expiresAt.Sub(now) > 24*time.Hour {
		return ErrHumanLoginInvalid
	}
	nowText := now.UTC().Format(rfc3339)
	_, err := s.DB.ExecContext(ctx, `INSERT INTO human_login_tokens
		(token_sha256,identity,session_id,state,expires_at,created_at,consumed_at)
		VALUES (?,?,?,'pending',?,?,'')`, humanLoginDigest(rawToken), identity, sessionID,
		expiresAt.UTC().Format(rfc3339), nowText)
	return err
}

// ConsumeHumanLoginToken is a serialized, one-way state transition. A crash
// after commit cannot make the bearer valid again, and concurrent/replayed
// exchanges have exactly one winner.
func (s *Store) ConsumeHumanLoginToken(ctx context.Context, rawToken string, now time.Time) (HumanLogin, error) {
	if len(strings.TrimSpace(rawToken)) < 32 {
		return HumanLogin{}, ErrHumanLoginInvalid
	}
	var out HumanLogin
	err := s.tx(ctx, func(tx *sql.Tx) error {
		var state, expiresText string
		err := tx.QueryRowContext(ctx, `SELECT identity,session_id,state,expires_at
			FROM human_login_tokens WHERE token_sha256=?`, humanLoginDigest(rawToken)).
			Scan(&out.Identity, &out.SessionID, &state, &expiresText)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrHumanLoginInvalid
		}
		if err != nil {
			return err
		}
		expires, err := time.Parse(rfc3339, expiresText)
		if err != nil {
			return ErrHumanLoginInvalid
		}
		out.ExpiresAt = expires
		if state == "consumed" {
			return ErrHumanLoginUsed
		}
		if state != "pending" {
			return ErrHumanLoginExpired
		}
		nowText := now.UTC().Format(rfc3339)
		if !expires.After(now) {
			_, err = tx.ExecContext(ctx, `UPDATE human_login_tokens SET state='expired'
				WHERE token_sha256=? AND state='pending'`, humanLoginDigest(rawToken))
			if err != nil {
				return err
			}
			return ErrHumanLoginExpired
		}
		res, err := tx.ExecContext(ctx, `UPDATE human_login_tokens
			SET state='consumed',consumed_at=? WHERE token_sha256=? AND state='pending'`,
			nowText, humanLoginDigest(rawToken))
		if err != nil {
			return err
		}
		changed, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if changed != 1 {
			return ErrHumanLoginUsed
		}
		return nil
	})
	return out, err
}

func humanLoginDigest(raw string) string {
	sum := sha256.Sum256([]byte("flowbee-human-login/v1\x00" + raw))
	return "sha256:" + hex.EncodeToString(sum[:])
}
