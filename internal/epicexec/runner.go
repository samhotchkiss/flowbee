// Package epicexec executes Flowbee-owned GitHub effects from the durable v2
// action ledger. It never accepts mutable routing/artifact identifiers from a
// worker and treats a successful write as an effect receipt, not as workflow
// completion; authoritative GitHub facts drive the next stage.
package epicexec

import (
	"context"
	"errors"
	"fmt"
	"time"

	flowgithub "github.com/samhotchkiss/flowbee/internal/github"
	"github.com/samhotchkiss/flowbee/internal/store"
)

type ActionStore interface {
	ClaimNextEpicDomainAction(context.Context, string, time.Time, time.Duration) (store.EpicDomainAction, bool, error)
	MarkEpicDomainActionVerifying(context.Context, store.EpicDomainAction, string, string, time.Time) error
	RetryEpicDomainAction(context.Context, store.EpicDomainAction, string, string, time.Time, time.Time) error
	DeadLetterEpicDomainAction(context.Context, store.EpicDomainAction, string, string, time.Time) error
	MarkEpicMergeSuperseded(context.Context, store.EpicDomainAction, string, string, time.Time) error
	HoldEpicMergeAuthorization(context.Context, store.EpicDomainAction, string, string, time.Time) error
	MarkEpicMergeConflict(context.Context, store.EpicDomainAction, string, string, time.Time) error
	CompleteEpicCleanup(context.Context, store.EpicDomainAction, string, time.Time) error
	NextVerifyingEpicDomainAction(context.Context, time.Time) (store.EpicDomainAction, bool, error)
	DeferVerifyingEpicDomainAction(context.Context, store.EpicDomainAction, string, time.Time, time.Time) error
	RecordEpicMergeFactFromEffect(context.Context, store.EpicDomainAction, string, time.Time) error
	RequeueVerifiedEpicDomainAction(context.Context, store.EpicDomainAction, string, time.Time, time.Time) error
}

type GitHub interface {
	flowgithub.Client
	flowgithub.Writer
	BranchExists(context.Context, string) (bool, error)
}

type GitHubResolver interface {
	ForRepo(context.Context, string) (GitHub, error)
}

// MergeAuthorizer performs the Flowbee-owned content, scope, contract-evidence,
// repository-policy, and circuit-breaker checks against the exact current
// artifact. It is deliberately separate from transport. A missing authorizer
// fails closed; GitHub's expected-head interlock is not a substitute for these
// product safety gates.
type MergeAuthorizer interface {
	AuthorizeEpicMerge(context.Context, store.EpicDomainAction) error
}

// MergeAuthorizationDenied is returned by a product safety gate when retrying
// the identical artifact cannot make it safe (for example a denylisted path or
// incomplete epic evidence). The executor parks the delivery visibly for a
// human instead of burning the transient retry/dead-letter budget.
type MergeAuthorizationDenied struct{ Reason string }

func (e *MergeAuthorizationDenied) Error() string { return e.Reason }

type Runner struct {
	Store      ActionStore
	GitHub     GitHub
	Resolver   GitHubResolver
	Authorizer MergeAuthorizer
	Owner      string
	ClaimTTL   time.Duration
	Now        func() time.Time
}

func (r Runner) githubFor(ctx context.Context, repo string) (GitHub, error) {
	if r.Resolver != nil {
		return r.Resolver.ForRepo(ctx, repo)
	}
	if r.GitHub == nil {
		return nil, fmt.Errorf("no GitHub client registered for repository %q", repo)
	}
	return r.GitHub, nil
}

func (r Runner) now() time.Time {
	if r.Now != nil {
		return r.Now().UTC()
	}
	return time.Now().UTC()
}

