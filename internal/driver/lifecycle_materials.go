package driver

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/samhotchkiss/flowbee/internal/auth"
)

// SQLLifecycleLaunchMaterials resolves public and secret launch material only
// after a durable lifecycle action is claimed. Public bytes come from the
// immutable worker contract. Secret bytes are minted once into an owner-only
// envelope file and replayed from that exact file, so signing-key rotation can
// never change an already-committed Driver idempotency body.
type SQLLifecycleLaunchMaterials struct {
	DB                *sql.DB
	EnvelopeDirectory string
	WorkerAuthSecret  []byte
}

func (r SQLLifecycleLaunchMaterials) ResolveLifecycleLaunch(ctx context.Context, action Action,
	now time.Time) (Action, func(bool), error) {
	if r.DB == nil || r.EnvelopeDirectory == "" || len(r.WorkerAuthSecret) == 0 {
		return action, nil, errors.New("worker launch envelope resolver is not configured")
	}
	if action.ExecutorKind == "project_actor_lifecycle" {
		return r.resolveProjectActorLaunch(ctx, action, now)
	}
	role := "builder"
	if action.Kind == "reviewer_launch" || action.TargetRole == "code_reviewer" {
		role = "reviewer"
	}
	var projectID, displayName, bootstrapPayload, bootstrapHash, identity string
	var envelopeID, persistedHash, expiresAt string
	var generation int64
	err := r.DB.QueryRowContext(ctx, `SELECT w.project_id,w.display_name,w.bootstrap_payload,
		w.bootstrap_sha256,w.flowbee_identity,c.envelope_ref,c.payload_sha256,c.generation,c.expires_at
		FROM epic_worker_sessions w JOIN epic_worker_credentials c
		ON c.epic_id=w.epic_id AND c.worker_role=w.worker_role
		WHERE w.epic_id=? AND w.worker_role=? AND w.ensure_action_id=?
		AND w.state='ensure_pending' AND c.state IN ('issued','installed')`,
		action.EpicID, role, action.ActionID).Scan(&projectID, &displayName, &bootstrapPayload,
		&bootstrapHash, &identity, &envelopeID, &persistedHash, &generation, &expiresAt)
	if err != nil {
		return action, nil, fmt.Errorf("resolve durable worker launch contract: %w", err)
	}
	if sha256Text(bootstrapPayload) != bootstrapHash {
		return action, nil, errors.New("immutable worker bootstrap hash mismatch")
	}
	expires, err := time.Parse(time.RFC3339Nano, expiresAt)
	if err != nil || !now.Before(expires) || generation < 1 || envelopeID == "" {
		return action, nil, errors.New("worker credential is absent or expired")
	}
	publicContent := "FLOWBEE MANAGED WORKER BOOTSTRAP\n" + bootstrapPayload +
		"\nFLOWBEE FENCED LIFECYCLE ACTION\n" + action.Payload
	action.LifecycleBootstrap = &LifecycleBootstrapArtifact{
		ArtifactID: action.ActionID + "-bootstrap", Format: "initial_prompt_utf8/v1",
		PayloadSHA256: sha256Text(publicContent), ContentUTF8: publicContent}

	secret, _, err := r.resolveEnvelope(envelopeID)
	if err != nil {
		return action, nil, err
	}
	secretHash := sha256Text(secret)
	if persistedHash == "" || persistedHash != secretHash {
		return action, nil, errors.New("worker credential envelope hash changed")
	}
	action.LifecycleCredential = &LifecycleCredentialEnvelope{EnvelopeID: envelopeID,
		Format: "flowbee_target_bearer_utf8/v1", CredentialEpoch: generation,
		PayloadSHA256: secretHash, SecretUTF8: secret}
	action.LifecyclePresentationName = displayName
	cleanup := func(_ bool) {
		action.LifecycleCredential.SecretUTF8 = ""
	}
	return action, cleanup, nil
}

