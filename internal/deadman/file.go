package deadman

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

type FileStore struct{ Path string }

func (f FileStore) Load() (State, error) {
	var state State
	fd, err := syscall.Open(f.Path, syscall.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if errors.Is(err, syscall.ENOENT) {
		return state, nil
	}
	if err != nil {
		return state, err
	}
	file := os.NewFile(uintptr(fd), f.Path)
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return state, err
	}
	if !info.Mode().IsRegular() {
		return state, fmt.Errorf("state path is not a regular file")
	}
	if info.Mode().Perm()&0077 != 0 {
		return state, fmt.Errorf("state file permissions %04o are not owner-only", info.Mode().Perm())
	}
	dec := json.NewDecoder(io.LimitReader(file, 4<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&state); err != nil {
		return state, err
	}
	return state, nil
}

func (f FileStore) Save(state State) error {
	if strings.TrimSpace(f.Path) == "" {
		return errors.New("watchdog state path is required")
	}
	dir := filepath.Dir(f.Path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	if err := os.Chmod(dir, 0700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".flowbee-watchdog-state-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0600); err != nil {
		_ = tmp.Close()
		return err
	}
	enc := json.NewEncoder(tmp)
	enc.SetIndent("", "  ")
	if err := enc.Encode(state); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, f.Path); err != nil {
		return err
	}
	if err := os.Chmod(f.Path, 0600); err != nil {
		return err
	}
	if dirFile, err := os.Open(dir); err == nil {
		_ = dirFile.Sync()
		_ = dirFile.Close()
	}
	return nil
}

// Lock prevents a cron invocation and a long-running service from publishing
// competing transitions from the same durable state.
func (f FileStore) Lock() (io.Closer, error) {
	if strings.TrimSpace(f.Path) == "" {
		return nil, errors.New("watchdog state path is required")
	}
	if err := os.MkdirAll(filepath.Dir(f.Path), 0700); err != nil {
		return nil, err
	}
	lock, err := os.OpenFile(f.Path+".lock", os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = lock.Close()
		return nil, fmt.Errorf("watchdog state is already locked: %w", err)
	}
	return &fileLock{File: lock}, nil
}

type fileLock struct{ *os.File }

func (l *fileLock) Close() error {
	_ = syscall.Flock(int(l.Fd()), syscall.LOCK_UN)
	return l.File.Close()
}

// ReadOwnerOnlySecret reads the webhook key without following symlinks. The
// secret is deliberately file-backed so it never appears in argv, unit files,
// process environments, or generated documentation.
func ReadOwnerOnlySecret(path string) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", errors.New("webhook secret file is required")
	}
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return "", err
	}
	file := os.NewFile(uintptr(fd), path)
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", errors.New("webhook secret path is not a regular file")
	}
	if info.Mode().Perm()&0077 != 0 {
		return "", fmt.Errorf("webhook secret permissions %04o are not owner-only (want 0600 or stricter)", info.Mode().Perm())
	}
	data, err := io.ReadAll(io.LimitReader(file, (1<<20)+1))
	if err != nil {
		return "", err
	}
	if len(data) > 1<<20 {
		return "", errors.New("webhook secret file exceeds 1 MiB")
	}
	secret := strings.TrimSpace(string(data))
	if secret == "" {
		return "", errors.New("webhook secret file is empty")
	}
	return secret, nil
}
