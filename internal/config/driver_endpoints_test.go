package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const (
	testHostExternal  = "11111111-1111-4111-8111-111111111111"
	testStoreExternal = "22222222-2222-4222-8222-222222222222"
	testHostManaged   = "33333333-3333-4333-8333-333333333333"
	testStoreManaged  = "44444444-4444-4444-8444-444444444444"
)

func writeDriverInventoryFixture(t *testing.T) (string, DriverEndpointInventory) {
	t.Helper()
	dir := t.TempDir()
	for _, name := range []string{"external.token", "managed.token"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("bearer-"+name+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	inventory := DriverEndpointInventory{
		FormatVersion: driverEndpointsFormat,
		Endpoints: []DriverEndpoint{
			{
				InstanceRef: "external-default", UDSPath: filepath.Join(dir, "external.sock"),
				TokenFile: filepath.Join(dir, "external.token"), ExpectedHostID: testHostExternal,
				ExpectedStoreID: testStoreExternal, ExpectedTmuxServerDomainID: "default",
				ExpectedTmuxServerOwnership: "external",
			},
			{
				InstanceRef: "flowbee-managed", UDSPath: filepath.Join(dir, "managed.sock"),
				TokenFile: filepath.Join(dir, "managed.token"), ExpectedHostID: testHostManaged,
				ExpectedStoreID: testStoreManaged, ExpectedTmuxServerDomainID: "flowbee",
				ExpectedTmuxServerOwnership: "managed_dedicated",
			},
		},
	}
	data, err := json.Marshal(inventory)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "driver-endpoints.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path, inventory
}

func rewriteDriverInventoryFixture(t *testing.T, path string, inventory DriverEndpointInventory) {
	t.Helper()
	data, err := json.Marshal(inventory)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestLoadDriverEndpointInventoryRequiresExplicitExactTopology(t *testing.T) {
	path, want := writeDriverInventoryFixture(t)
	got, err := LoadDriverEndpointInventory(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Endpoints) != 2 || got.Endpoints[0] != want.Endpoints[0] || got.Endpoints[1] != want.Endpoints[1] {
		t.Fatalf("inventory mismatch: got=%+v want=%+v", got, want)
	}

	t.Setenv(DriverEndpointsFileEnv, path)
	fromEnv, configured, err := LoadDriverEndpointInventoryFromEnv()
	if err != nil || !configured || len(fromEnv.Endpoints) != 2 {
		t.Fatalf("environment inventory: configured=%v inventory=%+v err=%v", configured, fromEnv, err)
	}
}

func TestDriverEndpointInventoryNeverSynthesizesLegacyDefault(t *testing.T) {
	t.Setenv(DriverEndpointsFileEnv, "")
	t.Setenv("FLOWBEE_DRIVER_SOCKET", "/tmp/legacy.sock")
	t.Setenv("FLOWBEE_DRIVER_TOKEN_FILE", "/tmp/legacy.token")
	t.Setenv("FLOWBEE_DRIVER_INSTANCE_REF", "legacy-default")
	inventory, configured, err := LoadDriverEndpointInventoryFromEnv()
	if err != nil || configured || len(inventory.Endpoints) != 0 {
		t.Fatalf("legacy single endpoint synthesized into v2 inventory: configured=%v inventory=%+v err=%v", configured, inventory, err)
	}
}

func TestDriverEndpointInventoryRejectsUnknownOrInlineSecretFields(t *testing.T) {
	path, _ := writeDriverInventoryFixture(t)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	bad := strings.Replace(string(data), `"instance_ref":"external-default"`, `"instance_ref":"external-default","token":"inline-secret"`, 1)
	if err := os.WriteFile(path, []byte(bad), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadDriverEndpointInventory(path); err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("unknown inline secret accepted: %v", err)
	}
}

func TestDriverEndpointInventoryRejectsLooseOrSymlinkedFiles(t *testing.T) {
	path, inventory := writeDriverInventoryFixture(t)
	if err := os.Chmod(path, 0o640); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadDriverEndpointInventory(path); err == nil || !strings.Contains(err.Error(), "not owner-only") {
		t.Fatalf("loose inventory accepted: %v", err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(filepath.Dir(path), "inventory-link.json")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadDriverEndpointInventory(link); err == nil {
		t.Fatal("symlinked inventory accepted")
	}

	if err := os.Chmod(inventory.Endpoints[0].TokenFile, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadDriverEndpointInventory(path); err == nil || !strings.Contains(err.Error(), "not owner-only") {
		t.Fatalf("loose token file accepted: %v", err)
	}
	if err := os.Remove(inventory.Endpoints[0].TokenFile); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(inventory.Endpoints[1].TokenFile, inventory.Endpoints[0].TokenFile); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadDriverEndpointInventory(path); err == nil {
		t.Fatal("symlinked token file accepted")
	}
}

func TestDriverEndpointInventoryAllowsMultipleExternalHosts(t *testing.T) {
	path, inventory := writeDriverInventoryFixture(t)
	secondExternal := inventory.Endpoints[0]
	secondExternal.InstanceRef = "external-default-host-2"
	secondExternal.UDSPath = filepath.Join(filepath.Dir(path), "external-host-2.sock")
	secondExternal.ExpectedHostID = "55555555-5555-4555-8555-555555555555"
	secondExternal.ExpectedStoreID = "66666666-6666-4666-8666-666666666666"
	inventory.Endpoints = append(inventory.Endpoints, secondExternal)
	rewriteDriverInventoryFixture(t, path, inventory)
	got, err := LoadDriverEndpointInventory(path)
	if err != nil {
		t.Fatalf("multiple exact external/default hosts rejected: %v", err)
	}
	if len(got.Endpoints) != 3 {
		t.Fatalf("endpoint count=%d want 3", len(got.Endpoints))
	}
}

func TestDriverEndpointInventoryRejectsHardlinkedInventoryAndToken(t *testing.T) {
	path, _ := writeDriverInventoryFixture(t)
	if err := os.Link(path, path+".hardlink"); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadDriverEndpointInventory(path); err == nil || !strings.Contains(err.Error(), "must not have hard links") {
		t.Fatalf("hardlinked inventory accepted: %v", err)
	}

	path, inventory := writeDriverInventoryFixture(t)
	if err := os.Link(inventory.Endpoints[0].TokenFile, inventory.Endpoints[0].TokenFile+".hardlink"); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadDriverEndpointInventory(path); err == nil || !strings.Contains(err.Error(), "must not have hard links") {
		t.Fatalf("hardlinked token accepted: %v", err)
	}
}

func TestDriverEndpointInventoryRejectsDuplicateResolutionAuthority(t *testing.T) {
	path, inventory := writeDriverInventoryFixture(t)
	inventory.Endpoints[1].InstanceRef = inventory.Endpoints[0].InstanceRef
	rewriteDriverInventoryFixture(t, path, inventory)
	if _, err := LoadDriverEndpointInventory(path); err == nil || !strings.Contains(err.Error(), "duplicate Driver instance_ref") {
		t.Fatalf("duplicate instance ref accepted: %v", err)
	}

	path, inventory = writeDriverInventoryFixture(t)
	inventory.Endpoints[1].ExpectedHostID = inventory.Endpoints[0].ExpectedHostID
	inventory.Endpoints[1].ExpectedStoreID = inventory.Endpoints[0].ExpectedStoreID
	inventory.Endpoints[1].ExpectedTmuxServerDomainID = inventory.Endpoints[0].ExpectedTmuxServerDomainID
	inventory.Endpoints[1].ExpectedTmuxServerOwnership = "external"
	rewriteDriverInventoryFixture(t, path, inventory)
	if _, err := LoadDriverEndpointInventory(path); err == nil || !strings.Contains(err.Error(), "duplicate Driver expected host/store/domain tuple") {
		t.Fatalf("duplicate authority tuple accepted: %v", err)
	}
}

func TestDriverEndpointInventoryRejectsCollapsedOrIncompleteIsolation(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*DriverEndpointInventory)
		want   string
	}{
		{"external non-default", func(i *DriverEndpointInventory) { i.Endpoints[0].ExpectedTmuxServerDomainID = "outside" }, "external endpoint must use the default"},
		{"managed default", func(i *DriverEndpointInventory) { i.Endpoints[1].ExpectedTmuxServerDomainID = "default" }, "managed_dedicated endpoint must use a non-default"},
		{"no external", func(i *DriverEndpointInventory) { i.Endpoints = i.Endpoints[1:] }, "requires external/default and managed_dedicated/non-default"},
		{"relative uds", func(i *DriverEndpointInventory) { i.Endpoints[1].UDSPath = "driver.sock" }, "paths must be absolute"},
		{"relative token", func(i *DriverEndpointInventory) { i.Endpoints[1].TokenFile = "driver.token" }, "paths must be absolute"},
		{"missing store", func(i *DriverEndpointInventory) { i.Endpoints[1].ExpectedStoreID = "" }, "expected_store_id must be a canonical UUID"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path, inventory := writeDriverInventoryFixture(t)
			tt.mutate(&inventory)
			rewriteDriverInventoryFixture(t, path, inventory)
			if _, err := LoadDriverEndpointInventory(path); err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("invalid inventory accepted or wrong error: %v", err)
			}
		})
	}
}

func TestDriverEndpointInventoryRejectsSymlinkedUDS(t *testing.T) {
	path, inventory := writeDriverInventoryFixture(t)
	target := filepath.Join(filepath.Dir(path), "socket-target")
	if err := os.WriteFile(target, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, inventory.Endpoints[0].UDSPath); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadDriverEndpointInventory(path); err == nil || !strings.Contains(err.Error(), "UDS path must not be a symlink") {
		t.Fatalf("symlinked UDS accepted: %v", err)
	}
}
