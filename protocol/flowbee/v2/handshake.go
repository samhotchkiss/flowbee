package actorprotocol

import (
	"fmt"
	"sort"
)

// ActorHello is the fail-closed compatibility envelope presented before an actor
// can receive work. Stable incarnation fields are mandatory for session actors;
// raw pane names, PIDs, CWDs, and transcript text are deliberately absent.
type ActorHello struct {
	ActorID          string            `json:"actor_id"`
	Role             string            `json:"role"`
	ProjectID        string            `json:"project_id"`
	HostID           string            `json:"host_id,omitempty"`
	StoreID          string            `json:"store_id,omitempty"`
	SessionID        string            `json:"session_id,omitempty"`
	PaneInstanceID   string            `json:"pane_instance_id,omitempty"`
	AgentRunID       string            `json:"agent_run_id,omitempty"`
	ProtocolMajor    int               `json:"protocol_major"`
	ProtocolMinor    int               `json:"protocol_minor"`
	Schemas          map[string]string `json:"schemas"`
	RoleBundleSHA256 string            `json:"role_bundle_sha256"`
	Capabilities     []string          `json:"capabilities"`
}

type Negotiation struct {
	Compatible       bool     `json:"compatible"`
	ProtocolVersion  string   `json:"protocol_version,omitempty"`
	GrantedProjectID string   `json:"granted_project_id,omitempty"`
	GrantedRole      string   `json:"granted_role,omitempty"`
	Capabilities     []string `json:"capabilities,omitempty"`
	ServerBundleHash string   `json:"server_bundle_hash"`
	Reason           string   `json:"reason,omitempty"`
}

// Negotiate refuses any major, role, scope, bundle, or required-schema ambiguity.
// Optional capabilities are intersected with the role's closed capability set.
func (c Contract) Negotiate(hello ActorHello, serverBundleHash string, requiredSchemaIDs []string) Negotiation {
	reject := func(reason string) Negotiation {
		return Negotiation{Compatible: false, ServerBundleHash: serverBundleHash, Reason: reason}
	}
	if hello.ActorID == "" || hello.ProjectID == "" || hello.AgentRunID == "" {
		return reject("actor identity, project, and incarnation are required")
	}
	role, ok := c.Role(hello.Role)
	if !ok {
		return reject("unknown role")
	}
	if hello.ProtocolMajor != c.Protocol.Major {
		return reject("protocol major mismatch")
	}
	if hello.ProtocolMinor > c.Protocol.Minor {
		return reject("actor requires unsupported protocol minor")
	}
	if hello.RoleBundleSHA256 != serverBundleHash {
		return reject("role bundle mismatch")
	}
	knownSchemas := make(map[string]string, len(c.RequiredSchemas))
	for _, schema := range c.RequiredSchemas {
		knownSchemas[schema.ID] = schema.SHA256
	}
	for _, id := range requiredSchemaIDs {
		want, exists := knownSchemas[id]
		if !exists {
			return reject("server requires unknown schema " + id)
		}
		if got := hello.Schemas[id]; got != want {
			return reject(fmt.Sprintf("schema mismatch for %s", id))
		}
	}
	allowed := make(map[string]bool, len(role.Capabilities))
	for _, capability := range role.Capabilities {
		allowed[capability] = true
	}
	granted := make([]string, 0, len(hello.Capabilities))
	seen := map[string]bool{}
	for _, capability := range hello.Capabilities {
		if allowed[capability] && !seen[capability] {
			granted = append(granted, capability)
			seen[capability] = true
		}
	}
	sort.Strings(granted)
	return Negotiation{
		Compatible: true, ProtocolVersion: c.Version(), GrantedProjectID: hello.ProjectID,
		GrantedRole: hello.Role, Capabilities: granted, ServerBundleHash: serverBundleHash,
	}
}
