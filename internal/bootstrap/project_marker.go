package bootstrap

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

const (
	projectMarkerFormat = "flowbee.project-marker/v1"
	projectMarkerRel    = ".flowbee/project.json"
	projectMarkerIgnore = "/.flowbee/project.json"
)

// OriginResolver resolves the exact first-bootstrap repository origin. It is an
// injected boundary: the core never infers a project from CWD or a remote name.
type OriginResolver interface {
	ExactOrigin(context.Context, string) (string, error)
}

type ProjectMarker struct {
	Format           string `json:"format"`
	ProjectID        string `json:"project_id"`
	RepositoryOrigin string `json:"repository_origin"`
	RepositoryPath   string `json:"repository_path"`
}

// ExistingProjectMarker reads only an already-admitted private marker. It does
// not resolve Git origin and never creates state, allowing a rerun to recover
// while Git/network origin discovery is unavailable.
func ExistingProjectMarker(repoRoot string) (ProjectMarker, bool, error) {
	root, err := exactAbsoluteDir(repoRoot)
	if err != nil {
		return ProjectMarker{}, false, err
	}
	marker, found, err := readProjectMarker(filepath.Join(root, filepath.FromSlash(projectMarkerRel)))
	if err != nil || !found {
		return marker, found, err
	}
	if marker.RepositoryPath != root {
		return ProjectMarker{}, false, errors.New("project marker repository path changed")
	}
	return marker, true, nil
}

// FileProjectInitResolver owns only the approved machine-local marker. GitInfoDir
// is explicit so linked worktrees are supported without guessing where .git is.
type FileProjectInitResolver struct {
	RepoRoot           string
	GitInfoDir         string
	RequestedProjectID string
	Origins            OriginResolver
}

func (r FileProjectInitResolver) ResolveProjectInit(ctx context.Context, projectID string) (ProjectInit, error) {
	if r.Origins == nil {
		return ProjectInit{}, errors.New("exact repository origin resolver is required")
	}
	root, err := exactAbsoluteDir(r.RepoRoot)
	if err != nil {
		return ProjectInit{}, err
	}
	if projectID == "" || r.RequestedProjectID == "" || projectID != r.RequestedProjectID {
		return ProjectInit{}, errors.New("project marker requires one exact requested project id")
	}
	markerPath := filepath.Join(root, filepath.FromSlash(projectMarkerRel))
	marker, found, err := readProjectMarker(markerPath)
	if err != nil {
		return ProjectInit{}, err
	}
	if found {
		// A private, immutable marker is the rerun authority. Do not make
		// bootstrap availability depend on Git/network origin resolution after
		// first admission; the server later revalidates project/repository scope.
		if marker.ProjectID != projectID || marker.RepositoryPath != root {
			return ProjectInit{}, errors.New("project marker disagrees with the exact project or machine-local path")
		}
		if err := ensureLocalIgnore(r.GitInfoDir); err != nil {
			return ProjectInit{}, err
		}
		return ProjectInit{ProjectID: marker.ProjectID, RepositoryOrigin: marker.RepositoryOrigin,
			CWD: marker.RepositoryPath}, nil
	}
	origin, err := r.Origins.ExactOrigin(ctx, root)
	if err != nil {
		return ProjectInit{}, fmt.Errorf("resolve exact repository origin: %w", err)
	}
	origin, err = NormalizeRepositoryOrigin(origin)
	if err != nil {
		return ProjectInit{}, err
	}
	marker = ProjectMarker{Format: projectMarkerFormat, ProjectID: projectID,
		RepositoryOrigin: origin, RepositoryPath: root}
	if err := ensureLocalIgnore(r.GitInfoDir); err != nil {
		return ProjectInit{}, err
	}
	if err := writeProjectMarker(markerPath, marker); err != nil {
		return ProjectInit{}, err
	}
	return ProjectInit{ProjectID: marker.ProjectID, RepositoryOrigin: marker.RepositoryOrigin,
		CWD: marker.RepositoryPath}, nil
}

func exactAbsoluteDir(path string) (string, error) {
	if path == "" || !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return "", errors.New("repository root must be an exact absolute clean path")
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", fmt.Errorf("resolve canonical repository root: %w", err)
	}
	resolved = filepath.Clean(resolved)
	if !filepath.IsAbs(resolved) {
		return "", errors.New("canonical repository root is not absolute")
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", fmt.Errorf("repository root is not a directory: %w", err)
	}
	if !info.IsDir() {
		return "", errors.New("repository root is not a directory")
	}
	return resolved, nil
}

