// Package epicspec parses and reasons about epic-lane markdown specs (epic-lane
// Phase 2): the frontmatter + ## Steps a spec author writes once (frozen the moment
// `flowbee epic start` triggers — spec immutability, per
// scratchpad/epic-docs/author-epic/SKILL.md), and the ## Status block a running
// agent updates on its own branch afterward. Every parser here is LENIENT by design
// (§ task brief): a missing optional field is fine, a malformed status block
// degrades to a mostly-empty StatusBlock rather than an error, because the status
// ingestion loop must never let one epic's format hiccup blind it to every other
// epic (see cmd/flowbee's ingestEpicStatuses). Only ParseSpec — run ONCE, at launch
// time, off main — is asked to fail loudly on a genuinely unusable spec (no scope,
// no steps): that one refusal is cheap up front and expensive to discover mid-flight.
//
// PURE text parsing, no I/O, no clock — the caller (cmd/flowbee) supplies file
// content read via the control-plane mirror and decides what to do with the result.
package epicspec

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// Frontmatter is the epic file's YAML header (author-epic/SKILL.md "## File").
type Frontmatter struct {
	Title string   `yaml:"title"`
	Scope []string `yaml:"scope"`
	Host  string   `yaml:"host"`  // optional — pin to a specific box
	Agent string   `yaml:"agent"` // optional — override the default coding agent
}

// Step is one ## Steps entry: the ordered task text plus its executable Validate:
// evidence line (author-epic/SKILL.md "Quality bar" — never a vibe).
type Step struct {
	N        int
	Text     string
	Validate string
}

// Spec is a fully parsed epic file, ready for `flowbee epic start` to act on.
type Spec struct {
	Frontmatter
	Goal        string
	Constraints string
	Steps       []Step
}

// ParseSpec parses an epic markdown file's frontmatter + ## Steps. It is
// INTENTIONALLY the one strict parser in this package (see package doc): missing
// optional frontmatter fields (host/agent) are fine, but a missing title, an empty
// scope:, or zero parsed steps is a launch-blocking error — "flowbee epic start
// refuses to launch ... if the epic needs a path outside scope:" starts from scope:
// actually being present, and an epic with no steps has nothing to run.
func ParseSpec(content string) (Spec, error) {
	fm, body, err := parseFrontmatter(content)
	if err != nil {
		return Spec{}, fmt.Errorf("parse frontmatter: %w", err)
	}
	if strings.TrimSpace(fm.Title) == "" {
		return Spec{}, fmt.Errorf("epic frontmatter is missing required `title:`")
	}
	if len(fm.Scope) == 0 {
		return Spec{}, fmt.Errorf("epic frontmatter is missing required `scope:` (a blast-radius reservation is mandatory — see author-epic/SKILL.md)")
	}
	goal := section(body, "Goal")
	constraints := section(body, "Constraints / Non-Goals")
	stepsBody := section(body, "Steps")
	steps, err := parseSteps(stepsBody)
	if err != nil {
		return Spec{}, fmt.Errorf("parse ## Steps: %w", err)
	}
	if len(steps) == 0 {
		return Spec{}, fmt.Errorf("epic has no ## Steps (need at least one step with a Validate: line)")
	}
	return Spec{Frontmatter: fm, Goal: goal, Constraints: constraints, Steps: steps}, nil
}

// frontmatterFence matches the leading "---\n...\n---" YAML block. Anchored to the
// very start of the file (frontmatter must be the first thing, per convention).
var frontmatterFence = regexp.MustCompile(`(?s)\A---\r?\n(.*?)\r?\n---\r?\n?`)

// parseFrontmatter splits content into its YAML frontmatter and the markdown body
// that follows. Missing frontmatter entirely is not itself an error here (ParseSpec
// catches the resulting empty title/scope) — this function's only job is the split.
func parseFrontmatter(content string) (Frontmatter, string, error) {
	m := frontmatterFence.FindStringSubmatchIndex(content)
	if m == nil {
		return Frontmatter{}, content, nil
	}
	yamlBlock := content[m[2]:m[3]]
	rest := content[m[1]:]
	var fm Frontmatter
	if err := yaml.Unmarshal([]byte(yamlBlock), &fm); err != nil {
		return Frontmatter{}, content, fmt.Errorf("invalid YAML: %w", err)
	}
	return fm, rest, nil
}

