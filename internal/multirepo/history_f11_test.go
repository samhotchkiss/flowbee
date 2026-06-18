package multirepo_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/samhotchkiss/flowbee/internal/clock"
	gh "github.com/samhotchkiss/flowbee/internal/github"
	"github.com/samhotchkiss/flowbee/internal/gitops"
	"github.com/samhotchkiss/flowbee/internal/job"
	"github.com/samhotchkiss/flowbee/internal/multirepo"
	"github.com/samhotchkiss/flowbee/internal/store"
	"github.com/samhotchkiss/flowbee/internal/testutil"
)

// recordingHistory captures CommitHistory calls so the test can assert the per-repo
// F11 writer is wired and fires on merge with the right branch + archive files.
type recordingHistory struct {
	branch string
	files  []gitops.HistoryFile
	calls  int
}

func (r *recordingHistory) CommitHistory(branch, _ string, files []gitops.HistoryFile) (string, bool, error) {
	r.calls++
	r.branch = branch
	r.files = files
	return "deadbeef", true, nil
}

func (r *recordingHistory) HeadSHA(string) (string, error)          { return "deadbeef", nil }
func (r *recordingHistory) FetchBranch(string) error                { return nil }
func (r *recordingHistory) DiffBetween(_, _ string) (string, error) { return "", nil }

// TestF11HistoryWiredPerRepo: the F11 issue-archive projection (build-list §F) is
// wired per repo through multirepo.WithHistory. On a merged->done reconcile, draining
// the repo's project-OUT loop lands the dedicated post-merge history write through
// that repo's writer, on that repo's integration branch, carrying the card + the TOC.
func TestF11HistoryWiredPerRepo(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	clk := clock.NewFake(time.Unix(20_000, 0))

	if err := st.RegisterRepo(ctx, store.Repo{ID: "web", Owner: "acme", Repo: "web", DefaultBranch: "trunk", Active: true}); err != nil {
		t.Fatalf("register: %v", err)
	}
	fake := gh.NewFake()
	hist := &recordingHistory{}
	mgr, err := multirepo.New(ctx, st, clk, nil,
		func(store.Repo) (gh.Client, gh.Writer, error) { return fake, fake, nil },
		multirepo.WithHistory(func(store.Repo) multirepo.HistoryWriter { return hist }))
	if err != nil {
		t.Fatalf("manager: %v", err)
	}
	_ = mgr

	// seed a build job in the web repo, bind a PR, and reconcile it MERGED -> done.
	const id = "web-build-1"
	if _, err := st.SeedJob(ctx, store.SeedParams{
		ID: id, Kind: job.KindBuild, Flow: "build", Stage: "build", Role: job.RoleEngWorker,
		BaseSHA: "base0", Repo: "web", Now: clk.Now(), TaskText: "Wire the navbar",
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := st.BindPRNumber(ctx, id, 77); err != nil {
		t.Fatalf("bind pr: %v", err)
	}
	out, err := st.ApplyReconciledPR(ctx, id, store.ReconciledPR{
		Number: 77, UpdatedAt: clk.Now(), HeadSHA: "h", BaseSHA: "base0",
		Merged: true, MergeCommit: "mc-web", CIGreen: true,
	}, clk.Now())
	if err != nil || !out.Done {
		t.Fatalf("merge reconcile: out=%+v err=%v", out, err)
	}

	// drain the per-repo project-OUT loop: the F11 history write fires.
	if _, err := mgr.DrainAll(ctx); err != nil {
		t.Fatalf("drain: %v", err)
	}
	if hist.calls != 1 {
		t.Fatalf("history writer called %d times, want 1", hist.calls)
	}
	if hist.branch != "trunk" {
		t.Fatalf("history landed on %q, want the repo integration branch 'trunk'", hist.branch)
	}
	if len(hist.files) != 2 {
		t.Fatalf("history write must carry the card + the TOC, got %d files", len(hist.files))
	}
	var sawCard, sawTOC bool
	for _, f := range hist.files {
		if strings.Contains(f.Path, "docs/history/"+id+".md") {
			sawCard = true
			if !strings.Contains(f.Content, "Wire the navbar") {
				t.Fatalf("card missing curated title:\n%s", f.Content)
			}
		}
		if strings.HasSuffix(f.Path, "docs/history/README.md") {
			sawTOC = true
		}
	}
	if !sawCard || !sawTOC {
		t.Fatalf("history files missing card=%v toc=%v: %+v", sawCard, sawTOC, hist.files)
	}
}
