package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/samhotchkiss/flowbee/internal/store"
	"github.com/samhotchkiss/flowbee/internal/testutil"
	"github.com/samhotchkiss/flowbee/internal/workintent"
)

type epicWorkerMaterialFixture struct {
	store    *store.Store
	provider epicWorkerMaterialProvider
	epic     store.EpicRun
	repoDir  string
	mirror   string
}

func newEpicWorkerMaterialFixture(t *testing.T) epicWorkerMaterialFixture {
	t.Helper()
	ctx := context.Background()
	st := testutil.NewStore(t)
	now := time.Date(2026, 7, 19, 18, 0, 0, 0, time.UTC)
	if _, err := st.CreatePortfolioProject(ctx, store.PortfolioProject{ID: "russ", Name: "Russ"}, now); err != nil {
		t.Fatal(err)
	}
	if err := st.RegisterRepo(ctx, store.Repo{ID: "russ-repo", Owner: "sam", Repo: "russ", DefaultBranch: "main", Active: true}); err != nil {
		t.Fatal(err)
	}
	if err := st.AddProjectRepo(ctx, "russ", "russ-repo", now); err != nil {
		t.Fatal(err)
	}
	binding, err := st.UpsertDriverSessionBinding(ctx, store.DriverSessionBinding{
		ProjectID: "russ", WorkerIdentity: "russ-orchestrator", Role: store.DriverOrchestratorRole,
		HostID: "host-local", StoreID: "store-local", TmuxServerDomainID: "flowbee",
		TmuxServerInstanceID: "server-local", LifecycleOwnership: "driver_managed",
		LifecycleKey: "russ-orchestrator", TargetEpoch: 1, ProfileID: "codex_orchestrator",
		WorkspaceRootID: "flowbee", WorkspaceRelativePath: "russ",
		SessionID: "session-orchestrator", PaneInstanceID: "pane-orchestrator",
		AgentRunID: "run-orchestrator", Provider: "codex",
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	spec := "# Durable review handoff\n\nRecover an interrupted build-to-review handoff.\n"
	intent, err := st.CreateWorkIntent(ctx, store.CreateWorkIntentInput{
		ProjectID: "russ", SourceConversationID: "conversation-1", SourceMessageID: "message-1",
		SourceMessageVersion: 1, InteractorIncarnationID: "interactor-run-1",
		Title: "Durable review handoff", ArtifactRef: "flowbee://projects/russ/artifacts/review-handoff/v1",
		ArtifactSHA256: sha256Text(spec), IntentVersion: 1, DefinitionComplete: true,
		OwnerActorID: "russ-interactor", OrchestratorRegistration: "russ-orchestrator",
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB.ExecContext(ctx, `UPDATE work_intents SET state='orchestrating',state_version=2 WHERE id=?`, intent.ID); err != nil {
		t.Fatal(err)
	}
	intent, err = st.GetWorkIntent(ctx, "russ", intent.ID)
	if err != nil {
		t.Fatal(err)
	}
	contract := store.WorkIntentEpicContract{Slug: "review-handoff", Title: "Durable review handoff",
		Repositories: []string{"russ-repo"}, DeliveryRepo: "russ-repo",
		SpecPath: "epics/review-handoff.md", Scope: []string{"internal/**"},
		Acceptance: []string{"an interrupted dispatch self-heals"}}
	contractHash, err := store.WorkIntentEpicContractSHA256(contract)
	if err != nil {
		t.Fatal(err)
	}
	key, err := workintent.AdmissionKey(workintent.Intent{ID: intent.ID, ProjectID: "russ", Version: 1})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.RecordWorkIntentEpicContract(ctx, store.RecordWorkIntentEpicContractInput{
		ProjectID: "russ", WorkIntentID: intent.ID, IntentVersion: 1,
		ExpectedStateVersion: intent.StateVersion, SourceArtifactSHA256: sha256Text(spec),
		ContractVersion: 1, ContractRef: "flowbee://projects/russ/contracts/review-handoff/v1",
		ContractSHA256: contractHash, Contract: contract, OrchestratorBindingID: binding.BindingID,
		SubmissionKey: key,
	}, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}

	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	repoDir := filepath.Join(root, "source")
	if err := os.MkdirAll(filepath.Join(repoDir, "epics"), 0o700); err != nil {
		t.Fatal(err)
	}
	runGitFixture(t, "", "init", "-b", "main", repoDir)
	if err := os.WriteFile(filepath.Join(repoDir, contract.SpecPath), []byte(spec), 0o600); err != nil {
		t.Fatal(err)
	}
	runGitFixture(t, repoDir, "config", "user.email", "flowbee@example.invalid")
	runGitFixture(t, repoDir, "config", "user.name", "Flowbee Test")
	runGitFixture(t, repoDir, "add", "--", contract.SpecPath)
	runGitFixture(t, repoDir, "commit", "-m", "admit exact epic spec")
	mirror := filepath.Join(root, "russ-repo.git")
	runGitFixture(t, "", "clone", "--bare", repoDir, mirror)

	provider := newEpicWorkerMaterialProvider(st)
	provider.MirrorPath = func(repo store.Repo) string {
		if repo.ID != "russ-repo" {
			t.Fatalf("wrong mirror repository requested: %q", repo.ID)
		}
		return mirror
	}
	return epicWorkerMaterialFixture{store: st, provider: provider, repoDir: repoDir, mirror: mirror,
		epic: store.EpicRun{ID: "epic-review-handoff", ProjectID: "russ", Slug: contract.Slug,
			WorkIntentID: intent.ID, IntentVersion: 1, ContractHash: contractHash,
			Repositories: []string{"russ-repo"}, DeliveryRepo: "russ-repo", Repo: "russ-repo",
			FilePath: contract.SpecPath, Title: contract.Title, Scope: contract.Scope,
			Branch: "epic/russ/review-handoff"}}
}

func runGitFixture(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
}

func TestEpicWorkerMaterialProviderResolvesExactAdmittedBytes(t *testing.T) {
	f := newEpicWorkerMaterialFixture(t)
	material, err := f.provider.Resolve(context.Background(), f.epic)
	if err != nil {
		t.Fatal(err)
	}
	if material.GoalFormat != store.EpicWorkerGoalFormat ||
		material.SourceArtifactSHA256 != sha256Text(material.EpicSpecGoalUTF8) ||
		material.AdmissionContractSHA256 != f.epic.ContractHash ||
		material.BuilderDisciplineUTF8 != epicBuilderDisciplineV1 ||
		material.ReviewerDisciplineUTF8 != epicReviewerDisciplineV1 || len(material.ReferenceDocuments) != 2 {
		t.Fatalf("material did not preserve exact admitted bytes: %+v", material)
	}
	for _, doc := range material.ReferenceDocuments {
		if doc.SHA256 != sha256Text(doc.ContentUTF8) || !strings.HasPrefix(doc.Reference, "flowbee://") {
			t.Fatalf("reference is not content-addressed: %+v", doc)
		}
	}
}

func TestEpicWorkerMaterialProviderRejectsStaleSpec(t *testing.T) {
	f := newEpicWorkerMaterialFixture(t)
	if err := os.WriteFile(filepath.Join(f.repoDir, f.epic.FilePath), []byte("# moved without new admission\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGitFixture(t, f.repoDir, "add", "--", f.epic.FilePath)
	runGitFixture(t, f.repoDir, "commit", "-m", "move source")
	runGitFixture(t, "", "--git-dir", f.mirror, "fetch", f.repoDir, "+main:main")
	if _, err := f.provider.Resolve(context.Background(), f.epic); err == nil || !strings.Contains(err.Error(), "stale") {
		t.Fatalf("stale source artifact accepted: %v", err)
	}
}

func TestEpicWorkerMaterialProviderRejectsStaleDisciplineAndReference(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*epicWorkerMaterialProvider)
	}{
		{"discipline", func(p *epicWorkerMaterialProvider) { p.Builder.content += "changed" }},
		{"reference", func(p *epicWorkerMaterialProvider) { p.References[0].content += "changed" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newEpicWorkerMaterialFixture(t)
			tc.mutate(&f.provider)
			if _, err := f.provider.Resolve(context.Background(), f.epic); err == nil || !strings.Contains(err.Error(), "version hash") {
				t.Fatalf("stale %s accepted: %v", tc.name, err)
			}
		})
	}
}

func TestEpicWorkerMaterialProviderRejectsPathEscapeAndSymlink(t *testing.T) {
	t.Run("contract path escape", func(t *testing.T) {
		f := newEpicWorkerMaterialFixture(t)
		f.epic.FilePath = "epics/../outside.md"
		if _, err := f.provider.Resolve(context.Background(), f.epic); err == nil {
			t.Fatal("path escape accepted")
		}
	})
	t.Run("git symlink", func(t *testing.T) {
		f := newEpicWorkerMaterialFixture(t)
		if err := os.Remove(filepath.Join(f.repoDir, f.epic.FilePath)); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink("../outside.md", filepath.Join(f.repoDir, f.epic.FilePath)); err != nil {
			t.Fatal(err)
		}
		runGitFixture(t, f.repoDir, "add", "--", f.epic.FilePath)
		runGitFixture(t, f.repoDir, "commit", "-m", "replace source with symlink")
		runGitFixture(t, "", "--git-dir", f.mirror, "fetch", f.repoDir, "+main:main")
		if _, err := f.provider.Resolve(context.Background(), f.epic); err == nil || !strings.Contains(err.Error(), "symlink") {
			t.Fatalf("git symlink accepted: %v", err)
		}
	})
}

func TestEpicWorkerMaterialProviderRejectsWrongProjectOrRepository(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*store.EpicRun)
	}{
		{"project", func(e *store.EpicRun) { e.ProjectID = "other" }},
		{"delivery repository", func(e *store.EpicRun) { e.DeliveryRepo, e.Repo = "other", "other" }},
		{"repository set", func(e *store.EpicRun) { e.Repositories = []string{"other"} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newEpicWorkerMaterialFixture(t)
			tc.mutate(&f.epic)
			if _, err := f.provider.Resolve(context.Background(), f.epic); err == nil {
				t.Fatalf("wrong %s authority accepted", tc.name)
			}
		})
	}
}

func TestEpicWorkerMaterialProviderRejectsSymlinkMirrorAuthority(t *testing.T) {
	f := newEpicWorkerMaterialFixture(t)
	link := filepath.Join(filepath.Dir(f.mirror), "mirror-link.git")
	if err := os.Symlink(f.mirror, link); err != nil {
		t.Fatal(err)
	}
	f.provider.MirrorPath = func(store.Repo) string { return link }
	if _, err := f.provider.Resolve(context.Background(), f.epic); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("symlink mirror authority accepted: %v", err)
	}
}

func TestEpicWorkerMaterialFailureCommitsNoWorkerOrActionRows(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*testing.T, *epicWorkerMaterialFixture)
	}{
		{"mirror replacement symlink", func(t *testing.T, f *epicWorkerMaterialFixture) {
			link := filepath.Join(filepath.Dir(f.mirror), "replacement-mirror.git")
			if err := os.Symlink(f.mirror, link); err != nil {
				t.Fatal(err)
			}
			f.provider.MirrorPath = func(store.Repo) string { return link }
		}},
		{"intent version mismatch", func(_ *testing.T, f *epicWorkerMaterialFixture) {
			f.epic.IntentVersion++
		}},
		{"source bytes mismatch", func(t *testing.T, f *epicWorkerMaterialFixture) {
			if err := os.WriteFile(filepath.Join(f.repoDir, f.epic.FilePath), []byte("# stale replacement\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			runGitFixture(t, f.repoDir, "add", "--", f.epic.FilePath)
			runGitFixture(t, f.repoDir, "commit", "-m", "replace admitted source")
			runGitFixture(t, "", "--git-dir", f.mirror, "fetch", f.repoDir, "+main:main")
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newEpicWorkerMaterialFixture(t)
			tc.mutate(t, &f)
			f.store.EnableEpicDedicatedWorkersV2 = true
			f.store.EpicWorkerBootstrapMaterialProvider = f.provider.Resolve
			if err := f.store.AddEpicRun(context.Background(), f.epic, 1,
				time.Date(2026, 7, 19, 18, 5, 0, 0, time.UTC)); err == nil {
				t.Fatalf("%s did not fail closed", tc.name)
			}
			var epics, workers, actions int
			if err := f.store.DB.QueryRow(`SELECT COUNT(*) FROM epics WHERE id=?`, f.epic.ID).Scan(&epics); err != nil {
				t.Fatal(err)
			}
			if err := f.store.DB.QueryRow(`SELECT COUNT(*) FROM epic_worker_sessions WHERE epic_id=?`, f.epic.ID).Scan(&workers); err != nil {
				t.Fatal(err)
			}
			if err := f.store.DB.QueryRow(`SELECT COUNT(*) FROM epic_actions WHERE epic_id=?`, f.epic.ID).Scan(&actions); err != nil {
				t.Fatal(err)
			}
			if epics != 0 || workers != 0 || actions != 0 {
				t.Fatalf("failed material resolution mutated workflow epics=%d workers=%d actions=%d",
					epics, workers, actions)
			}
		})
	}
}
