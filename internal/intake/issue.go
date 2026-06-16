// Package intake turns external intent (a GitHub issue body) into the resolved
// task/context a job carries (F1). It is the seam between Domain-B issues and the
// task text a worker reads from the worktree. PURE parsing only: no clock, no
// network — the GitHub fetch lives in internal/github; this package only shapes
// an already-fetched body into Task/Spec/Acceptance.
package intake

import "strings"

// Task is the parsed-from-an-issue task context an agent acts on (§B). It maps
// directly onto store.SeedParams' TaskText / SpecText / AcceptanceCriteria.
type Task struct {
	Text               string
	Spec               string
	AcceptanceCriteria string
}

// TaskFromIssueBody is the STUB intake parser (F1): it shapes a GitHub issue body
// into a Task. The convention is lightweight markdown the issue author already
// writes — an optional "## Acceptance Criteria" (or "## Done When") section is
// peeled off into AcceptanceCriteria and an optional "## Spec" / "## Details"
// section into Spec; everything before the first such heading is the Task text.
// With no recognized headings the whole body is the task. Deterministic.
func TaskFromIssueBody(body string) Task {
	lines := strings.Split(body, "\n")
	var task, spec, accept []string
	cur := &task
	for _, ln := range lines {
		switch sectionOf(ln) {
		case sectionAcceptance:
			cur = &accept
			continue
		case sectionSpec:
			cur = &spec
			continue
		case sectionTask:
			cur = &task
			continue
		}
		*cur = append(*cur, ln)
	}
	return Task{
		Text:               strings.TrimSpace(strings.Join(task, "\n")),
		Spec:               strings.TrimSpace(strings.Join(spec, "\n")),
		AcceptanceCriteria: strings.TrimSpace(strings.Join(accept, "\n")),
	}
}

type section int

const (
	sectionNone section = iota
	sectionTask
	sectionSpec
	sectionAcceptance
)

// sectionOf classifies a markdown heading line into a known intake section.
func sectionOf(line string) section {
	t := strings.TrimSpace(line)
	if !strings.HasPrefix(t, "#") {
		return sectionNone
	}
	h := strings.ToLower(strings.TrimSpace(strings.TrimLeft(t, "#")))
	switch {
	case h == "acceptance criteria" || h == "done when" || h == "acceptance":
		return sectionAcceptance
	case h == "spec" || h == "details" || h == "specification" || h == "context":
		return sectionSpec
	case h == "task" || h == "summary" || h == "description":
		return sectionTask
	default:
		return sectionNone
	}
}
