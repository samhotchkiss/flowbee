package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// ReconcilerLease fences durable liveness writes from an older process
// incarnation. Grace is the maximum permitted time between durable heartbeats,
// not merely the loop's nominal tick interval.
type ReconcilerLease struct {
	Name, Owner string
	Epoch       int64
	Grace       time.Duration
}

// ReconcilerProgress is the durable observation boundary completed by a loop.
// Loops without a cursor still record the global ledger sequence they observed.
type ReconcilerProgress struct {
	Cursor    string
	LedgerSeq int64
}

type ReconcilerHealth struct {
	Name, Owner, State, Cursor, LastError                                        string
	Epoch, LedgerSeq, ConsecutiveFailures, StaleEpoch                            int64
	LastStartedAt, LastHeartbeatAt, HeartbeatDueAt, LastSuccessAt, LastFailureAt time.Time
	LastPanicAt                                                                  time.Time
}

type ReconcilerHealthSummary struct {
	Total, Overdue int
	OverdueNames   []string
}

// ReconcilerSummary computes liveness from the due clock at read time. This is
// intentionally independent of the in-process watchdog: an external dead-man can
// detect a wedged watchdog, a wedged loop, or an alive-but-nonprogressing process
// through /healthz without waiting for another database write.
func (s *Store) ReconcilerSummary(ctx context.Context, now time.Time) (ReconcilerHealthSummary, error) {
	var out ReconcilerHealthSummary
	rows, err := s.DB.QueryContext(ctx, `SELECT name,state,heartbeat_due_at
		FROM reconciler_health ORDER BY name`)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		var name, state, dueText string
		if err := rows.Scan(&name, &state, &dueText); err != nil {
			return out, err
		}
		out.Total++
		due, parseErr := time.Parse(rfc3339, dueText)
		if state == "stale" || dueText == "" || parseErr != nil || !now.Before(due) {
			out.Overdue++
			out.OverdueNames = append(out.OverdueNames, name)
		}
	}
	return out, rows.Err()
}

var ErrStaleReconcilerLease = errors.New("stale reconciler lease")

// BeginReconciler starts a new durable loop incarnation. It always advances the
// epoch, so a goroutine left behind by a replacement process cannot make the new
// loop look healthy.
func (s *Store) BeginReconciler(ctx context.Context, name, owner string, now time.Time, grace time.Duration) (ReconcilerLease, error) {
	if name == "" || owner == "" {
		return ReconcilerLease{}, errors.New("reconciler name and owner are required")
	}
	if grace <= 0 {
		return ReconcilerLease{}, errors.New("reconciler heartbeat grace must be positive")
	}
	lease := ReconcilerLease{Name: name, Owner: owner, Grace: grace}
	err := s.tx(ctx, func(tx *sql.Tx) error {
		var priorState string
		var priorEpoch, priorStale int64
		err := tx.QueryRowContext(ctx, `SELECT state,run_epoch,stale_epoch FROM reconciler_health WHERE name=?`, name).
			Scan(&priorState, &priorEpoch, &priorStale)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		lease.Epoch = priorEpoch + 1
		nowText := now.UTC().Format(rfc3339)
		dueText := now.Add(grace).UTC().Format(rfc3339)
		_, err = tx.ExecContext(ctx, `INSERT INTO reconciler_health
			(name,owner,run_epoch,state,last_started_at,last_heartbeat_at,heartbeat_due_at,
			 consecutive_failures,last_error,updated_at)
			VALUES (?,?,?,'starting',?,?,?,0,'',?)
			ON CONFLICT(name) DO UPDATE SET owner=excluded.owner,run_epoch=excluded.run_epoch,
			 state='starting',last_started_at=excluded.last_started_at,
			 last_heartbeat_at=excluded.last_heartbeat_at,heartbeat_due_at=excluded.heartbeat_due_at,
			 consecutive_failures=0,last_error='',updated_at=excluded.updated_at`,
			name, owner, lease.Epoch, nowText, nowText, dueText, nowText)
		if err != nil {
			return err
		}
		if priorState == "stale" {
			return resolveOperationalAttentionTx(ctx, tx, reconcilerStaleDedup(name, priorEpoch, priorStale), "reconciler_restarted", now)
		}
		return nil
	})
	return lease, err
}

