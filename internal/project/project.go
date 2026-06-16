// Package project is the project-OUT loop (DESIGN §3.3, §8.2): the ONLY writer to
// GitHub (R4). It drains the transactional outbox through a SINGLE serialized
// sender — ≤1 in-flight GitHub mutation at a time (§8.2.4) — under the single
// installation identity. Every action is keyed (job_id, action, head_sha) for
// idempotent dedupe; a drained row writes exactly one audit-log entry keyed the
// same way (§3.3). It honors Retry-After by parking the WHOLE outbox.
//
// It suppresses every action on an ADOPT-quiescent job (the §8.2.3 exception,
// I-16): a human's label on a quiescent job is not drift, so Flowbee never
// reasserts a rendering over human-owned in-flight work.
//
// It is NOT a deterministic-core package (it does network I/O via the GitHub
// Writer and reads a clock); archcheck forbids the core from importing it.
package project

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	gh "github.com/samhotchkiss/flowbee/internal/github"
	"github.com/samhotchkiss/flowbee/internal/gitops"
	"github.com/samhotchkiss/flowbee/internal/job"
	"github.com/samhotchkiss/flowbee/internal/store"
)

// Clock is the injected clock (DESIGN: Flowbee is the sole clock).
type Clock interface{ Now() time.Time }

// Publisher surfaces a project-OUT action live on the SSE feed (optional).
type Publisher interface {
	PublishReconcile(jobID, event string)
}

// Sender drains the outbox to GitHub. There is exactly ONE sender (§8.2.4), so
// the read-send-mark loop needs no cross-sender locking.
type Sender struct {
	store *store.Store
	gh    gh.Writer
	clock Clock
	pub   Publisher

	// repo is the F9 repo-scope handle this sender drains for (a repos.id). Empty is
	// the legacy single-repo scope (drains all rows). One control plane runs one
	// Sender per repo, each over the repo's own github.Writer, so a sender only
	// renders side-effects for its own repo's jobs (build-list F9).
	repo string
	// baseBranch is the repo's integration branch (the PR base when an OpenPR payload
	// omits one). Empty defaults to "main".
	baseBranch string

	// parkedUntil is the Retry-After park horizon (§8.2.4): while now < it, the
	// WHOLE outbox is parked. Single-sender, so a plain field is safe (Drain is
	// not called concurrently with itself).
	parkedUntil time.Time

	// history is the LOCAL-git writer for the issue-archive projection (build-list
	// §F). The history.write action is a dedicated post-merge git commit (Flowbee
	// the sole writer), NOT a GitHub mutation — so it goes through this writer, not
	// gh. Optional: when nil, a history.write row is dropped to sent as a no-op (the
	// ledger remains canonical; the markdown is simply not materialized). historyBranch
	// is the branch the dedicated commit lands on (default "main").
	history       HistoryWriter
	historyBranch string
}

// HistoryWriter commits the issue-archive markdown projection as a dedicated commit
// (build-list §F). Satisfied by *gitops.Mirror. Kept as an interface so the Sender
// neither hard-depends on gitops nor reaches GitHub for an archive write.
type HistoryWriter interface {
	CommitHistory(branch, message string, files []gitops.HistoryFile) (sha string, ok bool, err error)
	// HeadSHA resolves a ref (e.g. refs/heads/main) to its commit SHA, so the
	// signed_off_issue -> build seeding can bind the build to the current main tip.
	HeadSHA(ref string) (string, error)
}

// WithHistory wires the local-git history writer + the branch its dedicated
// post-merge commits land on (build-list §F). Returns the Sender for chaining. With
// no writer wired, history.write rows drain as audited no-ops.
func (s *Sender) WithHistory(w HistoryWriter, branch string) *Sender {
	s.history = w
	s.historyBranch = branch
	return s
}

// New builds a Sender over a github.Writer for the legacy single-repo scope.
func New(st *store.Store, w gh.Writer, clk Clock, pub Publisher) *Sender {
	return &Sender{store: st, gh: w, clock: clk, pub: pub}
}

// NewForRepo builds a Sender bound to a specific F9 repo scope (a repos.id): it
// drains ONLY that repo's outbox rows, over that repo's own github.Writer, and
// opens PRs against baseBranch by default. One control plane holds one Sender per
// managed repo.
func NewForRepo(repo, baseBranch string, st *store.Store, w gh.Writer, clk Clock, pub Publisher) *Sender {
	return &Sender{store: st, gh: w, clock: clk, pub: pub, repo: repo, baseBranch: baseBranch}
}

// Repo returns the repo-scope handle this sender is bound to ("" = legacy).
func (s *Sender) Repo() string { return s.repo }

