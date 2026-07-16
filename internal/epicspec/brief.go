package epicspec

import (
	"fmt"
	"strings"
)

// RenderCriteria renders the epic-lane Phase 3 criteria-driven reviewer brief section
// (task brief point 3): the epic's Goal, Constraints/Non-Goals, and full ## Steps
// list with each step's Validate: criterion, plus explicit reviewer instructions for
// judging an epic PR against its OWN contract rather than as a generic diff. This is
// the FIXED half of the brief (spec-frozen per author-epic/SKILL.md, so it never
// grows with a long-running epic's status churn) — see RenderChecklist for the
// separately-truncatable claimed-status half, and the caller
// (internal/worker/review.go's renderReviewBrief) for how the two combine under the
// brief's byte cap.
func RenderCriteria(spec Spec) string {
	var b strings.Builder
	if g := strings.TrimSpace(spec.Goal); g != "" {
		fmt.Fprintf(&b, "**Goal:**\n\n%s\n\n", g)
	}
	if c := strings.TrimSpace(spec.Constraints); c != "" {
		fmt.Fprintf(&b, "**Constraints / Non-Goals:**\n\n%s\n\n", c)
	}
	if len(spec.Steps) > 0 {
		b.WriteString("**Steps (each with its own Validate: criterion):**\n\n")
		for _, step := range spec.Steps {
			fmt.Fprintf(&b, "%d. %s\n", step.N, step.Text)
			if v := strings.TrimSpace(step.Validate); v != "" {
				fmt.Fprintf(&b, "   Validate: %s\n", v)
			}
		}
		b.WriteString("\n")
	}
	b.WriteString("**Reviewer instructions for THIS epic PR — judge it against its OWN contract above:**\n\n" +
		"- RUN THE CODE — do not judge this epic PR from the diff alone. The epic lane's trust model REQUIRES " +
		"execution, which SUPERSEDES the generic 'you are not expected to run tests' guidance above for this PR: " +
		"independently build the code and run targeted tests — at minimum the epic's own `Validate:` commands " +
		"listed above — at the PR head, and judge on what you OBSERVE, not on what the checklist claims. Rigged " +
		"tests and real bugs (a buried error path, a broken flow) surface only when the code actually runs. This " +
		"composes with — never defers to — the automated evidence re-execution gate; if you cannot obtain the " +
		"source to run it here, say so explicitly in notes rather than approving on a diff-read alone.\n" +
		"- Verify EACH claimed `[x]` step below against the ACTUAL diff. A step claimed done but with NO " +
		"corresponding change visible in the diff is the PRIMARY failure mode — flag it as `changes_requested`, " +
		"not a nit.\n" +
		"- Spot-check evidence plausibility: does the test/command named as evidence actually appear in the " +
		"diff (or exist in the repo, if you have reason to doubt it)? Evidence that names a nonexistent test " +
		"is itself a blocking finding.\n" +
		"- Commits on this PR are expected to carry `Epic-Step: N/M` trailers — use them to walk the diff " +
		"step-by-step against the Steps list above where that helps you verify a specific claim.\n" +
		"- Approve ONLY if the diff honors the Constraints/Non-Goals above; a diff that is otherwise correct " +
		"but violates a stated constraint is not approvable.\n")
	return b.String()
}

// RenderChecklist renders the epic's claimed ## Status (State/Blockers/checklist) as
// the reviewer brief's "claimed status" section — the part RenderCriteria's doc calls
// out as separately truncatable (it scales with step count and evidence verbosity,
// unlike the fixed Goal/Constraints/Steps text).
func RenderChecklist(sb StatusBlock) string {
	var b strings.Builder
	state := sb.State
	if state == "" {
		state = "(none given)"
	}
	fmt.Fprintf(&b, "State: %s\n", state)
	if bl := strings.TrimSpace(sb.Blockers); bl != "" {
		fmt.Fprintf(&b, "Blockers: %s\n", bl)
	}
	if len(sb.Checklist) > 0 {
		b.WriteString("\n")
		for _, item := range sb.Checklist {
			box := "[ ]"
			if item.Checked {
				box = "[x]"
			}
			ev := strings.TrimSpace(item.Evidence)
			if ev == "" {
				ev = "(NO EVIDENCE GIVEN)"
			}
			fmt.Fprintf(&b, "- %s Step %d — %s (evidence: %s)\n", box, item.Step, item.Text, ev)
		}
	}
	return b.String()
}