func (s *Store) HeartbeatReconciler(ctx context.Context, lease ReconcilerLease, now time.Time, progress ReconcilerProgress) error {
	return s.updateReconcilerHealth(ctx, lease, now, progress, "running", "", false)
}

func (s *Store) MarkReconcilerSuccess(ctx context.Context, lease ReconcilerLease, now time.Time, progress ReconcilerProgress) error {
	return s.updateReconcilerHealth(ctx, lease, now, progress, "healthy", "", false)
}

func (s *Store) MarkReconcilerFailure(ctx context.Context, lease ReconcilerLease, now time.Time, progress ReconcilerProgress, detail string) error {
	return s.updateReconcilerHealth(ctx, lease, now, progress, "degraded", detail, false)
}

// MarkReconcilerPanic records a recovered per-iteration panic and immediately
// commits an alert. The loop remains restartable and its next heartbeat can
// prove recovery; a panic is never merely a log line.
func (s *Store) MarkReconcilerPanic(ctx context.Context, lease ReconcilerLease, now time.Time, progress ReconcilerProgress, detail string) error {
	return s.updateReconcilerHealth(ctx, lease, now, progress, "panicked", detail, true)
}

func (s *Store) updateReconcilerHealth(ctx context.Context, lease ReconcilerLease, now time.Time, progress ReconcilerProgress, state, detail string, panicAlert bool) error {
	if lease.Name == "" || lease.Owner == "" || lease.Epoch <= 0 || lease.Grace <= 0 {
		return ErrStaleReconcilerLease
	}
	return s.tx(ctx, func(tx *sql.Tx) error {
		var priorState string
		var staleEpoch int64
		if err := tx.QueryRowContext(ctx, `SELECT state,stale_epoch FROM reconciler_health
			WHERE name=? AND owner=? AND run_epoch=?`, lease.Name, lease.Owner, lease.Epoch).
			Scan(&priorState, &staleEpoch); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrStaleReconcilerLease
			}
			return err
		}
		nowText := now.UTC().Format(rfc3339)
		dueText := now.Add(lease.Grace).UTC().Format(rfc3339)
		failureAt, panicAt, successAt := "", "", ""
		if state == "healthy" {
			successAt = nowText
		}
		if state == "degraded" || state == "panicked" {
			failureAt = nowText
		}
		if state == "panicked" {
			panicAt = nowText
		}
		res, err := tx.ExecContext(ctx, `UPDATE reconciler_health SET state=?,last_heartbeat_at=?,
			heartbeat_due_at=?,cursor=?,ledger_seq=?,
			last_success_at=CASE WHEN ?<>'' THEN ? ELSE last_success_at END,
			last_failure_at=CASE WHEN ?<>'' THEN ? ELSE last_failure_at END,
			last_panic_at=CASE WHEN ?<>'' THEN ? ELSE last_panic_at END,
			consecutive_failures=CASE WHEN ?='healthy' THEN 0 WHEN ? IN ('degraded','panicked')
				THEN consecutive_failures+1 ELSE consecutive_failures END,
			last_error=?,updated_at=? WHERE name=? AND owner=? AND run_epoch=?`,
			state, nowText, dueText, progress.Cursor, progress.LedgerSeq,
			successAt, successAt, failureAt, failureAt, panicAt, panicAt,
			state, state, detail, nowText, lease.Name, lease.Owner, lease.Epoch)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return ErrStaleReconcilerLease
		}
		if priorState == "stale" {
			if err := resolveOperationalAttentionTx(ctx, tx, reconcilerStaleDedup(lease.Name, lease.Epoch, staleEpoch), "reconciler_recovered", now); err != nil {
				return err
			}
		}
		if !panicAlert {
			return nil
		}
		payload, _ := json.Marshal(map[string]any{"reconciler": lease.Name, "owner": lease.Owner,
			"run_epoch": lease.Epoch, "error": detail})
		dedup := fmt.Sprintf("reconciler_panic:%s:%d:%s", lease.Name, lease.Epoch, stableID(nowText+detail))
		if err := ensureControlAlertTx(ctx, tx, "default", "", "reconciler_panic", dedup, string(payload), now); err != nil {
			return err
		}
		if err := ensureOperationalAttentionTx(ctx, tx, "reconciler_panic", dedup, string(payload), "reconciler iteration panic recovered", now); err != nil {
			return err
		}
		return appendGlobalControlEventTx(ctx, tx, "reconciler_panicked", string(payload), now)
	})
}

