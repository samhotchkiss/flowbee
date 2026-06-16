package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/samhotchkiss/flowbee/internal/flow"
	"github.com/samhotchkiss/flowbee/internal/job"
	"github.com/samhotchkiss/flowbee/internal/store"
	"github.com/samhotchkiss/flowbee/internal/testutil"
)

// TestF5ResolvedIdentityGoesIntoLease is the F5 lease-wiring acceptance test: an
// identity RESOLVED from flows/ (registry + default flow) is fenced into the
// lease — the worker never chooses its own identity. It drives a real build job
// to review_pending against a temp-file SQLite store, then claims the review
// gate AS the resolved reviewer identity (with its model_family + lens), and
// asserts the PERSISTED bound_* columns equal the resolved identity. This proves
// "resolved identity goes into the lease (F1)".
func TestF5ResolvedIdentityGoesIntoLease(t *testing.T) {
	ctx := context.Background()
	st := testutil.NewStore(t)

	// ── resolve the flow from flows/ (the same files the engine ships) ──
	reg, err := flow.LoadIdentities("../../flows")
	if err != nil {
		t.Fatalf("LoadIdentities: %v", err)
	}
	doc, err := flow.LoadFlowDoc("../../flows/default.yaml")
	if err != nil {
		t.Fatalf("LoadFlowDoc: %v", err)
	}
	stages, err := doc.Resolve(reg, reg.RoleDefaults(), flow.ResolveOptions{})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	var builder flow.ResolvedActor
	var reviewer flow.ResolvedActor // pick the first fan-out reviewer (correctness)
	for _, s := range stages {
		switch s.Name {
		case "build":
			builder = s.Actors[0]
		case "build_review":
			for _, a := range s.Actors {
				if a.Lens == "correctness" {
					reviewer = a
				}
			}
		}
	}
	if builder.Identity.ID == "" || reviewer.Identity.ID == "" {
		t.Fatalf("flow did not resolve builder/reviewer: builder=%q reviewer=%q",
			builder.Identity.ID, reviewer.Identity.ID)
	}
	// The resolved identities must differ (anti-affinity) — the reviewer is fenced,
	// not the builder, into the review lease.
	if reviewer.Identity.ID == builder.Identity.ID {
		t.Fatalf("anti-affinity: reviewer must differ from builder, both %q", builder.Identity.ID)
	}

	// ── seed a build job and claim it AS the resolved builder identity ──
	if _, err := st.SeedJob(ctx, store.SeedParams{
		ID: "f5-job", Kind: job.KindBuild, Flow: "build", Stage: "build",
		Role: job.RoleEngWorker, BaseSHA: "base", Now: time.Unix(1000, 0),
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	// The builder is RUN on a model_family distinct from the reviewer's so the
	// engine's M4 anti-affinity (reviewer model_family ≠ builder model_family,
	// I-10) is satisfied — the seeded hire defaults all recommend the same family,
	// so the operator's RUNTIME model_family is the live axis (the identity's
	// recommended model is just a default the operator may run on a different box).
	builderFamily := "openai"
	if builderFamily == reviewer.Identity.ModelFamily {
		builderFamily = "google"
	}
	bl, err := st.ClaimReadyJob(ctx, store.ClaimParams{
		JobID: "f5-job", LeaseID: "L-build", Identity: builder.Identity.ID,
		ModelFamily: builderFamily, Role: job.RoleEngWorker,
		TTL: 5 * time.Minute, Now: time.Unix(1001, 0),
	})
	if err != nil {
		t.Fatalf("claim build: %v", err)
	}

	// the build's resolved identity is fenced into the lease row.
	jb, err := st.GetJob(ctx, "f5-job")
	if err != nil {
		t.Fatalf("get job after build claim: %v", err)
	}
	if jb.BoundIdentity != builder.Identity.ID || jb.BoundModelFamily != builderFamily {
		t.Fatalf("build lease bound %q/%q, want resolved %q/%q",
			jb.BoundIdentity, jb.BoundModelFamily, builder.Identity.ID, builderFamily)
	}

	// ── land the build result → review_pending (persists builder_identity) ──
	if _, err := st.Result(ctx, store.ResultParams{
		JobID: "f5-job", Epoch: bl.Epoch, IdempotencyKey: "k1", Now: time.Unix(1002, 0),
		PushedRef: "refs/flowbee/f5-job/e" + itoa(bl.Epoch), PatchDiff: "diff --git a/x b/x\n",
	}); err != nil {
		t.Fatalf("result: %v", err)
	}
	jr, err := st.GetJob(ctx, "f5-job")
	if err != nil {
		t.Fatalf("get job after result: %v", err)
	}
	if jr.State != job.StateReviewPending {
		t.Fatalf("after result state=%s want review_pending", jr.State)
	}

	// ── claim the review gate AS the RESOLVED reviewer identity + lens ──
	rl, err := st.ClaimReviewJob(ctx, store.ClaimReviewParams{
		JobID: "f5-job", LeaseID: "L-review", Identity: reviewer.Identity.ID,
		ModelFamily: reviewer.Identity.ModelFamily, Lens: reviewer.Lens,
		Attested: []string{"role:code_reviewer", "model_family:" + reviewer.Identity.ModelFamily},
		TTL:      5 * time.Minute, Now: time.Unix(1003, 0),
	})
	if err != nil {
		t.Fatalf("claim review: %v", err)
	}
	if rl.Identity != reviewer.Identity.ID {
		t.Fatalf("review lease identity=%q want resolved %q", rl.Identity, reviewer.Identity.ID)
	}

	// ── the PERSISTED lease carries the resolved identity + model_family + lens ──
	jv, err := st.GetJob(ctx, "f5-job")
	if err != nil {
		t.Fatalf("get job after review claim: %v", err)
	}
	if jv.BoundIdentity != reviewer.Identity.ID {
		t.Errorf("bound_identity=%q want resolved reviewer %q", jv.BoundIdentity, reviewer.Identity.ID)
	}
	if jv.BoundModelFamily != reviewer.Identity.ModelFamily {
		t.Errorf("bound_model_family=%q want resolved %q", jv.BoundModelFamily, reviewer.Identity.ModelFamily)
	}
	if jv.BoundLens != reviewer.Lens {
		t.Errorf("bound_lens=%q want resolved lens %q", jv.BoundLens, reviewer.Lens)
	}
	if reviewer.Lens != "correctness" {
		t.Errorf("resolved reviewer lens=%q want correctness", reviewer.Lens)
	}
}

// itoa is a tiny int→string without importing strconv into this focused test.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	if neg {
		b = append([]byte{'-'}, b...)
	}
	return string(b)
}
