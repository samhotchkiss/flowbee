package acceptance

import "github.com/samhotchkiss/flowbee/internal/gitops"

// fakeMergeHistory is a no-op HistoryWriter for acceptance tests that exercise AUTONOMOUS
// self-merge. Self-merge requires a mirror (the SHA-pin + CP-authoritative content re-verify),
// so the project Sender must have a history writer wired or it correctly routes to the human
// gate. These tests' merging jobs carry no jobs-row head_sha, so the re-verify is skipped and
// the merge proceeds — this fake just makes s.history non-nil (production parity: self-merge
// is configured alongside FLOWBEE_MIRROR_PATH).
type fakeMergeHistory struct{}

func (fakeMergeHistory) CommitHistory(branch, message string, files []gitops.HistoryFile) (string, bool, error) {
	return "", false, nil
}
func (fakeMergeHistory) HeadSHA(ref string) (string, error)            { return "", nil }
func (fakeMergeHistory) FetchBranch(branch string) error               { return nil }
func (fakeMergeHistory) DiffBetween(base, head string) (string, error) { return "", nil }
func (fakeMergeHistory) ReadFileAtRef(ref, path string) (string, bool, error) {
	return "", false, nil
}
