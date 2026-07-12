package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/samhotchkiss/flowbee/internal/epicspec"
)

// ListEpicRunsForRepo returns every epic ever registered for repo (any state,
// including terminal done/abandoned) ordered by id. Unlike ListActiveEpicRuns, this
// deliberately does NOT filter to in-flight states: by the time an epic's PR reaches
// review/merge its own agent has typically ALREADY written "State: done" to its
// ## Status, which UpsertEpicStatus maps straight to the epics.state='done' TERMINAL
// state (nextEpicState) — so a filter to "active" would exclude the exact epics whose
// PRs are passing the Phase 3 evidence gate, the most important case to still find.
func (s *Store) ListEpicRunsForRepo(ctx context.Context, repo string) ([]EpicRun, error) {
	return queryEpicRuns(ctx, s.DB, epicRunSelect+` WHERE repo = ? ORDER BY id`, repo)
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
// match: for each of repo's registered epics (ListEpicRunsForRepo — ALL states, not
// just active, for the same reason documented there), fetch that epic's OWN branch
// (epics.branch, "epic/<slug>" by convention) on the mirror and compare its CURRENT
// tip commit to headSHA. A tip match is unambiguous (two branches cannot share a tip
// SHA without being the same ref) and needs no new GitHub fact or schema column.
//
// ok=false (not an error) is the expected, common result for a non-epic PR, OR when
// repo has never registered any epic (the loop body never runs — ZERO mirror I/O in
// that case, since ListEpicRunsForRepo returns empty; only a repo that has actually
// used the epic-lane feature pays any fetch cost at all). A per-epic fetch/HeadSHA
// error is treated as "not this epic" and skipped (best-effort scan, mirroring
// ingestEpicStatuses' own "one epic's hiccup must not blind the rest" posture) rather
// than aborting the whole scan.
func (s *Store) EpicForHeadSHA(ctx context.Context, mirror EpicMirrorReader, repo, headSHA string) (EpicRun, bool, error) {
	// NOTE: repo == "" is NOT rejected here — it is the legacy single-repo default
	// (job.Job.Repo's own doc: "Empty is the legacy single-repo default"), under
	// which an epic can legitimately be registered with repo="" too. Rejecting it
	// would make Epic-PR detection permanently dead code for every non-F9 (single
	// managed repo) deployment — exactly the deployment shape this control plane
	// runs today.
	if mirror == nil || headSHA == "" {
		return EpicRun{}, false, nil
	}
	epics, err := s.ListEpicRunsForRepo(ctx, repo)
	if err != nil {
		return EpicRun{}, false, err
	}
	for _, e := range epics {
		branch := e.Branch
		if branch == "" {
			branch = "epic/" + e.ID
		}
		if ferr := mirror.FetchBranch(branch); ferr != nil {
			continue
		}
		tip, herr := mirror.HeadSHA("refs/heads/" + branch)
		if herr != nil {
			continue
		}
		if tip == headSHA {
			return e, true, nil
		}
	}
	return EpicRun{}, false, nil
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
