// Package flow loads the declarative flow/role/lens YAML (DESIGN §5.3) and runs
// the §5.6 provider-neutrality lint. A flow is a DAG of stages; each stage binds a
// role; each role declares required capability tags and a lens. The lint is the
// enforcement arm of R1 (provider-agnostic): a provider literal may appear ONLY in
// a `model_family:*` capability tag or a `lens.prompt_ref` path — anywhere else
// (a role/stage key, a `when:` predicate, an independence term, a requires tag
// other than model_family) FAILS the build. tools/providerlint shells this.
//
// This is NOT a deterministic-core package; it loads config off disk. It performs
// no clock/rand/network work.
package flow

import (
	"fmt"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// Lens is a role's persona/focus config — the only provider-flavored surface, and
// it is data (§5.6 item 4). prompt_ref is an allowlisted position for a literal.
type Lens struct {
	Persona   string `yaml:"persona"`
	Focus     string `yaml:"focus"`
	PromptRef string `yaml:"prompt_ref"`
}

// Role is a capability+lens slot (§5.2).
type Role struct {
	Requires []string `yaml:"requires"`
	Lens     Lens     `yaml:"lens"`
	Gate     bool     `yaml:"gate"`
	Grants   []string `yaml:"grants"`
	Context  string   `yaml:"context"`
	Emits    string   `yaml:"emits"`
}

// Stage binds a role within a flow's DAG.
type Stage struct {
	Role       string   `yaml:"role"`
	Needs      []string `yaml:"needs"`
	Gate       bool     `yaml:"gate"`
	When       string   `yaml:"when"`
	Context    string   `yaml:"context"`
	MaxBounces int      `yaml:"max_bounces"`
}

// Signoff describes a flow's tamper-evident sign-off rule (§5.5).
type Signoff struct {
	Kind   string   `yaml:"kind"`
	Source string   `yaml:"source"`
	Binds  []string `yaml:"binds"`
}

// Flow is a declarative DAG of stages.
type Flow struct {
	Entry        string           `yaml:"entry"`
	Stages       map[string]Stage `yaml:"stages"`
	Independence []string         `yaml:"independence"`
	Signoff      Signoff          `yaml:"signoff"`
}

// Config is the parsed flows/roles document.
type Config struct {
	Roles map[string]Role `yaml:"roles"`
	Flows map[string]Flow `yaml:"flows"`
}

// Unmarshal decodes the flow/role YAML into c WITHOUT running the neutrality
// lint, so a caller (providerlint) can collect every violation rather than failing
// on the first. Use Parse for the fail-fast config-load path.
func Unmarshal(data []byte, c *Config) error {
	return yaml.Unmarshal(data, c)
}

// Parse decodes the flow/role YAML and runs the §5.6 neutrality lint. A lint
// violation (a provider literal in a control position) is returned as an error so
// loading config FAILS the build, exactly as the design requires.
func Parse(data []byte) (*Config, error) {
	var c Config
	if err := yaml.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("parse flows yaml: %w", err)
	}
	if errs := c.LintNeutrality(); len(errs) > 0 {
		return nil, fmt.Errorf("provider-neutrality lint failed (§5.6):\n  - %s", strings.Join(errs, "\n  - "))
	}
	return &c, nil
}

// ProviderLiterals is the (extensible) set of known provider names the §5.6 lint
// rejects outside allowlisted positions. Matched case-insensitively as whole words.
var ProviderLiterals = []string{
	"codex", "opus", "sonnet", "haiku", "claude", "anthropic",
	"gpt", "openai", "gemini", "llama", "mistral", "cohere", "ollama", "aider", "grok",
}

// LintNeutrality returns one message per §5.6 violation. A provider literal is
// allowed ONLY (a) as the value of a `model_family:` capability tag, or (b) inside
// a `lens.prompt_ref` path. It is forbidden everywhere else: role names, stage
// names, `requires` tags other than model_family, `when:` predicates, independence
// terms, grants, context, persona/focus text, and the entry field.
func (c *Config) LintNeutrality() []string {
	var out []string
	add := func(where, text string) {
		if lit := findProviderLiteral(text); lit != "" {
			out = append(out, fmt.Sprintf("provider literal %q in control position: %s = %q", lit, where, text))
		}
	}

	for name, role := range c.Roles {
		add("role name", name)
		for _, req := range role.Requires {
			// model_family:* is the ONE allowlisted capability position; skip it.
			if strings.HasPrefix(req, "model_family:") {
				continue
			}
			add(fmt.Sprintf("roles.%s.requires", name), req)
		}
		for _, g := range role.Grants {
			add(fmt.Sprintf("roles.%s.grants", name), g)
		}
		add(fmt.Sprintf("roles.%s.context", name), role.Context)
		add(fmt.Sprintf("roles.%s.emits", name), role.Emits)
		add(fmt.Sprintf("roles.%s.lens.persona", name), role.Lens.Persona)
		add(fmt.Sprintf("roles.%s.lens.focus", name), role.Lens.Focus)
		// lens.prompt_ref is the OTHER allowlisted position; NOT linted.
	}

	for fname, fl := range c.Flows {
		add("flow name", fname)
		add(fmt.Sprintf("flows.%s.entry", fname), fl.Entry)
		for sname, st := range fl.Stages {
			add(fmt.Sprintf("flows.%s stage name", fname), sname)
			add(fmt.Sprintf("flows.%s.stages.%s.role", fname, sname), st.Role)
			add(fmt.Sprintf("flows.%s.stages.%s.when", fname, sname), st.When)
			add(fmt.Sprintf("flows.%s.stages.%s.context", fname, sname), st.Context)
		}
		for _, term := range fl.Independence {
			add(fmt.Sprintf("flows.%s.independence", fname), term)
		}
		add(fmt.Sprintf("flows.%s.signoff.source", fname), fl.Signoff.Source)
	}

	sort.Strings(out)
	return out
}

// findProviderLiteral returns the first provider literal appearing as a
// whole-word token in s (case-insensitive), or "" if none.
func findProviderLiteral(s string) string {
	if s == "" {
		return ""
	}
	lower := strings.ToLower(s)
	for _, p := range ProviderLiterals {
		if containsWord(lower, p) {
			return p
		}
	}
	return ""
}

// containsWord reports whether word appears in s delimited by non-alphanumeric
// boundaries (so "completeness" does not match "complete", and "opus" matches in
// "model:opus" or "opus_reviewer" but not in "octopus").
func containsWord(s, word string) bool {
	idx := 0
	for {
		i := strings.Index(s[idx:], word)
		if i < 0 {
			return false
		}
		start := idx + i
		end := start + len(word)
		leftOK := start == 0 || !isAlphaNum(s[start-1])
		rightOK := end == len(s) || !isAlphaNum(s[end])
		if leftOK && rightOK {
			return true
		}
		idx = start + 1
		if idx >= len(s) {
			return false
		}
	}
}

func isAlphaNum(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9')
}
