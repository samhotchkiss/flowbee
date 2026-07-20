package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
)

const (
	// DriverEndpointsFileEnv selects the only v2 multi-endpoint inventory source.
	// The inventory contains file paths, never bearer values.
	DriverEndpointsFileEnv = "FLOWBEE_DRIVER_ENDPOINTS_FILE"
	driverEndpointsFormat  = "flowbee.driver-endpoints/v1"
	driverEndpointsMaxSize = 1 << 20
)

var (
	driverInstanceRefPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$`)
	driverDomainPattern      = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,31}$`)
	canonicalUUIDPattern     = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[1-8][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
)

// DriverEndpointInventory is the explicit set of independently isolated Driver
// control domains Flowbee may use. It deliberately has no "default endpoint": a
// caller must resolve an endpoint by the complete host/store/domain tuple.
type DriverEndpointInventory struct {
	FormatVersion string           `json:"format_version"`
	Endpoints     []DriverEndpoint `json:"endpoints"`
}

// DriverEndpoint contains public routing coordinates plus a path to an
// owner-only bearer. Bearer material is never accepted inline.
type DriverEndpoint struct {
	InstanceRef                 string `json:"instance_ref"`
	UDSPath                     string `json:"uds_path"`
	TokenFile                   string `json:"token_file"`
	ExpectedHostID              string `json:"expected_host_id"`
	ExpectedStoreID             string `json:"expected_store_id"`
	ExpectedTmuxServerDomainID  string `json:"expected_tmux_server_domain_id"`
	ExpectedTmuxServerOwnership string `json:"expected_tmux_server_ownership"`
}

// LoadDriverEndpointInventoryFromEnv loads the explicit endpoint inventory when
// configured. configured=false means no v2 inventory was selected; it does not
// synthesize one from the legacy single-endpoint environment variables.
func LoadDriverEndpointInventoryFromEnv() (inventory DriverEndpointInventory, configured bool, err error) {
	path := strings.TrimSpace(os.Getenv(DriverEndpointsFileEnv))
	if path == "" {
		return DriverEndpointInventory{}, false, nil
	}
	inventory, err = LoadDriverEndpointInventory(path)
	return inventory, true, err
}

// LoadDriverEndpointInventory reads and validates an owner-only inventory and
// every referenced bearer file without following symlinks.
func LoadDriverEndpointInventory(path string) (DriverEndpointInventory, error) {
	if !filepath.IsAbs(path) {
		return DriverEndpointInventory{}, errors.New("Driver endpoints inventory path must be absolute")
	}
	data, err := readOwnerOnlyRegularFile(path, driverEndpointsMaxSize, "Driver endpoints inventory")
	if err != nil {
		return DriverEndpointInventory{}, err
	}
	var inventory DriverEndpointInventory
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&inventory); err != nil {
		return DriverEndpointInventory{}, fmt.Errorf("decode Driver endpoints inventory: %w", err)
	}
	if err := requireJSONEOF(decoder); err != nil {
		return DriverEndpointInventory{}, err
	}
	if err := inventory.Validate(); err != nil {
		return DriverEndpointInventory{}, err
	}
	return inventory, nil
}

