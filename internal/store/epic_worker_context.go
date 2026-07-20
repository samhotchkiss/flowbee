package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"
)

const EpicWorkerGoalFormat = "flowbee.epic-goal-markdown/v1"

const (
	driverLifecycleBootstrapMaxBytes = 16 * 1024
	epicWorkerSpecMaxBytes           = 12 * 1024
	epicWorkerDisciplineMaxBytes     = 4 * 1024
	epicWorkerReferenceMaxBytes      = 1024 * 1024
	epicWorkerReferenceMaxCount      = 64
)

// EpicWorkerReferenceDocument is an immutable, content-addressed reference
// delivered to an epic worker. Reference is a Flowbee-owned logical reference,
// never a CWD-relative authority discovered by the worker.
type EpicWorkerReferenceDocument struct {
	Reference   string `json:"reference"`
	Format      string `json:"format"`
	ContentUTF8 string `json:"content_utf8"`
	SHA256      string `json:"sha256"`
}

// EpicWorkerBootstrapMaterials are the authoritative bytes from which both
// dedicated worker manifests are committed. The caller obtains these bytes
// from the admitted epic artifact and versioned discipline documents before
// Store opens the admission transaction. Store never reads a path/CWD or
// invents prose when these bytes are unavailable.
type EpicWorkerBootstrapMaterials struct {
	GoalFormat              string
	EpicSpecGoalUTF8        string
	AdmissionContractSHA256 string
	SourceArtifactSHA256    string
	// SourceCommitSHA is the immutable local-mirror commit which contained the
	// admitted artifact bytes. It is captured before admission and later copied
	// into each lifecycle action; workspace preparation must never resolve a
	// moving branch head after the action has been committed.
	SourceCommitSHA        string
	BuilderDisciplineUTF8  string
	ReviewerDisciplineUTF8 string
	ReferenceDocuments     []EpicWorkerReferenceDocument
}

// EpicWorkerBootstrapMaterialProvider is the production I/O seam for resolving
// immutable worker context. It is always invoked before an admission or
// activation transaction; implementations may read the control-plane mirror or
// another authoritative material store, but must not infer authority from CWD,
// tmux names, or display names.
type EpicWorkerBootstrapMaterialProvider func(context.Context, EpicRun) (EpicWorkerBootstrapMaterials, error)

func sha256String(value string) string {
	h := sha256.Sum256([]byte(value))
	return "sha256:" + hex.EncodeToString(h[:])
}

func normalizeEpicWorkerBootstrapMaterials(in EpicWorkerBootstrapMaterials) (EpicWorkerBootstrapMaterials, error) {
	if in.GoalFormat == "" {
		in.GoalFormat = EpicWorkerGoalFormat
	}
	if in.GoalFormat != EpicWorkerGoalFormat {
		return EpicWorkerBootstrapMaterials{}, fmt.Errorf("unsupported epic worker goal format %q", in.GoalFormat)
	}
	if in.EpicSpecGoalUTF8 == "" {
		return EpicWorkerBootstrapMaterials{}, errors.New("epic worker context requires exact epic spec goal bytes")
	}
	if !validEpicWorkerUTF8(in.EpicSpecGoalUTF8, epicWorkerSpecMaxBytes) {
		return EpicWorkerBootstrapMaterials{}, errors.New("epic worker spec bytes are invalid UTF-8, contain NUL, or exceed 12 KiB")
	}
	if in.SourceArtifactSHA256 != sha256String(in.EpicSpecGoalUTF8) {
		return EpicWorkerBootstrapMaterials{}, errors.New("epic worker spec bytes do not match the authoritative source artifact hash")
	}
	if !validGitObjectID(in.SourceCommitSHA) {
		return EpicWorkerBootstrapMaterials{}, errors.New("epic worker context requires an immutable source commit SHA")
	}
	if !validSHA256Text(in.AdmissionContractSHA256) {
		return EpicWorkerBootstrapMaterials{}, errors.New("epic worker context requires a valid admission contract hash")
	}
	if in.BuilderDisciplineUTF8 == "" {
		return EpicWorkerBootstrapMaterials{}, errors.New("epic worker context requires builder discipline bytes")
	}
	if in.ReviewerDisciplineUTF8 == "" {
		return EpicWorkerBootstrapMaterials{}, errors.New("epic worker context requires reviewer discipline bytes")
	}
	if !validEpicWorkerUTF8(in.BuilderDisciplineUTF8, epicWorkerDisciplineMaxBytes) ||
		!validEpicWorkerUTF8(in.ReviewerDisciplineUTF8, epicWorkerDisciplineMaxBytes) {
		return EpicWorkerBootstrapMaterials{}, errors.New("epic worker discipline bytes are invalid UTF-8, contain NUL, or exceed 4 KiB")
	}
	if len(in.ReferenceDocuments) == 0 {
		return EpicWorkerBootstrapMaterials{}, errors.New("epic worker context requires a reference-document manifest")
	}
	if len(in.ReferenceDocuments) > epicWorkerReferenceMaxCount {
		return EpicWorkerBootstrapMaterials{}, errors.New("epic worker reference-document manifest exceeds 64 entries")
	}
	in.ReferenceDocuments = append([]EpicWorkerReferenceDocument(nil), in.ReferenceDocuments...)
	sort.Slice(in.ReferenceDocuments, func(i, j int) bool {
		return in.ReferenceDocuments[i].Reference < in.ReferenceDocuments[j].Reference
	})
	for i := range in.ReferenceDocuments {
		doc := &in.ReferenceDocuments[i]
		if !validEpicWorkerReference(doc.Reference) || !validEpicWorkerReferenceFormat(doc.Format) ||
			!validEpicWorkerUTF8(doc.ContentUTF8, epicWorkerReferenceMaxBytes) {
			return EpicWorkerBootstrapMaterials{}, fmt.Errorf("epic worker reference document %d is incomplete", i)
		}
		if i > 0 && doc.Reference == in.ReferenceDocuments[i-1].Reference {
			return EpicWorkerBootstrapMaterials{}, fmt.Errorf("duplicate epic worker reference %q", doc.Reference)
		}
		actual := sha256String(doc.ContentUTF8)
		if doc.SHA256 != "" && doc.SHA256 != actual {
			return EpicWorkerBootstrapMaterials{}, fmt.Errorf("epic worker reference %q hash mismatch", doc.Reference)
		}
		doc.SHA256 = actual
	}
	return in, nil
}

