package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/samhotchkiss/flowbee/internal/epicspec"
)

// epicDoneRetention is how long a TERMINAL done/achieved epic stays visible to
// Phase 3's Epic-PR detection scan after finished_at (review F3). The window exists
// because "the epic reached State: done" and "the epic's PR merged" are DISTINCT
// events, often days apart — the whole point of the gate is judging that in-between
// PR — so a done epic must stay detectable while its PR can still plausibly be in
// flight. Two weeks comfortably covers a stalled human handoff; beyond it an
// unmerged epic PR is operator territory anyway, and without SOME bound the
// per-lease/per-merge mirror scan would grow with every epic ever run.
const epicDoneRetention = 14 * 24 * time.Hour

// ListEpicRunsForRepo returns the epics registered for repo that Phase 3's Epic-PR
// detection should still scan for, ordered by id: every NON-terminal epic
// (launching/running/blocked — deliberately NOT ListActiveEpicRuns's filter, which
// serves the launch-time reservation gates), PLUS terminal done/achieved epics whose
// finished_at is within epicDoneRetention of now. Terminal states are included at
// all because by the time an epic's PR reaches review/merge its own agent has
// typically ALREADY written "State: done" (UpsertEpicStatus maps that straight to
// the terminal epics.state) — an active-only filter would exclude the exact epics
// whose PRs the gate exists to judge. 'abandoned' is excluded entirely (review F3):
// the operator explicitly gave up on it, its branch must never self-merge as an
// evidenced epic, and a manually revived abandoned-epic PR correctly reviews as an
// ordinary PR.
func (s *Store) ListEpicRunsForRepo(ctx context.Context, repo string, now time.Time) ([]EpicRun, error) {
	cutoff := now.Add(-epicDoneRetention).Format(rfc3339)
	return queryEpicRuns(ctx, s.DB, epicRunSelect+`
		 WHERE repo = ?
		   AND state != 'abandoned'
		   AND (state NOT IN ('done','achieved') OR finished_at >= ?)
		 ORDER BY id`, repo, cutoff)
}

// EpicForRepoBranch is the epic-lane Phase 3 Epic-PR detection helper's DIRECT form
// (task brief point 1: "a PR whose head branch matches branch epic/<slug> where an
// epics row exists for that slug+repo"), for any caller that already knows the PR's
// actual head branch name. ok=false (not an error) covers both the overwhelmingly
// common case — an ordinary, non-epic branch — and a near-miss branch name
// (SlugFromBranch) or a slug registered for a DIFFERENT repo (same slug, two repos,
// never the same epic).
//
// Nothing in this control plane currently calls this with a real branch name: GitHub
// gives Flowbee no fact naming a PR's head branch (BoardSweep/PullRequest fetch only
// headRefOid, the SHA — see EpicForHeadSHA's doc for the practical runtime substitute
// this repo actually wires up). This direct form is kept as the literal, narrowly
// testable realization of the task brief's own wording, and is what a FUTURE caller
// that does have a branch name (e.g. a webhook payload, which DOES carry
// pull_request.head.ref) should reach for first.
func (s *Store) EpicForRepoBranch(ctx context.Context, repo, branch string) (EpicRun, bool, error) {
	slug, ok := epicspec.SlugFromBranch(branch)
	if !ok {
		return EpicRun{}, false, nil
	}
	e, err := s.GetEpicRun(ctx, slug)
	if errors.Is(err, ErrEpicRunNotFound) {
		return EpicRun{}, false, nil
	}
	if err != nil {
		return EpicRun{}, false, err
	}
	if e.Repo != repo {
		return EpicRun{}, false, nil
	}
	return e, true, nil
}

// EpicMirrorReader is the minimal control-plane-mirror capability the epic-lane
// Phase 3 gate/brief need: force-refresh a branch, resolve a ref to its tip SHA, and
// read a file's bytes AS OF a given ref. Satisfied by *gitops.Mirror (see
// cmd/flowbee/epic.go's ingestEpicStatuses, which already reads an epic file this
// same way) and, in internal/project, by the Sender's own HistoryWriter — kept as an
// interface here (rather than importing internal/gitops's concrete type) so this
// package does not force a harder dependency on ITS callers than they already carry,
// and so a test can fake it cheaply.
type EpicMirrorReader interface {
	FetchBranch(branch string) error
	HeadSHA(ref string) (string, error)
	ReadFileAtRef(ref, path string) (string, bool, error)
}

