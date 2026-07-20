package store_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"testing"
	"time"

	"github.com/samhotchkiss/flowbee/internal/store"
	"github.com/samhotchkiss/flowbee/internal/testutil"
)

func bootstrapInput(id, project, kind, payload string) store.BootstrapActionInput {
	sum := sha256.Sum256([]byte(payload))
	return store.BootstrapActionInput{ID: id, BootstrapID: "bootstrap-" + project, ProjectID: project,
		Kind: kind, PayloadJSON: payload, PayloadSHA256: "sha256:" + hex.EncodeToString(sum[:])}
}

func TestBootstrapActionLedgerIsImmutableAndEpochFenced(t *testing.T) {
	ctx := context.Background()
	st := testutil.NewStore(t)
	now := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	in := bootstrapInput("action-project", "russ", "project_upsert", `{"id":"russ"}`)
	first, err := st.CommitBootstrapAction(ctx, in, now)
	if err != nil || first.State != "pending" {
		t.Fatalf("commit=%+v err=%v", first, err)
	}
	if replay, err := st.CommitBootstrapAction(ctx, in, now.Add(time.Minute)); err != nil || replay.ID != first.ID {
		t.Fatalf("replay=%+v err=%v", replay, err)
	}
	changed := in
	changed.PayloadJSON = `{"id":"other"}`
	sum := sha256.Sum256([]byte(changed.PayloadJSON))
	changed.PayloadSHA256 = "sha256:" + hex.EncodeToString(sum[:])
	if _, err := st.CommitBootstrapAction(ctx, changed, now); !errors.Is(err, store.ErrBootstrapActionConflict) {
		t.Fatalf("changed replay err=%v", err)
	}
	claim, err := st.ClaimNextBootstrapAction(ctx, "serve-a", now, time.Minute)
	if err != nil || claim.ClaimEpoch != 1 || claim.ActionEpoch != 1 {
		t.Fatalf("claim=%+v err=%v", claim, err)
	}
	if _, err := st.RecordBootstrapActionReceipt(ctx, claim.ID, "serve-b", claim.ClaimEpoch, "receipt", "accepted", false, now); !errors.Is(err, store.ErrBootstrapActionStale) {
		t.Fatalf("stale owner err=%v", err)
	}
	verifying, err := st.RecordBootstrapActionReceipt(ctx, claim.ID, "serve-a", claim.ClaimEpoch, "receipt", "accepted", false, now)
	if err != nil || verifying.State != "verifying" {
		t.Fatalf("receipt=%+v err=%v", verifying, err)
	}
	done, err := st.CompleteBootstrapAction(ctx, claim.ID, claim.ActionEpoch, "project exists", now)
	if err != nil || done.State != "succeeded" {
		t.Fatalf("complete=%+v err=%v", done, err)
	}
}

func TestExpiredBootstrapClaimBecomesVisibleUncertainAndNeverBlindlyReclaims(t *testing.T) {
	ctx := context.Background()
	st := testutil.NewStore(t)
	now := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	if _, err := st.CreatePortfolioProject(ctx, store.PortfolioProject{ID: "russ", Name: "Russ"}, now); err != nil {
		t.Fatal(err)
	}
	in := bootstrapInput("action-topology", "russ", "managed_topology", `{"id":"flowbee-russ"}`)
	if _, err := st.CommitBootstrapAction(ctx, in, now); err != nil {
		t.Fatal(err)
	}
	if _, err := st.ClaimNextBootstrapAction(ctx, "serve-a", now, time.Minute); err != nil {
		t.Fatal(err)
	}
	if n, err := st.RecoverExpiredBootstrapClaims(ctx, now.Add(2*time.Minute)); err != nil || n != 1 {
		t.Fatalf("recover=%d err=%v", n, err)
	}
	recovered, err := st.GetBootstrapAction(ctx, in.ID)
	if err != nil || recovered.State != "uncertain" || !recovered.AlertPending {
		t.Fatalf("recovered=%+v err=%v", recovered, err)
	}
	if _, err := st.ClaimNextBootstrapAction(ctx, "serve-b", now.Add(3*time.Minute), time.Minute); err == nil {
		t.Fatal("uncertain action was blindly reclaimed")
	}
	var attention int
	if err := st.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM attention_items WHERE project_id='russ' AND dedup_key='bootstrap-action:action-topology' AND state='open'`).Scan(&attention); err != nil || attention != 1 {
		t.Fatalf("attention=%d err=%v", attention, err)
	}
}

func TestHeldBootstrapActionRearmsOnlyThroughBoundedCapabilitySeam(t *testing.T) {
	ctx := context.Background()
	st := testutil.NewStore(t)
	now := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	if _, err := st.CreatePortfolioProject(ctx, store.PortfolioProject{ID: "russ", Name: "Russ"}, now); err != nil {
		t.Fatal(err)
	}
	in := bootstrapInput("action-topology", "russ", "managed_topology", `{"id":"flowbee-russ"}`)
	if _, err := st.CommitBootstrapAction(ctx, in, now); err != nil {
		t.Fatal(err)
	}
	claim, err := st.ClaimBootstrapAction(ctx, in.ID, "serve", now, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.HoldBootstrapAction(ctx, in.ID, "serve", claim.ClaimEpoch, "utility capability missing", now.Add(time.Minute), false, now); err != nil {
		t.Fatal(err)
	}
	if n, err := st.RearmHeldBootstrapActions(ctx, "managed_topology", 1, now.Add(time.Minute)); err != nil || n != 1 {
		t.Fatalf("rearm=%d err=%v", n, err)
	}
	rearmed, err := st.GetBootstrapAction(ctx, in.ID)
	if err != nil || rearmed.State != "pending" || rearmed.RecoveryCount != 1 {
		t.Fatalf("rearmed=%+v err=%v", rearmed, err)
	}
}

func TestPreProjectBootstrapHoldUsesLedgerWithoutAttentionForeignKey(t *testing.T) {
	ctx := context.Background()
	st := testutil.NewStore(t)
	now := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	in := bootstrapInput("action-new-project", "new-project", "project_upsert", `{"id":"new-project"}`)
	if _, err := st.CommitBootstrapAction(ctx, in, now); err != nil {
		t.Fatal(err)
	}
	claim, err := st.ClaimBootstrapAction(ctx, in.ID, "serve", now, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	held, err := st.HoldBootstrapAction(ctx, in.ID, "serve", claim.ClaimEpoch,
		"project configuration rejected", now.Add(time.Minute), false, now)
	if err != nil || held.State != "held" || !held.AlertPending {
		t.Fatalf("held=%+v err=%v", held, err)
	}
	var events, attention int
	if err := st.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM bootstrap_action_events WHERE action_id=?`, in.ID).Scan(&events); err != nil {
		t.Fatal(err)
	}
	if err := st.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM attention_items WHERE project_id=?`, in.ProjectID).Scan(&attention); err != nil {
		t.Fatal(err)
	}
	if events < 3 || attention != 0 {
		t.Fatalf("pre-project visibility events=%d attention=%d", events, attention)
	}
}