// RecordReconcilerPoisonFact quarantines one malformed fact without stopping the
// loop. Repeated observations of the same open fact update occurrence history and
// retain one active attention/alert; recurrence after resolution gets a new epoch.
func (s *Store) RecordReconcilerPoisonFact(ctx context.Context, reconciler, factKey, detail string, now time.Time) error {
	if reconciler == "" || factKey == "" {
		return errors.New("reconciler and poison fact key are required")
	}
	return s.tx(ctx, func(tx *sql.Tx) error {
		var state string
		var epoch int64
		err := tx.QueryRowContext(ctx, `SELECT state,poison_epoch FROM reconciler_poison_facts
			WHERE reconciler_name=? AND fact_key=?`, reconciler, factKey).Scan(&state, &epoch)
		nowText := now.UTC().Format(rfc3339)
		if errors.Is(err, sql.ErrNoRows) {
			epoch = 1
			_, err = tx.ExecContext(ctx, `INSERT INTO reconciler_poison_facts
				(reconciler_name,fact_key,state,poison_epoch,occurrences,first_seen_at,last_seen_at,last_error)
				VALUES (?,?,'open',1,1,?,?,?)`, reconciler, factKey, nowText, nowText, detail)
		} else if err == nil {
			if state == "resolved" {
				epoch++
				_, err = tx.ExecContext(ctx, `UPDATE reconciler_poison_facts SET state='open',poison_epoch=?,
					occurrences=occurrences+1,last_seen_at=?,resolved_at='',last_error=?
					WHERE reconciler_name=? AND fact_key=?`, epoch, nowText, detail, reconciler, factKey)
			} else {
				_, err = tx.ExecContext(ctx, `UPDATE reconciler_poison_facts SET occurrences=occurrences+1,
					last_seen_at=?,last_error=? WHERE reconciler_name=? AND fact_key=?`,
					nowText, detail, reconciler, factKey)
			}
		}
		if err != nil {
			return err
		}
		payload, _ := json.Marshal(map[string]any{"reconciler": reconciler, "fact_key": factKey,
			"poison_epoch": epoch, "error": detail})
		dedup := fmt.Sprintf("reconciler_poison_fact:%s:%s:%d", reconciler, stableID(factKey), epoch)
		if err := ensureControlAlertTx(ctx, tx, "default", "", "reconciler_poison_fact", dedup, string(payload), now); err != nil {
			return err
		}
		if err := ensureOperationalAttentionTx(ctx, tx, "reconciler_poison_fact", dedup, string(payload), "poison fact quarantined", now); err != nil {
			return err
		}
		return appendGlobalControlEventTx(ctx, tx, "reconciler_poison_fact_quarantined", string(payload), now)
	})
}

