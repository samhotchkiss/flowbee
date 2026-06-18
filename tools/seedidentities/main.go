// Dev utility — run once by a developer to seed worker identities into the Flowbee registry.
// Not part of the engine and not invoked by any automated flow.

// Command seedidentities materializes Flowbee's default per-actor identity files
// (flows/identities/*.yaml + flows/lenses/*.md) from the `hire` corpus
// (~/dev/russell/public/hire). It is a one-shot GENERATOR run by a developer, not
// part of the engine: it reads assets/data/profiles.json + download-files.json,
// selects the stage→role mapping the F5 backlog item fixes, and emits one identity
// YAML + one lens markdown per Flowbee stage.
//
//	issue-review                 = engineering-manager      (amends the issue)
//	build                        = engineering-generalist   (writes the patch)
//	build-review / correctness   = senior-code-reviewer
//	build-review / tests         = qa-engineer
//	build-review / security      = security-auditor
//
// For each, AGENT.md becomes the lens (flows/lenses/<id>.md) and
// roster_entry.model_recommendations.best_overall becomes the recommended model.
// Models are DATA here (the model_family:* / lens path neutrality allowance, §5.6),
// exactly as hire stores them — these files are NOT run through the §5.6 control-
// position lint (that lint guards flows/flows.yaml's control surface only).
//
// Usage:
//
//	go run ./tools/seedidentities [-hire ~/dev/russell/public/hire] [-out flows]
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

// stageMapping is the F5 stage→hire-slug assignment. lens/model are filled from
// the hire package; the Flowbee stage name + role are fixed by the flow design.
type stageMapping struct {
	IdentityID string // the flows/identities/<id>.yaml stem and identity id
	Role       string // the Flowbee role this identity is the default for
	Stage      string // the flow stage it defaults into (doc/anti-affinity aid)
	Lens       string // build-review lens axis (correctness|tests|security|"")
	Slug       string // the hire profile slug to source from
}

var mappings = []stageMapping{
	{IdentityID: "issue-reviewer", Role: "issue_reviewer", Stage: "issue_review", Lens: "", Slug: "engineering-manager"},
	{IdentityID: "builder", Role: "eng_worker", Stage: "build", Lens: "", Slug: "engineering-generalist"},
	{IdentityID: "reviewer-correctness", Role: "code_reviewer", Stage: "build_review", Lens: "correctness", Slug: "senior-code-reviewer"},
	{IdentityID: "reviewer-tests", Role: "code_reviewer", Stage: "build_review", Lens: "tests", Slug: "qa-engineer"},
	{IdentityID: "reviewer-security", Role: "code_reviewer", Stage: "build_review", Lens: "security", Slug: "security-auditor"},
}

type profile struct {
	Slug      string `json:"slug"`
	Name      string `json:"name"`
	Role      string `json:"role"`
	Tagline   string `json:"tagline"`
	ModelTier string `json:"modelTier"`
	BestModel string `json:"bestModel"`
}

type profilesDoc struct {
	Profiles []profile `json:"profiles"`
}

type pkgFile struct {
	Name    string `json:"name"`
	Content string `json:"content"`
}

type pkg struct {
	Slug  string    `json:"slug"`
	Name  string    `json:"name"`
	Role  string    `json:"role"`
	Files []pkgFile `json:"files"`
}

type downloadsDoc struct {
	Packages map[string]pkg `json:"packages"`
}

type rosterEntry struct {
	RoleID             string `json:"role_id"`
	DisplayName        string `json:"display_name"`
	RoleName           string `json:"role_name"`
	ModelRecommendations struct {
		Tier        string `json:"tier"`
		BestOverall string `json:"best_overall"`
	} `json:"model_recommendations"`
}

