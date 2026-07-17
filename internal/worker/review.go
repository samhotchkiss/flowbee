package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"unicode/utf8"

	"github.com/samhotchkiss/flowbee/client"
	"github.com/samhotchkiss/flowbee/internal/gitops"
)

// reviewVerdict is the structured decision a review/author agent writes to
// $FLOWBEE_VERDICT_FILE (.flowbee/verdict.json). The harness reads ONLY this file
// (or the authored spec) — never the agent's prose or exit code (I-9). It posts
// the role-appropriate endpoint from these fields.
type reviewVerdict struct {
	Decision     string `json:"decision"`           // approved|changes_requested|signed_off|amended|needs_design
	Disposition  string `json:"disposition"`        // self_merge|handoff (code_review)
	MeetsStyle   bool   `json:"meets_style"`        // spec_review
	MeetsReq     bool   `json:"meets_requirements"` // spec_review
	SpecMarkdown string `json:"spec_markdown"`      // spec_author authored spec, or the amended spec
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
		caps = []string{"role:" + cfg.Role, "model_family:" + cfg.ModelFamily, "model:" + cfg.modelTag(), "arch:" + arch, "os:" + osName}
	}

	c := client.NewWithToken(cfg.BaseURL, cfg.BearerToken)
	c.Model = cfg.ModelLabel
	reg := client.Registration{
		Identity: cfg.Identity, Host: hostname(), Capabilities: caps, Arch: arch, OS: osName,
		ModelSlots: cfg.ModelSlots, Weight: cfg.Weight, Accounts: cfg.Accounts,
	}
	if resp, err := c.Register(ctx, reg); err != nil {
		return HarnessOutcome{}, fmt.Errorf("register: %w", err)
	} else {
		reg.WorkerID = resp.WorkerID
	}

	grant, ok, err := c.Lease(ctx, cfg.Identity, cfg.ModelFamily, cfg.Role)
	if err != nil {
		return HarnessOutcome{}, fmt.Errorf("lease: %w", err)
	}
	if !ok {
		return HarnessOutcome{Got: false}, nil
	}
	out := HarnessOutcome{Got: true, JobID: grant.JobID, LeaseEpoch: grant.LeaseEpoch}

	// A code_reviewer only judges a PR once CI is reconciled GREEN: an approval
	// before then cannot mint and would bounce the build into a rebuild thrash. If
	// CI is not ready, release the lease (re-arms review_pending, NOT the build) and
	// signal the loop to back off — no agent is spawned, nothing is wasted. This is
	// now a DEFENSIVE BACKSTOP: the server no longer offers a CI-not-ready review as
	// a lease candidate (ReviewPendingCandidates filters it), so a reviewer should
	// never be granted one. Kept in case a grant races a CI status flip to not-green.
	if grant.Role == "code_reviewer" && grant.Context != nil && !grant.Context.CIReady {
		_, _ = c.Release(ctx, grant.JobID, grant.LeaseEpoch)
		out.Skipped = true
		return out, nil
	}

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
	if err := writeReviewDiffArtifact(diffFile, cctx); err != nil {
		return out, err
	}
	// INSTRUMENTATION: surface the review INPUT size. A code review of an empty/tiny diff
	// (cctx.Diff unpopulated at lease-grant) can only bounce — the reviewer has nothing to
	// approve. Logged to the worker stderr (fleet log) to diagnose the bounce rate.
	if grant.Role == "code_reviewer" {
		fmt.Fprintf(os.Stderr, "[%s] REVIEW-INPUT job=%s diff_bytes=%d task_bytes=%d spec_bytes=%d ac_bytes=%d\n",
			cfg.Identity, grant.JobID, len(cctx.Diff), len(cctx.Task), len(cctx.Spec), len(cctx.AcceptanceCriteria))
		// dump the EXACT brief + diff the agent will see, to compare against a known-good
		// controlled run (diagnosing the spurious-bounce rate). Best-effort.
		dumpDir := filepath.Join("/tmp/flowbee-rev", grant.JobID)
		if os.MkdirAll(dumpDir, 0o755) == nil {
			_ = os.WriteFile(filepath.Join(dumpDir, "brief.md"), []byte(renderReviewBrief(grant.JobID, grant.Role, cctx)), 0o644)
			_ = os.WriteFile(filepath.Join(dumpDir, "diff.patch"), []byte(cctx.Diff), 0o644)
		}
	}
	verdictFile := filepath.Join(fbDir, "verdict.json")
	specFile := filepath.Join(fbDir, "spec.md")

	agentEnv := append(os.Environ(),
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
		"FLOWBEE_DIFF_EMPTY="+fmt.Sprintf("%t", cctx.DiffEmpty),
		"FLOWBEE_VERDICT_FILE="+verdictFile,
		"FLOWBEE_SPEC_FILE="+specFile,
	)
	agentOut, err := runAgentHeartbeatIO(ctx, c, &reg, grant.JobID, grant.LeaseEpoch, grant.LeaseTTLS, dir, cfg.AgentCmd, agentEnv, true)
	if err != nil {
		// The agent CLI failed to even RUN (e.g. `sh: 1: codex: Argument list too long` — a
		// large diff embedded inline in the review brief blowing the OS's per-argument exec
		// limit; see RunOnceHarness's identical guard, commit 7b5cc91, which fixed this for
		// the BUILD / conflict_resolver harnesses but missed this review harness). Without an
		// explicit release the lease sits silently claimed (one heartbeat already sent above,
		// then nothing) until the server's heartbeat-stale reap (~4min), which just hands the
		// SAME oversized diff to the next reviewer to fail identically — the review-path
		// analogue of the conflict_resolver thrash (russ #3388's chronic needs_human bounce:
		// review_claimed -> silence -> heartbeat_stale reap -> review_claimed by a different
		// identity -> silence -> ... for hours, night after night). Release as FAILED (burns
		// an attempt, matching this file's other review-failure paths below) so a persistent
		// failure escalates to needs_human after max_attempts instead of thrashing in ~4min
		// cycles forever. Best-effort: if the lease was already revoked (stale epoch), this is
		// a no-op.
		_, _ = c.ReleaseFailed(ctx, grant.JobID, grant.LeaseEpoch)
		return out, err
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
		// fallback: a generic `claude -p` agent prints the spec to stdout instead of
		// writing $FLOWBEE_SPEC_FILE. Use what it emitted rather than discarding the
		// whole run and re-claiming forever (the first-live-run spec_author wedge).
		// agentResultText unwraps claude's JSON `.result` when the agent ran with
		// --output-format json (for cost capture), else returns the raw stdout.
		if strings.TrimSpace(spec) == "" {
			spec = strings.TrimSpace(agentResultText(agentOut))
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
			// the agent produced no parseable verdict: release as a FAILED attempt (burns an
			// attempt) so a persistently-broken reviewer escalates after max_attempts instead
			// of churning claim↔TTL-expiry. Mirrors the build path's no-output abandon.
			_, _ = c.ReleaseFailed(ctx, grant.JobID, grant.LeaseEpoch)
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
			// the agent produced no parseable verdict: release as a FAILED attempt (burns an
			// attempt) so a persistently-broken reviewer escalates after max_attempts instead
			// of churning claim↔TTL-expiry. Mirrors the build path's no-output abandon.
			_, _ = c.ReleaseFailed(ctx, grant.JobID, grant.LeaseEpoch)
			return out, fmt.Errorf("read verdict: %w", e)
		}
		// CAPTURE the RAW verdict the agent wrote, to resolve why approving notes get a
		// changes_requested decision in production (the decision field disagrees with the prose).
		if raw, rerr := os.ReadFile(verdictFile); rerr == nil {
			dd := filepath.Join("/tmp/flowbee-rev", grant.JobID)
			_ = os.MkdirAll(dd, 0o755)
			_ = os.WriteFile(filepath.Join(dd, "verdict-raw.json"), raw, 0o644)
		}
		// Normalize the decision before matching: agents (both codex and claude) intermittently
		// emit a case/whitespace/synonym variant of "approved" ("Approved", "approve", "lgtm")
		// while their notes clearly approve ("No blocking defect identifiable from the diff"),
		// and the old exact == "approved" silently bounced those — a false rejection that sends a
		// good PR back to a full rebuild. An EMPTY decision means the agent wrote a verdict file
		// but never filled the field: retry the review (ReleaseFailed) rather than burn the build
		// with a bounce. Log the raw decision on any non-approval so a real miscalibration is
		// visible rather than hiding behind a generic bounce.
		norm := strings.ToLower(strings.Trim(strings.TrimSpace(v.Decision), "\"'`.,!"))
		if norm == "" {
			fmt.Fprintf(os.Stderr, "[%s] review: empty decision in verdict.json for %s — retrying (not bouncing)\n", cfg.Identity, grant.JobID)
			_, _ = c.ReleaseFailed(ctx, grant.JobID, grant.LeaseEpoch)
			return out, fmt.Errorf("review: empty decision")
		}
		verdict := "changes_requested"
		switch norm {
		case "approved", "approve", "approves", "approved_with_nits", "approve_with_comments",
			"accept", "accepted", "lgtm", "ok", "pass":
			verdict = "approved"
		}
		if verdict == "changes_requested" {
			tail := v.Notes
			if len(tail) > 220 {
				tail = tail[len(tail)-220:]
			}
			fmt.Fprintf(os.Stderr, "[%s] review BOUNCE %s raw_decision=%q notes_tail=%q\n", cfg.Identity, grant.JobID, v.Decision, tail)
		}
		disp := v.Disposition
		if disp == "" {
			disp = "self_merge"
		}
		// the reviewer lands an EMPTY findings-commit on the issue branch (build-list:
		// "reviewers perform an empty commit with their findings + verdict"), authored as
		// the reviewer, so the branch history records the review the same way the issue
		// comment does. Best-effort: the verdict is canonical in Flowbee's ledger; a push
		// failure (e.g. the branch moved) is logged, never voids a valid review.
		reviewerHead := reviewerEmptyCommit(cfg, grant, verdict, v.Notes)
		resp, st, err := c.Review(ctx, grant.JobID, grant.LeaseEpoch, idem, verdict, disp, v.Notes, reviewerHead)
		if err != nil {
			return out, fmt.Errorf("review: %w", err)
		}
		if st != 200 {
			return out, fmt.Errorf("review status %d", st)
		}
		out.JobState = resp.JobState
	}

	// NO trailing Release: the verdict POST (/spec, /spec-review, /review) already
	// consumed the lease and transitioned the job. An extra release would re-arm a
	// just-advanced job (spec_review -> ready). A failed POST leaves the lease to
	// expire on TTL and re-arm for a clean retry.
	return out, nil
}