// DrainOnce drains every currently-pending outbox row, oldest first, ≤1 in-flight
// (§8.2.4). It stops early if a Retry-After parks the outbox or a send errors
// transiently (the row stays pending for the next drain). It returns the number of
// rows successfully sent. The drain is the project-OUT tick the runtime calls on a
// timer + after each state change.
func (s *Sender) DrainOnce(ctx context.Context) (int, error) {
	if now := s.clock.Now(); now.Before(s.parkedUntil) {
		return 0, nil // the whole outbox is parked (§8.2.4)
	}
	sent := 0
	for {
		var (
			row store.OutboxRow
			ok  bool
			err error
		)
		if s.repo != "" {
			// F9: a repo-scoped sender drains ONLY its own repo's rows.
			row, ok, err = s.store.NextPendingOutboxForRepo(ctx, s.repo)
		} else {
			row, ok, err = s.store.NextPendingOutbox(ctx)
		}
		if err != nil {
			return sent, err
		}
		if !ok {
			return sent, nil
		}
		// §8.2.3 / I-16: suppress every action on an adopted-quiescent job. Mark the
		// row abandoned so it never renders over human-owned in-flight work.
		quiescent, qerr := s.store.IsQuiescent(ctx, row.JobID)
		if qerr == nil && quiescent {
			// abandon WITHOUT auditing: a suppressed action never happened (§8.2.3).
			if err := s.store.MarkOutboxSuppressed(ctx, row.ID); err != nil {
				return sent, err
			}
			continue
		}

		detail, err := s.send(ctx, row)
		if err != nil {
			var ra *gh.ErrRetryAfter
			if errors.As(err, &ra) {
				// authoritative secondary limit: park the WHOLE outbox (§8.2.4) and
				// leave the row pending for the next drain after the park expires.
				s.parkedUntil = s.clock.Now().Add(ra.RetryAfter)
				_ = s.store.BumpOutboxAttempts(ctx, row.ID)
				return sent, nil
			}
			// a transient error: bump attempts, leave pending, stop this drain.
			_ = s.store.BumpOutboxAttempts(ctx, row.ID)
			return sent, err
		}
		if err := s.store.MarkOutboxSent(ctx, row.ID, detail); err != nil {
			return sent, err
		}
		if s.pub != nil {
			s.pub.PublishReconcile(row.JobID, "project_out:"+row.Action)
		}
		sent++
	}
}

