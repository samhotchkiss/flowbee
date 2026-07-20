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
	"log/slog"
	"strings"
	"time"

	"github.com/samhotchkiss/flowbee/internal/content"
	"github.com/samhotchkiss/flowbee/internal/epicspec"
	gh "github.com/samhotchkiss/flowbee/internal/github"
	"github.com/samhotchkiss/flowbee/internal/gitops"
	"github.com/samhotchkiss/flowbee/internal/job"
	"github.com/samhotchkiss/flowbee/internal/store"
)

// contentDenyReason re-runs the content gate's SAFETY conditions — the path denylist and
// the deterministic static checks (secret-scan, binary blob, malformed-diff) — over the
// ACTUAL branch diff, returning a non-empty reason if any fail (so an autonomous merge is
// downgraded to the human gate). Size/file-count bounds are disabled here: they are a
// resource/blast-radius policy (operator-configurable, already applied at verdict time),
// not a content-safety condition, and the Sender does not carry the operator policy. The
// blast-radius declared-vs-actual check is also omitted (a declaration-consistency tamper
// signal whose "declared" set is itself worker-reported, not a content-safety property).
// isUnreachableRev reports whether a git error is a PERMANENT "this revision/object does
// not exist" failure (as opposed to a transient network/lock error). Used so the
// autonomous-merge verify fails-open to the human gate when its HeadSHA is unreachable
// (GitHub squash-merged the PR and discarded Flowbee's promoted epoch commit) instead of
// retrying that diff forever. Matches git's stderr for a missing object / bad range.
func isUnreachableRev(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "invalid revision range") ||
		strings.Contains(s, "unknown revision") ||
		strings.Contains(s, "bad revision") ||
		strings.Contains(s, "ambiguous argument") ||
		strings.Contains(s, "bad object")
}

func contentDenyReason(actualDiff string, allowOwnSource bool) string {
	r := content.CheckWithPolicy(content.Patch{Diff: actualDiff}, content.Policy{
		Limits:         content.Limits{MaxDiffBytes: 1 << 30, MaxChangedFiles: 1 << 20},
		AllowOwnSource: allowOwnSource, // a NON-control-plane repo: its internal//cmd/ are its own
	})
	var reasons []string
	if !r.DenylistClear {
		reasons = append(reasons, "denylist:"+strings.Join(r.DenylistHits, ","))
	}
	if !r.StaticChecksPass {
		reasons = append(reasons, "static:"+strings.Join(r.StaticFailures, ","))
	}
	if len(reasons) == 0 {
		return ""
	}
	return "content gate (actual diff) " + strings.Join(reasons, "; ")
}

// epicDenyReason is the epic-lane extension of the content-integrity gate (task brief
// point 2): for an epic PR — detected via store.EpicForHeadSHA, which SHA-tip-matches
// j.HeadSHA against every epic registered for repo (GitHub gives Flowbee no fact naming
// a PR's head branch NAME, only its SHA — see that function's doc) — judge the diff
// AGAINST THE EPIC'S OWN LAUNCH-PINNED CONTRACT rather than as a generic diff: every
// pinned ## Steps entry must be checked with non-empty evidence, State: must be exactly
// "done", Blockers: must be empty, AND every touched path must fall inside the epic's
// launch-pinned scope (a REAL per-file glob match, epicspec.CheckScope — not the
// conservative launch-time overlap heuristic).
//
// review M1 — WHOSE contract: the Goal/Steps and scope: are frozen at launch and MUST
// come from the pinned source (scope from e.Scope, stored by AddEpicRun from the
// main-committed spec; Steps from the epic file at the PR BASE), because the PR HEAD is
// author-controlled — a lying agent could widen scope: to '**' or delete/renumber its
// own ## Steps at head and self-certify. Only the CLAIMED ## Status is read from head.
// A head whose ## Steps drift from the pinned ones is a spec-immutability breach and is
// denied explicitly. NB — detection is a coincidental-SHA match (headSHA == an epic
// branch tip). In the astronomically-unlikely event an ORDINARY PR's head SHA collides
// with an epic branch tip, this gate would judge it against that epic's contract and,
// at worst, route a legitimate merge to a human — the FAIL-CLOSED direction, never a
// wrongful merge; the safe way to be wrong.
//
// Returns ("", nil) — no denial — for the overwhelmingly common non-epic PR, and
// when repo has no epics in the detection window at all (zero extra mirror I/O
// then — see store.EpicForHeadSHA). Detection trouble fails CLOSED-to-RETRY
// (review F2): a mirror fetch/rev-parse error against a live epic's branch, or a
// bare I/O error reading the pinned contract at the (reachable) base, returns a
// non-nil error so the caller RETRIES the merge row — a transient mirror outage must
// never let an unevidenced epic PR merge disguised as an ordinary one. Once an epic IS
// identified: an empty base SHA, an absent/unparseable pinned contract, or an
// unreadable contract at head all fail CLOSED to HANDOFF (a deny reason, not an
// error) — a PR that provably belongs to epic <slug> but can't be contract-verified
// must reach a human, not retry forever.
func (s *Sender) epicDenyReason(ctx context.Context, repo, baseSHA, headSHA, actualDiff string) (string, error) {
	return s.epicDenyReasonFor(ctx, "", repo, baseSHA, headSHA, actualDiff)
}

