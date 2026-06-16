package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/samhotchkiss/flowbee/client"
)

// reviewVerdict is the structured decision a review/author agent writes to
// $FLOWBEE_VERDICT_FILE (.flowbee/verdict.json). The harness reads ONLY this file
// (or the authored spec) — never the agent's prose or exit code (I-9). It posts
// the role-appropriate endpoint from these fields.
type reviewVerdict struct {
	Decision     string `json:"decision"`           // approved|changes_requested|signed_off|amended|needs_design
	Disposition  string `json:"disposition"`        // self_merge|handoff (code_review)
	MeetsStyle   bool   `json:"meets_style"`         // spec_review
	MeetsReq     bool   `json:"meets_requirements"`  // spec_review
	SpecMarkdown string `json:"spec_markdown"`       // spec_author authored spec, or the amended spec
	Notes        string `json:"notes"`
}

// IsReviewRole reports whether a role runs through the verdict-file review harness
// (the agent emits a DECISION) rather than the worktree build harness (a PATCH).
func IsReviewRole(role string) bool {
	switch role {
	case "spec_author", "spec_reviewer", "code_reviewer":
		return true
	}
	return false
}

// RunOnceReviewHarness performs ONE Mode-A cycle for a REVIEW/AUTHOR role: lease,
// brief the agent (task + spec + diff) in a scratch dir, run it, read its decision
// from .flowbee/verdict.json (or its authored spec.md), and POST the role-correct
// verdict — /spec (author), /spec-review (issue-review), or /review (code-review).
// No worktree, no git: the agent's output is a decision, not a patch. Got=false on 204.
func RunOnceReviewHarness(ctx context.Context, cfg HarnessConfig) (HarnessOutcome, error) {
	arch, osName := cfg.Arch, cfg.OS
	if arch == "" {
		arch = runtime.GOARCH
	}
	if osName == "" {
		osName = runtime.GOOS
	}
	caps := cfg.Capabilities
	if len(caps) == 0 {
		caps = []string{"role:" + cfg.Role, "model_family:" + cfg.ModelFamily, "arch:" + arch, "os:" + osName}
	}

	c := client.NewWithToken(cfg.BaseURL, cfg.BearerToken)
	if _, err := c.Register(ctx, client.Registration{
		Identity: cfg.Identity, Host: hostname(), Capabilities: caps, Arch: arch, OS: osName,
		ModelSlots: cfg.ModelSlots, Weight: cfg.Weight, Accounts: cfg.Accounts,
	}); err != nil {
		return HarnessOutcome{}, fmt.Errorf("register: %w", err)
	}

	grant, ok, err := c.Lease(ctx, cfg.Identity, cfg.ModelFamily, cfg.Role)
	if err != nil {
		return HarnessOutcome{}, fmt.Errorf("lease: %w", err)
	}
	if !ok {
		return HarnessOutcome{Got: false}, nil
	}
	out := HarnessOutcome{Got: true, JobID: grant.JobID, LeaseEpoch: grant.LeaseEpoch}

	if _, st, err := c.Heartbeat(ctx, grant.JobID, grant.LeaseEpoch); err != nil {
		return out, fmt.Errorf("heartbeat: %w", err)
	} else if st != 200 {
		return out, fmt.Errorf("heartbeat status %d", st)
	}

	// scratch dir (no git): the brief in, the verdict/spec out.
	workRoot := cfg.WorkRoot
	if workRoot == "" {
		workRoot, err = os.MkdirTemp("", "flowbee-rv-")
		if err != nil {
			return out, fmt.Errorf("mkdir workroot: %w", err)
		}
		defer os.RemoveAll(workRoot)
	}
	dir := filepath.Join(workRoot, "rv-"+grant.JobID)
	fbDir := filepath.Join(dir, ".flowbee")
	if err := os.MkdirAll(fbDir, 0o755); err != nil {
		return out, fmt.Errorf("mkdir scratch: %w", err)
	}
	cctx := grant.Context
	if cctx == nil {
		cctx = &client.LeaseContext{Role: grant.Role}
	}
	taskFile := filepath.Join(fbDir, "task.md")
	if err := os.WriteFile(taskFile, []byte(renderReviewBrief(grant.JobID, grant.Role, cctx)), 0o644); err != nil {
		return out, fmt.Errorf("write brief: %w", err)
	}
	diffFile := filepath.Join(fbDir, "diff.patch")
	if cctx.Diff != "" {
		_ = os.WriteFile(diffFile, []byte(cctx.Diff), 0o644)
	}
	verdictFile := filepath.Join(fbDir, "verdict.json")
	specFile := filepath.Join(fbDir, "spec.md")

	if cfg.AgentCmd != "" {
		cmd := exec.CommandContext(ctx, "sh", "-c", cfg.AgentCmd)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"FLOWBEE_JOB_ID="+grant.JobID,
			"FLOWBEE_ROLE="+grant.Role,
			"FLOWBEE_BASE_SHA="+grant.BaseSHA,
			"FLOWBEE_TASK_FILE="+taskFile,
			"FLOWBEE_TASK="+cctx.Task,
			"FLOWBEE_SPEC="+cctx.Spec,
			"FLOWBEE_ACCEPTANCE="+cctx.AcceptanceCriteria,
			"FLOWBEE_IDENTITY="+cctx.Identity,
			"FLOWBEE_LENS="+cctx.Lens,
			"FLOWBEE_DIFF_FILE="+diffFile,
			"FLOWBEE_VERDICT_FILE="+verdictFile,
			"FLOWBEE_SPEC_FILE="+specFile,
		)
		var errb strings.Builder
		cmd.Stderr = &errb
		if err := cmd.Run(); err != nil {
			return out, fmt.Errorf("agent cmd: %v: %s", err, strings.TrimSpace(errb.String()))
		}
	}

	idem := fmt.Sprintf("%s-e%d", grant.JobID, grant.LeaseEpoch)
	switch grant.Role {
	case "spec_author":
		spec := ""
		if v, e := readVerdict(verdictFile); e == nil {
			spec = v.SpecMarkdown
		}
		if strings.TrimSpace(spec) == "" {
			if b, e := os.ReadFile(specFile); e == nil {
				spec = string(b)
			}
		}
		if strings.TrimSpace(spec) == "" {
			_, _ = c.Release(ctx, grant.JobID, grant.LeaseEpoch)
			return out, fmt.Errorf("spec_author produced no spec")
		}
		version := grant.SpecVersion
		if version == 0 {
			version = 1
		}
		_, _, st, err := c.SpecSubmit(ctx, grant.JobID, grant.LeaseEpoch, spec, version)
		if err != nil {
			return out, fmt.Errorf("spec submit: %w", err)
		}
		if st != 200 {
			return out, fmt.Errorf("spec submit status %d", st)
		}
		out.JobState = "spec_review"

	case "spec_reviewer":
		v, e := readVerdict(verdictFile)
		if e != nil {
			return out, fmt.Errorf("read verdict: %w", e)
		}
		switch v.Decision {
		case "amended":
			resp, st, err := c.SpecReviewAmend(ctx, grant.JobID, grant.LeaseEpoch, idem, grant.SpecContentHash, v.SpecMarkdown, grant.SpecVersion+1)
			if err != nil {
				return out, fmt.Errorf("spec amend: %w", err)
			}
			if st != 200 {
				return out, fmt.Errorf("spec amend status %d", st)
			}
			out.JobState = resp.JobState
		case "needs_design":
			resp, st, err := c.SpecReviewNeedsDesign(ctx, grant.JobID, grant.LeaseEpoch, idem, grant.SpecContentHash)
			if err != nil {
				return out, fmt.Errorf("spec needs_design: %w", err)
			}
			if st != 200 {
				return out, fmt.Errorf("spec needs_design status %d", st)
			}
			out.JobState = resp.JobState
		default: // signed_off | changes_requested
			dec := v.Decision
			if dec == "" {
				dec = "signed_off"
			}
			resp, st, err := c.SpecReview(ctx, grant.JobID, grant.LeaseEpoch, idem, dec, grant.SpecContentHash, v.MeetsStyle, v.MeetsReq)
			if err != nil {
				return out, fmt.Errorf("spec review: %w", err)
			}
			if st != 200 {
				return out, fmt.Errorf("spec review status %d", st)
			}
			out.JobState = resp.JobState
		}

	case "code_reviewer":
		v, e := readVerdict(verdictFile)
		if e != nil {
			return out, fmt.Errorf("read verdict: %w", e)
		}
		verdict := "changes_requested"
		if v.Decision == "approved" {
			verdict = "approved"
		}
		disp := v.Disposition
		if disp == "" {
			disp = "self_merge"
		}
		resp, st, err := c.Review(ctx, grant.JobID, grant.LeaseEpoch, idem, verdict, disp)
		if err != nil {
			return out, fmt.Errorf("review: %w", err)
		}
		if st != 200 {
			return out, fmt.Errorf("review status %d", st)
		}
		out.JobState = resp.JobState
	}

	if _, err := c.Release(ctx, grant.JobID, grant.LeaseEpoch); err != nil {
		_ = err
	}
	return out, nil
}