// send performs the single outbound GitHub mutation for one outbox row and
// returns an audit detail string. The PR/issue-number-returning actions stamp the
// returned number back onto the job (Flowbee opens the PR and stamps #, §7.3).
func (s *Sender) send(ctx context.Context, row store.OutboxRow) (string, error) {
	var p map[string]any
	_ = json.Unmarshal([]byte(row.Payload), &p)
	now := s.clock.Now()

	switch row.Action {
	case store.ActionOpenPR:
		// the head branch was published to GitHub by the control plane (result
		// handler) under the deterministic name store.PRBranch(jobID); the payload's
		// epoch ref is not a GitHub branch, so reference the published branch here.
		j, _ := s.store.GetJob(ctx, row.JobID)
		number, err := s.gh.OpenPR(ctx, gh.OpenPRInput{
			Title:   prTitle(j, row.JobID),
			Body:    s.prBody(ctx, j),
			HeadRef: store.IssueBranch(s.resolveIssueNum(ctx, j), row.JobID),
			BaseRef: orDefault(str(p, "base_ref"), orDefault(s.baseBranch, "main")),
			Draft:   false,
		})
		if err != nil {
			return "", err
		}
		// seed facts with the base SHA the build was cut from (j.BaseSHA), NOT the PR
		// base_ref name ("main") — reconcile compares facts.base_sha to the PR's base
		// OID, so a ref name there reads as a phantom base move and supersedes.
		if err := s.store.StampPRNumber(ctx, row.JobID, number, row.HeadSHA, j.BaseSHA, now); err != nil {
			return "", fmt.Errorf("stamp pr: %w", err)
		}
		return fmt.Sprintf("pr=%d", number), nil

	case store.ActionCreateIssue:
		// Render the issue from the signed-off spec content (build-list §B): title
		// is the spec's first heading/line, body is the spec prose + acceptance
		// criteria — not a placeholder. Flowbee is the sole author.
		j, err := s.store.GetJob(ctx, row.JobID)
		if err != nil {
			return "", fmt.Errorf("load job for issue: %w", err)
		}
		number, err := s.gh.CreateIssue(ctx, gh.CreateIssueInput{
			Title:  issueTitle(j),
			Body:   issueBody(j),
			Labels: []string{"flowbee", "flowbee:spec"},
		})
		if err != nil {
			return "", err
		}
		if err := s.store.StampIssueNumber(ctx, row.JobID, number, now); err != nil {
			return "", fmt.Errorf("stamp issue: %w", err)
		}
		// signed_off_issue -> build (build-list flows.yaml entry): the materialized
		// issue becomes a build job carrying the spec, so an eng_worker implements
		// it. Best-effort + idempotent (fixed id) so a re-drain never dupes and a
		// seed failure does NOT unwind the already-created issue.
		detail := fmt.Sprintf("issue=%d", number)
		if bid, berr := s.seedBuildFromSpec(ctx, j, now); berr != nil {
			detail += " build_seed_err=" + berr.Error()
		} else {
			detail += " build=" + bid
		}
		return detail, nil

	case store.ActionComment:
		// build-list §F: post the reviewer's findings + verdict into the originating
		// GitHub issue. Resolve the issue from the job (an adopted issue is stamped on
		// the build; a spec-flow build descends from the spec job that carries the
		// materialized issue number). No issue to comment on => audited no-op.
		j, _ := s.store.GetJob(ctx, row.JobID)
		number := s.resolveIssueNum(ctx, j)
		if number == 0 {
			return "comment:no-issue", nil
		}
		if err := s.gh.IssueComment(ctx, number, str(p, "body")); err != nil {
			return "", err
		}
		return fmt.Sprintf("comment issue=%d", number), nil

	case store.ActionSetLabels:
		// Prefer the job's stamped PR number; fall back to the payload `number` (an
		// actively-tracked ISSUE has an issue number but no PR — F7 umbrella labels).
		number, _ := s.store.JobPR(ctx, row.JobID)
		if number == 0 {
			if n, ok := p["number"].(float64); ok {
				number = int(n)
			}
		}
		labels := strSlice(p, "labels")
		if err := s.gh.SetLabels(ctx, number, labels); err != nil {
			return "", err
		}
		return fmt.Sprintf("labels=%v", labels), nil

	case store.ActionCreateCheck:
		if err := s.gh.CreateCheck(ctx, row.HeadSHA, str(p, "name"), orDefault(str(p, "conclusion"), "success")); err != nil {
			return "", err
		}
		return str(p, "name"), nil

	case store.ActionEnqueueMerge:
		number, _ := s.store.JobPR(ctx, row.JobID)
		if number == 0 {
			if n, ok := p["pr_number"].(float64); ok {
				number = int(n)
			}
		}
		if err := s.gh.EnqueueMergeQueue(ctx, number); err != nil {
			return "", err
		}
		return fmt.Sprintf("merge_enqueue pr=%d", number), nil

	case store.ActionDraftPR:
		// M11 compensation (§6.5.4): never leave a revoked zombie's PR ready-for-review.
		number, _ := s.store.JobPR(ctx, row.JobID)
		if number == 0 {
			if n, ok := p["pr_number"].(float64); ok {
				number = int(n)
			}
		}
		if number == 0 {
			return "draft:no-pr", nil // nothing was opened for the dead attempt
		}
		if err := s.gh.ConvertToDraft(ctx, number); err != nil {
			return "", err
		}
		return fmt.Sprintf("draft_back pr=%d", number), nil

	case store.ActionWriteHistory:
		// build-list §F: the dedicated post-merge issue-archive commit. Flowbee folds
		// the job's ledger into a curated card + regenerates the TOC, then commits both
		// as ONE dedicated commit on the integration branch (the sole writer; never
		// entangled with the feature PR). No GitHub call — a LOCAL git write.
		arts, err := s.store.BuildHistoryArtifacts(ctx, row.JobID)
		if err != nil {
			return "", fmt.Errorf("build history: %w", err)
		}
		if s.history == nil {
			// no writer wired: the ledger stays canonical, the markdown is just not
			// materialized. Drop to sent (audited) so the queue never wedges.
			return fmt.Sprintf("history:noop files=%d", len(arts)), nil
		}
		files := make([]gitops.HistoryFile, 0, len(arts))
		for _, a := range arts {
			files = append(files, gitops.HistoryFile{Path: a.Path, Content: a.Content})
		}
		sha, ok, err := s.history.CommitHistory(orDefault(s.historyBranch, "main"),
			fmt.Sprintf("flowbee: archive history for %s", row.JobID), files)
		if err != nil {
			return "", fmt.Errorf("commit history: %w", err)
		}
		if !ok {
			return "history:nochange", nil
		}
		return fmt.Sprintf("history sha=%s files=%d", sha, len(files)), nil

	default:
		// an unknown action is dropped to sent (audited) so the queue never wedges.
		return "noop:" + row.Action, nil
	}
}

func str(m map[string]any, k string) string {
	if v, ok := m[k].(string); ok {
		return v
	}
	return ""
}