func (s *Sender) epicDenyReasonFor(ctx context.Context, expectedEpicID, repo, baseSHA, headSHA, actualDiff string) (string, error) {
	if s.history == nil || headSHA == "" {
		return "", nil
	}
	e, ok, err := s.store.EpicForHeadSHA(ctx, s.history, repo, headSHA, s.clock.Now())
	if err != nil {
		return "", fmt.Errorf("epic-PR detection: %w", err)
	}
	if !ok {
		return "", nil // clean non-match across every epic in the window: an ordinary PR
	}
	if expectedEpicID != "" && e.ID != expectedEpicID {
		return fmt.Sprintf("expected epic %q but head resolves to epic %q", expectedEpicID, e.ID), nil
	}
	// review m4: an epic-detected PR with NO base SHA cannot be contract-verified (no
	// diff to scope-check, no launch-pinned ref to read). Fail CLOSED to a human rather
	// than the (base-guarded) content path silently skipping the epic gate entirely.
	if baseSHA == "" {
		return fmt.Sprintf("epic %q PR has no base SHA — cannot verify against the launch-pinned contract", e.ID), nil
	}

	// review M1 (the load-bearing fix): judge against the LAUNCH-PINNED contract, not
	// the author-controlled PR head. The Goal/Steps and the scope: globs are frozen at
	// launch (spec-immutable by contract, committed to main pre-launch); reading them
	// from head would let a lying agent widen scope: to '**' or delete/renumber its own
	// ## Steps at head and self-certify. So: pinned Steps come from the epic file at the
	// PR BASE, scope comes from the launch-stored e.Scope, and ONLY the claimed ##
	// Status is read from head.
	specPinned, _, perr := s.store.EpicContractAtRef(s.history, e, baseSHA)
	if perr != nil {
		// permanent contract problems at the pinned ref → handoff; a bare I/O error
		// against the (reachable) base is transient → retry (review M1/F2).
		if errors.Is(perr, store.ErrEpicFileAbsent) {
			return fmt.Sprintf("epic %q: launch-pinned contract %s absent at PR base — cannot verify spec immutability", e.ID, e.FilePath), nil
		}
		if errors.Is(perr, store.ErrEpicSpecUnparseable) {
			return fmt.Sprintf("epic %q: launch-pinned contract unparseable at PR base: %v", e.ID, perr), nil
		}
		return "", fmt.Errorf("epic %q pinned-contract read at base: %w", e.ID, perr)
	}
	specHead, sbHead, herr := s.store.EpicContractAtRef(s.history, e, headSHA)
	if herr != nil {
		// head is author-controlled; an absent/unparseable/unreadable contract at head
		// is a claim the author must fix — route to a human, do not retry forever.
		return fmt.Sprintf("epic %q contract unreadable at PR head: %v", e.ID, herr), nil
	}

	var reasons []string
	// spec-immutability breach (review M1c): the ## Steps the agent presents at head
	// MUST match the launch-pinned ones. A head that adds/removes/renumbers/reworries a
	// step has edited a frozen contract — deny explicitly (and the evidence check below
	// still judges against the PINNED steps regardless, so shrinking head's list can
	// never shrink what must be evidenced).
	if !stepsMatch(specPinned.Steps, specHead.Steps) {
		reasons = append(reasons, fmt.Sprintf("epic %q spec-immutability breach: ## Steps changed since launch (%d pinned vs %d at head)",
			e.ID, len(specPinned.Steps), len(specHead.Steps)))
	}
	// evidence: PINNED steps vs the CLAIMED head ## Status.
	if ev := epicspec.CheckEvidence(specPinned, sbHead); !ev.Clear {
		reasons = append(reasons, fmt.Sprintf("epic %q evidence incomplete: %s", e.ID, strings.Join(ev.Failures, "; ")))
	}
	// scope honesty (review M1a + F1): match against the LAUNCH-PINNED scope (e.Scope,
	// stored by AddEpicRun from the main-committed spec — never the head frontmatter a
	// lying agent could widen to '**'), PLUS the epic's OWN spec file AND its explainer
	// (epics/<slug>-explainer.html) implicitly. Every real epic PR touches both: the
	// contract MANDATES ## Status updates on the epic's own .md and a maintained
	// -explainer.html on its branch, so author-globs-only would fail every all-green epic
	// on its own mandatory status/explainer commits (the F1 catch-22). Only e's OWN spec
	// and explainer are implicit; a diff touching a DIFFERENT epic's file still violates.
	scope := append(append([]string{}, e.Scope...), e.FilePath, epicExplainerPath(e.FilePath))
	if out := epicspec.CheckScope(scope, content.TouchedPaths(actualDiff)); len(out) > 0 {
		reasons = append(reasons, fmt.Sprintf("epic %q scope violation: %s", e.ID, strings.Join(out, ", ")))
	}
	return strings.Join(reasons, "; "), nil
}

// MergeAuthorizationDeniedError is a deterministic product-safety denial. The
// v2 executor must park it for a human rather than retrying the same immutable
// artifact. Transport/mirror errors remain ordinary retryable errors.
type MergeAuthorizationDeniedError struct{ Reason string }

func (e *MergeAuthorizationDeniedError) Error() string { return e.Reason }

