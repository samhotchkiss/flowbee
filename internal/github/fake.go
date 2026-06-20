package github

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// Fake is an in-memory, scriptable Client (BUILD.md §6.4). It records every call
// (for dedupe/idempotency assertions) and lets a test script the board the sweep
// returns — driving supersession, CI transitions, the merged terminal fact. No
// real GitHub: ALL reconcile-IN tests from M6 on run against this.
//
// It is safe for concurrent use (the control plane sweeps from one goroutine but
// tests may drive it from several).
type Fake struct {
	mu          sync.Mutex
	prs         map[int]PullRequest
	boardIssues map[int]Issue // open issues the BoardSweep returns (F7 direct-to-GitHub issues)
	rate        RateLimit
	branchCI    CIState  // scripted integration-branch CI rollup (green-main signal); "" => SUCCESS
	calls       []string // "BoardSweep" / "PullRequest(N)" / "OpenPR" / ... in order, for assertions

	// project-OUT write state (§8.2).
	nextPR      int
	nextIssue   int
	issues      map[int]CreateIssueInput
	comments    map[int][]string // issue/PR number -> comment bodies in order (reviewer findings, §F)
	labels      map[int][]string
	checks      []string     // "name@sha=conclusion"
	enqueued    []int        // PR numbers enqueued to the merge queue
	conflictPRs map[int]bool // PRs whose merge returns ErrMergeConflict (set via SetMergeConflict)

	baseModifiedPRs map[int]bool      // PRs whose merge returns ErrMergeBaseModified (retryable)
	headMovedPRs    map[int]string    // PRs whose live head != this SHA -> merge returns ErrMergeHeadModified
	mergeHeads      map[int]string    // expectedHead passed to EnqueueMergeQueue, for pin assertions
	drafted         []int             // PR numbers converted back to draft (compensation, §6.5.4)
	deletedBranches []string          // branches deleted post-merge (cleanup)
	cancelled       []string          // SHAs whose CI was cancelled (compensation, §6.5.4)
	putFiles        map[string][]byte // path -> last content written via PutFile (§F archive)
	protection      map[string]Protection

	mergeableStates map[int]string // scripted PR mergeable_state ("behind"/"clean"/…) for the #214 un-stick driver
	updatedBranches []int          // PR numbers passed to UpdateBranch, for assertions

	// retryAfter, when >0, makes the NEXT write return *ErrRetryAfter (§8.2.4),
	// then resets — so a test can prove the sender parks the outbox and retries.
	retryAfter time.Duration
	// nextErr, when set, makes the NEXT write return it (then resets) — so a test can
	// inject a permanent *ErrGitHub and prove the sender dead-letters the poison row.
	nextErr error
}

// NewFake builds an empty Fake with a healthy starting rate-limit budget.
func NewFake() *Fake {
	return &Fake{
		prs:         map[int]PullRequest{},
		boardIssues: map[int]Issue{},
		rate:        RateLimit{Limit: 5000, Remaining: 5000},
		nextPR:      1000,
		nextIssue:   2000,
		issues:      map[int]CreateIssueInput{},
		comments:    map[int][]string{},
		labels:      map[int][]string{},
		protection:  map[string]Protection{},

		mergeableStates: map[int]string{},
	}
}

// SetMergeableState scripts a PR's REST mergeable_state ("behind"/"clean"/"unknown"/…) for
// the #214 un-stick driver tests.
func (f *Fake) SetMergeableState(number int, state string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.mergeableStates[number] = state
}

// PullMergeableState implements MergeUnsticker: a scripted state wins; else a known PR reads
// "clean"; else not found.
func (f *Fake) PullMergeableState(ctx context.Context, number int) (string, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, fmt.Sprintf("PullMergeableState(%d)", number))
	if s, ok := f.mergeableStates[number]; ok {
		return s, true, nil
	}
	if _, ok := f.prs[number]; ok {
		return "clean", true, nil
	}
	return "", false, nil
}

// UpdateBranch implements MergeUnsticker: records the call, and (unless nextErr is set, e.g. a
// scripted 422 conflict) marks the PR no-longer-behind, mirroring GitHub's server-side FF.
func (f *Fake) UpdateBranch(ctx context.Context, number int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, fmt.Sprintf("UpdateBranch(%d)", number))
	if f.nextErr != nil {
		err := f.nextErr
		f.nextErr = nil
		return err
	}
	f.updatedBranches = append(f.updatedBranches, number)
	f.mergeableStates[number] = "clean"
	return nil
}

