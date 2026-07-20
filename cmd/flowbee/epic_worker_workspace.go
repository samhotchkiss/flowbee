package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/samhotchkiss/flowbee/internal/driver"
	"github.com/samhotchkiss/flowbee/internal/gitops"
	"github.com/samhotchkiss/flowbee/internal/store"
)

const localWorkspaceRootsEnv = "FLOWBEE_LOCAL_WORKSPACE_ROOTS_JSON"

type localEpicWorkerWorkspaceManager struct {
	DB           *sql.DB
	Roots        map[string]string
	LocalHostIDs map[string]bool
	MirrorPath   func(store.Repo) string
	locksMu      sync.Mutex
	locks        map[string]*sync.Mutex
}

type localEpicWorkspaceMarker struct {
	Format                string `json:"format"`
	ProjectID             string `json:"project_id"`
	EpicID                string `json:"epic_id"`
	Role                  string `json:"role"`
	LifecycleKey          string `json:"lifecycle_key"`
	WorkspaceRootID       string `json:"workspace_root_id"`
	WorkspaceRelativePath string `json:"workspace_relative_path"`
	RepositoryID          string `json:"repository_id"`
	SourceSHA             string `json:"source_sha"`
}

func localWorkspaceRootsFromEnvironment() (map[string]string, error) {
	raw := os.Getenv(localWorkspaceRootsEnv)
	if raw == "" {
		return nil, fmt.Errorf("%s is required for managed epic workers", localWorkspaceRootsEnv)
	}
	var roots map[string]string
	if err := json.Unmarshal([]byte(raw), &roots); err != nil {
		return nil, fmt.Errorf("parse %s: %w", localWorkspaceRootsEnv, err)
	}
	if len(roots) == 0 {
		return nil, fmt.Errorf("%s contains no configured roots", localWorkspaceRootsEnv)
	}
	cleaned := make(map[string]string, len(roots))
	for id, root := range roots {
		if id == "" || strings.ContainsAny(id, "/\\\x00") {
			return nil, errors.New("local workspace root ID is invalid")
		}
		if !filepath.IsAbs(root) {
			return nil, fmt.Errorf("local workspace root %q must be absolute", id)
		}
		resolved, err := filepath.EvalSymlinks(filepath.Clean(root))
		if err != nil {
			return nil, fmt.Errorf("resolve local workspace root %q: %w", id, err)
		}
		if err := requireOwnerOnlyDirectory(resolved); err != nil {
			return nil, fmt.Errorf("local workspace root %q: %w", id, err)
		}
		cleaned[id] = resolved
	}
	return cleaned, nil
}

func (m *localEpicWorkerWorkspaceManager) PrepareLifecycleWorkspace(ctx context.Context,
	action driver.Action, _ time.Time) error {
	if !isEpicWorkerEnsure(action) {
		return nil
	}
	workspace, markerPath, sourceSHA, repo, err := m.resolve(ctx, action)
	if err != nil {
		return err
	}
	unlock := m.lock(workspace)
	defer unlock()
	if err := ensureSecureParents(m.Roots[action.WorkspaceRootID], workspace); err != nil {
		return err
	}
	mirrorPath := m.MirrorPath(repo)
	if mirrorPath == "" {
		return errors.New("workspace source mirror path is unavailable")
	}
	resolvedMirror, err := filepath.EvalSymlinks(filepath.Clean(mirrorPath))
	if err != nil || resolvedMirror != filepath.Clean(mirrorPath) {
		return errors.New("workspace source mirror is missing or symlinked")
	}
	mirror := gitops.Open(resolvedMirror)
	registeredSHA, registered, err := mirror.RegisteredWorktree(workspace)
	if err != nil {
		return err
	}
	if registered {
		if registeredSHA != sourceSHA {
			return fmt.Errorf("existing registered workspace is at %s, want immutable %s", registeredSHA, sourceSHA)
		}
		if info, statErr := os.Lstat(workspace); statErr != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return errors.New("registered workspace path is absent or not a real directory")
		}
		// A missing marker is the one recoverable crash seam: git durably
		// registered the exact path+SHA before Flowbee wrote its own marker.
		if err := m.ensureMarker(markerPath, markerFor(action, repo.ID, sourceSHA), true); err != nil {
			return err
		}
		return nil
	}
	if _, err := os.Lstat(workspace); err == nil {
		return errors.New("workspace path exists but is not registered to the exact mirror")
	} else if !os.IsNotExist(err) {
		return err
	}
	if _, err := os.Lstat(markerPath); err == nil {
		return errors.New("workspace marker exists without a registered workspace")
	} else if !os.IsNotExist(err) {
		return err
	}
	if _, err := mirror.AddWorktree(workspace, sourceSHA); err != nil {
		return err
	}
	if err := m.ensureMarker(markerPath, markerFor(action, repo.ID, sourceSHA), true); err != nil {
		// Do not erase the worktree here. The committed action plus exact git
		// registration is the crash-recovery evidence for the next pass.
		return err
	}
	return nil
}