// AuthorizeEpicV2Merge reuses the production autonomous-merge safety boundary
// for a v2 delivery. It binds the action to the registered epic, verifies the
// mirror's exact branch tip, evaluates the real base..head diff and launch-pinned
// contract, then performs a final authoritative PR/required-CI refetch. No
// GitHub mutation occurs here; the caller immediately follows a nil result with
// an expected-head merge request.
func (s *Sender) AuthorizeEpicV2Merge(ctx context.Context, epicID, projectID, repo string, prNumber int, branch, baseSHA, headSHA string) error {
	deny := func(format string, args ...any) error {
		return &MergeAuthorizationDeniedError{Reason: fmt.Sprintf(format, args...)}
	}
	if s.repo != repo {
		return deny("merge repository %q is not owned by sender %q", repo, s.repo)
	}
	if s.history == nil {
		return deny("v2 autonomous merge requires a repository mirror")
	}
	if epicID == "" || prNumber <= 0 || branch == "" || baseSHA == "" || headSHA == "" {
		return deny("v2 merge action lacks exact epic, PR, branch, base, or head identity")
	}
	e, err := s.store.GetEpicRun(ctx, epicID)
	if err != nil {
		return deny("registered epic %q is unavailable: %v", epicID, err)
	}
	if e.ProjectID != projectID || e.Repo != repo || e.Branch != branch {
		return deny("merge action does not match registered epic identity")
	}
	if err := s.history.FetchBranch(branch); err != nil {
		return fmt.Errorf("authorize v2 merge: fetch %s: %w", branch, err)
	}
	tip, err := s.history.HeadSHA("refs/heads/" + branch)
	if err != nil {
		return fmt.Errorf("authorize v2 merge: resolve %s: %w", branch, err)
	}
	if tip != headSHA {
		return fmt.Errorf("%w: mirror branch moved from %s to %s", gh.ErrMergeHeadModified, headSHA, tip)
	}
	actualDiff, err := s.history.DiffBetween(baseSHA, headSHA)
	if err != nil {
		return fmt.Errorf("authorize v2 merge: diff %s..%s: %w", baseSHA, headSHA, err)
	}
	if reason := contentDenyReason(actualDiff, s.allowOwnSource); reason != "" {
		return deny("%s", reason)
	}
	if reason, err := s.epicDenyReasonFor(ctx, epicID, repo, baseSHA, headSHA, actualDiff); err != nil {
		return fmt.Errorf("authorize v2 merge: %w", err)
	} else if reason != "" {
		return deny("%s", reason)
	}

	// This is deliberately last: it is the immediate authoritative PR/base/head/
	// required-check read before the expected-head mutation.
	ci, err := s.liveMergeCI(ctx, prNumber, baseSHA, headSHA)
	if err != nil {
		var stale *staleMergeAuthorizationError
		if errors.As(err, &stale) {
			if stale.observedBase != baseSHA {
				return fmt.Errorf("%w: PR base moved from %s to %s", gh.ErrMergeBaseModified, baseSHA, stale.observedBase)
			}
			return fmt.Errorf("%w: PR head moved from %s", gh.ErrMergeHeadModified, headSHA)
		}
		return err
	}
	if ci.failed {
		return deny("authoritative CI failed: %s", strings.Join(ci.failingChecks, ", "))
	}
	if !ci.green {
		return errMergeCINotReady
	}
	return nil
}

// epicExplainerPath derives an epic's human-facing explainer path from its spec path:
// epics/<slug>.md -> epics/<slug>-explainer.html (contract §Explainer / plan §15.14). It
// is implicitly in scope alongside the spec so maintaining it never trips the gate.
func epicExplainerPath(specPath string) string {
	return strings.TrimSuffix(specPath, ".md") + "-explainer.html"
}

// stepsMatch reports whether two ## Steps lists are the same frozen contract: same
// count, and each step's number/text/validate equal after trimming (so a pure
// reformatting is tolerated, but any added/removed/renumbered/reworded step — the
// evidence-shrinking edit the immutability rule exists to catch — is not).
func stepsMatch(pinned, head []epicspec.Step) bool {
	if len(pinned) != len(head) {
		return false
	}
	for i := range pinned {
		if pinned[i].N != head[i].N ||
			strings.TrimSpace(pinned[i].Text) != strings.TrimSpace(head[i].Text) ||
			strings.TrimSpace(pinned[i].Validate) != strings.TrimSpace(head[i].Validate) {
			return false
		}
	}
	return true
}

type liveMergeCIResult struct {
	green         bool
	failed        bool
	failingChecks []string
	checkURLs     map[string]string
}

// staleMergeAuthorizationError carries the live base Flowbee observed while
// re-checking the PR. The outbox drain uses it to re-arm the build on the new
// integration head instead of preserving the now-stale reviewed base.
type staleMergeAuthorizationError struct {
	observedBase string
}

func (e *staleMergeAuthorizationError) Error() string { return errStaleMergeAuthorization.Error() }
func (e *staleMergeAuthorizationError) Unwrap() error { return errStaleMergeAuthorization }

func (s *Sender) liveMergeCI(ctx context.Context, prNumber int, expectedBase, expectedHead string) (liveMergeCIResult, error) {
	reader, ok := s.gh.(gh.Client)
	if !ok || prNumber == 0 {
		return liveMergeCIResult{}, errMergeCINotReady
	}
	pr, ok, err := reader.PullRequest(ctx, prNumber)
	if err != nil {
		return liveMergeCIResult{}, err
	}
	if !ok {
		return liveMergeCIResult{}, errMergeCINotReady
	}
	if pr.IsDraft || pr.ClosedUnmerged || pr.Merged || pr.IsCrossRepository || pr.CheckContextsTruncated {
		return liveMergeCIResult{}, errMergeCINotReady
	}
	if pr.BaseRefOid != expectedBase || pr.HeadRefOid != expectedHead {
		return liveMergeCIResult{}, &staleMergeAuthorizationError{observedBase: pr.BaseRefOid}
	}
	if ms, ok := s.gh.(gh.MergeUnsticker); ok {
		state, found, err := ms.PullMergeableState(ctx, prNumber)
		if err != nil {
			return liveMergeCIResult{}, err
		}
		if !found || strings.EqualFold(state, "unknown") {
			return liveMergeCIResult{}, errMergeCINotReady
		}
	}
	required, err := s.requiredChecks(ctx)
	if err != nil {
		return liveMergeCIResult{}, err
	}
	if len(required) > 0 {
		// #4165 hardening: a repo ruleset can mark only a THIN check required (russ
		// requires just "Migration version guard"), so gating solely on required checks
		// let a red SUBSTANTIVE non-required shard (e.g. a backend test split) self-merge
		// while its failure was ignored. Block the merge on any failing check that is not
		// a known-cosmetic gate (lint/format/style) in addition to the required set, while
		// still tolerating cosmetic non-required regressions so the board does not freeze
		// on a lint that main itself already carries. Default-deny: an unrecognized failing
		// check is treated as substantive and blocks.
		blocking := unionNames(intersectNames(pr.FailingChecks, required), substantiveFailures(pr.FailingChecks))
		return liveMergeCIResult{
			green:         pr.CIHasRealSuccess && checksContainAll(pr.PassedChecks, required) && len(blocking) == 0,
			failed:        len(blocking) > 0,
			failingChecks: blocking,
			checkURLs:     filterCheckURLs(pr.FailingCheckURLs, blocking),
		}, nil
	}
	return liveMergeCIResult{
		green:         pr.CIHasRealSuccess && pr.CIRollup == gh.CISuccess,
		failed:        pr.CIRollup == gh.CIFailure || pr.CIRollup == gh.CIError,
		failingChecks: append([]string(nil), pr.FailingChecks...),
		checkURLs:     cloneCheckURLs(pr.FailingCheckURLs),
	}, nil
}