func (s *Store) ResolveReconcilerPoisonFact(ctx context.Context, reconciler, factKey string, now time.Time) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		var epoch int64
		if err := tx.QueryRowContext(ctx, `SELECT poison_epoch FROM reconciler_poison_facts
			WHERE reconciler_name=? AND fact_key=? AND state='open'`, reconciler, factKey).Scan(&epoch); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return nil
			}
			return err
		}
		nowText := now.UTC().Format(rfc3339)
		if _, err := tx.ExecContext(ctx, `UPDATE reconciler_poison_facts SET state='resolved',resolved_at=?
			WHERE reconciler_name=? AND fact_key=? AND state='open'`, nowText, reconciler, factKey); err != nil {
			return err
		}
		dedup := fmt.Sprintf("reconciler_poison_fact:%s:%s:%d", reconciler, stableID(factKey), epoch)
		return resolveOperationalAttentionTx(ctx, tx, dedup, "fact_recovered", now)
	})
}

type ReconcilerWatchdogResult struct{ Scanned, Alerted int }

// ReconcileStaleReconcilers is the in-process dead-loop detector. It deliberately
// excludes its own heartbeat: the independent external dead-man owns detection of
// a dead process or dead watchdog loop.
func (s *Store) ReconcileStaleReconcilers(ctx context.Context, now time.Time) (ReconcilerWatchdogResult, error) {
	var out ReconcilerWatchdogResult
	rows, err := s.DB.QueryContext(ctx, `SELECT name FROM reconciler_health
		WHERE name<>'reconciler_watchdog' AND state<>'stale' AND heartbeat_due_at<>''
		  AND julianday(heartbeat_due_at)<=julianday(?) ORDER BY name`, now.UTC().Format(rfc3339))
	if err != nil {
		return out, err
	}
	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			rows.Close()
			return out, err
		}
		names = append(names, name)
	}
	if err := rows.Close(); err != nil {
		return out, err
	}
	for _, name := range names {
		out.Scanned++
		alerted, err := s.reconcileOneStaleReconciler(ctx, name, now)
		if err != nil {
			continue // poison isolation: one corrupt row cannot blind every loop.
		}
		if alerted {
			out.Alerted++
		}
	}
	return out, nil
}

func (s *Store) reconcileOneStaleReconciler(ctx context.Context, name string, now time.Time) (bool, error) {
	alerted := false
	err := s.tx(ctx, func(tx *sql.Tx) error {
		var owner, state, heartbeat, due, cursor, lastError string
		var runEpoch, ledgerSeq, staleEpoch int64
		if err := tx.QueryRowContext(ctx, `SELECT owner,run_epoch,state,last_heartbeat_at,heartbeat_due_at,
			cursor,ledger_seq,stale_epoch,last_error FROM reconciler_health WHERE name=?`, name).
			Scan(&owner, &runEpoch, &state, &heartbeat, &due, &cursor, &ledgerSeq, &staleEpoch, &lastError); err != nil {
			return err
		}
		deadline, err := time.Parse(rfc3339, due)
		if err != nil || state == "stale" || now.Before(deadline) {
			return err
		}
		staleEpoch++
		nowText := now.UTC().Format(rfc3339)
		if _, err := tx.ExecContext(ctx, `UPDATE reconciler_health SET state='stale',stale_epoch=?,
			last_error='heartbeat overdue',updated_at=? WHERE name=? AND run_epoch=? AND state<>'stale'`,
			staleEpoch, nowText, name, runEpoch); err != nil {
			return err
		}
		payload, _ := json.Marshal(map[string]any{"reconciler": name, "owner": owner,
			"run_epoch": runEpoch, "stale_epoch": staleEpoch, "last_heartbeat_at": heartbeat,
			"heartbeat_due_at": due, "cursor": cursor, "ledger_seq": ledgerSeq, "last_error": lastError})
		dedup := reconcilerStaleDedup(name, runEpoch, staleEpoch)
		if err := ensureControlAlertTx(ctx, tx, "default", "", "reconciler_dead", dedup, string(payload), now); err != nil {
			return err
		}
		if err := ensureOperationalAttentionTx(ctx, tx, "reconciler_dead", dedup, string(payload), "reconciler heartbeat overdue", now); err != nil {
			return err
		}
		if err := appendGlobalControlEventTx(ctx, tx, "reconciler_declared_stale", string(payload), now); err != nil {
			return err
		}
		alerted = true
		return nil
	})
	return alerted, err
}