func (m *localEpicWorkerWorkspaceManager) FinalizeLifecycleWorkspace(ctx context.Context,
	action driver.Action, receipt driver.LifecycleReceipt, _ time.Time) error {
	if action.Kind != "worker_stop" {
		return nil
	}
	if receipt.Operation != "stop" || (receipt.Status != "stopped" && receipt.Status != "target_absent") ||
		receipt.AbsenceObservedAt == "" {
		return errors.New("workspace cleanup requires exact positive Driver absence")
	}
	return m.finalizeExactWorkspace(ctx, action)
}

// FinalizePreEffectLifecycleWorkspace deletes a workspace only for Flowbee's
// synthetic worker_workspace_cleanup action.  Store materializes that action
// after it has atomically rechecked the original Ensure has an explicit
// pre-effect certificate and zero Driver receipts.  This method deliberately
// performs no Driver inspection or mutation; it proves filesystem authority
// solely from the immutable action tuple and its owner-only marker.
func (m *localEpicWorkerWorkspaceManager) FinalizePreEffectLifecycleWorkspace(ctx context.Context,
	action driver.Action, _ time.Time) error {
	if action.Kind != "worker_workspace_cleanup" {
		return errors.New("pre-effect workspace cleanup requires exact cleanup action")
	}
	return m.finalizeExactWorkspace(ctx, action)
}

func (m *localEpicWorkerWorkspaceManager) finalizeExactWorkspace(ctx context.Context, action driver.Action) error {
	workspace, markerPath, sourceSHA, repo, err := m.resolve(ctx, action)
	if err != nil {
		return err
	}
	unlock := m.lock(workspace)
	defer unlock()
	_, workspaceErr := os.Lstat(workspace)
	_, markerErr := os.Lstat(markerPath)
	if os.IsNotExist(workspaceErr) && os.IsNotExist(markerErr) {
		return nil
	}
	if workspaceErr != nil && !os.IsNotExist(workspaceErr) {
		return workspaceErr
	}
	if markerErr != nil {
		return errors.New("workspace cleanup refused: exact Flowbee marker is missing")
	}
	want := markerFor(action, repo.ID, sourceSHA)
	if err := m.ensureMarker(markerPath, want, false); err != nil {
		return fmt.Errorf("workspace cleanup refused: %w", err)
	}
	mirrorPath := m.MirrorPath(repo)
	resolvedMirror, err := filepath.EvalSymlinks(filepath.Clean(mirrorPath))
	if err != nil || resolvedMirror != filepath.Clean(mirrorPath) {
		return errors.New("workspace cleanup mirror is missing or symlinked")
	}
	if workspaceErr == nil {
		mirror := gitops.Open(resolvedMirror)
		var removeErr error
		if action.TargetRole == store.DriverReviewerRole || action.TargetRole == "code_reviewer" {
			removeErr = mirror.RemoveRegisteredWorktree(workspace, sourceSHA)
		} else {
			removeErr = mirror.RemoveExactRegisteredWorktree(workspace)
		}
		if removeErr != nil {
			return removeErr
		}
	} else {
		// Missing directory with a marker is a crash after removal but before
		// marker deletion. Prune only the exact mirror's stale metadata.
		mirror := gitops.Open(resolvedMirror)
		_ = mirror.PruneWorktrees()
		if _, registered, checkErr := mirror.RegisteredWorktree(workspace); checkErr != nil || registered {
			if checkErr != nil {
				return checkErr
			}
			return errors.New("missing workspace remains registered; cleanup is ambiguous")
		}
	}
	if err := os.Remove(markerPath); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func (m *localEpicWorkerWorkspaceManager) resolve(ctx context.Context, action driver.Action) (string, string, string, store.Repo, error) {
	var repo store.Repo
	if m == nil || m.DB == nil || m.MirrorPath == nil || len(m.Roots) == 0 || len(m.LocalHostIDs) == 0 {
		return "", "", "", repo, errors.New("local epic worker workspace manager is not configured")
	}
	if !m.LocalHostIDs[action.TargetHostID] {
		return "", "", "", repo, errors.New("remote worker workspace remains fail closed")
	}
	root, ok := m.Roots[action.WorkspaceRootID]
	if !ok {
		return "", "", "", repo, fmt.Errorf("workspace root ID %q has no machine-local mapping", action.WorkspaceRootID)
	}
	rel := action.WorkspaceRelativePath
	if rel == "" || strings.Contains(rel, "\\") || filepath.IsAbs(rel) || filepath.Clean(rel) != filepath.FromSlash(rel) ||
		rel == "." || strings.HasPrefix(rel, "../") || strings.ContainsRune(rel, '\x00') {
		return "", "", "", repo, errors.New("workspace relative path is not clean local authority")
	}
	workspace := filepath.Join(root, filepath.FromSlash(rel))
	if !pathWithin(root, workspace) {
		return "", "", "", repo, errors.New("workspace path escapes configured root")
	}
	sourceSHA := action.BaseSHA
	if action.TargetRole == store.DriverReviewerRole || action.TargetRole == "code_reviewer" {
		sourceSHA = action.HeadSHA
	}
	if !validWorkspaceGitSHA(sourceSHA) {
		return "", "", "", repo, errors.New("workspace action has no immutable source SHA")
	}
	var repoID string
	if err := m.DB.QueryRowContext(ctx, `SELECT COALESCE(NULLIF(d.delivery_repo,''),e.repo)
		FROM epics e JOIN epic_deliveries d ON d.epic_id=e.id AND d.project_id=e.project_id
		WHERE e.id=? AND e.project_id=?`, action.EpicID, action.ProjectID).Scan(&repoID); err != nil {
		return "", "", "", repo, fmt.Errorf("resolve workspace repository: %w", err)
	}
	err := m.DB.QueryRowContext(ctx, `SELECT id,owner,repo,default_branch,active FROM repos WHERE id=?`, repoID).
		Scan(&repo.ID, &repo.Owner, &repo.Repo, &repo.DefaultBranch, &repo.Active)
	if err != nil {
		return "", "", "", repo, fmt.Errorf("resolve workspace repository registry: %w", err)
	}
	return workspace, workspace + ".flowbee-workspace.json", sourceSHA, repo, nil
}

func (m *localEpicWorkerWorkspaceManager) lock(path string) func() {
	m.locksMu.Lock()
	if m.locks == nil {
		m.locks = make(map[string]*sync.Mutex)
	}
	mu := m.locks[path]
	if mu == nil {
		mu = &sync.Mutex{}
		m.locks[path] = mu
	}
	m.locksMu.Unlock()
	mu.Lock()
	return mu.Unlock
}

func (m *localEpicWorkerWorkspaceManager) ensureMarker(path string, want localEpicWorkspaceMarker, allowCreate bool) error {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) && allowCreate {
		return writeWorkspaceMarker(path, want)
	}
	if err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0o077 != 0 {
		return errors.New("workspace marker is not an owner-only regular file")
	}
	var got localEpicWorkspaceMarker
	if json.Unmarshal(data, &got) != nil || got != want {
		return errors.New("workspace marker does not match immutable action authority")
	}
	return nil
}