// reviewerEmptyCommit lands the reviewer's verdict as an EMPTY commit on the issue
// branch (build-list: "reviewers perform an empty commit with their findings and
// declaration of approved or rejected"), authored AS the reviewer so `git log`
// attributes the verdict. It fetches the branch tip into the reviewer's own mirror,
// stacks an --allow-empty commit on it, and pushes with the reviewer's key. Entirely
// best-effort: the verdict is canonical in Flowbee's ledger, so any git failure here
// is swallowed (the issue comment + the ledger still record the review). A no-op when
// the reviewer has no branch/remote configured (same-box / bundle deployments).
// reviewerEmptyCommit returns the issue-branch HEAD it pushed (empty when it did not push
// — no branch/remote, a git failure, or a push race), so the caller can report it on the
// review submission. That lets Flowbee track the head the reviewer just advanced: an N>1
// consensus panel keeps the job in review across rounds, and without tracking this move the
// async reconcile would see the reviewer's own empty commit as a SHA move and supersede the
// round (resetting the accumulated approvals).
// reviewerEmptyCommitEnabled gates the empty findings-commit push. It is OFF.
//
// Pushing a no-op commit to the PR branch as a review attestation is net-negative against
// a repo with required status checks (russ): GitHub re-runs the FULL required-CI matrix on
// the empty commit (so every review costs a 6-shard backend run, ~3-4m each) AND advances
// the branch tip PAST the head the verdict pins to. Self-merge SHA-interlocks on the
// reviewed head, so the live tip (the empty commit) invalidates that approval and re-arms
// review while its own required CI is pending. The result is a churn loop: approve -> empty
// commit -> CI reset + head move -> re-arm -> repeat (observed on russ #2359/#2407, and the driver behind
// #2466's dozens-of-builder-passes churn). Review attribution does NOT depend on this commit:
// the verdict is canonical in Flowbee's ledger and the control plane mirrors it to the GitHub
// issue as a durable comment (api.server.review). Re-enable only once CI is made to skip
// same-tree (empty) pushes so an attestation commit no longer resets required checks.
var reviewerEmptyCommitEnabled = false