func (s *Sender) requiredChecks(ctx context.Context) ([]string, error) {
	branch := orDefault(s.baseBranch, "main")
	if rr, ok := s.gh.(gh.RequiredChecksReader); ok {
		checks, err := rr.BranchRequiredChecks(ctx, branch)
		if err != nil {
			return nil, err
		}
		if len(checks) > 0 {
			return checks, nil
		}
	}
	if br, ok := s.gh.(gh.BranchProtectionReader); ok {
		prot, ok, err := br.BranchProtection(ctx, branch)
		if err != nil {
			return nil, err
		}
		if ok {
			return prot.RequiredChecks, nil
		}
	}
	return nil, nil
}

func checksContainAll(have, required []string) bool {
	set := make(map[string]bool, len(have))
	for _, h := range have {
		set[h] = true
	}
	for _, req := range required {
		if !set[req] {
			return false
		}
	}
	return true
}

func checksIntersect(xs, ys []string) bool {
	return len(intersectNames(xs, ys)) > 0
}

func filterCheckURLs(urls map[string]string, names []string) map[string]string {
	if len(urls) == 0 || len(names) == 0 {
		return nil
	}
	out := make(map[string]string)
	for _, name := range names {
		if url := urls[name]; url != "" {
			out[name] = url
		}
	}
	return out
}

func cloneCheckURLs(urls map[string]string) map[string]string {
	if len(urls) == 0 {
		return nil
	}
	out := make(map[string]string, len(urls))
	for name, url := range urls {
		out[name] = url
	}
	return out
}

func intersectNames(xs, ys []string) []string {
	if len(xs) == 0 || len(ys) == 0 {
		return nil
	}
	set := make(map[string]bool, len(ys))
	for _, y := range ys {
		set[y] = true
	}
	var out []string
	for _, x := range xs {
		if set[x] {
			out = append(out, x)
		}
	}
	return out
}

// unionNames returns the de-duplicated union of two check-name lists, preserving
// first-seen order (all of a, then any of b not already seen).
func unionNames(a, b []string) []string {
	if len(a) == 0 {
		return append([]string(nil), b...)
	}
	if len(b) == 0 {
		return append([]string(nil), a...)
	}
	seen := make(map[string]bool, len(a)+len(b))
	out := make([]string, 0, len(a)+len(b))
	for _, xs := range [][]string{a, b} {
		for _, x := range xs {
			if !seen[x] {
				seen[x] = true
				out = append(out, x)
			}
		}
	}
	return out
}

// cosmeticCheckPatterns are case-insensitive substrings identifying checks whose
// failure is advisory (formatters/linters/style) rather than a substantive test or
// build gate. A failing check matching one of these does NOT block an otherwise-
// approved self-merge; every other failing check does. Kept as a package var so tests
// can override and a future config can supply repo-specific patterns. Deliberately
// tight — default-deny means an unrecognized failing check counts as substantive and
// blocks, which is the safe error direction after #4165 (a red backend shard slipped
// a required-only gate because the repo ruleset marked only a thin check required).
var cosmeticCheckPatterns = []string{
	"lint", "prettier", "eslint", "stylelint", "markdownlint",
	"gofmt", "goimports", "gofumpt", "format",
}

func isCosmeticCheck(name string) bool {
	n := strings.ToLower(name)
	for _, p := range cosmeticCheckPatterns {
		if strings.Contains(n, p) {
			return true
		}
	}
	return false
}

// substantiveFailures returns the failing checks that are NOT known-cosmetic — the
// gates whose failure must block a self-merge regardless of whether the repo ruleset
// marks them required. Default-deny: an unrecognized failing check is substantive.
func substantiveFailures(failing []string) []string {
	var out []string
	for _, f := range failing {
		if !isCosmeticCheck(f) {
			out = append(out, f)
		}
	}
	return out
}

// Clock is the injected clock (DESIGN: Flowbee is the sole clock).
type Clock interface{ Now() time.Time }

// maxOutboxAttempts is the dead-letter backstop: a row that has failed to send this
// many times is treated as poison even if its error looked transient (a misclassified
// permanent error, or a multi-hour GitHub outage). Generous, because the head-of-line
// serialization means only the STUCK row accumulates attempts — a brief outage recovers
// long before this. A genuinely permanent 4xx is dead-lettered immediately, not here.
const maxOutboxAttempts = 100