// UpdatedBranches returns the PR numbers UpdateBranch was called with, in order.
func (f *Fake) UpdatedBranches() []int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]int(nil), f.updatedBranches...)
}

// SetPR scripts (or replaces) one PR in the fake's board. A later call with the
// same number — e.g. a new HeadRefOid, a flipped CIRollup, or Merged=true — is how
// a test drives a CI transition / SHA move / terminal merge through the sweep.
func (f *Fake) SetPR(pr PullRequest) {
	f.mu.Lock()
	defer f.mu.Unlock()
	// A scripted SUCCESS rollup means "CI really passed" in a test; default the real-success flag
	// so existing green-CI tests need no change now the real client also requires a real (non-
	// skipped) passing check. The all-skipped bypass is unit-tested against the real parser
	// (TestBoardSweepRejectsAllSkippedCI); the reconcile gate ANDs this flag into CIGreen.
	if pr.CIRollup == CISuccess {
		pr.CIHasRealSuccess = true
	}
	f.prs[pr.Number] = pr
}

// SetIssue scripts (or replaces) one OPEN issue in the fake's board — the
// direct-to-GitHub issue the F7 adopt sweep imports mirrored-quiescent (a
// flowbee:adopt label opts it in to a single-issue flow at issue-review).
func (f *Fake) SetIssue(iss Issue) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.boardIssues[iss.Number] = iss
}

// SetRateLimit scripts the budget gauge the sweep self-meters (I-14).
func (f *Fake) SetRateLimit(r RateLimit) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rate = r
}

// Calls returns the recorded call log (a copy) for dedupe/idempotency assertions.
func (f *Fake) Calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

// BoardSweep returns the scripted snapshot and records the call.
// BranchCIState returns the scripted integration-branch CI rollup (defaults to SUCCESS so
// existing tests see a green main and bounce as before).
func (f *Fake) BranchCIState(ctx context.Context, branch string) (CIState, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.branchCI == "" {
		return CISuccess, nil
	}
	return f.branchCI, nil
}

// SetBranchCI scripts the integration-branch CI rollup for the green-main tests.
func (f *Fake) SetBranchCI(s CIState) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.branchCI = s
}

func (f *Fake) BoardSweep(ctx context.Context) (BoardSnapshot, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "BoardSweep")
	var snap BoardSnapshot
	for _, pr := range f.prs {
		snap.PullRequests = append(snap.PullRequests, pr)
	}
	for _, iss := range f.boardIssues {
		snap.Issues = append(snap.Issues, iss)
	}
	snap.RateLimit = f.rate
	return snap, nil
}

// PullRequest returns one scripted PR (the targeted refetch) and records the call.
// The returned fact is the REAL scripted state — so a forged webhook claiming a
// PR is approved/merged, when the script says otherwise, refetches the true state
// and can never fast-track (§8.1.3, I-3).
func (f *Fake) PullRequest(ctx context.Context, number int) (PullRequest, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, sprintfPR(number))
	pr, ok := f.prs[number]
	return pr, ok, nil
}

// ── project-OUT scripting helpers ──

// SetBranchProtection scripts the server-side protection on a branch (I-8, §9.6).
func (f *Fake) SetBranchProtection(branch string, p Protection) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.protection[branch] = p
}

// FailNextWriteWithRetryAfter makes the next write return *ErrRetryAfter (§8.2.4).
func (f *Fake) FailNextWriteWithRetryAfter(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.retryAfter = d
}

// PRState returns a scripted PR (for assertions about an opened/labeled PR).
func (f *Fake) PRState(number int) (PullRequest, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	pr, ok := f.prs[number]
	return pr, ok
}

// Labels returns the labels SET on a PR/issue (a copy).
func (f *Fake) Labels(number int) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.labels[number]...)
}

// Enqueued returns the PR numbers enqueued to the merge queue (a copy).
func (f *Fake) Enqueued() []int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]int(nil), f.enqueued...)
}