func reviewerEmptyCommit(cfg HarnessConfig, grant client.LeaseGrant, verdict, notes string) (head string) {
	if !reviewerEmptyCommitEnabled {
		// no head advance: the verdict stays pinned to the green reviewed SHA so self-merge
		// can SHA-interlock-merge it immediately, and CI is not re-triggered on a no-op.
		return ""
	}
	issueBranch, repoURL := "", cfg.RepoURL
	if grant.Context != nil {
		issueBranch = grant.Context.IssueBranch
		if grant.Context.RepoURL != "" {
			repoURL = grant.Context.RepoURL // F9: the job's repo, not a single configured one
		}
	}
	if issueBranch == "" || repoURL == "" {
		return
	}
	mirrorPath := workerMirrorFor(cfg.MirrorPath, repoURL)
	branch := cfg.Branch
	if branch == "" {
		branch = "main"
	}
	if err := ensureMirror(context.Background(), mirrorPath, repoURL, branch); err != nil {
		return
	}
	mirror := gitops.Open(mirrorPath)
	tip, exists, err := mirror.RemoteBranchTip(repoURL, issueBranch)
	if err != nil || !exists || tip == "" {
		return // nothing to stack the verdict on (no build commit yet)
	}
	if err := mirror.FetchRef(repoURL, "refs/heads/"+issueBranch, "refs/flowbee/review-tip/"+grant.JobID); err != nil {
		return
	}
	wsRoot, err := os.MkdirTemp("", "flowbee-rvc-")
	if err != nil {
		return
	}
	defer os.RemoveAll(wsRoot)
	wsDir := filepath.Join(wsRoot, "ws")
	wt, err := mirror.AddWorktree(wsDir, tip)
	if err != nil {
		return
	}
	defer wt.Destroy()

	label := "CHANGES REQUESTED"
	if verdict == "approved" {
		label = "APPROVED"
	}
	msg := fmt.Sprintf("review(%s): %s\n\n", cfg.Identity, label)
	if n := strings.TrimSpace(notes); n != "" {
		msg += n + "\n"
	} else {
		msg += "(no findings recorded)\n"
	}
	sha, err := wt.CommitAuthored(cfg.Identity, msg, true)
	if err != nil || sha == "" {
		return
	}
	if err := wt.PushTo(repoURL, issueBranch, false); err != nil {
		return // push failed (e.g. the branch moved): the remote head did NOT advance, so report nothing
	}
	head = sha
	return
}

