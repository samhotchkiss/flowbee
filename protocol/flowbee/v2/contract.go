// Package actorprotocol loads and validates Flowbee's normative v2 actor contract.
// The embedded document is the single source for role cards, recovery runbooks,
// compatibility checks, dashboard help, and protocol diagnostics.
package actorprotocol

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

//go:embed actor-protocol.yaml schemas/*.json
var files embed.FS

type Contract struct {
	Protocol        Protocol       `yaml:"protocol"`
	CommonKernel    []string       `yaml:"common_kernel"`
	Roles           []Role         `yaml:"roles"`
	RecoveryCodes   []RecoveryCode `yaml:"recovery_codes"`
	RequiredSchemas []Schema       `yaml:"required_schemas"`
}

type Protocol struct {
	ID                   string `yaml:"id"`
	Major                int    `yaml:"major"`
	Minor                int    `yaml:"minor"`
	MaximumKernelWords   int    `yaml:"maximum_kernel_words"`
	MaximumRoleCardWords int    `yaml:"maximum_role_card_words"`
}

type Schema struct {
	ID     string `yaml:"id"`
	SHA256 string `yaml:"sha256"`
}

type Role struct {
	ID           string   `yaml:"id"`
	Authority    string   `yaml:"authority"`
	Outputs      []string `yaml:"outputs"`
	Capabilities []string `yaml:"capabilities"`
	Recovery     []string `yaml:"recovery"`
	EscalatesTo  []string `yaml:"escalates_to"`
	Forbidden    []string `yaml:"forbidden"`
}

type RecoveryCode struct {
	Code             string `yaml:"code"`
	AttentionKind    string `yaml:"attention_kind"`
	Invariant        string `yaml:"invariant"`
	Predicate        string `yaml:"predicate"`
	Owner            string `yaml:"owner"`
	Automatic        bool   `yaml:"automatic"`
	RepairAction     string `yaml:"repair_action"`
	Fence            string `yaml:"fence"`
	MaximumAttempts  int    `yaml:"maximum_attempts"`
	EscalationTarget string `yaml:"escalation_target"`
	Severity         string `yaml:"severity"`
	HelpPath         string `yaml:"help_path"`
	TestID           string `yaml:"test_id"`
}

func Load() (Contract, error) {
	b, err := files.ReadFile("actor-protocol.yaml")
	if err != nil {
		return Contract{}, err
	}
	var c Contract
	if err := yaml.Unmarshal(b, &c); err != nil {
		return Contract{}, fmt.Errorf("decode actor contract: %w", err)
	}
	if err := c.Validate(); err != nil {
		return Contract{}, err
	}
	return c, nil
}

func BundleHash() (string, error) {
	paths := []string{"actor-protocol.yaml"}
	entries, err := files.ReadDir("schemas")
	if err != nil {
		return "", err
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			paths = append(paths, "schemas/"+entry.Name())
		}
	}
	sort.Strings(paths)
	h := sha256.New()
	for _, path := range paths {
		b, err := files.ReadFile(path)
		if err != nil {
			return "", err
		}
		_, _ = h.Write([]byte(path))
		_, _ = h.Write([]byte{0})
		_, _ = h.Write(b)
		_, _ = h.Write([]byte{0})
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil)), nil
}

func (c Contract) Version() string {
	return fmt.Sprintf("%d.%d", c.Protocol.Major, c.Protocol.Minor)
}

func (c Contract) Validate() error {
	if c.Protocol.ID == "" || c.Protocol.Major < 1 || c.Protocol.Minor < 0 {
		return errors.New("actor contract has invalid protocol identity")
	}
	if len(c.CommonKernel) == 0 || words(c.CommonKernel) > c.Protocol.MaximumKernelWords {
		return fmt.Errorf("common kernel word budget exceeded or empty: %d/%d", words(c.CommonKernel), c.Protocol.MaximumKernelWords)
	}
	roleIDs := map[string]bool{}
	for _, role := range c.Roles {
		if role.ID == "" || role.Authority == "" || len(role.Outputs) == 0 || len(role.Forbidden) == 0 {
			return fmt.Errorf("incomplete role %q", role.ID)
		}
		if roleIDs[role.ID] {
			return fmt.Errorf("duplicate role %q", role.ID)
		}
		roleIDs[role.ID] = true
		if words(append(append([]string{role.Authority}, role.Outputs...), role.Forbidden...)) > c.Protocol.MaximumRoleCardWords {
			return fmt.Errorf("role %s exceeds role-card word budget", role.ID)
		}
	}
	for _, required := range []string{"human", "dashboard", "interactor", "orchestrator", "flowbee", "operational_agent", "builder", "reviewer", "driver", "capacity_collector"} {
		if !roleIDs[required] {
			return fmt.Errorf("missing required role %s", required)
		}
	}
	codes := map[string]bool{}
	for _, recovery := range c.RecoveryCodes {
		if recovery.Code == "" || recovery.Invariant == "" || recovery.Predicate == "" ||
			recovery.Owner == "" || recovery.RepairAction == "" || recovery.Fence == "" ||
			recovery.EscalationTarget == "" || recovery.Severity == "" || recovery.HelpPath == "" || recovery.TestID == "" {
			return fmt.Errorf("incomplete recovery code %q", recovery.Code)
		}
		if !roleIDs[recovery.Owner] || !roleIDs[recovery.EscalationTarget] {
			return fmt.Errorf("recovery code %s references unknown actor", recovery.Code)
		}
		if codes[recovery.Code] {
			return fmt.Errorf("duplicate recovery code %q", recovery.Code)
		}
		codes[recovery.Code] = true
		if recovery.Automatic && recovery.MaximumAttempts < 1 {
			return fmt.Errorf("automatic recovery %s has no bounded attempt budget", recovery.Code)
		}
	}
	for _, schema := range c.RequiredSchemas {
		if schema.ID == "" || !strings.HasPrefix(schema.SHA256, "sha256:") || len(schema.SHA256) != 71 {
			return fmt.Errorf("invalid required schema identity %q", schema.ID)
		}
		name := strings.TrimPrefix(schema.ID, "flowbee.")
		name = strings.TrimSuffix(name, "/v2") + ".schema.json"
		body, err := files.ReadFile("schemas/" + name)
		if err != nil {
			return fmt.Errorf("required schema %s: %w", schema.ID, err)
		}
		digest := sha256.Sum256(body)
		got := "sha256:" + hex.EncodeToString(digest[:])
		if got != schema.SHA256 {
			return fmt.Errorf("required schema %s hash mismatch: contract=%s actual=%s", schema.ID, schema.SHA256, got)
		}
	}
	return nil
}

func (c Contract) Role(id string) (Role, bool) {
	for _, role := range c.Roles {
		if role.ID == id {
			return role, true
		}
	}
	return Role{}, false
}

func (c Contract) Recovery(code string) (RecoveryCode, bool) {
	for _, recovery := range c.RecoveryCodes {
		if recovery.Code == code {
			return recovery, true
		}
	}
	return RecoveryCode{}, false
}

func (c Contract) RecoveryForAttention(kind string) (RecoveryCode, bool) {
	for _, recovery := range c.RecoveryCodes {
		if recovery.AttentionKind == kind {
			return recovery, true
		}
	}
	return RecoveryCode{}, false
}

func (c Contract) RoleIDs() []string {
	ids := make([]string, 0, len(c.Roles))
	for _, role := range c.Roles {
		ids = append(ids, role.ID)
	}
	sort.Strings(ids)
	return ids
}

func words(parts []string) int {
	n := 0
	for _, part := range parts {
		n += len(strings.Fields(part))
	}
	return n
}
