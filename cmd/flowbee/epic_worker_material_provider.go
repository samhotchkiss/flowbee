package main

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/samhotchkiss/flowbee/internal/gitops"
	"github.com/samhotchkiss/flowbee/internal/store"
)

// These documents are part of the pinned Flowbee binary, not discovered from
// the process CWD. Changing bytes without also changing the explicit digest is
// a startup-visible error instead of a silent change to an admitted worker's
// operating contract.
const (
	epicBuilderDisciplineV1 = `Flowbee dedicated builder discipline (v1)

Work only from the immutable epic goal and its declared scope. Plant a failing
proof before changing behavior, keep commits reviewable, report progress through
Flowbee, and stop on missing authority. Never infer project, repository, branch,
credentials, or routing from CWD, tmux names, display names, prose, or proximity.
`
	epicBuilderDisciplineV1SHA256 = "sha256:2414c2840837545e1a8c545837726449bf9552f77a63b925d55f98626bdd6b19"

	epicReviewerDisciplineV1 = `Flowbee dedicated reviewer discipline (v1)

Review the exact immutable head/base and epic acceptance contract. Independently
falsify the implementation, cite mechanical evidence, and bind the verdict to the
reviewed SHA and action epoch. A transport receipt or agent prose is never stage
success. Stop on stale identity, moved content, missing evidence, or route ambiguity.
`
	epicReviewerDisciplineV1SHA256 = "sha256:aa39e4d8e4b7a5241f002642fe0d4021322013658effdfb301b90cb88e00cb6e"

	epicWorkerEvidenceGuideV1 = `Flowbee dedicated worker evidence guide (v1)

The bootstrap goal, admission contract, role discipline, and reference manifest
are immutable and content-addressed. GitHub and CI authority remains with Flowbee.
Workers authenticate only to the Flowbee control plane and communicate with other
managed sessions only through Driver grants and receipts. Driver transport success
does not prove build, review, CI, merge, or cleanup completion.
`
	epicWorkerEvidenceGuideV1SHA256 = "sha256:0f7e1b91e58bb0ca3077b2bc0da85627131b1feaf1f3a5fd444afe5a1a4cd1e9"
)

type pinnedEpicWorkerDocument struct {
	reference, format, content, sha256 string
}

// epicWorkerMaterialProvider resolves bytes only from stable admission identity:
// the exact project/work-intent/version contract in SQLite plus the registered
// delivery repository's local control-plane mirror. Flowbee embeds the exact
// content-addressed public bytes in Driver's single initial_prompt_utf8/v1
// lifecycle bootstrap artifact at Ensure time. There is no separate workspace
// materialization operation; this provider never writes into or discovers a
// workspace itself.
type epicWorkerMaterialProvider struct {
	Store      *store.Store
	MirrorPath func(store.Repo) string
	Builder    pinnedEpicWorkerDocument
	Reviewer   pinnedEpicWorkerDocument
	References []pinnedEpicWorkerDocument
}

func newEpicWorkerMaterialProvider(st *store.Store) epicWorkerMaterialProvider {
	return epicWorkerMaterialProvider{
		Store:      st,
		MirrorPath: controlMirrorFor,
		Builder: pinnedEpicWorkerDocument{
			reference: "flowbee://disciplines/dedicated-builder/v1", format: "text/markdown",
			content: epicBuilderDisciplineV1, sha256: epicBuilderDisciplineV1SHA256,
		},
		Reviewer: pinnedEpicWorkerDocument{
			reference: "flowbee://disciplines/dedicated-reviewer/v1", format: "text/markdown",
			content: epicReviewerDisciplineV1, sha256: epicReviewerDisciplineV1SHA256,
		},
		References: []pinnedEpicWorkerDocument{{
			reference: "flowbee://references/dedicated-worker-evidence/v1", format: "text/markdown",
			content: epicWorkerEvidenceGuideV1, sha256: epicWorkerEvidenceGuideV1SHA256,
		}},
	}
}

type epicWorkerAdmissionMaterial struct {
	ProjectID, WorkIntentID, ArtifactRef, SourceArtifactSHA256 string
	IntentVersion                                              int
	ContractRef, ContractSourceArtifactSHA256                  string
	ContractSHA256, ContractJSON, ContractState                string
	AdmittedEpicID                                             string
	Contract                                                   store.WorkIntentEpicContract
}

