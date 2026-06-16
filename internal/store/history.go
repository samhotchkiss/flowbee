package store

import (
	"context"
	"database/sql"

	"github.com/samhotchkiss/flowbee/internal/history"
	"github.com/samhotchkiss/flowbee/internal/job"
)

// ActionWriteHistory is the project-OUT side-effect that lands the issue-archive
// markdown projection (build-list §F): a DEDICATED post-merge commit that writes
// docs/history/<id>.md (the curated card) + regenerates docs/history/README.md (the
// TOC). It is enqueued transactionally with the merged->done transition so the
// archive is never lost, and drained by Flowbee alone (the sole writer) — never
// entangled with the feature PR. The head_sha key is the job's id so a re-drain for
// the same job collapses to one write.
const ActionWriteHistory = "history.write"

// HistoryArtifact is one file the history writer must commit (path relative to the
// repo root + its full UTF-8 content). A write lands two: the job's card and the
// regenerated TOC.
type HistoryArtifact struct {
	Path    string
	Content string
}

// HistoryCardForJob folds a job's full event ledger into its curated history Card
// (build-list §F). It is a pure read-model FOLD: the card is reconstructable from
// job_events alone, so the markdown is regenerable and the ledger stays canonical.
// A precedent-gate hook can call this directly to query a prior attempt.
func (s *Store) HistoryCardForJob(ctx context.Context, jobID string) (history.Card, error) {
	events, err := s.LoadEvents(ctx, jobID)
	if err != nil {
		return history.Card{}, err
	}
	return history.Fold(events)
}

// archivedJobIDs returns the ids of every job that has reached a terminal `done`
// state — exactly the set that owns a history card. The TOC enumerates this set so
// it is a faithful index of the archive (and is itself a fold over the ledger: a
// job's done-ness is the folded projection the merged->done event produces).
func (s *Store) archivedJobIDs(ctx context.Context) ([]string, error) {
	rows, err := s.DB.QueryContext(ctx,
		`SELECT id FROM jobs WHERE state = ? ORDER BY id ASC`, string(job.StateDone))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// HistoryTOCEntries folds every archived (done) job into a TOC entry. Pure read
// model: each entry is derived from that job's events, so the TOC reconstructs from
// job_events exactly.
func (s *Store) HistoryTOCEntries(ctx context.Context) ([]history.TOCEntry, error) {
	ids, err := s.archivedJobIDs(ctx)
	if err != nil {
		return nil, err
	}
	var entries []history.TOCEntry
	for _, id := range ids {
		card, err := s.HistoryCardForJob(ctx, id)
		if err != nil {
			return nil, err
		}
		entries = append(entries, history.EntryFromCard(card))
	}
	return entries, nil
}

// BuildHistoryArtifacts produces the two files a history write must commit for a
// job: the job's own card (docs/history/<id>.md) and the regenerated TOC
// (docs/history/README.md). Both are pure folds over the ledger — the writer just
// commits the bytes. The TOC is rebuilt from the full archived set so a freshly
// completed job appears in it. The card is always first in the returned slice.
func (s *Store) BuildHistoryArtifacts(ctx context.Context, jobID string) ([]HistoryArtifact, error) {
	card, err := s.HistoryCardForJob(ctx, jobID)
	if err != nil {
		return nil, err
	}
	entries, err := s.HistoryTOCEntries(ctx)
	if err != nil {
		return nil, err
	}
	// a job racing reconcile may not yet show in the done set queried above (e.g.
	// the fold sees done but the row lagged); make the freshly-completed card's
	// entry present regardless so the TOC never omits the job it was built for.
	if !containsEntry(entries, jobID) {
		entries = append(entries, history.EntryFromCard(card))
	}
	return []HistoryArtifact{
		{Path: history.CardPath(jobID), Content: history.Render(card)},
		{Path: history.TOCPath, Content: history.RenderTOC(entries)},
	}, nil
}

func containsEntry(entries []history.TOCEntry, jobID string) bool {
	for _, e := range entries {
		if e.JobID == jobID {
			return true
		}
	}
	return false
}

// enqueueHistoryWriteTx enqueues the dedicated post-merge history-write side-effect
// for a job (build-list §F), in the caller's transaction so it lands atomically
// with the merged->done state change. The job id is the dedupe key (head_sha
// column) so a re-reconcile of the same merge collapses to one write.
func enqueueHistoryWriteTx(ctx context.Context, tx *sql.Tx, jobID string) error {
	return enqueueOutboxTx(ctx, tx, OutboxRow{
		JobID:  jobID,
		Action: ActionWriteHistory,
		// the job id keys the dedupe so the post-merge write is once-per-job; it is
		// not a real git SHA (the card folds whatever the ledger holds at drain time).
		HeadSHA: jobID,
	})
}
