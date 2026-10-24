package events

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"math"
)

func (s *Store) Append(ctx context.Context, e Event) (Record, error) {
	body, err := encodeEvent(e)
	if err != nil {
		return Record{}, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed {
		return Record{}, ErrClosed
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Record{}, err
	}
	defer tx.Rollback()
	var cursor uint64
	var previous, packed, digest []byte
	var originalBytes int64
	err = tx.QueryRowContext(ctx, `SELECT e.cursor,e.body,coalesce(a.body,X''),coalesce(a.original_bytes,0),coalesce(a.digest,X'') FROM events e LEFT JOIN event_archive a ON a.cursor=e.cursor WHERE e.event_id=?`, e.ID).Scan(&cursor, &previous, &packed, &originalBytes, &digest)
	if err == nil {
		previous, err = s.loadBody(cursor, previous, packed, originalBytes, digest)
		if err != nil {
			return Record{}, err
		}
		if !bytes.Equal(body, previous) {
			return Record{}, ErrConflict
		}
		return decodeEvent(cursor, previous)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return Record{}, err
	}
	if err = s.reserve(ctx, tx, "events", int64(len(body)), s.options.MaxEvents, s.options.MaxEventBytes); err != nil {
		return Record{}, err
	}
	if err = s.reserve(ctx, tx, "history", int64(len(body)), s.options.MaxHistoryEvents, s.options.MaxHistoryBytes); err != nil {
		return Record{}, err
	}
	result, err := tx.ExecContext(ctx, `INSERT INTO events(event_id,body) VALUES(?,?)`, e.ID, body)
	if err != nil {
		return Record{}, err
	}
	id, err := result.LastInsertId()
	if err != nil {
		return Record{}, err
	}
	if err = tx.Commit(); err != nil {
		return Record{}, err
	}
	return decodeEvent(uint64(id), body)
}

func (s *Store) reserve(ctx context.Context, tx *sql.Tx, kind string, size, maxRows, maxBytes int64) error {
	var rows, used int64
	if err := tx.QueryRowContext(ctx, `SELECT rows_count,bytes_count FROM usage WHERE kind=?`, kind).Scan(&rows, &used); err != nil {
		return err
	}
	if rows >= maxRows || size > maxBytes-used {
		return ErrCapacity
	}
	_, err := tx.ExecContext(ctx, `UPDATE usage SET rows_count=rows_count+1,bytes_count=bytes_count+? WHERE kind=?`, size, kind)
	return err
}

func decodeEvent(cursor uint64, body []byte) (Record, error) {
	var event Event
	if len(body) > maxBody {
		return Record{}, errors.New("stored event exceeds its bound")
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&event); err != nil {
		return Record{}, err
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return Record{}, errors.New("stored event contains trailing data")
	}
	if _, err := encodeEvent(event); err != nil {
		return Record{}, err
	}
	return Record{Cursor: cursor, Event: event}, nil
}

func pageLimit(limit int) bool { return limit >= 1 && limit <= 256 }

func (s *Store) List(ctx context.Context, after uint64, limit int) ([]Record, error) {
	if !pageLimit(limit) || after > math.MaxInt64 {
		return nil, errors.New("invalid event page")
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed {
		return nil, ErrClosed
	}
	var tip uint64
	if err := s.db.QueryRowContext(ctx, `SELECT coalesce(max(cursor),0) FROM events`).Scan(&tip); err != nil {
		return nil, err
	}
	if after > tip {
		return nil, ErrConflict
	}
	rows, err := s.db.QueryContext(ctx, `SELECT e.cursor,e.event_id,e.body,coalesce(a.body,X''),coalesce(a.original_bytes,0),coalesce(a.digest,X'') FROM events e LEFT JOIN event_archive a ON a.cursor=e.cursor WHERE e.cursor>? ORDER BY e.cursor LIMIT ?`, after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	type pageBody struct {
		id string
		archivedRead
	}
	page := make([]pageBody, 0, limit)
	for rows.Next() {
		var item pageBody
		if err := rows.Scan(&item.cursor, &item.id, &item.body, &item.packed, &item.size, &item.digest); err != nil {
			return nil, err
		}
		page = append(page, item)
	}
	if err = errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, err
	}
	// Materialize only this bounded immutable page, then release the single
	// database connection before gzip/JSON validation. A slow archive reader
	// must not stall live publication or consumer/producer checkpoints.
	result := make([]Record, 0, len(page))
	for _, item := range page {
		if err = ctx.Err(); err != nil {
			return nil, err
		}
		body, err := s.loadBody(item.cursor, item.body, item.packed, item.size, item.digest)
		if err != nil {
			return nil, err
		}
		record, err := decodeEvent(item.cursor, body)
		if err != nil {
			return nil, err
		}
		if record.Event.ID != item.id {
			return nil, errors.New("stored event identity mismatch")
		}
		result = append(result, record)
	}
	return result, nil
}

// Cursor initializes a new consumer at the current tip in the same write
// transaction. An existing consumer resumes exactly its durable position.
func (s *Store) Cursor(ctx context.Context, name string) (uint64, error) {
	checkpoint, err := s.consumer(ctx, name, false)
	return checkpoint.Cursor, err
}

// Drain preserves at-least-once delivery: a callback followed by a failed
// cursor save is repeated. External effects must use the stable event ID for
// their own durable idempotency; journal cursors cannot make them exactly once.
func (s *Store) Drain(ctx context.Context, name string, process func(context.Context, Record) error) error {
	if process == nil {
		return errors.New("event consumer callback is required")
	}
	cursor, err := s.Cursor(ctx, name)
	if err != nil {
		return err
	}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		rows, err := s.List(ctx, cursor, 256)
		if err != nil {
			return err
		}
		for _, record := range rows {
			if err = process(ctx, record); err != nil {
				return err
			}
			if err = s.Schedule(ctx, name, cursor, record.Cursor, nil); err != nil {
				return err
			}
			cursor = record.Cursor
		}
		if len(rows) < 256 {
			return nil
		}
	}
}