// Issues returns the materialized issues (a copy).
func (f *Fake) Issues() map[int]CreateIssueInput {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make(map[int]CreateIssueInput, len(f.issues))
	for k, v := range f.issues {
		out[k] = v
	}
	return out
}

// FailNextWriteWith makes the next write return err (then resets) — for injecting a
// permanent *ErrGitHub to prove the sender dead-letters the poison row.
func (f *Fake) FailNextWriteWith(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextErr = err
}

// retryGate returns an armed injected error once, if any. Caller holds f.mu.
func (f *Fake) retryGate() error {
	if f.nextErr != nil {
		e := f.nextErr
		f.nextErr = nil
		return e
	}
	if f.retryAfter > 0 {
		d := f.retryAfter
		f.retryAfter = 0
		return &ErrRetryAfter{RetryAfter: d}
	}
	return nil
}

// ── Writer implementation ──

func (f *Fake) OpenPR(ctx context.Context, in OpenPRInput) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "OpenPR")
	if err := f.retryGate(); err != nil {
		return 0, err
	}
	f.nextPR++
	n := f.nextPR
	f.prs[n] = PullRequest{
		Number: n, IsDraft: in.Draft, HeadRefOid: in.HeadRef, BaseRefOid: in.BaseRef,
		CIRollup: CIPending, UpdatedAt: time.Unix(0, 0),
	}
	return n, nil
}

func (f *Fake) CreateIssue(ctx context.Context, in CreateIssueInput) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "CreateIssue")
	if err := f.retryGate(); err != nil {
		return 0, err
	}
	f.nextIssue++
	n := f.nextIssue
	f.issues[n] = in
	f.labels[n] = append([]string(nil), in.Labels...)
	return n, nil
}

func (f *Fake) IssueComment(ctx context.Context, number int, body string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, fmt.Sprintf("IssueComment(%d)", number))
	if err := f.retryGate(); err != nil {
		return err
	}
	f.comments[number] = append(f.comments[number], body)
	return nil
}

// Comments returns the comment bodies posted to an issue/PR number, in order (a
// copy) — the §F reviewer-findings record assertions read this.
func (f *Fake) Comments(number int) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.comments[number]...)
}

func (f *Fake) SetLabels(ctx context.Context, number int, labels []string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, fmt.Sprintf("SetLabels(%d)", number))
	if err := f.retryGate(); err != nil {
		return err
	}
	f.labels[number] = append([]string(nil), labels...)
	if pr, ok := f.prs[number]; ok {
		pr.Labels = append([]string(nil), labels...)
		f.prs[number] = pr
	}
	return nil
}

func (f *Fake) CreateCheck(ctx context.Context, sha, name, conclusion string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, fmt.Sprintf("CreateCheck(%s)", name))
	if err := f.retryGate(); err != nil {
		return err
	}
	f.checks = append(f.checks, fmt.Sprintf("%s@%s=%s", name, sha, conclusion))
	return nil
}

func (f *Fake) EnqueueMergeQueue(ctx context.Context, number int, expectedHead string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, fmt.Sprintf("EnqueueMergeQueue(%d)", number))
	if f.mergeHeads == nil {
		f.mergeHeads = map[int]string{}
	}
	f.mergeHeads[number] = expectedHead
	if err := f.retryGate(); err != nil {
		return err
	}
	// SHA interlock: if the test declared the PR's live head moved to something other
	// than the pinned expectedHead, GitHub would 409 — model that here.
	if live, ok := f.headMovedPRs[number]; ok && expectedHead != "" && expectedHead != live {
		return fmt.Errorf("merge %d: %w", number, ErrMergeHeadModified)
	}
	if f.conflictPRs[number] {
		return fmt.Errorf("merge %d: %w", number, ErrMergeConflict)
	}
	if f.baseModifiedPRs[number] {
		return fmt.Errorf("merge %d: %w", number, ErrMergeBaseModified)
	}
	f.enqueued = append(f.enqueued, number)
	return nil
}

// SetHeadMoved makes PR `number`'s live head `live` so a merge pinned to any other
// expectedHead returns ErrMergeHeadModified (the approve-then-push race).
func (f *Fake) SetHeadMoved(number int, live string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.headMovedPRs == nil {
		f.headMovedPRs = map[int]string{}
	}
	f.headMovedPRs[number] = live
}