// Validate enforces the isolation topology and verifies token-file posture. It
// intentionally does not probe Driver; runtime readiness separately pins these
// expected identities to fresh authenticated metadata.
func (inventory DriverEndpointInventory) Validate() error {
	if inventory.FormatVersion != driverEndpointsFormat {
		return fmt.Errorf("Driver endpoints format_version=%q, want %q", inventory.FormatVersion, driverEndpointsFormat)
	}
	if len(inventory.Endpoints) < 2 {
		return errors.New("Driver endpoints inventory requires external/default and managed_dedicated/non-default endpoints")
	}
	refs := make(map[string]struct{}, len(inventory.Endpoints))
	tuples := make(map[string]struct{}, len(inventory.Endpoints))
	externalDefaults := 0
	managedDedicated := 0
	for i, endpoint := range inventory.Endpoints {
		field := fmt.Sprintf("Driver endpoints[%d]", i)
		if !driverInstanceRefPattern.MatchString(endpoint.InstanceRef) {
			return fmt.Errorf("%s instance_ref is invalid", field)
		}
		if _, exists := refs[endpoint.InstanceRef]; exists {
			return fmt.Errorf("duplicate Driver instance_ref %q", endpoint.InstanceRef)
		}
		refs[endpoint.InstanceRef] = struct{}{}
		if !canonicalUUIDPattern.MatchString(endpoint.ExpectedHostID) {
			return fmt.Errorf("%s expected_host_id must be a canonical UUID", field)
		}
		if !canonicalUUIDPattern.MatchString(endpoint.ExpectedStoreID) {
			return fmt.Errorf("%s expected_store_id must be a canonical UUID", field)
		}
		if !driverDomainPattern.MatchString(endpoint.ExpectedTmuxServerDomainID) {
			return fmt.Errorf("%s expected_tmux_server_domain_id is invalid", field)
		}
		switch endpoint.ExpectedTmuxServerOwnership {
		case "external":
			if endpoint.ExpectedTmuxServerDomainID != "default" {
				return fmt.Errorf("%s external endpoint must use the default tmux server domain", field)
			}
			externalDefaults++
		case "managed_dedicated":
			if endpoint.ExpectedTmuxServerDomainID == "default" {
				return fmt.Errorf("%s managed_dedicated endpoint must use a non-default tmux server domain", field)
			}
			managedDedicated++
		default:
			return fmt.Errorf("%s expected_tmux_server_ownership is invalid", field)
		}
		if !filepath.IsAbs(endpoint.UDSPath) || !filepath.IsAbs(endpoint.TokenFile) {
			return fmt.Errorf("%s UDS and token_file paths must be absolute", field)
		}
		if err := validateSocketPath(endpoint.UDSPath); err != nil {
			return fmt.Errorf("%s: %w", field, err)
		}
		token, err := readOwnerOnlyRegularFile(endpoint.TokenFile, 64<<10, field+" token_file")
		if err != nil {
			return err
		}
		if strings.TrimSpace(string(token)) == "" {
			return fmt.Errorf("%s token_file is empty", field)
		}
		tuple := endpoint.ExpectedHostID + "\x00" + endpoint.ExpectedStoreID + "\x00" + endpoint.ExpectedTmuxServerDomainID
		if _, exists := tuples[tuple]; exists {
			return fmt.Errorf("duplicate Driver expected host/store/domain tuple for %s", field)
		}
		tuples[tuple] = struct{}{}
	}
	if externalDefaults == 0 {
		return errors.New("Driver endpoints inventory requires at least one external/default endpoint")
	}
	if managedDedicated == 0 {
		return errors.New("Driver endpoints inventory requires at least one managed_dedicated/non-default endpoint")
	}
	return nil
}

func requireJSONEOF(decoder *json.Decoder) error {
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("decode Driver endpoints inventory: multiple JSON values")
		}
		return fmt.Errorf("decode Driver endpoints inventory trailing data: %w", err)
	}
	return nil
}

func readOwnerOnlyRegularFile(path string, limit int64, label string) ([]byte, error) {
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", label, err)
	}
	file := os.NewFile(uintptr(fd), path)
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, fmt.Errorf("stat %s: %w", label, err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s must be a regular file", label)
	}
	if info.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("%s permissions %04o are not owner-only", label, info.Mode().Perm())
	}
	if stat, ok := info.Sys().(*syscall.Stat_t); ok {
		if stat.Uid != uint32(os.Geteuid()) {
			return nil, fmt.Errorf("%s is not owned by the current user", label)
		}
		if stat.Nlink != 1 {
			return nil, fmt.Errorf("%s must not have hard links", label)
		}
	}
	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", label, err)
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("%s exceeds %d bytes", label, limit)
	}
	return data, nil
}

func validateSocketPath(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil // daemon lifecycle may create it after configuration is loaded
	}
	if err != nil {
		return fmt.Errorf("inspect UDS path: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return errors.New("UDS path must not be a symlink")
	}
	if info.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("UDS permissions %04o are not owner-only", info.Mode().Perm())
	}
	if stat, ok := info.Sys().(*syscall.Stat_t); ok && stat.Uid != uint32(os.Geteuid()) {
		return errors.New("UDS path is not owned by the current user")
	}
	if info.Mode()&os.ModeSocket == 0 {
		return errors.New("UDS path exists but is not a Unix socket")
	}
	return nil
}