func validGitObjectID(value string) bool {
	if len(value) != 40 && len(value) != 64 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func (s *Store) resolveEpicWorkerBootstrapMaterials(ctx context.Context, e EpicRun) (*EpicWorkerBootstrapMaterials, error) {
	if e.WorkerBootstrapMaterials != nil {
		material, err := normalizeEpicWorkerBootstrapMaterials(*e.WorkerBootstrapMaterials)
		if err == nil && e.ContractHash != "" && material.AdmissionContractSHA256 != e.ContractHash {
			return nil, errors.New("epic worker context admission contract hash mismatch")
		}
		return &material, err
	}
	if s.EpicWorkerBootstrapMaterialProvider == nil {
		return nil, errors.New("epic worker bootstrap material provider unavailable")
	}
	material, err := s.EpicWorkerBootstrapMaterialProvider(ctx, e)
	if err != nil {
		return nil, fmt.Errorf("resolve authoritative epic worker context: %w", err)
	}
	material, err = normalizeEpicWorkerBootstrapMaterials(material)
	if err != nil {
		return nil, err
	}
	if e.ContractHash != "" && material.AdmissionContractSHA256 != e.ContractHash {
		return nil, errors.New("epic worker context admission contract hash mismatch")
	}
	return &material, nil
}

func epicWorkerReferenceManifestHash(docs []EpicWorkerReferenceDocument) string {
	var b strings.Builder
	for _, doc := range docs {
		b.WriteString(doc.Reference)
		b.WriteByte(0)
		b.WriteString(doc.Format)
		b.WriteByte(0)
		b.WriteString(doc.SHA256)
		b.WriteByte('\n')
	}
	return sha256String(b.String())
}

func validEpicWorkerUTF8(value string, maximum int) bool {
	return value != "" && len(value) <= maximum && utf8.ValidString(value) && !strings.ContainsRune(value, '\x00')
}

func validSHA256Text(value string) bool {
	if !strings.HasPrefix(value, "sha256:") || len(value) != len("sha256:")+64 {
		return false
	}
	_, err := hex.DecodeString(strings.TrimPrefix(value, "sha256:"))
	return err == nil
}

func validEpicWorkerReference(value string) bool {
	if len(value) > 512 || !strings.HasPrefix(value, "flowbee://") || !utf8.ValidString(value) ||
		strings.ContainsRune(value, '\x00') {
		return false
	}
	for _, r := range value {
		if r <= ' ' || r == 0x7f {
			return false
		}
	}
	return true
}

func validEpicWorkerReferenceFormat(value string) bool {
	switch value {
	case "text/markdown", "text/plain", "application/json":
		return true
	default:
		return false
	}
}

func epicWorkerLifecycleBootstrapSize(bootstrapPayload, actionPayload string) int {
	const publicPrefix = "FLOWBEE MANAGED WORKER BOOTSTRAP\n"
	const actionPrefix = "\nFLOWBEE FENCED LIFECYCLE ACTION\n"
	return len(publicPrefix) + len(bootstrapPayload) + len(actionPrefix) + len(actionPayload)
}

func validateEpicWorkerLifecycleBootstrapSizeTx(ctx context.Context, tx *sql.Tx, epicID, role,
	actionPayload string) error {
	var bootstrapPayload string
	if err := tx.QueryRowContext(ctx, `SELECT bootstrap_payload FROM epic_worker_sessions
		WHERE epic_id=? AND worker_role=?`, epicID, role).Scan(&bootstrapPayload); err != nil {
		return err
	}
	size := epicWorkerLifecycleBootstrapSize(bootstrapPayload, actionPayload)
	if size > driverLifecycleBootstrapMaxBytes {
		return fmt.Errorf("epic %s %s lifecycle bootstrap is %d bytes; Driver limit is %d",
			epicID, role, size, driverLifecycleBootstrapMaxBytes)
	}
	return nil
}