// headingRe matches an ATX heading of any level ("## Steps", "### Status", etc.),
// capturing its level and text.
var headingRe = regexp.MustCompile(`(?m)^(#{1,6})\s+(.+?)\s*$`)

// section extracts the body text under a "## <name>" heading, up to (not including)
// the next heading of the SAME OR SHALLOWER level, or end of document. name is
// matched case-insensitively and with surrounding whitespace trimmed so small
// authoring variance ("## Goal " vs "## Goal") doesn't break the split. Returns ""
// if the heading is absent — callers treat an absent optional section as empty, not
// an error (leniency for Goal/Constraints; ParseSpec enforces Steps non-emptiness
// itself since an absent ## Steps heading parses to zero steps either way).
// ParseStatusSection extracts the "## Status" section body out of a FULL epic
// file's content (frontmatter + all sections) — the entry point the status-
// ingestion tick (cmd/flowbee's ingestEpicStatuses) uses on content read off an
// epic's own branch, feeding the result straight into ParseStatus. A thin exported
// wrapper over section() so callers outside this package never need to know the
// heading-splitting mechanics, only "give me content, I'll give you the Status
// section text (possibly empty)".
func ParseStatusSection(content string) string {
	return section(content, "Status")
}

func section(body, name string) string {
	locs := headingRe.FindAllStringSubmatchIndex(body, -1)
	for i, loc := range locs {
		level := len(body[loc[2]:loc[3]])
		text := strings.TrimSpace(body[loc[4]:loc[5]])
		if !strings.EqualFold(text, name) {
			continue
		}
		start := loc[1]
		end := len(body)
		for _, next := range locs[i+1:] {
			nextLevel := len(body[next[2]:next[3]])
			if nextLevel <= level {
				end = next[0]
				break
			}
		}
		return strings.TrimSpace(body[start:end])
	}
	return ""
}

// stepMarker matches a step's leading marker at the start of a line: an ordered
// "1. " / "1) " form or a bulleted "- " / "* " form. Steps are split on whichever
// convention the author used (the design doc doesn't mandate one), so both are
// accepted uniformly rather than picking a single required style. Deliberately
// matches ONLY the marker + its trailing whitespace (not "(.*)$" for the rest of
// the line) — parseSteps needs the marker's own end offset to strip it cleanly
// without swallowing the step's first line of text along with it.
var stepMarker = regexp.MustCompile(`(?m)^\s*(?:(\d+)[.)]|[-*])[ \t]+`)

// validateLine matches a "Validate:" line anywhere inside a step's block (it is
// documented as the LAST line of a step, but leniently matched wherever it appears
// so a step that puts Validate: before a trailing blank line still parses).
var validateLine = regexp.MustCompile(`(?m)^\s*Validate:\s*(.*)$`)

// parseSteps splits a ## Steps section body into ordered Step values. A step
// without its own explicit number (bulleted form) is numbered by POSITION
// (1-indexed) — the numbering only needs to be internally consistent for
// `Current: step N/M` bookkeeping, not to match some author-chosen scheme.
func parseSteps(body string) ([]Step, error) {
	if strings.TrimSpace(body) == "" {
		return nil, nil
	}
	markers := stepMarker.FindAllStringSubmatchIndex(body, -1)
	if len(markers) == 0 {
		return nil, fmt.Errorf("no numbered or bulleted steps found")
	}
	var steps []Step
	for i, m := range markers {
		blockStart := m[0]
		blockEnd := len(body)
		if i+1 < len(markers) {
			blockEnd = markers[i+1][0]
		}
		block := body[blockStart:blockEnd]
		n := i + 1
		if m[2] >= 0 { // an explicit "N." number was captured
			if parsed, err := strconv.Atoi(body[m[2]:m[3]]); err == nil {
				n = parsed
			}
		}
		// strip the marker itself off the front of the block first (m[1]-blockStart
		// is the marker's own end offset within block, since block starts at m[0]).
		rest := block[m[1]-blockStart:]
		validate := ""
		text := rest
		if vm := validateLine.FindStringSubmatchIndex(rest); vm != nil {
			validate = strings.TrimSpace(rest[vm[2]:vm[3]])
			text = rest[:vm[0]]
		}
		text = strings.TrimSpace(text)
		steps = append(steps, Step{N: n, Text: text, Validate: validate})
	}
	return steps, nil
}
