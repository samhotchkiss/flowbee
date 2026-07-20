package driver

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
)

const driverContractsFormat = "tmux-driver.contract-capabilities/v1"

var tmuxServerDomainPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,31}$`)

var requiredDriverContracts = map[string]string{
	"managed_tmux_server_domain":               "tmux-driver.tmux-server-domain/v1",
	"managed_tmux_server_isolation":            "tmux-driver.tmux-server-isolation/v1",
	"lifecycle_ensure":                         "tmux-driver.lifecycle-ensure/v2",
	"lifecycle_external_adopt":                 "tmux-driver.lifecycle-adopt/v1",
	"lifecycle_external_release":               "tmux-driver.lifecycle-release/v1",
	"control_origin_recipient_agent_run_fence": "tmux-driver.control-recipient-agent-run-fence/v1",
}

func defaultDriverContractCapabilities() DriverContractCapabilities {
	capability := func(name string) DriverContractCapability {
		return DriverContractCapability{Supported: true, ContractID: requiredDriverContracts[name]}
	}
	return DriverContractCapabilities{
		FormatVersion:                       driverContractsFormat,
		ManagedTmuxServerDomain:             capability("managed_tmux_server_domain"),
		ManagedTmuxServerIsolation:          capability("managed_tmux_server_isolation"),
		LifecycleEnsure:                     capability("lifecycle_ensure"),
		LifecycleExternalAdopt:              capability("lifecycle_external_adopt"),
		LifecycleExternalRelease:            capability("lifecycle_external_release"),
		ControlOriginRecipientAgentRunFence: capability("control_origin_recipient_agent_run_fence"),
	}
}

func (m *TmuxServerMetadata) UnmarshalJSON(data []byte) error {
	if err := requireExactWireKeys(data, []string{"domain_id", "ownership", "instance_id", "connection_visibility"}); err != nil {
		return err
	}
	type plain TmuxServerMetadata
	var value plain
	if err := decodeStrictWire(data, &value); err != nil {
		return err
	}
	*m = TmuxServerMetadata(value)
	return validateTmuxServerMetadata(*m)
}

func validateTmuxServerMetadata(m TmuxServerMetadata) error {
	if !tmuxServerDomainPattern.MatchString(m.DomainID) {
		return errors.New("driver metadata: invalid tmux server domain")
	}
	switch m.Ownership {
	case "managed_dedicated":
		if m.DomainID == "default" || m.ConnectionVisibility != "isolated_socket" {
			return errors.New("driver metadata: invalid managed tmux server mapping")
		}
	case "external":
		if m.DomainID != "default" || m.ConnectionVisibility != "default_or_external" {
			return errors.New("driver metadata: invalid external tmux server mapping")
		}
	default:
		return errors.New("driver metadata: invalid tmux server ownership")
	}
	if m.InstanceID != "" {
		if err := validateCanonicalUUID(m.InstanceID, "tmux_server.instance_id"); err != nil {
			return err
		}
	}
	return nil
}

func (c *DriverContractCapability) UnmarshalJSON(data []byte) error {
	if err := requireExactWireKeys(data, []string{"supported", "contract_id"}); err != nil {
		return err
	}
	type plain DriverContractCapability
	var value plain
	if err := decodeStrictWire(data, &value); err != nil {
		return err
	}
	*c = DriverContractCapability(value)
	return nil
}

func (c *DriverContractCapabilities) UnmarshalJSON(data []byte) error {
	keys := []string{"format_version", "managed_tmux_server_domain", "managed_tmux_server_isolation",
		"lifecycle_ensure", "lifecycle_external_adopt", "lifecycle_external_release",
		"control_origin_recipient_agent_run_fence"}
	if err := requireExactWireKeys(data, keys); err != nil {
		return err
	}
	type plain DriverContractCapabilities
	var value plain
	if err := decodeStrictWire(data, &value); err != nil {
		return err
	}
	*c = DriverContractCapabilities(value)
	return validateDriverContracts(*c)
}

func validateDriverContracts(c DriverContractCapabilities) error {
	if c.FormatVersion != driverContractsFormat {
		return fmt.Errorf("driver metadata: unsupported contracts format %q", c.FormatVersion)
	}
	actual := map[string]DriverContractCapability{
		"managed_tmux_server_domain":               c.ManagedTmuxServerDomain,
		"managed_tmux_server_isolation":            c.ManagedTmuxServerIsolation,
		"lifecycle_ensure":                         c.LifecycleEnsure,
		"lifecycle_external_adopt":                 c.LifecycleExternalAdopt,
		"lifecycle_external_release":               c.LifecycleExternalRelease,
		"control_origin_recipient_agent_run_fence": c.ControlOriginRecipientAgentRunFence,
	}
	for name, contractID := range requiredDriverContracts {
		capability := actual[name]
		if !capability.Supported || capability.ContractID != contractID {
			return fmt.Errorf("driver metadata: unsupported contract %s", name)
		}
	}
	return nil
}

func validateDriverMetadata(m DriverMetadata) error {
	if !m.LifecycleControl {
		return errors.New("driver metadata: lifecycle_control feature is not enabled")
	}
	if err := validateTmuxServerMetadata(m.TmuxServer); err != nil {
		return err
	}
	if err := validateDriverContracts(m.Contracts); err != nil {
		return err
	}
	return nil
}

// compile-time guard that the strict decoder remains JSON based.
var _ json.Unmarshaler = (*TmuxServerMetadata)(nil)
