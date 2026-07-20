package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/samhotchkiss/flowbee/internal/bootstrap"
)

type serviceEnsureRunner interface {
	Run(context.Context, string, []string) ([]byte, int, error)
}

type execServiceEnsureRunner struct{}

func (execServiceEnsureRunner) Run(ctx context.Context, executable string, args []string) ([]byte, int, error) {
	cmd := exec.CommandContext(ctx, executable, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	if err == nil {
		return stdout.Bytes(), 0, nil
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		// Exit 1 is part of the service-manager protocol for a durable accepted
		// or uncertain receipt. The caller validates stdout before deciding.
		return stdout.Bytes(), exit.ExitCode(), nil
	}
	return nil, -1, fmt.Errorf("execute pinned tmux-driver-service: %w", err)
}

type pinnedDriverServiceEnsurer struct {
	ManagerPath, ManagerSHA256 string
	Timeout                    time.Duration
	Runner                     serviceEnsureRunner
}

func (e pinnedDriverServiceEnsurer) EnsureDriverService(ctx context.Context,
	req bootstrap.DriverServiceEnsureRequest) (bootstrap.DriverServiceEnsureReceipt, error) {
	if err := verifyPinnedExecutable(e.ManagerPath, e.ManagerSHA256); err != nil {
		return bootstrap.DriverServiceEnsureReceipt{}, fmt.Errorf("verify tmux-driver-service: %w", err)
	}
	timeout := e.Timeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	if timeout > 2*time.Minute {
		return bootstrap.DriverServiceEnsureReceipt{}, errors.New("tmux-driver-service timeout exceeds the bounded maximum")
	}
	runner := e.Runner
	if runner == nil {
		runner = execServiceEnsureRunner{}
	}
	args := []string{"ensure", "--kind", "launchd", "--config", req.ConfigPath,
		"--executable", req.ExecutablePath, "--instance", req.InstanceRef, "--json",
		"--action-id", req.ActionID, "--release-id", req.ReleaseID,
		"--expected-executable-sha256", req.ExecutableSHA256,
		"--expected-config-sha256", req.ConfigSHA256,
		"--expected-store-id", req.ExpectedStoreID,
		"--expected-server-domain", req.ExpectedDomainID,
		"--timeout-seconds", strconv.FormatFloat(timeout.Seconds(), 'f', -1, 64)}
	contractNames := make([]string, 0, len(req.RequiredContracts))
	for name := range req.RequiredContracts {
		contractNames = append(contractNames, name)
	}
	sort.Strings(contractNames)
	for _, name := range contractNames {
		args = append(args, "--expected-contract", name+"="+req.RequiredContracts[name])
	}
	if req.UpdateAuthorized {
		args = append(args, "--update")
	}
	callCtx, cancel := context.WithTimeout(ctx, timeout+5*time.Second)
	defer cancel()
	stdout, exitCode, err := runner.Run(callCtx, e.ManagerPath, args)
	if err != nil {
		return bootstrap.DriverServiceEnsureReceipt{}, err
	}
	if len(stdout) == 0 || len(stdout) > 128<<10 {
		return bootstrap.DriverServiceEnsureReceipt{}, errors.New("tmux-driver-service returned an invalid receipt size")
	}
	decoder := json.NewDecoder(bytes.NewReader(stdout))
	decoder.DisallowUnknownFields()
	var receipt bootstrap.DriverServiceEnsureReceipt
	if err := decoder.Decode(&receipt); err != nil {
		return bootstrap.DriverServiceEnsureReceipt{}, errors.New("tmux-driver-service returned no valid durable receipt")
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return bootstrap.DriverServiceEnsureReceipt{}, errors.New("tmux-driver-service receipt contains trailing data")
	}
	switch receipt.Status {
	case "ready":
		if exitCode != 0 {
			return bootstrap.DriverServiceEnsureReceipt{}, errors.New("ready tmux-driver-service receipt had nonzero exit")
		}
	case "accepted", "uncertain":
		if exitCode != 1 {
			return bootstrap.DriverServiceEnsureReceipt{}, errors.New("non-ready tmux-driver-service receipt had unexpected exit")
		}
	default:
		return bootstrap.DriverServiceEnsureReceipt{}, errors.New("tmux-driver-service returned an unsupported receipt state")
	}
	return receipt, nil
}

func verifyPinnedExecutable(path, expected string) error {
	if path == "" || !strings.HasPrefix(expected, "sha256:") || len(expected) != len("sha256:")+64 {
		return errors.New("exact manager path and SHA-256 are required")
	}
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	file := os.NewFile(uintptr(fd), path)
	if file == nil {
		_ = syscall.Close(fd)
		return errors.New("open pinned manager executable")
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
		return errors.New("pinned manager must be an executable regular file")
	}
	h := sha256.New()
	if _, err := io.Copy(h, file); err != nil {
		return err
	}
	got := "sha256:" + hex.EncodeToString(h.Sum(nil))
	if got != expected {
		return errors.New("pinned manager SHA-256 mismatch")
	}
	return nil
}
