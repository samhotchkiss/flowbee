package flow

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// Identity is a fully-resolved per-actor identity (F5): who a worker is told to
// be for a job. It is seeded from the `hire` corpus (tools/seedidentities) into
// flows/identities/<id>.yaml and referenced by id from the configurable flow.
//
// `Model` / `ModelTier` / `ModelFamily` are DATA (hire model recommendations),
// not §5.6 control-position tokens; identity files are loaded by LoadIdentities,
// NOT through the neutrality lint that guards flows/flows.yaml. ModelFamily is the
// anti-affinity axis input (identity + model_family + fresh context, NOT machine).
type Identity struct {
	ID          string `yaml:"id"`
	Role        string `yaml:"role"`
	Stage       string `yaml:"stage"`
	ReviewLens  string `yaml:"review_lens"`
	SourceSlug  string `yaml:"source_slug"`
	DisplayName string `yaml:"display_name"`
	RoleName    string `yaml:"role_name"`
	Tagline     string `yaml:"tagline"`
	Model       string `yaml:"model"`
	ModelTier   string `yaml:"model_tier"`
	ModelFamily string `yaml:"model_family"`
	// Lens is the flows-dir-relative path to the lens markdown (AGENT.md prose).
	Lens string `yaml:"lens"`
}

// Registry is the set of loaded identities keyed by id, plus the flows dir they
// were loaded from (so a lens path can be read back).
type Registry struct {
	byID     map[string]Identity
	flowsDir string
}

// LoadIdentities reads every flows/identities/*.yaml under flowsDir into a
// Registry. It does NOT run the §5.6 lint (identity files legitimately carry
// model literals as data). A duplicate id is an error.
func LoadIdentities(flowsDir string) (*Registry, error) {
	dir := filepath.Join(flowsDir, "identities")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read identities dir %s: %w", dir, err)
	}
	reg := &Registry{byID: map[string]Identity{}, flowsDir: flowsDir}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".yaml") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			return nil, fmt.Errorf("read identity %s: %w", e.Name(), err)
		}
		var id Identity
		if err := yaml.Unmarshal(b, &id); err != nil {
			return nil, fmt.Errorf("parse identity %s: %w", e.Name(), err)
		}
		if id.ID == "" {
			return nil, fmt.Errorf("identity %s has no id", e.Name())
		}
		if _, dup := reg.byID[id.ID]; dup {
			return nil, fmt.Errorf("duplicate identity id %q (in %s)", id.ID, e.Name())
		}
		reg.byID[id.ID] = id
	}
	return reg, nil
}

// Get returns the identity with id, or false.
func (r *Registry) Get(id string) (Identity, bool) {
	i, ok := r.byID[id]
	return i, ok
}

// IDs returns all loaded identity ids, sorted (determinism).
func (r *Registry) IDs() []string {
	out := make([]string, 0, len(r.byID))
	for id := range r.byID {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// LensMarkdown reads the lens markdown body for an identity (the AGENT.md prose),
// resolved relative to the flows dir. Empty Lens path yields "".
func (r *Registry) LensMarkdown(id Identity) (string, error) {
	if id.Lens == "" {
		return "", nil
	}
	b, err := os.ReadFile(filepath.Join(r.flowsDir, id.Lens))
	if err != nil {
		return "", fmt.Errorf("read lens %s: %w", id.Lens, err)
	}
	return string(b), nil
}

// RoleDefaults derives the bottom-layer role→identity map: a role with EXACTLY
// one identity in the registry gets that identity as its default; a role with
// several (e.g. code_reviewer's fan-out) is left unset, because no single default
// is correct — those stages must be staffed at the flow layer (reviewer slots).
// This is the role-default layer of the F5 precedence (role < flow < epic < job).
func (r *Registry) RoleDefaults() map[string]string {
	byRole := map[string][]string{}
	for _, id := range r.byID {
		if id.Role != "" {
			byRole[id.Role] = append(byRole[id.Role], id.ID)
		}
	}
	out := map[string]string{}
	for role, ids := range byRole {
		if len(ids) == 1 {
			out[role] = ids[0]
		}
	}
	return out
}

// Overrides carries the per-step identity-override inputs at the three layers
// above the role default. Precedence (lowest→highest): role default < Flow <
// Epic < Job. An empty string at a layer means "no override at this layer".
type Overrides struct {
	Flow string // flow-stage identity (from the flow's stage.identity / reviewer)
	Epic string // epic-level override (epic steering object)
	Job  string // per-job override (the most specific)
}

// ResolveIdentity applies the F5 precedence — role default < flow < epic < job —
// and returns the winning identity from the registry. roleDefault is the id a
// role maps to when nothing overrides it (may be ""). The first non-empty layer
// scanned from MOST specific (Job) to LEAST (roleDefault) wins. An override id
// that is not in the registry is an error (a typo must fail loudly, not silently
// fall through to a less-specific layer).
func (r *Registry) ResolveIdentity(roleDefault string, ov Overrides) (Identity, error) {
	type layer struct {
		name string
		id   string
	}
	// Most-specific first.
	for _, l := range []layer{
		{"job", ov.Job},
		{"epic", ov.Epic},
		{"flow", ov.Flow},
		{"role", roleDefault},
	} {
		if l.id == "" {
			continue
		}
		id, ok := r.byID[l.id]
		if !ok {
			return Identity{}, fmt.Errorf("identity override at %s layer references unknown id %q", l.name, l.id)
		}
		return id, nil
	}
	return Identity{}, fmt.Errorf("no identity resolved (no override at any layer and no role default)")
}
