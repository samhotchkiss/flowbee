package worker

import (
	"context"
	"fmt"

	"github.com/samhotchkiss/flowbee/client"
)

// StubConfig parameterizes one run of the stub worker (DESIGN §5.5 / §7.1: the
// thin loop with spawn(AGENT_CMD) replaced by an echo).
type StubConfig struct {
	BaseURL     string
	Identity    string
	ModelFamily string
	Role        string
}

// StubOutcome reports what one stub run did (for tests / observability).
type StubOutcome struct {
	Got        bool // got a lease this round
	JobID      string
	LeaseEpoch int
	JobState   string // final job state after result
}

// RunOnce performs one §7.1 cycle: register, long-poll-lease, heartbeat once,
// post an echo work-product (kind=patch, base_sha from the lease, NO pr field),
// and let the result land review_pending. It never opens a PR and never calls
// GitHub (R4). Returns Got=false if the long-poll yielded 204.
func RunOnce(ctx context.Context, cfg StubConfig) (StubOutcome, error) {
	c := client.New(cfg.BaseURL)

	if _, err := c.Register(ctx, client.Registration{
		Identity:     cfg.Identity,
		Host:         "stub",
		Capabilities: []string{"role:eng_worker", "model_family:" + cfg.ModelFamily},
	}); err != nil {
		return StubOutcome{}, fmt.Errorf("register: %w", err)
	}

	grant, ok, err := c.Lease(ctx, cfg.Identity, cfg.ModelFamily, cfg.Role)
	if err != nil {
		return StubOutcome{}, fmt.Errorf("lease: %w", err)
	}
	if !ok {
		return StubOutcome{Got: false}, nil
	}

	out := StubOutcome{Got: true, JobID: grant.JobID, LeaseEpoch: grant.LeaseEpoch}

	if _, st, err := c.Heartbeat(ctx, grant.JobID, grant.LeaseEpoch); err != nil {
		return out, fmt.Errorf("heartbeat: %w", err)
	} else if st != 200 {
		return out, fmt.Errorf("heartbeat: unexpected status %d", st)
	}

	// echo work-product: a patch bound to the lease's base SHA, NO pr field (§5.5).
	body := map[string]any{
		"kind":     "patch",
		"base_sha": grant.BaseSHA,
		"blast_radius": map[string]any{
			"paths": []string{"echo.txt"},
			"scope": "stub",
		},
	}
	res, st, err := c.Result(ctx, grant.JobID, grant.LeaseEpoch, "", body)
	if err != nil {
		return out, fmt.Errorf("result: %w", err)
	}
	if st != 200 {
		return out, fmt.Errorf("result: unexpected status %d", st)
	}
	out.JobState = res.JobState
	return out, nil
}