func (r SQLLifecycleLaunchMaterials) resolveProjectActorLaunch(ctx context.Context, action Action,
	now time.Time) (Action, func(bool), error) {
	var projectID, role, actorID, bootstrapFormat, bootstrapPayload, bootstrapHash string
	var envelopeID, persistedHash, expiresAt, presentationName string
	var generation int64
	err := r.DB.QueryRowContext(ctx, `SELECT project_id,role,actor_id,bootstrap_format,
		bootstrap_payload,bootstrap_sha256,credential_envelope_ref,credential_payload_sha256,
		credential_generation,credential_expires_at,presentation_name
		FROM project_actor_lifecycle_actions WHERE id=? AND operation='ensure'
		AND state IN ('delivering','verifying')`, action.ActionID).Scan(&projectID, &role, &actorID,
		&bootstrapFormat, &bootstrapPayload, &bootstrapHash, &envelopeID, &persistedHash,
		&generation, &expiresAt, &presentationName)
	if err != nil {
		return action, nil, fmt.Errorf("resolve durable project actor Q3 contract: %w", err)
	}
	if projectID != action.ProjectID || role != action.TargetRole || actorID == "" ||
		bootstrapFormat != "initial_prompt_utf8/v1" || sha256Text(bootstrapPayload) != bootstrapHash ||
		generation != action.TargetEpoch || presentationName == "" {
		return action, nil, errors.New("project actor Q3 contract identity/hash mismatch")
	}
	expires, err := time.Parse(time.RFC3339Nano, expiresAt)
	if err != nil || !now.Before(expires) || envelopeID == "" {
		return action, nil, errors.New("project actor credential is absent or expired")
	}
	action.LifecycleBootstrap = &LifecycleBootstrapArtifact{ArtifactID: action.ActionID + "-bootstrap",
		Format: bootstrapFormat, PayloadSHA256: bootstrapHash, ContentUTF8: bootstrapPayload}
	secret, _, err := r.resolveEnvelope(envelopeID)
	if err != nil {
		return action, nil, err
	}
	if sha256Text(secret) != persistedHash {
		return action, nil, errors.New("project actor credential envelope hash changed")
	}
	action.LifecycleCredential = &LifecycleCredentialEnvelope{EnvelopeID: envelopeID,
		Format: "flowbee_target_bearer_utf8/v1", CredentialEpoch: generation,
		PayloadSHA256: persistedHash, SecretUTF8: secret}
	action.LifecyclePresentationName = presentationName
	cleanup := func(_ bool) { action.LifecycleCredential.SecretUTF8 = "" }
	return action, cleanup, nil
}

// FinalizeLifecycleLaunch removes the one-shot source only after a terminal
// installed/rebound receipt. It is called for both the immediate response and
// by-action crash recovery; deletion and its durable tombstone are idempotent.
func (r SQLLifecycleLaunchMaterials) FinalizeLifecycleLaunch(ctx context.Context, action Action,
	receipt LifecycleReceipt, now time.Time) error {
	if action.ExecutorKind == "project_actor_lifecycle" {
		return r.finalizeProjectActorLaunch(ctx, action, receipt, now)
	}
	if receipt.Operation != "ensure" || receipt.CredentialInstall.Status != "installed" &&
		receipt.CredentialInstall.Status != "rebound" {
		return nil
	}
	role := "builder"
	if action.Kind == "reviewer_launch" || action.TargetRole == "code_reviewer" {
		role = "reviewer"
	}
	var envelopeID string
	if err := r.DB.QueryRowContext(ctx, `SELECT envelope_ref FROM epic_worker_credentials
		WHERE epic_id=? AND worker_role=? AND ensure_action_id=?`, action.EpicID, role,
		action.ActionID).Scan(&envelopeID); err != nil {
		return err
	}
	path := r.envelopePath(envelopeID)
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	_, err := r.DB.ExecContext(ctx, `UPDATE epic_worker_credentials SET envelope_deleted_at=?,updated_at=?
		WHERE epic_id=? AND worker_role=? AND ensure_action_id=?`, now.UTC().Format(time.RFC3339Nano),
		now.UTC().Format(time.RFC3339Nano), action.EpicID, role, action.ActionID)
	return err
}