// MergeHead returns the expectedHead the sender pinned on PR `number`'s merge call.
func (f *Fake) MergeHead(number int) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.mergeHeads[number]
}

// SetMergeBaseModified makes EnqueueMergeQueue return the retryable ErrMergeBaseModified
// for a PR — GitHub's 405 "Base branch was modified" when a sibling merged first.
func (f *Fake) SetMergeBaseModified(number int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.baseModifiedPRs == nil {
		f.baseModifiedPRs = map[int]bool{}
	}
	f.baseModifiedPRs[number] = true
}

// SetMergeConflict makes EnqueueMergeQueue return ErrMergeConflict for a PR — the
// GitHub-405 "not mergeable" a racing sibling merge produces.
func (f *Fake) SetMergeConflict(number int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.conflictPRs == nil {
		f.conflictPRs = map[int]bool{}
	}
	f.conflictPRs[number] = true
}

func (f *Fake) ConvertToDraft(ctx context.Context, number int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, fmt.Sprintf("ConvertToDraft(%d)", number))
	if err := f.retryGate(); err != nil {
		return err
	}
	f.drafted = append(f.drafted, number)
	if pr, ok := f.prs[number]; ok {
		pr.IsDraft = true
		f.prs[number] = pr
	}
	return nil
}

func (f *Fake) CancelCI(ctx context.Context, sha string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, fmt.Sprintf("CancelCI(%s)", sha))
	f.cancelled = append(f.cancelled, sha)
	return nil
}

func (f *Fake) DeleteBranch(ctx context.Context, branch string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, fmt.Sprintf("DeleteBranch(%s)", branch))
	if err := f.retryGate(); err != nil {
		return err
	}
	f.deletedBranches = append(f.deletedBranches, branch)
	return nil
}

func (f *Fake) PutFile(ctx context.Context, path string, content []byte, message, branch string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, fmt.Sprintf("PutFile(%s@%s)", path, branch))
	if err := f.retryGate(); err != nil {
		return err
	}
	if f.putFiles == nil {
		f.putFiles = map[string][]byte{}
	}
	f.putFiles[path] = append([]byte(nil), content...)
	return nil
}

// WrittenFiles returns the path->content map written via PutFile/PutFiles (a copy), for
// assertions.
func (f *Fake) WrittenFiles() map[string][]byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make(map[string][]byte, len(f.putFiles))
	for k, v := range f.putFiles {
		out[k] = append([]byte(nil), v...)
	}
	return out
}

// PutFiles records a multi-file commit (one fake call), applying every file — the Fake's
// model of the Git Data API batched commit.
func (f *Fake) PutFiles(ctx context.Context, files map[string][]byte, message, branch string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, fmt.Sprintf("PutFiles(%d@%s)", len(files), branch))
	if err := f.retryGate(); err != nil {
		return err
	}
	if f.putFiles == nil {
		f.putFiles = map[string][]byte{}
	}
	for path, content := range files {
		f.putFiles[path] = append([]byte(nil), content...)
	}
	return nil
}

// DeletedBranches returns the branches deleted (post-merge cleanup assertions).
func (f *Fake) DeletedBranches() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.deletedBranches...)
}

// Drafted returns the PR numbers converted back to draft (compensation assertions).
func (f *Fake) Drafted() []int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]int(nil), f.drafted...)
}

// Cancelled returns the SHAs whose CI was cancelled (compensation assertions).
func (f *Fake) Cancelled() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.cancelled...)
}

func (f *Fake) BranchProtection(ctx context.Context, branch string) (Protection, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, fmt.Sprintf("BranchProtection(%s)", branch))
	p, ok := f.protection[branch]
	return p, ok, nil
}

func sprintfPR(n int) string {
	// tiny local helper to avoid importing fmt just for the call log.
	const digits = "0123456789"
	if n == 0 {
		return "PullRequest(0)"
	}
	var b []byte
	neg := n < 0
	if neg {
		n = -n
	}
	for n > 0 {
		b = append([]byte{digits[n%10]}, b...)
		n /= 10
	}
	if neg {
		b = append([]byte{'-'}, b...)
	}
	return "PullRequest(" + string(b) + ")"
}