func writeReviewDiffArtifact(path string, cctx *client.LeaseContext) error {
	if cctx == nil {
		return nil
	}
	if cctx.Diff == "" && !cctx.DiffEmpty {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("mkdir diff artifact dir: %w", err)
	}
	if err := os.WriteFile(path, []byte(cctx.Diff), 0o644); err != nil {
		return fmt.Errorf("write diff artifact: %w", err)
	}
	return nil
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

// maxTotalBriefBytes caps the TOTAL rendered brief (task/spec/acceptance-criteria text
// PLUS diff) that renderReviewBrief/renderTaskMarkdown will embed INLINE in an agent brief.
// The rendered brief is what gets passed as a SINGLE shell argv element to the agent
// (`codex exec "$(cat "$FLOWBEE_TASK_FILE")"` / the claude equivalent — see
// cmd/flowbee/fleet.go's codexReviewTmpl/reviewAgentTmpl), and Linux enforces MAX_ARG_STRLEN
// — a hard ~128KiB cap on any SINGLE argv string, independent of the much larger total
// ARG_MAX (2MiB) most people check. A brief at or beyond that blows the exec call before the
// agent even starts: reproduced live against a real 269,705-byte review brief on the feller
// fleet box while diagnosing russ #3388's chronic needs_human bounce (`sh: 1: /bin/true:
// Argument list too long`, exit 126) — the review-path sibling of the conflict_resolver argv
// bug fixed for the build harnesses in commit 7b5cc91.
//
// An EARLIER version of this cap (maxInlineDiffBytes = 100KiB) checked only the diff's own
// size in isolation, on the assumption task/spec/acceptance-criteria text added "a few more
// KB on top". That assumption broke live: job 01KWMSKDKAV3WC9QZ4Q20B0N8E (diff_bytes=96559,
// under the 100KiB diff-only cap) still hit "Argument list too long" on EVERY reviewer across
// all three fleet boxes simultaneously, because task_bytes=20934 + spec_bytes=20934 +
// ac_bytes=662 pushed the TOTAL brief past the ~128KiB wall even with the "capped" diff.
// Budget against the total instead: callers check `b.Len()+len(diff) <= maxTotalBriefBytes`
// (b.Len() already reflects everything rendered before the diff decision), so oversized
// task/spec text correctly eats into the diff's inline budget rather than being ignored.
const maxTotalBriefBytes = 110 * 1024

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
		b.WriteString("**Output (REQUIRED):** write the full spec markdown to the file at the path in " +
			"the $FLOWBEE_SPEC_FILE environment variable. Use a shell command, e.g. " +
			"`cat > \"$FLOWBEE_SPEC_FILE\" <<'EOF'` … `EOF`. Do NOT just print the spec as your reply — " +
			"it is read from that file, and a run that writes nothing is discarded and retried.\n")
	case "spec_reviewer":
		b.WriteString("You are the issue-reviewer. Judge ONE thing: could an engineer BUILD this spec as-is? " +
			"Default to `signed_off` — a spec is signable if it is clear enough to implement, even if imperfect. " +
			"Use `amended` to fix small gaps yourself in place (supply the full corrected spec as spec_markdown). " +
			"Use `needs_design` ONLY when the work genuinely needs a human PRODUCT/design decision you cannot make " +
			"— this is rare; do NOT use it merely because a spec is simple or could be more detailed.\n\n")
		writeIf("Spec under review", c.Spec)
		writeIf("Task", c.Task)
		writeIf("Acceptance criteria", c.AcceptanceCriteria)
		b.WriteString("**Output:** write JSON to $FLOWBEE_VERDICT_FILE:\n" +
			"```json\n{\"decision\":\"signed_off|amended|needs_design|changes_requested\"," +
			"\"meets_style\":true,\"meets_requirements\":true," +
			"\"spec_markdown\":\"(ONLY if amended: the full corrected spec)\",\"notes\":\"...\"}\n```\n")
	case "code_reviewer":
		b.WriteString("You are the code-reviewer. Judge whether the unified diff (included in full below) correctly, " +
			"completely, and safely implements the task/spec below.\n\n")
		b.WriteString("**How to review (READ THIS):** You are given ONLY the diff (the changed lines) — by design. " +
			"The full source tree is NOT provided and you do NOT need it. Automated tests run SEPARATELY in CI and " +
			"gate the merge on their own: you are NOT expected to run tests, execute code, or inspect unchanged files, " +
			"and you MUST NOT withhold approval merely because you 'could not verify against the source tree', " +
			"'could not run the tests', or any similar unverifiable caveat. Judge the change FROM THE DIFF plus the " +
			"task/spec as written.\n\n")
		writeIf("Task", c.Task)
		writeIf("Spec", c.Spec)
		writeIf("Acceptance criteria", c.AcceptanceCriteria)
		writeEpicCriteria(&b, c)
		// Embed the diff INLINE in the brief rather than only referencing $FLOWBEE_DIFF_FILE.
		// A file reference relies on the agent proactively reading the file; in practice the
		// reviewer often did NOT (a 48 KB diff "reviewed" in ~70s = never opened), so it judged
		// blind on task/spec alone and bounced a perfectly good change ("can't verify"). With the
		// diff in the prompt the agent ALWAYS sees the actual change. (File is still written for
		// agents that prefer it.)
		//
		// EXCEPT past maxTotalBriefBytes (checked against the TOTAL brief so far, not the
		// diff alone — see the constant's doc): inlining then would make the agent invocation
		// blow the OS's per-argument exec limit and fail to launch on EVERY attempt. Fall back
		// to a forceful file-read instruction instead — a review that requires reading a file
		// beats one that can never even start.
		if d := strings.TrimSpace(c.Diff); d != "" && b.Len()+len(d) <= maxTotalBriefBytes {
			b.WriteString("## The change to review (full unified diff)\n\n" +
				"Review EXACTLY this diff — it is reproduced IN FULL below; you do not need to open any file:\n\n")
			b.WriteString("```diff\n")
			b.WriteString(d)
			b.WriteString("\n```\n\n")
		} else if d != "" {
			fmt.Fprintf(&b, "## The change to review (full unified diff)\n\n"+
				"This diff is %d bytes — too large to embed inline safely (it would blow the OS's "+
				"exec argument-length limit and make this review agent invocation fail to even "+
				"launch). The FULL unified diff is written to $FLOWBEE_DIFF_FILE (.flowbee/diff.patch) "+
				"in this working directory. You MUST open and read that file IN FULL before judging — "+
				"do not withhold approval merely because you have not read it yet; read it first, then "+
				"judge exactly as you would a diff shown inline.\n\n", len(d))
		} else {
			b.WriteString("The change to review is the unified diff at $FLOWBEE_DIFF_FILE (.flowbee/diff.patch).\n\n")
		}
		// Decision framing. For an EPIC PR the generic diff-only carve-outs below are the
		// WRONG posture (review M2): the epic lane's trust model requires executing the
		// code, and this Decision block renders LAST, so a diff-only framing here would
		// silently un-supersede the RUN-THE-CODE instruction the Epic Contract section
		// wrote earlier. Emit an epic-specific Decision that OVERRIDES the generic one when
		// epic criteria are present (until Phase 8's verify_evidence re-execution lands,
		// this running reviewer is the only layer catching fabricated-but-structurally-
		// complete evidence).
		if strings.TrimSpace(c.EpicCriteria) != "" {
			b.WriteString("**Decision (EPIC PR — this OVERRIDES the diff-only guidance above):** You MUST build the code " +
				"and run the epic's `Validate:` commands (listed in the Epic Contract) at the PR head BEFORE deciding. " +
				"Return `approved` ONLY if you actually built and ran them, they pass, and the diff honors the epic's " +
				"contract. Return `changes_requested` for any failure you OBSERVED by running — a failing `Validate:`, a " +
				"rigged or removed test, a step claimed `[x]` but not backed by the diff, or a violated Constraint — and " +
				"name it in notes. The diff-only carve-outs above (\"do not bounce for things you could not confirm " +
				"without the source/tests\") DO NOT apply to an epic PR: confirming by RUNNING is the job. Only if you " +
				"genuinely cannot obtain the source to run it here, say so explicitly in notes rather than approving.\n\n")
		} else {
			b.WriteString("**Decision:** `approved` if the diff is a correct, safe implementation of the task with no " +
				"blocking defect you can identify FROM THE DIFF. Use `changes_requested` ONLY for a CONCRETE blocking " +
				"problem visible in the diff — a real bug, a security issue, a missing acceptance criterion, or a clearly " +
				"wrong approach — and name it specifically in notes. Do NOT bounce for style nits, speculative concerns, " +
				"or things you simply could not confirm without the source/tests; those are not blocking.\n\n")
		}
		b.WriteString("**Output:** write JSON to $FLOWBEE_VERDICT_FILE:\n" +
			"```json\n{\"decision\":\"approved|changes_requested\",\"disposition\":\"self_merge\",\"notes\":\"...\"}\n```\n")
	}
	return b.String()
}