func (r SQLLifecycleLaunchMaterials) finalizeProjectActorLaunch(ctx context.Context, action Action,
	receipt LifecycleReceipt, now time.Time) error {
	if receipt.Operation != "ensure" && receipt.Operation != "stop" {
		return nil
	}
	var envelopeID string
	if err := r.DB.QueryRowContext(ctx, `SELECT credential_envelope_ref
		FROM project_actor_lifecycle_actions WHERE id=?`,
		action.ActionID).Scan(&envelopeID); err != nil {
		return err
	}
	// Pre-0065 managed Stop actions legitimately carry no Q3 envelope. Never
	// derive a filesystem deletion target from the empty legacy sentinel.
	if envelopeID != "" {
		if err := os.Remove(r.envelopePath(envelopeID)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	stamp := now.UTC().Format(time.RFC3339Nano)
	if receipt.Operation == "stop" {
		_, err := r.DB.ExecContext(ctx, `UPDATE project_actor_lifecycles
			SET credential_envelope_deleted_at=?,credential_revoked_at=?,updated_at=?
			WHERE project_id=? AND role=? AND current_action_id=? AND credential_envelope_ref=?`,
			stamp, stamp, stamp, action.ProjectID, action.TargetRole, action.ActionID, envelopeID)
		return err
	}
	if receipt.CredentialInstall.Status != "installed" && receipt.CredentialInstall.Status != "rebound" {
		return nil
	}
	_, err := r.DB.ExecContext(ctx, `UPDATE project_actor_lifecycles
		SET credential_envelope_deleted_at=?,updated_at=? WHERE project_id=? AND role=?
		AND current_action_id=? AND credential_envelope_ref=?`, stamp, stamp,
		action.ProjectID, action.TargetRole, action.ActionID, envelopeID)
	return err
}

// PrepareEnvelope is called inside Flowbee's lifecycle-action transaction before
// commit. It mints at most once and returns only the hash that the transaction
// binds; later resolution never regenerates missing bytes.
func (r SQLLifecycleLaunchMaterials) PrepareEnvelope(identity, projectID, role, envelopeID string,
	generation int64, expires time.Time) (string, error) {
	if err := os.MkdirAll(r.EnvelopeDirectory, 0o700); err != nil {
		return "", err
	}
	if err := os.Chmod(r.EnvelopeDirectory, 0o700); err != nil {
		return "", err
	}
	path := r.envelopePath(envelopeID)
	if b, err := readOwnerEnvelope(path); err == nil {
		return sha256Text(string(b)), nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	issuer := auth.NewBearer(r.WorkerAuthSecret, nil, false)
	secret := issuer.MintCredential(identity, projectID, role, envelopeID, generation, expires)
	if secret == "" || strings.ContainsAny(secret, "\r\n\x00") {
		return "", errors.New("worker credential mint produced invalid envelope")
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if errors.Is(err, os.ErrExist) {
		b, readErr := readOwnerEnvelope(path)
		if readErr != nil {
			return "", readErr
		}
		return sha256Text(string(b)), nil
	}
	if err != nil {
		return "", err
	}
	ok := false
	defer func() {
		_ = f.Close()
		if !ok {
			_ = os.Remove(path)
		}
	}()
	if _, err := f.WriteString(secret); err != nil {
		return "", err
	}
	if err := f.Sync(); err != nil {
		return "", err
	}
	if err := f.Close(); err != nil {
		return "", err
	}
	ok = true
	return sha256Text(secret), nil
}

func (r SQLLifecycleLaunchMaterials) resolveEnvelope(envelopeID string) (string, string, error) {
	path := r.envelopePath(envelopeID)
	b, err := readOwnerEnvelope(path)
	if err != nil {
		return "", "", err
	}
	return string(b), path, nil
}

func (r SQLLifecycleLaunchMaterials) envelopePath(envelopeID string) string {
	h := sha256.Sum256([]byte(envelopeID))
	return filepath.Join(r.EnvelopeDirectory, hex.EncodeToString(h[:])+".envelope")
}

func readOwnerEnvelope(path string) ([]byte, error) {
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), path)
	if f == nil {
		_ = syscall.Close(fd)
		return nil, errors.New("worker credential envelope has an invalid file descriptor")
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 ||
		stat.Uid != uint32(os.Geteuid()) || stat.Nlink != 1 {
		return nil, errors.New("worker credential envelope is not an owner-only regular file")
	}
	b, err := io.ReadAll(io.LimitReader(f, (8<<10)+1))
	if err != nil {
		return nil, err
	}
	if len(b) < 32 || len(b) > 8<<10 {
		return nil, errors.New("worker credential envelope size invalid")
	}
	return b, nil
}