func (p epicWorkerMaterialProvider) Resolve(ctx context.Context, e store.EpicRun) (store.EpicWorkerBootstrapMaterials, error) {
	if p.Store == nil || p.Store.DB == nil || p.MirrorPath == nil {
		return store.EpicWorkerBootstrapMaterials{}, errors.New("epic worker material provider is not configured")
	}
	if e.ProjectID == "" || e.WorkIntentID == "" || e.IntentVersion < 1 || e.ContractHash == "" {
		return store.EpicWorkerBootstrapMaterials{}, errors.New("epic worker material requires stable project/work-intent/version/contract identity")
	}
	authority, err := p.resolveAdmissionAuthority(ctx, e)
	if err != nil {
		return store.EpicWorkerBootstrapMaterials{}, err
	}
	if err := validateEpicWorkerAdmissionProjection(e, authority); err != nil {
		return store.EpicWorkerBootstrapMaterials{}, err
	}
	for _, repoID := range authority.Contract.Repositories {
		if err := p.Store.AssertProjectRepoMembership(ctx, authority.ProjectID, repoID); err != nil {
			return store.EpicWorkerBootstrapMaterials{}, fmt.Errorf("epic worker repository authority %q: %w", repoID, err)
		}
	}
	repo, err := p.Store.GetRepo(ctx, authority.Contract.DeliveryRepo)
	if err != nil || !repo.Active {
		if err == nil {
			err = errors.New("repository is inactive")
		}
		return store.EpicWorkerBootstrapMaterials{}, fmt.Errorf("epic worker delivery repository: %w", err)
	}
	mirrorPath, err := exactLocalMirrorPath(p.MirrorPath(repo))
	if err != nil {
		return store.EpicWorkerBootstrapMaterials{}, err
	}
	mirror := gitops.Open(mirrorPath)
	ref := "refs/heads/" + repo.DefaultBranch
	commit, err := mirror.HeadSHA(ref)
	if err != nil || commit == "" {
		return store.EpicWorkerBootstrapMaterials{}, fmt.Errorf("resolve exact epic source commit %s: %w", ref, err)
	}
	if err := requireRegularGitBlob(ctx, mirrorPath, commit, authority.Contract.SpecPath); err != nil {
		return store.EpicWorkerBootstrapMaterials{}, err
	}
	spec, ok, err := mirror.ReadFileAtRef(commit, authority.Contract.SpecPath)
	if err != nil || !ok {
		if err == nil {
			err = errors.New("source artifact is missing")
		}
		return store.EpicWorkerBootstrapMaterials{}, fmt.Errorf("read exact epic source artifact: %w", err)
	}
	if sha256Text(spec) != authority.SourceArtifactSHA256 {
		return store.EpicWorkerBootstrapMaterials{}, errors.New("epic worker source artifact bytes are stale or do not match admitted hash")
	}
	if err := verifyPinnedEpicWorkerDocument(p.Builder); err != nil {
		return store.EpicWorkerBootstrapMaterials{}, fmt.Errorf("builder discipline: %w", err)
	}
	if err := verifyPinnedEpicWorkerDocument(p.Reviewer); err != nil {
		return store.EpicWorkerBootstrapMaterials{}, fmt.Errorf("reviewer discipline: %w", err)
	}

	docs := make([]store.EpicWorkerReferenceDocument, 0, len(p.References)+1)
	for _, source := range p.References {
		if err := verifyPinnedEpicWorkerDocument(source); err != nil {
			return store.EpicWorkerBootstrapMaterials{}, fmt.Errorf("reference document %q: %w", source.reference, err)
		}
		docs = append(docs, store.EpicWorkerReferenceDocument{Reference: source.reference,
			Format: source.format, ContentUTF8: source.content, SHA256: source.sha256})
	}
	// The exact canonical Orchestrator contract is itself a reference document.
	// Its DB-pinned hash is independently recomputed above, so the worker sees the
	// same acceptance/scope/repository bytes which authorized admission.
	docs = append(docs, store.EpicWorkerReferenceDocument{
		Reference: "flowbee://projects/" + authority.ProjectID + "/work-intents/" + authority.WorkIntentID +
			"/epic-contract/v" + fmt.Sprint(authority.IntentVersion),
		Format: "application/json", ContentUTF8: authority.ContractJSON, SHA256: authority.ContractSHA256,
	})
	sort.Slice(docs, func(i, j int) bool { return docs[i].Reference < docs[j].Reference })
	return store.EpicWorkerBootstrapMaterials{
		GoalFormat: store.EpicWorkerGoalFormat, EpicSpecGoalUTF8: spec,
		AdmissionContractSHA256: authority.ContractSHA256,
		SourceArtifactSHA256:    authority.SourceArtifactSHA256,
		SourceCommitSHA:         commit,
		BuilderDisciplineUTF8:   p.Builder.content,
		ReviewerDisciplineUTF8:  p.Reviewer.content,
		ReferenceDocuments:      docs,
	}, nil
}