// ExecuteNext performs at most one claimed effect. Merge success enters
// verifying until an authoritative PR read supplies the concrete merge commit.
func (r Runner) ExecuteNext(ctx context.Context) (bool, error) {
	if r.Store == nil || (r.GitHub == nil && r.Resolver == nil) || r.Owner == "" {
		return false, errors.New("epic effect runner is not configured")
	}
	now := r.now()
	action, ok, err := r.Store.ClaimNextEpicDomainAction(ctx, r.Owner, now, r.ClaimTTL)
	if err != nil || !ok {
		return ok, err
	}
	github, err := r.githubFor(ctx, action.Repo)
	if err != nil {
		// Resolution failed before any external mutation, so the same action is
		// safely returned to pending rather than stranded as uncertain.
		return true, r.Store.RetryEpicDomainAction(ctx, action, r.Owner, err.Error(), now.Add(time.Minute), now)
	}
	switch action.Kind {
	case "merge_dispatch":
		if r.Authorizer == nil {
			return true, r.Store.RetryEpicDomainAction(ctx, action, r.Owner,
				"exact content/scope/evidence merge authorizer is unavailable", now.Add(time.Minute), now)
		}
		if err := r.Authorizer.AuthorizeEpicMerge(ctx, action); err != nil {
			switch {
			case errors.Is(err, flowgithub.ErrMergeHeadModified), errors.Is(err, flowgithub.ErrMergeBaseModified):
				return true, r.Store.MarkEpicMergeSuperseded(ctx, action, r.Owner, err.Error(), now)
			default:
				var denied *MergeAuthorizationDenied
				if errors.As(err, &denied) {
					return true, r.Store.HoldEpicMergeAuthorization(ctx, action, r.Owner, denied.Error(), now)
				}
				return true, r.Store.RetryEpicDomainAction(ctx, action, r.Owner, err.Error(), now.Add(time.Minute), now)
			}
		}
		err = github.EnqueueMergeQueue(ctx, action.PRNumber, action.HeadSHA)
		switch {
		case err == nil:
			return true, r.Store.MarkEpicDomainActionVerifying(ctx, action, r.Owner,
				"merge mutation accepted; awaiting authoritative merge fact", now)
		case errors.Is(err, flowgithub.ErrMergeHeadModified):
			return true, r.Store.MarkEpicMergeSuperseded(ctx, action, r.Owner, err.Error(), now)
		case errors.Is(err, flowgithub.ErrMergeConflict):
			return true, r.Store.MarkEpicMergeConflict(ctx, action, r.Owner, err.Error(), now)
		case errors.Is(err, flowgithub.ErrMergeBaseModified), errors.Is(err, flowgithub.ErrMergeRuleViolationPending):
			return true, r.Store.RetryEpicDomainAction(ctx, action, r.Owner, err.Error(), now.Add(30*time.Second), now)
		default:
			var permanent interface{ Permanent() bool }
			if errors.As(err, &permanent) && permanent.Permanent() {
				return true, r.Store.DeadLetterEpicDomainAction(ctx, action, r.Owner, err.Error(), now)
			}
			return true, r.Store.RetryEpicDomainAction(ctx, action, r.Owner, err.Error(), now.Add(time.Minute), now)
		}
	case "cleanup":
		if err := github.DeleteBranch(ctx, action.Branch); err != nil {
			var permanent interface{ Permanent() bool }
			if errors.As(err, &permanent) && permanent.Permanent() {
				return true, r.Store.DeadLetterEpicDomainAction(ctx, action, r.Owner, err.Error(), now)
			}
			return true, r.Store.RetryEpicDomainAction(ctx, action, r.Owner, err.Error(), now.Add(time.Minute), now)
		}
		// DELETE success (including an already-absent branch) is exact lifecycle
		// evidence for the explicit cleanup target.
		return true, r.Store.CompleteEpicCleanup(ctx, action, r.Owner, now)
	default:
		return true, fmt.Errorf("unsupported epic domain action %q", action.Kind)
	}
}

// VerifyNext recovers one action whose executor may have died after mutation.
// It observes first and only requeues when mechanical evidence proves no effect.
func (r Runner) VerifyNext(ctx context.Context) (bool, error) {
	if r.Store == nil || (r.GitHub == nil && r.Resolver == nil) {
		return false, errors.New("epic effect runner is not configured")
	}
	now := r.now()
	action, ok, err := r.Store.NextVerifyingEpicDomainAction(ctx, now)
	if err != nil || !ok {
		return ok, err
	}
	github, err := r.githubFor(ctx, action.Repo)
	if err != nil {
		return true, r.Store.DeferVerifyingEpicDomainAction(ctx, action, err.Error(), now.Add(time.Minute), now)
	}
	switch action.Kind {
	case "merge_dispatch":
		pr, exists, err := github.PullRequest(ctx, action.PRNumber)
		if err != nil {
			return true, r.Store.DeferVerifyingEpicDomainAction(ctx, action, err.Error(), now.Add(time.Minute), now)
		}
		if !exists {
			detail := fmt.Sprintf("verify merge action %s: PR #%d absent", action.ID, action.PRNumber)
			return true, r.Store.DeferVerifyingEpicDomainAction(ctx, action, detail, now.Add(time.Minute), now)
		}
		if pr.Merged {
			return true, r.Store.RecordEpicMergeFactFromEffect(ctx, action, pr.MergeCommit, now)
		}
		if pr.HeadRefOid != action.HeadSHA || pr.BaseRefOid != action.BaseSHA {
			// No live executor owns a verifying row. The empty owner is an
			// intentional fence for a post-crash supersession transition.
			return true, r.Store.MarkEpicMergeSuperseded(ctx, action, "", "artifact advanced while verifying merge", now)
		}
		return true, r.Store.RequeueVerifiedEpicDomainAction(ctx, action,
			"authoritative PR fact proves merge did not occur", now.Add(30*time.Second), now)
	case "cleanup":
		exists, err := github.BranchExists(ctx, action.Branch)
		if err != nil {
			return true, r.Store.DeferVerifyingEpicDomainAction(ctx, action, err.Error(), now.Add(time.Minute), now)
		}
		if !exists {
			return true, r.Store.CompleteEpicCleanup(ctx, action, "", now)
		}
		return true, r.Store.RequeueVerifiedEpicDomainAction(ctx, action,
			"authoritative branch fact proves cleanup did not occur", now.Add(30*time.Second), now)
	default:
		return true, fmt.Errorf("unsupported verifying epic action %q", action.Kind)
	}
}
