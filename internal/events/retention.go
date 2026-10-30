package events

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/imksoo/routerd2/internal/retention"
)

// PruneHistory examines one bounded page. Consumer state, pending work, completed
// job identities and the SQLite AUTOINCREMENT high-water mark are not deleted.
// Delivery happens without a transaction or pooled connection; a failed archive
// leaves that candidate in place while publishers keep using the same store.
func (s *Store) PruneHistory(ctx context.Context, after uint64, policy retention.Config, beforeDelete func(context.Context, Record, []byte) error) (next uint64, finished bool, deleted int, err error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed {
		return after, false, 0, ErrClosed
	}
	type candidate struct {
		archivedRead
		id       string
		eligible bool
	}
	rows, err := s.db.QueryContext(ctx, `SELECT e.cursor,e.event_id,e.body,coalesce(a.body,X''),coalesce(a.original_bytes,0),coalesce(a.digest,X''),
 e.cursor<(SELECT max(cursor) FROM events) AND NOT EXISTS(SELECT 1 FROM consumers WHERE cursor<e.cursor OR stateful=1 AND state_cursor=e.cursor) AND NOT EXISTS(SELECT 1 FROM outbox WHERE event_cursor=e.cursor)
 FROM events e LEFT JOIN event_archive a ON a.cursor=e.cursor WHERE e.cursor>? ORDER BY e.cursor LIMIT 64`, after)
	if err != nil {
		return after, false, 0, err
	}
	var batch []candidate
	for rows.Next() {
		var item candidate
		if err = rows.Scan(&item.cursor, &item.id, &item.body, &item.packed, &item.size, &item.digest, &item.eligible); err != nil {
			break
		}
		batch = append(batch, item)
	}
	err = errors.Join(err, rows.Err(), rows.Close())
	if err != nil {
		return after, false, 0, err
	}
	var count, bytes int64
	if err = s.db.QueryRowContext(ctx, `SELECT rows_count,bytes_count FROM usage WHERE kind='history'`).Scan(&count, &bytes); err != nil {
		return after, false, 0, err
	}
	cutoff := time.Now().Add(-time.Duration(policy.MaxAgeSeconds) * time.Second)
	next = after
	for _, item := range batch {
		if err = ctx.Err(); err != nil {
			return next, false, deleted, err
		}
		if !item.eligible {
			next = item.cursor
			continue
		}
		body, loadErr := s.loadBody(item.cursor, item.body, item.packed, item.size, item.digest)
		if loadErr != nil {
			return next, false, deleted, loadErr
		}
		record, decodeErr := decodeEvent(item.cursor, body)
		if decodeErr != nil {
			return next, false, deleted, decodeErr
		}
		expired := policy.MaxAgeSeconds > 0 && record.Event.ObservedAt.Before(cutoff)
		oversized := policy.MaxRecords > 0 && count > policy.MaxRecords || policy.MaxBytes > 0 && bytes > policy.MaxBytes
		if !expired && !oversized {
			next = item.cursor
			continue
		}
		if beforeDelete != nil {
			if err = beforeDelete(ctx, record, body); err != nil {
				return next, false, deleted, err
			}
		}
		tx, beginErr := s.db.BeginTx(ctx, nil)
		if beginErr != nil {
			return next, false, deleted, beginErr
		}
		changed, removeErr := removeHistory(ctx, tx, item.cursor)
		if removeErr == nil {
			removeErr = tx.Commit()
		} else {
			_ = tx.Rollback()
		}
		if removeErr != nil {
			return next, false, deleted, removeErr
		}
		if changed {
			count--
			bytes -= int64(len(body))
			deleted++
		}
		next = item.cursor
	}
	return next, len(batch) < 64, deleted, nil
}

func removeHistory(ctx context.Context, tx *sql.Tx, cursor uint64) (bool, error) {
	var hot, packed, original int64
	err := tx.QueryRowContext(ctx, `SELECT length(e.body),coalesce(length(a.body),0),coalesce(a.original_bytes,0)
 FROM events e LEFT JOIN event_archive a ON a.cursor=e.cursor WHERE e.cursor=?
 AND e.cursor<(SELECT max(cursor) FROM events) AND NOT EXISTS(SELECT 1 FROM consumers WHERE cursor<e.cursor OR stateful=1 AND state_cursor=e.cursor)
 AND NOT EXISTS(SELECT 1 FROM outbox WHERE event_cursor=e.cursor)`, cursor).Scan(&hot, &packed, &original)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	kind, size := "events", hot
	historyBytes := hot
	if hot == 0 {
		kind, size = "event_archive", packed+archivedEventOverhead
		historyBytes = original
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM event_archive WHERE cursor=?`, cursor); err != nil {
		return false, err
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM events WHERE cursor=?`, cursor); err != nil {
		return false, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE usage SET rows_count=rows_count-1,bytes_count=bytes_count-? WHERE kind=?`, size, kind); err != nil {
		return false, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE usage SET rows_count=rows_count-1,bytes_count=bytes_count-? WHERE kind='history'`, historyBytes); err != nil {
		return false, err
	}
	return true, nil
}

// VacuumHistory returns at most 64 free pages to the filesystem per call.
// The existing connection owner serializes this with writes, and ctx bounds work.
func (s *Store) VacuumHistory(ctx context.Context) error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed {
		return ErrClosed
	}
	var mode int
	if err := s.db.QueryRowContext(ctx, `PRAGMA auto_vacuum`).Scan(&mode); err != nil {
		return err
	}
	if mode != 2 {
		return errors.New("event store predates incremental vacuum; recreate disposable prototype state to reclaim free pages")
	}
	_, err := s.db.ExecContext(ctx, `PRAGMA incremental_vacuum(64)`)
	return err
}
