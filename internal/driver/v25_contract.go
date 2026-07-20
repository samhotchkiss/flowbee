package driver

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"time"
)

const driverContractsFormat = "tmux-driver.contract-capabilities/v1"

var tmuxServerDomainPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,31}$`)

var requiredDriverContracts = map[string]string{
	"managed_tmux_server_domain":               "tmux-driver.tmux-server-domain/v1",
	"managed_tmux_server_isolation":            "tmux-driver.tmux-server-isolation/v1",
	"lifecycle_ensure":                         "tmux-driver.lifecycle-ensure/v3",
	"lifecycle_profile_inventory":              "tmux-driver.lifecycle-profile-inventory/v1",
	"lifecycle_ensure_bootstrap_artifact":      "tmux-driver.lifecycle-ensure-bootstrap-artifact/v1",
	"lifecycle_human_visible_session":          "tmux-driver.lifecycle-human-visible-session/v1",
	"lifecycle_managed_display_name":           "tmux-driver.lifecycle-managed-display-name/v1",
	"lifecycle_flowbee_credential_install":     "tmux-driver.lifecycle-flowbee-credential-install/v1",
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
		LifecycleProfileInventory:           capability("lifecycle_profile_inventory"),
		LifecycleEnsureBootstrapArtifact:    capability("lifecycle_ensure_bootstrap_artifact"),
		LifecycleHumanVisibleSession:        capability("lifecycle_human_visible_session"),
		LifecycleManagedDisplayName:         capability("lifecycle_managed_display_name"),
		LifecycleFlowbeeCredentialInstall:   capability("lifecycle_flowbee_credential_install"),
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
		"lifecycle_ensure", "lifecycle_profile_inventory", "lifecycle_ensure_bootstrap_artifact", "lifecycle_human_visible_session",
		"lifecycle_managed_display_name", "lifecycle_flowbee_credential_install",
		"lifecycle_external_adopt", "lifecycle_external_release",
		"control_origin_recipient_agent_run_fence"}
	// Contract capability maps are extensible.  Flowbee must reject a missing or
	// malformed capability it depends on, but a Driver adding an unrelated
	// capability cannot make an otherwise-safe endpoint unusable.  Preserve
	// strict decoding for every required capability and intentionally discard
	// unrecognized extension keys.
	var object map[string]json.RawMessage
	if err := json.Unmarshal(data, &object); err != nil || object == nil {
		if err != nil {
			return err
		}
		return errors.New("driver wire object must be an object")
	}
	known := make(map[string]json.RawMessage, len(keys))
	for _, key := range keys {
		raw, ok := object[key]
		if !ok {
			return fmt.Errorf("driver wire object: missing %s", key)
		}
		known[key] = raw
	}
	knownJSON, err := json.Marshal(known)
	if err != nil {
		return err
	}
	type plain DriverContractCapabilities
	var value plain
	if err := decodeStrictWire(knownJSON, &value); err != nil {
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
		"lifecycle_profile_inventory":              c.LifecycleProfileInventory,
		"lifecycle_ensure_bootstrap_artifact":      c.LifecycleEnsureBootstrapArtifact,
		"lifecycle_human_visible_session":          c.LifecycleHumanVisibleSession,
		"lifecycle_managed_display_name":           c.LifecycleManagedDisplayName,
		"lifecycle_flowbee_credential_install":     c.LifecycleFlowbeeCredentialInstall,
		"lifecycle_external_adopt":                 c.LifecycleExternalAdopt,
		"lifecycle_external_release":               c.LifecycleExternalRelease,
		"control_origin_recipient_agent_run_fence": c.ControlOriginRecipientAgentRunFence,
	}
	for name, contractID := range requiredDriverContracts {
		capability := actual[name]
		if capability.ContractID != contractID {
			return fmt.Errorf("driver metadata: unsupported contract %s", name)
		}
	}
	// Control-origin is the only capability every configured endpoint must
	// provide: Flowbee can route a human escalation to an adopted external
	// Interactor as well as to managed workers.  Lifecycle presentation and
	// external-adoption capabilities are deliberately endpoint-scoped and are
	// checked immediately before the operation that needs them.
	if !c.ControlOriginRecipientAgentRunFence.Supported {
		return errors.New("driver metadata: unsupported contract control_origin_recipient_agent_run_fence")
	}
	return nil
}

func (p *LifecycleProfile) UnmarshalJSON(data []byte) error {
	keys := []string{"profile_id", "provider", "initial_prompt_adapter", "target_credential_adapter",
		"ensure_supported", "bootstrap_artifact_supported", "flowbee_credential_install_supported",
		"human_visible_session_supported", "managed_display_name_supported"}
	if err := requireExactWireKeys(data, keys); err != nil {
		return err
	}
	type plain LifecycleProfile
	var value plain
	if err := decodeStrictWire(data, &value); err != nil {
		return err
	}
	*p = LifecycleProfile(value)
	return validateLifecycleProfile(*p)
}

func validateLifecycleProfile(p LifecycleProfile) error {
	if !regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,127}$`).MatchString(p.ProfileID) ||
		(p.Provider != "claude" && p.Provider != "codex" && p.Provider != "grok") ||
		(p.InitialPromptAdapter != "disabled" && p.InitialPromptAdapter != "argv_element") ||
		(p.TargetCredentialAdapter != "disabled" && p.TargetCredentialAdapter != "file_environment") {
		return errors.New("driver lifecycle profile inventory contains invalid closed enum or id")
	}
	if !p.EnsureSupported && (p.BootstrapArtifactSupported || p.FlowbeeCredentialInstallSupported ||
		p.HumanVisibleSessionSupported || p.ManagedDisplayNameSupported) {
		return errors.New("disabled lifecycle profile advertises feature suitability")
	}
	if p.BootstrapArtifactSupported != (p.EnsureSupported && p.InitialPromptAdapter == "argv_element") ||
		p.FlowbeeCredentialInstallSupported != (p.EnsureSupported && p.TargetCredentialAdapter == "file_environment") ||
		p.HumanVisibleSessionSupported && p.ManagedDisplayNameSupported {
		return errors.New("driver lifecycle profile adapter suitability is inconsistent")
	}
	return nil
}