// mergeMergeabilityRetries is how many times a "not mergeable" 405 is retried before it
// is treated as a REAL conflict. GitHub recomputes a PR's mergeability ASYNCHRONOUSLY
// after its base moves (a sibling just merged), and a merge attempt in that ~1–5s window
// returns "not mergeable" even when the PR has NO content conflict. Retrying (the drain
// re-attempts every few seconds) lets the transient settle, so concurrent non-conflicting
// merges (epics, high concurrency) don't spuriously invoke the conflict_resolver. A REAL
// conflict stays not-mergeable past the retries and then routes correctly.
const mergeMergeabilityRetries = 3

var errStaleMergeAuthorization = errors.New("merge authorization does not match the reviewed base/head")
var errMergeCINotReady = errors.New("merge blocked: live required checks are not terminal green")
var errMergeCIFailedRouted = errors.New("merge blocked: live required checks failed and repair was routed")

// criticalAction reports whether a permanently-failed outbox action should escalate
// its job to a human (it blocks the pipeline: no PR -> no review/merge; no merge -> no
// completion; no issue -> no spec materialization). A cosmetic action (a comment, a
// label, a check, a draft-back) is simply dropped — the pipeline proceeds without it.
func criticalAction(action string) bool {
	switch action {
	case store.ActionOpenPR, store.ActionEnqueueMerge, store.ActionCreateIssue:
		return true
	default:
		return false
	}
}

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
	// logger records dead-lettered (abandoned) GitHub writes durably in the serve log —
	// the durable complement to the flowbee_outbox_abandoned metric + the ephemeral SSE
	// feed, and the ONLY surfacing for a CRITICAL action abandoned against an already-
	// terminal job (a signed-off spec whose issue-create fails, which the needs_human
	// escalation drops). Nil => no log (legacy single-repo callers).
	logger *slog.Logger

	// repo is the F9 repo-scope handle this sender drains for (a repos.id). Empty is
	// the legacy single-repo scope (drains all rows). One control plane runs one
	// Sender per repo, each over the repo's own github.Writer, so a sender only
	// renders side-effects for its own repo's jobs (build-list F9).
	repo string
	// baseBranch is the repo's integration branch (the PR base when an OpenPR payload
	// omits one). Empty defaults to "main".
	baseBranch string

	// allowOwnSource relaxes the flowbee_source content class for THIS repo's merge
	// cross-check — set true for a managed repo that is NOT the Flowbee control plane,
	// so its own internal//cmd/ changes self-merge instead of forced handoff. Default
	// false = fully protected. MUST mirror the store's AllowOwnSourceRepos[repo] so the
	// two gate sites agree (else a job clears one and is denied at the other).
	allowOwnSource bool

	// archiveHistory opts THIS repo into the §F durable history archive: on every merge,
	// Flowbee lands docs/history/<id>.md + the regenerated TOC on the integration branch
	// via the Contents API. Per-repo opt-in (default false) because it commits to the
	// repo's main on every merge — correct for a repo whose owner wants in-repo
	// provenance, not something to impose on an arbitrary managed repo.
	archiveHistory bool

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
	// FetchBranch force-updates a local branch from origin (GitHub). The mirror lags
	// after an API merge, so the merge-conflict router must fetch main BEFORE resolving
	// the resolver's base — else the resolver builds against a stale main lacking the
	// sibling's merge, the resolution re-conflicts, and the brief is nonsensical.
	FetchBranch(branch string) error
	// DiffBetween returns the full unified diff between base and head in the mirror — the
	// CP-computed ACTUAL change, used to re-run the WHOLE content gate against the real
	// branch (not the worker's self-reported patch) before an autonomous merge lands.
	DiffBetween(base, head string) (string, error)
	// ReadFileAtRef reads a single file's bytes AS OF ref (a full ref like
	// "refs/heads/<branch>" or a raw SHA) — the epic-lane Phase 3 evidence gate's
	// read of an epic's own spec file AT THE PR HEAD (epicDenyReason), the same way
	// cmd/flowbee/epic.go's ingestEpicStatuses already reads an epic file off its
	// branch. found=false (no error) means the path does not exist at that ref.
	ReadFileAtRef(ref, path string) (string, bool, error)
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

// SetLogger wires a logger so dead-lettered GitHub writes are recorded in the serve log
// (the durable complement to the metric/SSE). Optional; nil leaves dead-letters unlogged.
func (s *Sender) SetLogger(l *slog.Logger) { s.logger = l }

// Repo returns the repo-scope handle this sender is bound to ("" = legacy).
func (s *Sender) Repo() string { return s.repo }

// SetAllowOwnSource relaxes the flowbee_source merge cross-check for this repo (a
// managed repo that is NOT the Flowbee control plane). MUST mirror the store's
// AllowOwnSourceRepos[repo] so both gate sites agree. Default false = fully protected.
func (s *Sender) SetAllowOwnSource(v bool) { s.allowOwnSource = v }