func strSlice(m map[string]any, k string) []string {
	v, ok := m[k].([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(v))
	for _, e := range v {
		if s, ok := e.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

// prTitle renders the PR title from the build job's task/spec (the issue title),
// falling back to the job id.
func prTitle(j job.Job, jobID string) string {
	for _, s := range []string{j.TaskText, j.SpecText} {
		if line := firstNonEmptyLine(s); line != "" {
			return strings.TrimSpace(strings.TrimLeft(line, "# "))
		}
	}
	return "flowbee: " + jobID
}

// prBody links the PR to the originating issue with a "Closes #N" so the merge
// auto-closes it, then notes Flowbee as the author of the eng_worker patch. The
// issue number lives on the spec job the build descends from (FlowID).
func (s *Sender) prBody(ctx context.Context, j job.Job) string {
	var b strings.Builder
	// link the originating issue so the merge closes it.
	issueNum := s.resolveIssueNum(ctx, j)
	if issueNum > 0 {
		fmt.Fprintf(&b, "Closes #%d\n\n", issueNum)
	}
	b.WriteString("Implements the signed-off spec.\n\n---\n")
	b.WriteString("_Opened by Flowbee from the eng_worker patch (§7.3); Flowbee performed the git write, not the worker._")
	return b.String()
}

// resolveIssueNum finds the GitHub issue a job belongs to: an adopted GitHub issue
// is stamped on the build job itself; a spec-flow build descends from the spec job
// that carries the materialized issue number (FlowID). Returns 0 when no issue is
// bound yet (e.g. a build whose issue has not been materialized).
func (s *Sender) resolveIssueNum(ctx context.Context, j job.Job) int {
	return s.store.ResolveIssueNum(ctx, j.ID)
}

// seedBuildFromSpec turns a just-materialized signed-off spec into a ready build
// job (the signed_off_issue -> build link). The build carries the spec prose as
// its task and binds to the current main tip as base_sha. Idempotent: a build
// with the derived id already present is a no-op, so a re-drain never dupes.
func (s *Sender) seedBuildFromSpec(ctx context.Context, spec job.Job, now time.Time) (string, error) {
	buildID := spec.ID + "-build"
	if _, err := s.store.GetJob(ctx, buildID); err == nil {
		return buildID, nil // already seeded
	}
	if s.history == nil {
		return "", errors.New("no mirror configured to resolve base_sha")
	}
	base, err := s.history.HeadSHA("refs/heads/" + orDefault(s.baseBranch, "main"))
	if err != nil {
		return "", fmt.Errorf("resolve base_sha: %w", err)
	}
	if _, err := s.store.SeedJob(ctx, store.SeedParams{
		ID:                 buildID,
		Kind:               job.KindBuild,
		Flow:               "build",
		Stage:              "build",
		Role:               job.RoleEngWorker,
		BaseSHA:            base,
		TaskText:           orDefault(spec.SpecText, spec.TaskText),
		SpecText:           spec.SpecText,
		AcceptanceCriteria: spec.AcceptanceCriteria,
		FlowID:             spec.ID,
		Repo:               spec.Repo,
		Now:                now,
	}); err != nil {
		return "", err
	}
	return buildID, nil
}

// issueTitle derives a human GitHub issue title from a signed-off spec job: the
// first non-empty line of the task (then spec), with a leading markdown heading
// marker stripped ("# Add X" -> "Add X"), falling back to the job id.
func issueTitle(j job.Job) string {
	for _, s := range []string{j.TaskText, j.SpecText} {
		if line := firstNonEmptyLine(s); line != "" {
			return strings.TrimSpace(strings.TrimLeft(line, "# "))
		}
	}
	return j.ID
}

// issueBody renders the issue body from the spec prose + acceptance criteria,
// with a footer marking Flowbee as the materializing author (build-list §B/§F).
func issueBody(j job.Job) string {
	var b strings.Builder
	if s := strings.TrimSpace(j.SpecText); s != "" {
		b.WriteString(s)
		b.WriteString("\n\n")
	} else if t := strings.TrimSpace(j.TaskText); t != "" {
		b.WriteString(t)
		b.WriteString("\n\n")
	}
	if ac := strings.TrimSpace(j.AcceptanceCriteria); ac != "" {
		b.WriteString("## Acceptance criteria\n\n")
		b.WriteString(ac)
		b.WriteString("\n\n")
	}
	b.WriteString("---\n")
	fmt.Fprintf(&b, "_Materialized by Flowbee from the signed-off spec (job `%s`)._", j.ID)
	return b.String()
}

// firstNonEmptyLine returns the first line of s with non-whitespace content.
func firstNonEmptyLine(s string) string {
	for _, line := range strings.Split(s, "\n") {
		if t := strings.TrimSpace(line); t != "" {
			return t
		}
	}
	return ""
}
