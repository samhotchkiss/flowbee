package project

import (
	"context"
	"errors"
	"testing"

	gh "github.com/samhotchkiss/flowbee/internal/github"
)

func TestAuthorizeEpicV2MergeUsesExactMirrorContractAndLiveCI(t *testing.T) {
	st, fake, _, clk := newSender(t)
	mustRegisterEpic(t, st, "russ", "2026-07-03-foo", "epic/2026-07-03-foo", epicSpecPath)
	hist := &fakeHistory{
		diffOut: diffAdding("app/foo/a.go", "// x") +
			diffAdding(epicSpecPath, "- [x] evidence"),
		refTips: map[string]string{"refs/heads/epic/2026-07-03-foo": "epic-head-sha"},
		files:   epicFilesBaseHead(epicFile("done", "none", epicAllGreenChecklist)),
	}
	sender := NewForRepo("russ", "main", st, fake, clk, nil).WithHistory(hist, "main")
	setLiveGreenPR(fake, 42, "base-sha", "epic-head-sha")
	fake.SetPR(gh.PullRequest{Number: 42, HeadRefName: "epic/2026-07-03-foo",
		HeadRefOid: "epic-head-sha", BaseRefOid: "base-sha", CIRollup: gh.CISuccess,
		PassedChecks: []string{"ci"}})

	if err := sender.AuthorizeEpicV2Merge(context.Background(), "2026-07-03-foo", "default", "russ",
		42, "epic/2026-07-03-foo", "base-sha", "epic-head-sha"); err != nil {
		t.Fatalf("authorize exact v2 epic: %v", err)
	}
	if got := hist.fetched; len(got) < 2 || got[0] != "epic/2026-07-03-foo" {
		t.Fatalf("exact epic branch was not fetched before authorization: %v", got)
	}
}

func TestAuthorizeEpicV2MergeDeniesUnsafeDiffAndFencesMovedHead(t *testing.T) {
	st, fake, _, clk := newSender(t)
	mustRegisterEpic(t, st, "russ", "2026-07-03-foo", "epic/2026-07-03-foo", epicSpecPath)
	hist := &fakeHistory{
		diffOut: diffAdding(".github/workflows/pwn.yml", "steal"),
		refTips: map[string]string{"refs/heads/epic/2026-07-03-foo": "epic-head-sha"},
		files:   epicFilesBaseHead(epicFile("done", "none", epicAllGreenChecklist)),
	}
	sender := NewForRepo("russ", "main", st, fake, clk, nil).WithHistory(hist, "main")
	setLiveGreenPR(fake, 42, "base-sha", "epic-head-sha")
	err := sender.AuthorizeEpicV2Merge(context.Background(), "2026-07-03-foo", "default", "russ",
		42, "epic/2026-07-03-foo", "base-sha", "epic-head-sha")
	var denied *MergeAuthorizationDeniedError
	if !errors.As(err, &denied) {
		t.Fatalf("unsafe diff error=%T %v, want durable authorization denial", err, err)
	}

	hist.diffOut = diffAdding("app/foo/a.go", "// safe")
	hist.refTips["refs/heads/epic/2026-07-03-foo"] = "new-head"
	err = sender.AuthorizeEpicV2Merge(context.Background(), "2026-07-03-foo", "default", "russ",
		42, "epic/2026-07-03-foo", "base-sha", "epic-head-sha")
	if !errors.Is(err, gh.ErrMergeHeadModified) {
		t.Fatalf("moved mirror head error=%v, want ErrMergeHeadModified", err)
	}
}
