package project

import (
	"context"
	"fmt"

	gh "github.com/samhotchkiss/flowbee/internal/github"
)

// AssertBranchProtection is the I-8 startup backstop (§8.5.5, §9.6): Flowbee
// asserts the server-side branch protection on the target branch — the
// orchestrator-independent law that holds even if Flowbee has a bug. It requires
// no direct/force push AND required review from an identity distinct from the
// author. A missing or too-weak protection is a hard startup error: Flowbee
// complements branch protection, it never assumes it away.
func AssertBranchProtection(ctx context.Context, r gh.BranchProtectionReader, branch string) error {
	p, ok, err := r.BranchProtection(ctx, branch)
	if err != nil {
		return fmt.Errorf("read branch protection for %q: %w", branch, err)
	}
	if !ok {
		return fmt.Errorf("branch %q has no server-side protection (I-8): Flowbee requires it as the backstop", branch)
	}
	if !p.NoForcePush {
		return fmt.Errorf("branch %q permits force-push (I-8): protection must forbid direct/force push", branch)
	}
	if !p.RequireDistinctReviewer {
		return fmt.Errorf("branch %q does not require review from a distinct identity (I-8, §9.6)", branch)
	}
	return nil
}
