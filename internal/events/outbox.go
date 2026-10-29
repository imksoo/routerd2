package events

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"math"
	"unicode/utf8"
)

func normalizeJobs(jobs []Job) ([]Job, error) {
	if len(jobs) > 64 {
		return nil, errors.New("outbox batch exceeds 64 jobs")
	}
	normalized := make([]Job, len(jobs))
	seen := map[string]bool{}
	for i, job := range jobs {
		if !token(job.Key, 256) || seen[job.Key] || len(job.Payload) > maxBody || !utf8.Valid(job.Payload) || !json.Valid(job.Payload) {
			return nil, errors.New("invalid bounded outbox job")
		}
		seen[job.Key] = true
		var body bytes.Buffer
		if err := json.Compact(&body, job.Payload); err != nil {
			return nil, err
		}
		normalized[i] = Job{Key: job.Key, Payload: body.Bytes()}
	}
	return normalized, nil
}

// Schedule commits work and its source cursor together. Completed work remains
// a deduplication tombstone, including after a restored/rolled-back cursor.
// A crash between an external effect and Complete still requires idempotency
// in that effect's target, using this stable consumer/key pair.
func (s *Store) Schedule(ctx context.Context, name string, expected, eventCursor uint64, jobs []Job) error {
	return s.schedule(ctx, name, expected, eventCursor, jobs, nil, nil)
}

func (s *Store) schedule(ctx context.Context, name string, expected, eventCursor uint64, jobs []Job, checkpoint *Checkpoint, state json.RawMessage) error {
	if !token(name, 128) || expected > math.MaxInt64 || eventCursor > math.MaxInt64 || eventCursor < expected || checkpoint == nil && eventCursor == expected || eventCursor == 0 && len(jobs) > 0 || len(jobs) > 64 {
		return ErrConflict
	}
	normalized, err := normalizeJobs(jobs)
	if err != nil {
		return err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed {
		return ErrClosed
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var current uint64
	var version uint64
	var stateful bool
	var stateCursor uint64
	var stateBytes int64
	if err = tx.QueryRowContext(ctx, `SELECT cursor,version,stateful,state_cursor,length(body) FROM consumers WHERE name=?`, name).Scan(&current, &version, &stateful, &stateCursor, &stateBytes); err != nil {
		return errors.Join(ErrConflict, err)
	}
	if current != expected || version >= math.MaxInt64 || stateful != (checkpoint != nil) || checkpoint != nil && (checkpoint.Version != version || stateCursor != current) {
		return ErrConflict
	}
	if eventCursor != expected {
		var next sql.NullInt64
		if err = tx.QueryRowContext(ctx, `SELECT min(cursor) FROM events WHERE cursor>?`, expected).Scan(&next); err != nil {
			return err
		}
		if !next.Valid || uint64(next.Int64) != eventCursor {
			return ErrConflict
		}
	}
	for _, job := range normalized {
		var priorCursor uint64
		var payload []byte
		err = tx.QueryRowContext(ctx, `SELECT event_cursor,payload FROM outbox WHERE consumer=? AND key=?`, name, job.Key).Scan(&priorCursor, &payload)
		if err == nil {
			if priorCursor != eventCursor || !bytes.Equal(payload, job.Payload) {
				return ErrConflict
			}
			continue // Never change the completed flag on replay.
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		position, digest, lookupErr := completedIdentity(ctx, tx, 0, name, job.Key)
		if lookupErr == nil {
			if position != eventCursor || !sameCompletedPayload(digest, job.Payload) {
				return ErrConflict
			}
			continue
		}
		if !errors.Is(lookupErr, sql.ErrNoRows) {
			return lookupErr
		}
		if err = s.reserve(ctx, tx, "outbox", 1, int64(len(job.Payload)), s.options.MaxJobs, s.options.MaxJobBytes); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO outbox(consumer,key,event_cursor,payload) VALUES(?,?,?,?)`, name, job.Key, eventCursor, []byte(job.Payload)); err != nil {
			return err
		}
	}
	var result sql.Result
	if checkpoint != nil {
		var used int64
		if err = tx.QueryRowContext(ctx, `SELECT bytes_count FROM usage WHERE kind='consumer_state'`).Scan(&used); err != nil {
			return err
		}
		delta := int64(len(state)) - stateBytes
		if delta > s.options.MaxConsumerStateBytes-used {
			return ErrCapacity
		}
		if _, err = tx.ExecContext(ctx, `UPDATE usage SET bytes_count=bytes_count+? WHERE kind='consumer_state'`, delta); err != nil {
			return err
		}
		result, err = tx.ExecContext(ctx, `UPDATE consumers SET cursor=?,state_cursor=?,version=version+1,body=? WHERE name=? AND cursor=? AND version=?`, eventCursor, eventCursor, []byte(state), name, expected, version)
	} else {
		result, err = tx.ExecContext(ctx, `UPDATE consumers SET cursor=?,version=version+1 WHERE name=? AND cursor=? AND version=?`, eventCursor, name, expected, version)
	}
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed != 1 {
		return ErrConflict
	}
	return tx.Commit()
}

func (s *Store) Pending(ctx context.Context, name string, limit int) ([]Delivery, error) {
	if !token(name, 128) || !pageLimit(limit) {
		return nil, errors.New("invalid outbox page")
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed {
		return nil, ErrClosed
	}
	rows, err := s.db.QueryContext(ctx, `SELECT key,event_cursor,payload FROM outbox WHERE consumer=? AND done=0 ORDER BY event_cursor,key LIMIT ?`, name, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]Delivery, 0)
	for rows.Next() {
		var d Delivery
		if err = rows.Scan(&d.Key, &d.EventCursor, &d.Payload); err != nil {
			return nil, err
		}
		if len(d.Payload) > maxBody || !json.Valid(d.Payload) {
			return nil, errors.New("invalid stored outbox payload")
		}
		result = append(result, d)
	}
	return result, rows.Err()
}

func (s *Store) Complete(ctx context.Context, name, key string) error {
	if !token(name, 128) || !token(key, 256) {
		return ErrConflict
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed {
		return ErrClosed
	}
	result, err := s.db.ExecContext(ctx, `UPDATE outbox SET done=1 WHERE consumer=? AND key=? AND done=0`, name, key)
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed == 1 {
		return nil
	}
	var done bool
	err = s.db.QueryRowContext(ctx, `SELECT done FROM outbox WHERE consumer=? AND key=?`, name, key).Scan(&done)
	if errors.Is(err, sql.ErrNoRows) {
		return s.completedAcknowledged(ctx, 0, name, key)
	}
	if err != nil || !done {
		return errors.Join(ErrConflict, err)
	}
	return nil
}