func readVerdict(path string) (reviewVerdict, error) {
	var v reviewVerdict
	b, err := os.ReadFile(path)
	if err != nil {
		return v, err
	}
	if err := json.Unmarshal(b, &v); err != nil {
		return v, fmt.Errorf("parse verdict.json: %w", err)
	}
	return v, nil
}

// renderReviewBrief writes the role-specific instructions + the EXACT verdict
// schema the agent must emit to $FLOWBEE_VERDICT_FILE, so an operator's generic
// agent-cmd (e.g. `claude -p "$(cat $FLOWBEE_TASK_FILE)"`) produces a usable
// decision without knowing Flowbee internals.
func renderReviewBrief(jobID, role string, c *client.LeaseContext) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Flowbee %s — %s\n\n", role, jobID)
	if c.Identity != "" {
		fmt.Fprintf(&b, "- **Act as:** %s\n", c.Identity)
	}
	if c.Lens != "" {
		fmt.Fprintf(&b, "- **Lens:** %s\n", c.Lens)
	}
	b.WriteString("\n")
	writeIf := func(h, v string) {
		if strings.TrimSpace(v) != "" {
			fmt.Fprintf(&b, "## %s\n\n%s\n\n", h, v)
		}
	}
	switch role {
	case "spec_author":
		b.WriteString("Author a clear, buildable spec for the task below. Be specific about scope and acceptance.\n\n")
		writeIf("Task", c.Task)
		writeIf("Context", c.Spec)
		writeIf("Acceptance criteria", c.AcceptanceCriteria)
		b.WriteString("**Output:** write the full spec markdown to the file at $FLOWBEE_SPEC_FILE " +
			"(or as the `spec_markdown` field of a JSON object at $FLOWBEE_VERDICT_FILE).\n")
	case "spec_reviewer":
		b.WriteString("You are the issue-reviewer. Judge the spec below for scope, coverage, clarity, and standards. " +
			"Prefer `signed_off` if it is buildable; use `amended` to FIX it in place (supply the full corrected spec); " +
			"use `needs_design` ONLY if it needs human design input.\n\n")
		writeIf("Spec under review", c.Spec)
		writeIf("Task", c.Task)
		writeIf("Acceptance criteria", c.AcceptanceCriteria)
		b.WriteString("**Output:** write JSON to $FLOWBEE_VERDICT_FILE:\n" +
			"```json\n{\"decision\":\"signed_off|amended|needs_design|changes_requested\"," +
			"\"meets_style\":true,\"meets_requirements\":true," +
			"\"spec_markdown\":\"(ONLY if amended: the full corrected spec)\",\"notes\":\"...\"}\n```\n")
	case "code_reviewer":
		b.WriteString("You are the code-reviewer. Read the diff at $FLOWBEE_DIFF_FILE and judge whether it correctly, " +
			"completely, and safely implements the task/spec below. Approve ONLY if it is correct and safe to merge.\n\n")
		writeIf("Task", c.Task)
		writeIf("Spec", c.Spec)
		writeIf("Acceptance criteria", c.AcceptanceCriteria)
		b.WriteString("The change to review is the unified diff at $FLOWBEE_DIFF_FILE (.flowbee/diff.patch).\n\n")
		b.WriteString("**Output:** write JSON to $FLOWBEE_VERDICT_FILE:\n" +
			"```json\n{\"decision\":\"approved|changes_requested\",\"disposition\":\"self_merge\",\"notes\":\"...\"}\n```\n")
	}
	return b.String()
}