// writeEpicCriteria injects the epic-lane Phase 3 criteria-driven review section
// (task brief point 3) for a code_reviewer job the control plane detected as an epic
// PR (internal/api/server.go's leaseGrantForJob sets c.EpicCriteria/c.EpicChecklist;
// both are empty for a non-epic-PR review, so this is a complete no-op then — the
// task brief's required "zero behavior change" for the common case).
//
// It participates in the SAME maxTotalBriefBytes cap accounting as the diff (b.Len()
// tracks everything written so far): if the FULL section (criteria + claimed
// checklist) fits, both embed whole. If not, the CHECKLIST truncates first (it is
// what scales with a running agent's evidence verbosity), and — review F5 — the
// CRITERIA is bounded too: it is only "fixed" per epic, not fixed in SIZE (a
// pathological Steps list can alone exceed the cap), so past its own budget it also
// truncates with a note rather than blowing the argv limit unconditionally. Every
// cut is rune-boundary-safe (truncateRuneSafe) so a multi-byte character is never
// split into invalid UTF-8 mid-brief.
// epicTrailingReserve reserves headroom for the FIXED boilerplate renderReviewBrief
// always writes AFTER this section regardless of size (the "**Decision:**"/"**Output:**"
// instructions, and — when the diff itself doesn't fit inline — its forceful
// $FLOWBEE_DIFF_FILE fallback text): those additions are unconditional (not
// budget-gated the way the diff/checklist bodies are), so this section's own budget
// math must leave room for them rather than filling the cap exactly and letting the
// unconditional tail push the TOTAL brief past maxTotalBriefBytes.
const epicTrailingReserve = 2 * 1024