func reconcilerStaleDedup(name string, runEpoch, staleEpoch int64) string {
	return fmt.Sprintf("reconciler_dead:%s:%d:%d", name, runEpoch, staleEpoch)
}

func ensureOperationalAttentionTx(ctx context.Context, tx *sql.Tx, kind, dedup, evidence, detail string, now time.Time) error {
	nowText := now.UTC().Format(rfc3339)
	res, err := tx.ExecContext(ctx, `UPDATE attention_items SET occurrences=occurrences+1,last_seen_at=?,
		evidence_json=?,detail=?,updated_at=? WHERE dedup_key=? AND state<>'resolved'`,
		nowText, evidence, detail, nowText, dedup)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n > 0 {
		return nil
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO attention_items
		(id,kind,epic_id,repo,priority,state,dedup_key,blocking,leased_by,item_epoch,
		 lease_expires_at,awaiting_since,delivery_key,evidence_json,detail,resolution,
		 verdict,occurrences,first_seen_at,last_seen_at,resolved_at,created_at,updated_at)
		VALUES (?,?,?,'',3,'open',?,1,'',0,'','','',?,?,'','',1,?,?,'',?,?)`,
		"operational-"+stableID(dedup), kind, "", dedup, evidence, detail,
		nowText, nowText, nowText, nowText)
	if err != nil && isUniqueConstraintErr(err) {
		return nil
	}
	return err
}

func resolveOperationalAttentionTx(ctx context.Context, tx *sql.Tx, dedup, resolution string, now time.Time) error {
	nowText := now.UTC().Format(rfc3339)
	_, err := tx.ExecContext(ctx, `UPDATE attention_items SET state='resolved',resolution=?,resolved_at=?,
		leased_by='',lease_expires_at='',updated_at=? WHERE dedup_key=? AND state<>'resolved'`,
		resolution, nowText, nowText, dedup)
	return err
}

func appendGlobalControlEventTx(ctx context.Context, tx *sql.Tx, kind, payload string, now time.Time) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO control_events
		(project_id,epic_id,kind,epic_seq,actor_kind,payload_json,created_at)
		VALUES ('default','',?,0,'reconciler',?,?)`, kind, payload, now.UTC().Format(rfc3339))
	return err
}

func (s *Store) GetReconcilerHealth(ctx context.Context, name string) (ReconcilerHealth, error) {
	var out ReconcilerHealth
	var started, heartbeat, due, success, failure, panicAt string
	err := s.DB.QueryRowContext(ctx, `SELECT name,owner,state,run_epoch,cursor,ledger_seq,
		consecutive_failures,stale_epoch,last_error,last_started_at,last_heartbeat_at,
		heartbeat_due_at,last_success_at,last_failure_at,last_panic_at
		FROM reconciler_health WHERE name=?`, name).Scan(&out.Name, &out.Owner, &out.State,
		&out.Epoch, &out.Cursor, &out.LedgerSeq, &out.ConsecutiveFailures, &out.StaleEpoch,
		&out.LastError, &started, &heartbeat, &due, &success, &failure, &panicAt)
	if err != nil {
		return out, err
	}
	parse := func(raw string) time.Time {
		if raw != "" {
			parsed, _ := time.Parse(rfc3339, raw)
			return parsed
		}
		return time.Time{}
	}
	out.LastStartedAt = parse(started)
	out.LastHeartbeatAt = parse(heartbeat)
	out.HeartbeatDueAt = parse(due)
	out.LastSuccessAt = parse(success)
	out.LastFailureAt = parse(failure)
	out.LastPanicAt = parse(panicAt)
	return out, nil
}