// NormalizeRepositoryOrigin collapses credential-free HTTPS and SSH spellings
// onto one stable host/owner/repository identity.
func NormalizeRepositoryOrigin(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" || strings.ContainsAny(raw, "?#") {
		return "", errors.New("repository origin is empty or contains query/fragment data")
	}
	if strings.HasPrefix(raw, "git@") && !strings.Contains(raw, "://") {
		parts := strings.SplitN(strings.TrimPrefix(raw, "git@"), ":", 2)
		if len(parts) != 2 {
			return "", errors.New("invalid SSH repository origin")
		}
		return canonicalRepositoryIdentity(parts[0], parts[1])
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Hostname() == "" {
		return "", errors.New("repository origin must be exact HTTPS or SSH URL")
	}
	switch strings.ToLower(u.Scheme) {
	case "https":
		if u.User != nil {
			return "", errors.New("repository origin must not contain credentials")
		}
	case "ssh":
		if u.User != nil && u.User.Username() != "git" {
			return "", errors.New("SSH repository origin has an unexpected user")
		}
		if _, hasPassword := u.User.Password(); hasPassword {
			return "", errors.New("repository origin must not contain credentials")
		}
	default:
		return "", errors.New("repository origin scheme is not allowed")
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return "", errors.New("repository origin must not contain query or fragment data")
	}
	return canonicalRepositoryIdentity(u.Hostname(), u.EscapedPath())
}

func canonicalRepositoryIdentity(host, repoPath string) (string, error) {
	host = strings.ToLower(strings.TrimSpace(host))
	pathValue, err := url.PathUnescape(strings.Trim(repoPath, "/"))
	if err != nil {
		return "", errors.New("repository origin path is invalid")
	}
	pathValue = strings.TrimSuffix(pathValue, ".git")
	parts := strings.Split(pathValue, "/")
	if host == "" || len(parts) != 2 || parts[0] == "" || parts[1] == "" ||
		parts[0] == "." || parts[1] == "." || parts[0] == ".." || parts[1] == ".." {
		return "", errors.New("repository origin must identify exactly one owner/repository")
	}
	return host + "/" + strings.ToLower(parts[0]) + "/" + strings.ToLower(parts[1]), nil
}

func readProjectMarker(path string) (ProjectMarker, bool, error) {
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if errors.Is(err, syscall.ENOENT) {
		return ProjectMarker{}, false, nil
	}
	if err != nil {
		return ProjectMarker{}, false, fmt.Errorf("open project marker without following links: %w", err)
	}
	f := os.NewFile(uintptr(fd), path)
	if f == nil {
		_ = syscall.Close(fd)
		return ProjectMarker{}, false, errors.New("open project marker: invalid file descriptor")
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return ProjectMarker{}, false, fmt.Errorf("inspect open project marker: %w", err)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 ||
		stat.Uid != uint32(os.Geteuid()) || stat.Nlink != 1 {
		return ProjectMarker{}, false, errors.New("project marker must be a private regular file")
	}
	dec := json.NewDecoder(io.LimitReader(f, 64<<10))
	dec.DisallowUnknownFields()
	var marker ProjectMarker
	if err := dec.Decode(&marker); err != nil {
		return ProjectMarker{}, false, fmt.Errorf("decode project marker: %w", err)
	}
	var trailing any
	if err := dec.Decode(&trailing); !errors.Is(err, io.EOF) {
		return ProjectMarker{}, false, errors.New("project marker contains trailing data")
	}
	if marker.Format != projectMarkerFormat || marker.ProjectID == "" ||
		marker.RepositoryOrigin == "" || marker.RepositoryPath == "" {
		return ProjectMarker{}, false, errors.New("invalid project marker")
	}
	return marker, true, nil
}

func writeProjectMarker(path string, marker ProjectMarker) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create project marker directory: %w", err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return fmt.Errorf("secure project marker directory: %w", err)
	}
	body, err := json.MarshalIndent(marker, "", "  ")
	if err != nil {
		return err
	}
	body = append(body, '\n')
	tmp, err := os.CreateTemp(dir, ".project-*.tmp")
	if err != nil {
		return fmt.Errorf("create project marker temp file: %w", err)
	}
	tmpName := tmp.Name()
	defer func() {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
	}()
	if err := tmp.Chmod(0o600); err != nil {
		return err
	}
	if _, err := tmp.Write(body); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	// Link installs without replacing a marker created concurrently by a
	// different bootstrap plan. A collision fails closed and is reconciled by a
	// fresh marker read.
	if err := os.Link(tmpName, path); err != nil {
		return fmt.Errorf("install project marker: %w", err)
	}
	d, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("open project marker directory: %w", err)
	}
	defer d.Close()
	if err := d.Sync(); err != nil {
		return fmt.Errorf("sync project marker directory: %w", err)
	}
	return nil
}

func ensureLocalIgnore(infoDir string) error {
	if infoDir == "" || !filepath.IsAbs(infoDir) || filepath.Clean(infoDir) != infoDir {
		return errors.New("exact absolute git info directory is required")
	}
	if err := os.MkdirAll(infoDir, 0o700); err != nil {
		return fmt.Errorf("create git info directory: %w", err)
	}
	path := filepath.Join(infoDir, "exclude")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return fmt.Errorf("open local git exclude: %w", err)
	}
	defer f.Close()
	if err := os.Chmod(path, 0o600); err != nil {
		return err
	}
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		if strings.TrimSpace(scanner.Text()) == projectMarkerIgnore {
			return scanner.Err()
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	if _, err := f.Seek(0, io.SeekEnd); err != nil {
		return err
	}
	info, err := f.Stat()
	if err != nil {
		return err
	}
	prefix := ""
	if info.Size() > 0 {
		prefix = "\n"
	}
	if _, err := io.WriteString(f, prefix+projectMarkerIgnore+"\n"); err != nil {
		return err
	}
	return f.Sync()
}