func writeEpicCriteria(b *strings.Builder, c *client.LeaseContext) {
	criteria := strings.TrimSpace(c.EpicCriteria)
	if criteria == "" {
		return
	}
	header := "## Epic Contract — judge this PR AGAINST ITS OWN SPEC, not as a generic diff\n\n"
	checklistHeader := "### Claimed status (as of this PR's head — VERIFY, don't trust)\n\n"
	const critTruncNote = "\n\n_...criteria TRUNCATED to fit the brief size cap — the epic file at the PR head " +
		"carries the full contract; steps not shown are still binding._\n\n"
	const checklistTruncNote = "\n\n_...checklist TRUNCATED to fit the brief size cap — treat any step not shown above " +
		"as UNVERIFIED, not as passing; judge primarily from the diff._\n\n"
	const checklistOmitNote = "_(checklist omitted entirely — no room left in the brief size cap; judge from the diff alone.)_\n\n"

	full := header + criteria + "\n\n" + checklistHeader + c.EpicChecklist + "\n\n"
	if b.Len()+len(full)+epicTrailingReserve <= maxTotalBriefBytes {
		b.WriteString(full)
		return
	}

	// won't fit whole: bound the criteria to its own budget (review F5 — leaving
	// room for at least the checklist section's header + omit note, so the criteria
	// can never squeeze the claimed-status section out entirely unannounced), then
	// give the checklist whatever remains.
	b.WriteString(header)
	critBudget := maxTotalBriefBytes - b.Len() - epicTrailingReserve -
		len(critTruncNote) - len(checklistHeader) - len(checklistOmitNote)
	if len(criteria) > critBudget {
		b.WriteString(truncateRuneSafe(criteria, critBudget))
		b.WriteString(critTruncNote)
	} else {
		b.WriteString(criteria)
		b.WriteString("\n\n")
	}
	budget := maxTotalBriefBytes - b.Len() - len(checklistHeader) - len(checklistTruncNote) - epicTrailingReserve
	if budget <= 0 || strings.TrimSpace(c.EpicChecklist) == "" {
		b.WriteString(checklistHeader)
		b.WriteString(checklistOmitNote)
		return
	}
	if checklist := c.EpicChecklist; len(checklist) > budget {
		b.WriteString(checklistHeader)
		b.WriteString(truncateRuneSafe(checklist, budget))
		b.WriteString(checklistTruncNote)
	} else {
		b.WriteString(checklistHeader)
		b.WriteString(checklist)
		b.WriteString("\n\n")
	}
}

// truncateRuneSafe cuts s to at most max BYTES without ever splitting a multi-byte
// UTF-8 rune (review F5: a naive s[:max] can land mid-rune — e.g. inside the em
// dashes the epic checklist format itself uses — leaving invalid UTF-8 in the
// rendered brief). It backs the cut up to the nearest rune start; max <= 0 returns "".
func truncateRuneSafe(s string, max int) string {
	if max <= 0 {
		return ""
	}
	if len(s) <= max {
		return s
	}
	cut := max
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}
