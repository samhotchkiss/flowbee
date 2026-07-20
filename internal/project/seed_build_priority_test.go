package project

import (
	"context"
	"testing"
	"time"

	"github.com/samhotchkiss/flowbee/internal/gitops"
	"github.com/samhotchkiss/flowbee/internal/job"
	"github.com/samhotchkiss/flowbee/internal/store"
)

// fakeHist satisfies HistoryWriter; only HeadSHA matters for seedBuildFromSpec (it resolves
// the build's base_sha). The rest are inert.
type fakeHist struct{}

func (fakeHist) CommitHistory(string, string, []gitops.HistoryFile) (string, bool, error) {
	return "", false, nil
}

func TestSeedBuildFromSpecInheritsProjectAuthority(t *testing.T) {
	ctx := context.Background()
	st, fake, _, clk := newSender(t)
	if _, err := st.CreatePortfolioProject(ctx, store.PortfolioProject{ID: "mail", Name: "Mail"}, clk.Now()); err != nil {
		t.Fatal(err)
	}
	sender := New(st, fake, clk, nil).WithHistory(fakeHist{}, "main")
	buildID, err := sender.seedBuildFromSpec(ctx, job.Job{
		ID: "mail-spec", ProjectID: "mail", Kind: job.KindSpec,
		SpecText: "build mail", AcceptanceCriteria: "mail tests pass",
	}, clk.Now().Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	build, err := st.GetJob(ctx, buildID)
	if err != nil {
		t.Fatal(err)
	}
	if build.ProjectID != "mail" {
		t.Fatalf("child build project=%q want mail", build.ProjectID)
	}
}
func (fakeHist) HeadSHA(string) (string, error)             { return "mainsha", nil }
func (fakeHist) FetchBranch(string) error                   { return nil }
func (fakeHist) DiffBetween(string, string) (string, error) { return "", nil }
func (fakeHist) ReadFileAtRef(string, string) (string, bool, error) {
	return "", false, nil
}

// TestSeedBuildFromSpecInheritsPriority: a build descending from a signed-off spec inherits
// the spec's urgency (1..10, lower = more urgent), NOT the bare INSERT default 0 — which
// under the new ordering would sort the build as MORE urgent than 1 and jump every
// spec-flow build to the front of the queue. A spec at the default 5 yields a default build;
// an unset (0) spec normalizes to 5.
func TestSeedBuildFromSpecInheritsPriority(t *testing.T) {
	ctx := context.Background()

	for _, tc := range []struct {
		name     string
		specPrio int
		want     int
	}{
		{"urgent spec -> urgent build", 2, 2},
		{"nice-to-have spec -> nice build", 9, 9},
		{"default spec -> default build", 5, 5},
		{"unset (0) spec -> default 5 build", 0, 5},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st, fake, _, clk := newSender(t)
			sender := New(st, fake, clk, nil).WithHistory(fakeHist{}, "main")
			spec := job.Job{
				ID: tc.name, Kind: job.KindSpec, Priority: tc.specPrio,
				SpecText: "build the thing", AcceptanceCriteria: "done when X",
			}
			buildID, err := sender.seedBuildFromSpec(ctx, spec, clk.Now())
			if err != nil {
				t.Fatalf("seedBuildFromSpec: %v", err)
			}
			b, err := st.GetJob(ctx, buildID)
			if err != nil {
				t.Fatalf("get build: %v", err)
			}
			if b.Priority != tc.want {
				t.Fatalf("build priority = %d, want %d (inherited from spec priority %d)", b.Priority, tc.want, tc.specPrio)
			}
		})
	}
}