func (i *LifecycleProfileInventory) UnmarshalJSON(data []byte) error {
	keys := []string{"api_version", "server_time", "format_version", "lifecycle_enabled",
		"tmux_server_domain_id", "profiles"}
	// The inventory is an extensible Driver capability document. Require and
	// strictly decode every field Flowbee relies on, but do not make a new
	// unrelated profile category (for example utility_profiles) an outage for
	// managed agent lifecycle. This mirrors contract-capability decoding above.
	var object map[string]json.RawMessage
	if err := json.Unmarshal(data, &object); err != nil || object == nil {
		if err != nil {
			return err
		}
		return errors.New("driver lifecycle profile inventory must be an object")
	}
	known := make(map[string]json.RawMessage, len(keys))
	for _, key := range keys {
		raw, ok := object[key]
		if !ok {
			return fmt.Errorf("driver lifecycle profile inventory missing %s", key)
		}
		known[key] = raw
	}
	for key := range object {
		if key == "utility_profiles" {
			continue
		}
		if _, required := known[key]; !required {
			return fmt.Errorf("driver lifecycle profile inventory: missing or unknown field %s", key)
		}
	}
	knownJSON, err := json.Marshal(known)
	if err != nil {
		return err
	}
	type plain LifecycleProfileInventory
	var value plain
	if err := decodeStrictWire(knownJSON, &value); err != nil {
		return err
	}
	*i = LifecycleProfileInventory(value)
	return validateLifecycleProfileInventory(*i)
}

func validateLifecycleProfileInventory(i LifecycleProfileInventory) error {
	if i.APIVersion != "v2" || i.FormatVersion != "tmux-driver.lifecycle-profile-inventory/v1" ||
		!tmuxServerDomainPattern.MatchString(i.TmuxServerDomainID) {
		return errors.New("driver lifecycle profile inventory has invalid authority identity")
	}
	if _, err := time.Parse(time.RFC3339Nano, i.ServerTime); err != nil {
		return errors.New("driver lifecycle profile inventory has invalid server_time")
	}
	prior := ""
	for _, profile := range i.Profiles {
		if err := validateLifecycleProfile(profile); err != nil {
			return err
		}
		if prior != "" && profile.ProfileID <= prior {
			return errors.New("driver lifecycle profiles are not unique and sorted")
		}
		prior = profile.ProfileID
		if !i.LifecycleEnabled && profile.EnsureSupported {
			return errors.New("disabled lifecycle inventory has usable profile")
		}
		if i.TmuxServerDomainID == "default" && profile.ManagedDisplayNameSupported ||
			i.TmuxServerDomainID != "default" && profile.HumanVisibleSessionSupported {
			return errors.New("driver lifecycle profile presentation policy crosses server domain")
		}
	}
	return nil
}

func validateDriverMetadata(m DriverMetadata) error {
	if !m.LifecycleControl {
		return errors.New("driver metadata: lifecycle_control feature is not enabled")
	}
	if m.LifecycleProfileInventoryPath != "/v2/lifecycle/profiles" {
		return errors.New("driver metadata: lifecycle_profile_inventory path is not exact")
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