// SetArchiveHistory opts this repo into the durable §F history archive (default off).
func (s *Sender) SetArchiveHistory(v bool) { s.archiveHistory = v }

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
			if errors.Is(err, errStaleMergeAuthorization) || errors.Is(err, gh.ErrMergeHeadModified) {
				var stale *staleMergeAuthorizationError
				var observedBase string
				if errors.As(err, &stale) {
					observedBase = stale.observedBase
				}
				if ierr := s.store.InvalidateStaleMergeAuthorization(ctx, row.JobID, observedBase, s.clock.Now()); ierr != nil {
					return sent, ierr
				}
				if s.pub != nil {
					s.pub.PublishReconcile(row.JobID, "project_out:merge_head_superseded")
				}
				continue
			}
			if errors.Is(err, errMergeCINotReady) {
				return sent, nil
			}
			if errors.Is(err, errMergeCIFailedRouted) {
				continue
			}
			var ra *gh.ErrRetryAfter
			if errors.As(err, &ra) {
				// a rate-limit (primary 5000/hr, secondary/abuse, or GraphQL RATE_LIMITED):
				// park the WHOLE outbox (§8.2.4) until the window resets and leave the row
				// pending. Do NOT bump attempts — a rate-limit is a temporary outage, never
				// progress toward the poison/dead-letter threshold, so a rate-limited
				// issues.create/merge waits for the reset instead of being abandoned (russ #215).
				s.parkedUntil = s.clock.Now().Add(ra.RetryAfter)
				return sent, nil
			}
			// a merge that conflicts (a sibling merged into the same area after the
			// verdict minted) NEVER succeeds on retry. Route the job to a conflict_resolver
			// at the current main tip and CONSUME the merge row, instead of re-queuing the
			// merge forever (which also pollutes the drain for the whole repo).
			if errors.Is(err, gh.ErrMergeConflict) {
				// FIRST rule out a TRANSIENT "not mergeable": GitHub recomputes mergeability
				// async after a sibling merge, so an early merge attempt 405s with no real
				// conflict. Retry a few drains (it settles in seconds) before resolving — else
				// every near-simultaneous concurrent merge spuriously spins up the resolver.
				if row.Attempts+1 < mergeMergeabilityRetries {
					_ = s.store.BumpOutboxAttempts(ctx, row.ID)
					return sent, err // leave the merge pending; the next drain re-attempts
				}
				// the sibling merged via the GitHub API, so the local mirror's main lags —
				// fetch it FIRST so the resolver's base is the real post-merge main (with the
				// sibling's change present), or the resolution would build on a stale base and
				// re-conflict.
				mainBranch := orDefault(s.baseBranch, "main")
				// no local mirror wired (the legacy New() path, or a repo whose history
				// factory returned nil): we cannot resolve the post-merge main to rebase the
				// resolver against, so skip the route and fall through to the transient/
				// dead-letter path. Dereferencing a nil s.history here would PANIC the whole
				// single serialized sender and wedge every other GitHub write for the repo.
				if s.history != nil {
					_ = s.history.FetchBranch(mainBranch)
					mainTip, terr := s.history.HeadSHA("refs/heads/" + mainBranch)
					if terr == nil && mainTip != "" {
						if rerr := s.store.RouteMergeConflict(ctx, row.JobID, mainTip, s.clock.Now()); rerr == nil {
							if err := s.store.MarkOutboxSent(ctx, row.ID, "merge conflict -> resolving_conflict"); err != nil {
								return sent, err
							}
							if s.pub != nil {
								s.pub.PublishReconcile(row.JobID, "project_out:merge_conflict")
							}
							sent++
							continue
						}
					}
				}
				// could not route (no local mirror to resolve main, or route failed) — fall
				// through to the transient path so a human eventually sees the stuck merge.
			}
			// a PERMANENT GitHub failure (a 4xx: deleted branch/PR, 422, 404) never
			// succeeds on retry; a row that has also exhausted a generous retry budget is a
			// poison row (a transient error misclassified, or a multi-hour outage). Either
			// would wedge the SERIALIZED oldest-first outbox forever behind it — blocking
			// every other GitHub write for the repo. Dead-letter it (surfacing the job to a
			// human for a CRITICAL action) and CONTINUE draining the rest, instead of
			// stopping the whole drain on one bad row.
			var ghErr *gh.ErrGitHub
			permanent := errors.As(err, &ghErr) && ghErr.Permanent()
			if permanent || row.Attempts+1 >= maxOutboxAttempts {
				if derr := s.store.DeadLetterOutbox(ctx, row.ID, row.JobID,
					string(job.EscalationProjectOut), err.Error(), criticalAction(row.Action), s.clock.Now()); derr != nil {
					return sent, derr
				}
				if s.logger != nil {
					// durable record of dropped work — a critical action escalates to needs_human
					// unless the job is already terminal (then this WARN is the only surfacing).
					s.logger.Warn("dead-lettered GitHub write",
						"action", row.Action, "job", row.JobID, "repo", s.repo,
						"critical", criticalAction(row.Action), "permanent", permanent, "err", err.Error())
				}
				if s.pub != nil {
					s.pub.PublishReconcile(row.JobID, "project_out:dead_letter")
				}
				continue
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
		// Idempotency (mirrors ActionCreateIssue): a non-zero PR number means a prior
		// drain already opened + stamped this PR — a re-send after a CP crash between the
		// GitHub create and the row being marked sent. Don't open a DUPLICATE PR; consume
		// the row. (RealClient.OpenPR also recovers via GitHub's 422-already-exists, but
		// that is a single GitHub-layer guard; this is the cheaper, authoritative check.)
		if j.PRNumber > 0 {
			return fmt.Sprintf("pr=%d (already open)", j.PRNumber), nil
		}
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
		// Idempotency: a non-zero issue_number means a prior drain already created this
		// issue on GitHub and stamped it — this is a re-send after a CP crash between the
		// stamp and the row being marked sent. Do NOT create a DUPLICATE GitHub issue;
		// just ensure the build is seeded (idempotent) and consume the row.
		if j.IssueNum > 0 {
			detail := fmt.Sprintf("issue=%d (already materialized)", j.IssueNum)
			if bid, berr := s.seedBuildFromSpec(ctx, j, now); berr != nil {
				detail += " build_seed_err=" + berr.Error()
			} else {
				detail += " build=" + bid
			}
			return detail, nil
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
		// CP-AUTHORITATIVE content re-check (defense-in-depth, §5.4): before an AUTONOMOUS
		// merge, re-run the WHOLE content gate (denylist + secret-scan + binary/size) over
		// the ACTUAL base..head diff from the mirror — not the worker's self-reported patch
		// the verdict-time gate ran over. A failure on the REAL branch downgrades the job to
		// the HUMAN merge gate, so a worker can never land a privilege-escalating change, a
		// leaked secret, or a binary blob on main by under-reporting what it touched. A
		// verify FAILURE (can't fetch/diff) returns an error → the merge RETRIES (transient),
		// never a silent autonomous merge of unverified content.
		// expectedHead pins the GitHub merge to the EXACT head the gate reviewed.
		// Passing it as the merge `sha` makes GitHub 409 if the head moved; that
		// invalidates the stale verdict/outbox and re-arms review.
		// An AUTONOMOUS merge MUST be SHA-pinned to the reviewed head AND content-re-verified
		// against the REAL base..head diff — and BOTH require the local history/mirror writer.
		// If it is absent (no FLOWBEE_MIRROR_PATH) or the job has no bound base/head, neither
		// safeguard can run, so FAIL CLOSED to the human merge gate rather than merge the live
		// head unpinned + unchecked. Otherwise an approve-then-push race or an
		// under-reported denylisted/secret diff would land on main. The
		// guards used to sit INSIDE `if s.history != nil`, which silently downgraded the
		// highest-stakes action to "merge whatever is live" when no mirror was configured.
		// Self-merge therefore REQUIRES a mirror; without one, every autonomous merge handoffs.
		if s.history == nil {
			if rerr := s.store.RouteSelfMergeToHandoff(ctx, row.JobID, "self_merge_unverifiable", s.clock.Now()); rerr != nil {
				return "", fmt.Errorf("route unverifiable self-merge to handoff: %w", rerr)
			}
			if s.pub != nil {
				s.pub.PublishReconcile(row.JobID, "project_out:self_merge_unverifiable")
			}
			return "autonomous merge UNVERIFIABLE (no mirror to SHA-pin/content re-verify) -> merge_handoff", nil
		}
		var expectedHead string
		j, jerr := s.store.GetJob(ctx, row.JobID)
		if jerr != nil || j.Verdict == nil || row.HeadSHA == "" || j.HeadSHA != row.HeadSHA ||
			!j.Verdict.Verify(row.HeadSHA, j.BaseSHA) {
			return "", errStaleMergeAuthorization
		}
		expectedHead = row.HeadSHA
		var actualDiff string
		if j.BaseSHA != "" {
			br := store.IssueBranch(s.resolveIssueNum(ctx, j), row.JobID)
			if ferr := s.history.FetchBranch(br); ferr != nil {
				return "", fmt.Errorf("verify autonomous merge: fetch %s: %w", br, ferr)
			}
			var derr error
			actualDiff, derr = s.history.DiffBetween(j.BaseSHA, j.HeadSHA)
			if derr != nil {
				// An UNREACHABLE revision is permanent, not transient: Flowbee self-merged by
				// promoting an epoch commit, but GitHub SQUASH-merged the PR and discarded that
				// commit, so HeadSHA can never be diffed in the mirror — the verify would retry
				// forever (the observed serve-log loop). The PR is already merged on GitHub, so
				// fail-open to the human merge gate exactly like the no-mirror case; the next
				// reconcile sees merged=1 and marks the job done. A TRANSIENT diff error (network,
				// lock) is NOT unreachable and still returns -> retries, unchanged.
				if isUnreachableRev(derr) {
					if rerr := s.store.RouteSelfMergeToHandoff(ctx, row.JobID, "self_merge_unverifiable_head_unreachable", s.clock.Now()); rerr != nil {
						return "", fmt.Errorf("route unverifiable self-merge to handoff: %w", rerr)
					}
					if s.pub != nil {
						s.pub.PublishReconcile(row.JobID, "project_out:self_merge_unverifiable")
					}
					return "autonomous merge UNVERIFIABLE (head SHA unreachable — squash-discarded epoch) -> merge_handoff", nil
				}
				return "", fmt.Errorf("verify autonomous merge: diff %s..%s: %w", j.BaseSHA, j.HeadSHA, derr)
			}
			if reason := contentDenyReason(actualDiff, s.allowOwnSource); reason != "" {
				if rerr := s.store.RouteSelfMergeToHandoff(ctx, row.JobID, reason, s.clock.Now()); rerr != nil {
					return "", fmt.Errorf("route self-merge to handoff: %w", rerr)
				}
				if s.pub != nil {
					s.pub.PublishReconcile(row.JobID, "project_out:self_merge_denied")
				}
				return "autonomous merge DENIED (" + reason + ") -> merge_handoff", nil
			}
		}
		// epic-lane (task brief point 2, review M1/m4): an epic PR (branch epic/<slug>,
		// auto-adopted by Phase 0) is judged against the EPIC'S OWN LAUNCH-PINNED CONTRACT,
		// not as a generic diff — see epicDenyReason. This runs OUTSIDE the base-guarded
		// content block on purpose (m4): an epic-detected PR must fail CLOSED even when the
		// content re-verify was skipped for an empty base, never silently skip its own gate.
		// ("", nil) for the overwhelmingly common non-epic PR; a detection ERROR (transient
		// mirror trouble against a live epic's branch, or an I/O error reading the pinned
		// contract) RETRIES the merge row — never a blind merge of a possibly-unevidenced
		// epic PR (review F2/M1).
		reason, eerr := s.epicDenyReason(ctx, j.Repo, j.BaseSHA, j.HeadSHA, actualDiff)
		if eerr != nil {
			return "", fmt.Errorf("verify autonomous merge: %w", eerr)
		}
		if reason != "" {
			if rerr := s.store.RouteSelfMergeToHandoff(ctx, row.JobID, reason, s.clock.Now()); rerr != nil {
				return "", fmt.Errorf("route self-merge to handoff (epic evidence): %w", rerr)
			}
			if s.pub != nil {
				s.pub.PublishReconcile(row.JobID, "project_out:self_merge_denied")
			}
			return "autonomous merge DENIED (" + reason + ") -> merge_handoff", nil
		}
		ci, err := s.liveMergeCI(ctx, number, j.BaseSHA, expectedHead)
		if err != nil {
			return "", err
		}
		if ci.failed {
			if rerr := s.store.RoutePostApprovalCIFailure(ctx, row.JobID, ci.failingChecks, ci.checkURLs, s.clock.Now()); rerr != nil {
				return "", fmt.Errorf("route post-approval CI failure: %w", rerr)
			}
			if s.pub != nil {
				s.pub.PublishReconcile(row.JobID, "project_out:merge_ci_failed")
			}
			return "", errMergeCIFailedRouted
		}
		if !ci.green {
			return "", errMergeCINotReady
		}
		if err := s.gh.EnqueueMergeQueue(ctx, number, expectedHead); err != nil {
			if errors.Is(err, gh.ErrMergeRuleViolationPending) && gh.IsMergeRuleBehind(err) {
				// A branch-up-to-date ruleset violation is retryable, but it will not clear
				// until GitHub fast-forwards the PR head. Do that best-effort here, then let
				// the outbox retry path re-attempt after GitHub/reconcile observes the new CI.
				if u, ok := s.gh.(gh.MergeUnsticker); ok && number != 0 {
					_ = u.UpdateBranch(ctx, number)
				}
			}
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

	case store.ActionDeleteBranch:
		// post-merge cleanup: delete the merged job's flowbee/issue-N branch so the repo
		// does not accumulate stale flowbee/issue-* branches. Safe — the branch's commits
		// stay reachable from the merge commit on main. A missing branch is success.
		branch, _ := p["branch"].(string)
		if branch == "" {
			return "delete_branch:none", nil
		}
		if err := s.gh.DeleteBranch(ctx, branch); err != nil {
			return "", err
		}
		return "deleted_branch " + branch, nil

	case store.ActionWriteHistory:
		// build-list §F: the post-merge issue-archive. Flowbee folds the job's ledger into
		// a curated card + regenerates the TOC, then lands both on the integration branch
		// via the Contents API — one atomic commit per file onto the branch's current tip
		// (reconcile-first: no force-push, no fast-forward race with concurrent merges; the
		// CP is the sole writer, never entangled with the feature PR). Idempotent per file.
		arts, err := s.store.BuildHistoryArtifacts(ctx, row.JobID)
		if err != nil {
			return "", fmt.Errorf("build history: %w", err)
		}
		if !s.archiveHistory {
			// not opted in (default): the ledger stays canonical; the markdown is not
			// materialized on GitHub (archiving commits docs/history/*.md to the repo's
			// integration branch on every merge, so it is per-repo opt-in). Drop to sent
			// (audited) so the queue never wedges.
			return fmt.Sprintf("history:noop files=%d", len(arts)), nil
		}
		branch := orDefault(s.baseBranch, "main") // the repo's integration branch
		msg := fmt.Sprintf("flowbee: archive history for %s", row.JobID)
		// land the card + the regenerated TOC in ONE commit (not one per file) so a merge
		// adds a single archive commit. Idempotent on a re-drain (unchanged tree => no commit).
		files := make(map[string][]byte, len(arts))
		for _, a := range arts {
			files[a.Path] = []byte(a.Content)
		}
		if err := s.gh.PutFiles(ctx, files, msg, branch); err != nil {
			return "", fmt.Errorf("archive history for %s: %w", row.JobID, err)
		}
		return fmt.Sprintf("history:archived files=%d", len(arts)), nil

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

// gitHubTitleMax is GitHub's hard cap on issue/PR titles. A longer title 422s
// ("title is too long (maximum is 256 characters)"), which the drain rightly
// dead-letters as permanent — so every rendered title must be clamped. A
// one-paragraph `flowbee spec` task has no newline, making its "first line"
// 1000+ chars; this silently killed every spec materialization (russ, 2026-06).
const gitHubTitleMax = 256

// clampTitle truncates a rendered title to gitHubTitleMax characters, breaking
// at a word boundary where one exists in the back half and appending an
// ellipsis. Nothing is lost: the full task/spec text is always in the body.
func clampTitle(s string) string {
	r := []rune(s)
	if len(r) <= gitHubTitleMax {
		return s
	}
	cut := string(r[:gitHubTitleMax-1])
	if i := strings.LastIndex(cut, " "); i > gitHubTitleMax/2 {
		cut = cut[:i]
	}
	return strings.TrimRight(cut, " \t.,;:-") + "…"
}

// prTitle renders the PR title from the build job's task/spec (the issue title),
// falling back to the job id.
func prTitle(j job.Job, jobID string) string {
	for _, s := range []string{j.TaskText, j.SpecText} {
		if line := firstNonEmptyLine(s); line != "" {
			return clampTitle(strings.TrimSpace(strings.TrimLeft(line, "# ")))
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
		ProjectID:          spec.ProjectID,
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
		// inherit the spec's urgency (1..10, lower = more urgent) so a build descends at the
		// priority the issue was filed at — NOT the bare INSERT default 0, which now sorts as
		// MORE urgent than 1 and would jump every spec-flow build to the front of the queue.
		Priority: job.NormalizePriority(spec.Priority),
		Now:      now,
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
			return clampTitle(strings.TrimSpace(strings.TrimLeft(line, "# ")))
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