// EpicForHeadSHA is the PRACTICAL, actually-wired-at-runtime form of Epic-PR
// detection (task brief point 1) for the two call sites that DO have mirror access
// but no GitHub-reported branch name (internal/project's merge-time content gate,
// internal/api's review-lease brief injection) — see EpicForRepoBranch's doc for why
// no branch name is available here. Branch IDENTITY is instead established by SHA-tip
// match: for each epic in repo's detection window (ListEpicRunsForRepo — non-terminal
// plus recently-done, see its retention doc), fetch that epic's OWN branch
// (epics.branch, "epic/<slug>" by convention) on the mirror and compare its CURRENT
// tip commit to headSHA. A tip match is unambiguous (two branches cannot share a tip
// SHA without being the same ref) and needs no new GitHub fact or schema column.
//
// ok=false (not an error) is the expected, common result for a non-epic PR, OR when
// repo has no epics left in ListEpicRunsForRepo's detection window (the loop body
// never runs — zero mirror I/O then; a repo that HAS epics in the window pays one
// fetch per such epic on each call).
//
// Error posture (review F2 — fail CLOSED on mirror trouble, exactly like the
// generic content gate whose DiffBetween error retries the merge): a fetch or
// rev-parse failure against a NON-terminal epic's branch PROPAGATES as an error, so
// the caller (project.go's epicDenyReason) retries rather than waving the PR
// through as "ordinary" — a transient mirror outage must never let an unevidenced
// epic PR skip its own contract gate. Two deliberate exceptions stay a clean skip:
//   - "couldn't find remote ref" (isMissingRemoteRef): the branch genuinely does
//     not exist at origin — a just-launched epic that hasn't pushed yet (the same
//     expected-case ingestEpicStatuses tolerates), or a branch deleted post-merge.
//     A PR head cannot belong to a branch that doesn't exist, so this is a clean
//     non-match, and treating it as transient would block EVERY merge in the repo
//     for as long as any freshly-launched epic sits un-pushed.
//   - a TERMINAL (done/achieved, in-retention) epic's fetch error of any kind: its
//     own PR already passed or is past the gate; a hiccup on its branch must not
//     hold up unrelated merges.
func (s *Store) EpicForHeadSHA(ctx context.Context, mirror EpicMirrorReader, repo, headSHA string, now time.Time) (EpicRun, bool, error) {
	// NOTE: repo == "" is NOT rejected here — it is the legacy single-repo default
	// (job.Job.Repo's own doc: "Empty is the legacy single-repo default"), under
	// which an epic can legitimately be registered with repo="" too. Rejecting it
	// would make Epic-PR detection permanently dead code for every non-F9 (single
	// managed repo) deployment — exactly the deployment shape this control plane
	// runs today.
	if mirror == nil || headSHA == "" {
		return EpicRun{}, false, nil
	}
	epics, err := s.ListEpicRunsForRepo(ctx, repo, now)
	if err != nil {
		return EpicRun{}, false, err
	}
	for _, e := range epics {
		branch := e.Branch
		if branch == "" {
			branch = "epic/" + e.ID
		}
		terminal := e.State == "done" || e.State == "achieved"
		if ferr := mirror.FetchBranch(branch); ferr != nil {
			if terminal || isMissingRemoteRef(ferr) {
				continue
			}
			return EpicRun{}, false, fmt.Errorf("epic %q branch %q fetch: %w", e.ID, branch, ferr)
		}
		tip, herr := mirror.HeadSHA("refs/heads/" + branch)
		if herr != nil {
			if terminal {
				continue
			}
			return EpicRun{}, false, fmt.Errorf("epic %q branch %q rev-parse: %w", e.ID, branch, herr)
		}
		if tip == headSHA {
			return e, true, nil
		}
	}
	return EpicRun{}, false, nil
}

// isMissingRemoteRef reports whether a FetchBranch error is git's PERMANENT
// "this branch does not exist at origin" failure (stderr: "fatal: couldn't find
// remote ref refs/heads/x") as opposed to a transient network/lock/auth error.
// The distinction matters to EpicForHeadSHA's fail-closed posture (see its doc):
// a genuinely absent branch is a clean non-match; everything else must retry.
// Same substring-matching approach as project.go's isUnreachableRev (gitops wraps
// git's stderr into the error text, so the message is the only signal available).
func isMissingRemoteRef(err error) bool {
	return err != nil && strings.Contains(strings.ToLower(err.Error()), "couldn't find remote ref")
}

// EpicContractAtHead reads epic run e's spec file AS OF headSHA — the PR's
// reconciled head, NOT main and NOT the epics table's own status_* snapshot (which
// ingestEpicStatuses refreshes on its own ~2-minute cadence and can lag the exact
// commit under review) — via mirror, then parses it with epicspec's STRICT spec
// parser (Goal/Constraints/Steps — spec-frozen per author-epic/SKILL.md, so reading
// it at the PR head rather than at launch time changes nothing for a well-behaved
// epic, but catches a HAND-EDITED spec honestly) plus the lenient status parser (the
// claimed ## Status this exact commit carries). Both the evidence/scope GATE
// (project.go's epicDenyReason) and the REVIEWER BRIEF (api/server.go's
// leaseGrantForJob) call this, so the gate and what the reviewer reads always agree
// on the identical bytes.
func (s *Store) EpicContractAtHead(mirror EpicMirrorReader, e EpicRun, headSHA string) (epicspec.Spec, epicspec.StatusBlock, error) {
	raw, found, err := mirror.ReadFileAtRef(headSHA, e.FilePath)
	if err != nil {
		return epicspec.Spec{}, epicspec.StatusBlock{}, fmt.Errorf("read %s at %s: %w", e.FilePath, headSHA, err)
	}
	if !found {
		return epicspec.Spec{}, epicspec.StatusBlock{}, fmt.Errorf("%s not found at PR head %s", e.FilePath, headSHA)
	}
	spec, err := epicspec.ParseSpec(raw)
	if err != nil {
		return epicspec.Spec{}, epicspec.StatusBlock{}, fmt.Errorf("parse epic spec: %w", err)
	}
	sb := epicspec.ParseStatus(epicspec.ParseStatusSection(raw))
	return spec, sb, nil
}
