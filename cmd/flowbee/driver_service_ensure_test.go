package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/samhotchkiss/flowbee/internal/bootstrap"
)

type serviceRunnerFake struct {
	stdout []byte
	exit   int
	err    error
	path   string
	args   []string
}

func (f *serviceRunnerFake) Run(_ context.Context, path string, args []string) ([]byte, int, error) {
	f.path, f.args = path, append([]string(nil), args...)
	return f.stdout, f.exit, f.err
}

func pinnedManagerFixture(t *testing.T) (string, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "tmux-driver-service")
	body := []byte("#!/bin/sh\nexit 99\n")
	if err := os.WriteFile(path, body, 0o700); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(body)
	return path, "sha256:" + hex.EncodeToString(sum[:])
}

func serviceReceiptJSON(t *testing.T, status string) []byte {
	t.Helper()
	receipt := bootstrap.DriverServiceEnsureReceipt{FormatVersion: bootstrap.DriverServiceEnsureReceiptFormat,
		ServiceReceiptID: "receipt", ActionID: "action", RequestFingerprint: "sha256:" + strings.Repeat("a", 64),
		Status: status, Readiness: "pending", Change: "none", AcceptedAt: "2026-07-19T00:00:00Z"}
	if status == "uncertain" {
		receipt.Readiness, receipt.CompletedAt, receipt.DiagnosticCode = "unproven", "2026-07-19T00:00:01Z", "service_effect_unproven"
	}
	body, err := json.Marshal(receipt)
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func TestPinnedServiceEnsureParsesDurableNonzeroReceiptAndUsesClosedArgs(t *testing.T) {
	manager, hash := pinnedManagerFixture(t)
	runner := &serviceRunnerFake{stdout: serviceReceiptJSON(t, "accepted"), exit: 1}
	ensurer := pinnedDriverServiceEnsurer{ManagerPath: manager, ManagerSHA256: hash, Runner: runner}
	req := bootstrap.DriverServiceEnsureRequest{ActionID: "action", ReleaseID: "release",
		ExecutablePath: "/opt/tmux-driver", ExecutableSHA256: "sha256:" + strings.Repeat("b", 64),
		ConfigPath: "/etc/tmux-driver.toml", ConfigSHA256: "sha256:" + strings.Repeat("c", 64),
		InstanceRef: "managed", ExpectedStoreID: "store", ExpectedDomainID: "flowbee",
		RequiredContracts: map[string]string{"z": "contract-z", "a": "contract-a"}}
	receipt, err := ensurer.EnsureDriverService(context.Background(), req)
	if err != nil || receipt.Status != "accepted" {
		t.Fatalf("receipt=%+v err=%v", receipt, err)
	}
	want := []string{"ensure", "--kind", "launchd", "--config", req.ConfigPath, "--executable", req.ExecutablePath,
		"--instance", "managed", "--json", "--action-id", "action", "--release-id", "release",
		"--expected-executable-sha256", req.ExecutableSHA256, "--expected-config-sha256", req.ConfigSHA256,
		"--expected-store-id", "store", "--expected-server-domain", "flowbee", "--timeout-seconds", "30",
		"--expected-contract", "a=contract-a", "--expected-contract", "z=contract-z"}
	if runner.path != manager || !reflect.DeepEqual(runner.args, want) {
		t.Fatalf("command=%q %#v\nwant=%q %#v", runner.path, runner.args, manager, want)
	}
	for _, forbidden := range []string{"launchctl", "python", "--update"} {
		if strings.Contains(strings.Join(runner.args, " "), forbidden) {
			t.Fatalf("forbidden argument %q", forbidden)
		}
	}
}

func TestPinnedServiceEnsureUpdateRequiresExplicitAuthorityAndRejectsBadPin(t *testing.T) {
	manager, hash := pinnedManagerFixture(t)
	runner := &serviceRunnerFake{stdout: serviceReceiptJSON(t, "uncertain"), exit: 1}
	ensurer := pinnedDriverServiceEnsurer{ManagerPath: manager, ManagerSHA256: hash, Runner: runner}
	req := bootstrap.DriverServiceEnsureRequest{ActionID: "action", ReleaseID: "release",
		ExecutablePath: "/opt/tmux-driver", ExecutableSHA256: "sha256:" + strings.Repeat("b", 64),
		ConfigPath: "/etc/tmux-driver.toml", ConfigSHA256: "sha256:" + strings.Repeat("c", 64),
		InstanceRef: "managed", ExpectedStoreID: "store", ExpectedDomainID: "flowbee",
		RequiredContracts: map[string]string{"a": "contract-a"}, UpdateAuthorized: true}
	if _, err := ensurer.EnsureDriverService(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if runner.args[len(runner.args)-1] != "--update" {
		t.Fatalf("explicit update missing: %v", runner.args)
	}
	ensurer.ManagerSHA256 = "sha256:" + strings.Repeat("0", 64)
	if _, err := ensurer.EnsureDriverService(context.Background(), req); err == nil {
		t.Fatal("manager hash mismatch accepted")
	}
	if !errors.Is(runner.err, nil) {
		t.Fatal(runner.err)
	}
}