func main() {
	hireDir := flag.String("hire", os.ExpandEnv("$HOME/dev/russell/public/hire"), "path to the hire repo")
	outDir := flag.String("out", "flows", "output flows directory")
	flag.Parse()

	dataDir := filepath.Join(*hireDir, "assets", "data")

	var profs profilesDoc
	if err := readJSON(filepath.Join(dataDir, "profiles.json"), &profs); err != nil {
		fatal("read profiles.json: %v", err)
	}
	profBySlug := map[string]profile{}
	for _, p := range profs.Profiles {
		profBySlug[p.Slug] = p
	}

	var dl downloadsDoc
	if err := readJSON(filepath.Join(dataDir, "download-files.json"), &dl); err != nil {
		fatal("read download-files.json: %v", err)
	}

	identitiesDir := filepath.Join(*outDir, "identities")
	lensesDir := filepath.Join(*outDir, "lenses")
	mustMkdir(identitiesDir)
	mustMkdir(lensesDir)

	for _, m := range mappings {
		pf, ok := profBySlug[m.Slug]
		if !ok {
			fatal("profile slug %q not found in profiles.json", m.Slug)
		}
		p, ok := dl.Packages[m.Slug]
		if !ok {
			fatal("package slug %q not found in download-files.json", m.Slug)
		}
		files := map[string]string{}
		for _, f := range p.Files {
			files[f.Name] = f.Content
		}
		agentMD, ok := files["AGENT.md"]
		if !ok {
			fatal("slug %q has no AGENT.md", m.Slug)
		}
		var re rosterEntry
		if rs, ok := files["roster_entry.json"]; ok {
			if err := json.Unmarshal([]byte(rs), &re); err != nil {
				fatal("slug %q roster_entry.json: %v", m.Slug, err)
			}
		}
		model := re.ModelRecommendations.BestOverall
		if model == "" {
			model = pf.BestModel
		}
		tier := re.ModelRecommendations.Tier
		if tier == "" {
			tier = pf.ModelTier
		}

		// lens markdown = AGENT.md verbatim (the operating identity prose), with a
		// generated provenance header so a human knows where to re-seed it from.
		lensRel := filepath.Join("lenses", m.IdentityID+".md")
		lensBody := fmt.Sprintf("<!-- seeded by tools/seedidentities from hire profile %q (%s). Edit in place; re-running the seeder overwrites. -->\n\n%s",
			m.Slug, pf.Name, agentMD)
		writeFile(filepath.Join(lensesDir, m.IdentityID+".md"), lensBody)

		// identity yaml. Models/tiers are DATA, mirroring hire's roster_entry.
		writeFile(filepath.Join(identitiesDir, m.IdentityID+".yaml"),
			renderIdentity(m, pf, re, model, tier, lensRel))
	}

	fmt.Printf("seedidentities: wrote %d identities + lenses to %s\n", len(mappings), *outDir)
}

func renderIdentity(m stageMapping, pf profile, re rosterEntry, model, tier, lensRel string) string {
	displayName := re.DisplayName
	if displayName == "" {
		displayName = pf.Name
	}
	roleName := re.RoleName
	if roleName == "" {
		roleName = pf.Role
	}
	b := &sortableYAML{}
	b.line("# Default Flowbee identity, seeded by tools/seedidentities from the `hire`")
	b.line("# corpus. `model`/`model_tier` are DATA (recommendations), not control-position")
	b.line("# tokens — this file is NOT subject to the §5.6 provider-neutrality lint.")
	b.line("id: " + m.IdentityID)
	b.line("role: " + m.Role)
	b.line("stage: " + m.Stage)
	if m.Lens != "" {
		b.line("review_lens: " + m.Lens)
	}
	b.line("source_slug: " + m.Slug)
	b.line("display_name: " + yamlString(displayName))
	b.line("role_name: " + yamlString(roleName))
	b.line("tagline: " + yamlString(pf.Tagline))
	b.line("model: " + yamlString(model))
	b.line("model_tier: " + yamlString(tier))
	b.line("model_family: " + yamlString(modelFamily(model)))
	b.line("lens: " + lensRel)
	return b.String()
}

// modelFamily derives the provider/model family from a concrete model id, exactly
// as hire's roster keys it (anthropic/openai/google/…). Used as the anti-affinity
// axis input. This is DATA, not a control token.
func modelFamily(model string) string {
	switch {
	case hasPrefix(model, "claude"):
		return "anthropic"
	case hasPrefix(model, "gpt"):
		return "openai"
	case hasPrefix(model, "gemini"):
		return "google"
	case hasPrefix(model, "kimi"):
		return "kimi"
	case hasPrefix(model, "llama"), hasPrefix(model, "qwen"):
		return "ollama"
	default:
		return "unknown"
	}
}

func hasPrefix(s, p string) bool { return len(s) >= len(p) && s[:len(p)] == p }

// --- small helpers ---

type sortableYAML struct{ lines []string }

func (s *sortableYAML) line(l string) { s.lines = append(s.lines, l) }
func (s *sortableYAML) String() string {
	out := ""
	for _, l := range s.lines {
		out += l + "\n"
	}
	return out
}

func yamlString(s string) string {
	// Quote to be safe against ':' and other YAML metacharacters in taglines.
	b, _ := json.Marshal(s)
	return string(b)
}

func readJSON(path string, v any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

func mustMkdir(p string) {
	if err := os.MkdirAll(p, 0o755); err != nil {
		fatal("mkdir %s: %v", p, err)
	}
}

func writeFile(path, body string) {
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		fatal("write %s: %v", path, err)
	}
}

func fatal(f string, a ...any) {
	fmt.Fprintf(os.Stderr, "seedidentities: "+f+"\n", a...)
	os.Exit(1)
}

var _ = sort.Strings // reserved for future deterministic ordering