func (p epicWorkerMaterialProvider) resolveAdmissionAuthority(ctx context.Context, e store.EpicRun) (epicWorkerAdmissionMaterial, error) {
	var out epicWorkerAdmissionMaterial
	var admitted sql.NullString
	err := p.Store.DB.QueryRowContext(ctx, `SELECT w.project_id,w.id,w.intent_version,w.artifact_ref,
		w.artifact_sha256,c.contract_ref,c.source_artifact_sha256,c.contract_sha256,c.contract_json,c.state,c.admitted_epic_id
		FROM work_intents w JOIN work_intent_epic_contracts c
		  ON c.project_id=w.project_id AND c.work_intent_id=w.id AND c.intent_version=w.intent_version
		WHERE w.project_id=? AND w.id=? AND w.intent_version=?`, e.ProjectID, e.WorkIntentID, e.IntentVersion).
		Scan(&out.ProjectID, &out.WorkIntentID, &out.IntentVersion, &out.ArtifactRef,
			&out.SourceArtifactSHA256, &out.ContractRef, &out.ContractSourceArtifactSHA256, &out.ContractSHA256,
			&out.ContractJSON, &out.ContractState, &admitted)
	if errors.Is(err, sql.ErrNoRows) {
		return out, errors.New("epic worker admitted source identity is missing")
	}
	if err != nil {
		return out, fmt.Errorf("resolve epic worker admitted source identity: %w", err)
	}
	out.AdmittedEpicID = admitted.String
	if out.ArtifactRef == "" || out.ContractRef == "" ||
		(out.ContractState != "prepared" && out.ContractState != "admitted") {
		return out, errors.New("epic worker admitted source identity is incomplete or not live")
	}
	if err := json.Unmarshal([]byte(out.ContractJSON), &out.Contract); err != nil {
		return out, fmt.Errorf("decode admitted epic contract: %w", err)
	}
	hash, err := store.WorkIntentEpicContractSHA256(out.Contract)
	if err != nil || hash != out.ContractSHA256 {
		return out, errors.New("admitted epic contract bytes or hash are stale")
	}
	if out.SourceArtifactSHA256 == "" || out.ContractSHA256 == "" ||
		out.ContractSourceArtifactSHA256 != out.SourceArtifactSHA256 {
		return out, errors.New("epic worker admitted source hashes are missing or disagree")
	}
	return out, nil
}

func validateEpicWorkerAdmissionProjection(e store.EpicRun, a epicWorkerAdmissionMaterial) error {
	deliveryRepo := e.DeliveryRepo
	if deliveryRepo == "" {
		deliveryRepo = e.Repo // legacy/backfill projection; the contract remains authority.
	}
	if a.ProjectID != e.ProjectID || a.WorkIntentID != e.WorkIntentID || a.IntentVersion != e.IntentVersion ||
		a.ContractSHA256 != e.ContractHash || a.Contract.DeliveryRepo != deliveryRepo ||
		a.Contract.SpecPath != e.FilePath || a.Contract.Slug != e.Slug {
		return errors.New("epic worker admission projection does not match authoritative contract")
	}
	if a.AdmittedEpicID != "" && a.AdmittedEpicID != e.ID {
		return errors.New("epic worker contract is already bound to another epic")
	}
	if len(e.Repositories) > 0 {
		wantRepos := append([]string(nil), a.Contract.Repositories...)
		gotRepos := append([]string(nil), e.Repositories...)
		sort.Strings(wantRepos)
		sort.Strings(gotRepos)
		if strings.Join(wantRepos, "\x00") != strings.Join(gotRepos, "\x00") {
			return errors.New("epic worker repository set does not match authoritative contract")
		}
	}
	return nil
}

func verifyPinnedEpicWorkerDocument(doc pinnedEpicWorkerDocument) error {
	if doc.reference == "" || doc.format == "" || doc.content == "" || doc.sha256 == "" {
		return errors.New("pinned document is incomplete")
	}
	if sha256Text(doc.content) != doc.sha256 {
		return errors.New("pinned document bytes do not match version hash")
	}
	return nil
}

func sha256Text(value string) string {
	h := sha256.Sum256([]byte(value))
	return "sha256:" + hex.EncodeToString(h[:])
}

func exactLocalMirrorPath(value string) (string, error) {
	if value == "" || !filepath.IsAbs(value) || filepath.Clean(value) != value {
		return "", errors.New("epic worker mirror must be an exact absolute local path")
	}
	current := filepath.VolumeName(value) + string(filepath.Separator)
	for _, part := range strings.Split(strings.TrimPrefix(value, current), string(filepath.Separator)) {
		if part == "" {
			continue
		}
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if err != nil {
			return "", fmt.Errorf("epic worker mirror path: %w", err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return "", errors.New("epic worker mirror path may not traverse symlinks")
		}
	}
	info, err := os.Stat(value)
	if err != nil || !info.IsDir() {
		return "", errors.New("epic worker mirror path is not a local directory")
	}
	return value, nil
}

func requireRegularGitBlob(ctx context.Context, mirrorPath, commit, repoPath string) error {
	clean := path.Clean(repoPath)
	if clean != repoPath || clean == "." || path.IsAbs(clean) || strings.HasPrefix(clean, "../") ||
		strings.ContainsRune(clean, '\x00') {
		return errors.New("epic worker source path escapes repository authority")
	}
	cmd := exec.CommandContext(ctx, "git", "--git-dir", mirrorPath, "ls-tree", commit, "--", repoPath)
	out, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("inspect exact epic source artifact: %w", err)
	}
	line := strings.TrimSpace(string(out))
	fields := strings.Fields(line)
	if len(fields) < 4 || fields[0] != "100644" || fields[1] != "blob" ||
		!strings.HasSuffix(line, "\t"+repoPath) {
		return errors.New("epic worker source artifact is missing, ambiguous, executable, or a symlink")
	}
	return nil
}