func markerFor(action driver.Action, repoID, sourceSHA string) localEpicWorkspaceMarker {
	return localEpicWorkspaceMarker{Format: "flowbee.local-epic-workspace/v1", ProjectID: action.ProjectID,
		EpicID: action.EpicID, Role: action.TargetRole, LifecycleKey: action.LifecycleKey,
		WorkspaceRootID: action.WorkspaceRootID, WorkspaceRelativePath: action.WorkspaceRelativePath,
		RepositoryID: repoID, SourceSHA: sourceSHA}
}

func writeWorkspaceMarker(path string, marker localEpicWorkspaceMarker) error {
	b, _ := json.Marshal(marker)
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		parentInfo, parentErr := os.Lstat(filepath.Dir(path))
		return fmt.Errorf("create workspace marker (parent=%v parent_err=%v): %w", parentInfo, parentErr, err)
	}
	if _, err = f.Write(b); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	return closeErr
}

func ensureSecureParents(root, workspace string) error {
	if err := requireOwnerOnlyDirectory(root); err != nil {
		return err
	}
	parent := filepath.Dir(workspace)
	rel, err := filepath.Rel(root, parent)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return errors.New("workspace parent escapes configured root")
	}
	current := root
	if rel == "." {
		return nil
	}
	for _, part := range strings.Split(rel, string(filepath.Separator)) {
		current = filepath.Join(current, part)
		if err := os.Mkdir(current, 0o700); err != nil && !os.IsExist(err) {
			return err
		}
		if err := requireOwnerOnlyDirectory(current); err != nil {
			return err
		}
	}
	return nil
}

func requireOwnerOnlyDirectory(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0o077 != 0 {
		return errors.New("path is not a real owner-only directory")
	}
	if stat, ok := info.Sys().(*syscall.Stat_t); !ok || int(stat.Uid) != os.Geteuid() {
		return errors.New("path is not owned by the Flowbee process user")
	}
	return nil
}

func pathWithin(root, child string) bool {
	rel, err := filepath.Rel(root, child)
	return err == nil && rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func validWorkspaceGitSHA(value string) bool {
	if len(value) != 40 && len(value) != 64 {
		return false
	}
	for _, r := range value {
		if !strings.ContainsRune("0123456789abcdef", r) {
			return false
		}
	}
	return true
}

func isEpicWorkerEnsure(action driver.Action) bool {
	switch action.Kind {
	case "builder_launch", "builder_rework", "conflict_resolution", "reviewer_launch", "worker_recover":
		return action.EpicID != ""
	default:
		return false
	}
}
