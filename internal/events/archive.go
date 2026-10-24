package events

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"errors"
	"io"
)

const archivedEventOverhead int64 = 48 // cursor, original byte count, SHA256

func archivedBody(packed []byte, size int64, digest []byte) ([]byte, error) {
	if size < 1 || size > maxBody || len(packed) < 1 || len(packed) > maxBody+1024 || len(digest) != sha256.Size {
		return nil, errors.New("invalid archived event bounds")
	}
	source := bytes.NewReader(packed)
	reader, err := gzip.NewReader(source)
	if err != nil {
		return nil, err
	}
	reader.Multistream(false)
	body, err := io.ReadAll(io.LimitReader(reader, size+1))
	err = errors.Join(err, reader.Close())
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(body)
	if int64(len(body)) != size || source.Len() != 0 || !bytes.Equal(sum[:], digest) {
		return nil, errors.New("archived event length, framing or SHA256 mismatch")
	}
	return body, nil
}

func storedBody(hot, packed []byte, size int64, digest []byte) ([]byte, error) {
	if len(hot) > 0 {
		if len(packed) != 0 || size != 0 || len(digest) != 0 {
			return nil, errors.New("event has both hot and archived payloads")
		}
		return hot, nil
	}
	return archivedBody(packed, size, digest)
}

// ArchiveEvents moves a bounded batch of complete bodies to compressed storage
// in the same WAL/FULL transaction. The event row, ID, cursor and logical
// history remain; all readers and pending/timer references still use them.
// Archive and whole-history limits remain finite. This is neither off-device
// durability nor expiry, and does not shrink the filesystem database file.
func (s *Store) ArchiveEvents(ctx context.Context, limit int) (int, error) {
	if !pageLimit(limit) {
		return 0, errors.New("invalid event archive limit")
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed {
		return 0, ErrClosed
	}
	// Compression/verification runs without a SQLite writer transaction or
	// pooled connection, so live publication can continue during CPU work.
	rows, err := s.db.QueryContext(ctx, `SELECT cursor,event_id,body FROM events WHERE length(body)>0 ORDER BY cursor LIMIT ?`, limit)
	if err != nil {
		return 0, err
	}
	type candidate struct {
		cursor uint64
		id     string
		body   []byte
		packed []byte
		digest [32]byte
	}
	batch := make([]candidate, 0, limit)
	for rows.Next() {
		var c candidate
		if err = rows.Scan(&c.cursor, &c.id, &c.body); err != nil {
			break
		}
		batch = append(batch, c)
	}
	if err = errors.Join(err, rows.Err(), rows.Close()); err != nil {
		return 0, err
	}
	for i := range batch {
		c := &batch[i]
		if err = ctx.Err(); err != nil {
			return 0, err
		}
		record, err := decodeEvent(c.cursor, c.body)
		if err != nil {
			return 0, err
		}
		if record.Event.ID != c.id {
			return 0, errors.New("event archive source identity mismatch")
		}
		c.packed, err = s.archiveCodec.encode(c.body)
		if err != nil {
			return 0, err
		}
		c.digest = sha256.Sum256(c.body)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	count := 0
	for _, c := range batch {
		var hot, packed, digest []byte
		var size int64
		if err = tx.QueryRowContext(ctx, `SELECT e.body,coalesce(a.body,X''),coalesce(a.original_bytes,0),coalesce(a.digest,X'') FROM events e LEFT JOIN event_archive a ON a.cursor=e.cursor WHERE e.cursor=? AND e.event_id=?`, c.cursor, c.id).Scan(&hot, &packed, &size, &digest); err != nil {
			return 0, err
		}
		current, err := s.loadBody(c.cursor, hot, packed, size, digest)
		if err != nil {
			return 0, err
		}
		if !bytes.Equal(current, c.body) {
			return 0, ErrConflict
		}
		if len(hot) == 0 {
			continue
		} // A concurrent archive already moved it.
		if err = s.reserve(ctx, tx, "event_archive", int64(len(c.packed))+archivedEventOverhead, s.options.MaxArchivedEvents, s.options.MaxArchivedEventBytes); err != nil {
			if errors.Is(err, ErrCapacity) && count > 0 {
				break
			}
			return 0, err
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO event_archive(cursor,body,original_bytes,digest) VALUES(?,?,?,?)`, c.cursor, c.packed, len(c.body), c.digest[:]); err != nil {
			return 0, err
		}
		result, err := tx.ExecContext(ctx, `UPDATE events SET body=X'' WHERE cursor=? AND body=?`, c.cursor, c.body)
		if err != nil {
			return 0, err
		}
		changed, err := result.RowsAffected()
		if err != nil {
			return 0, err
		}
		if changed != 1 {
			return 0, ErrConflict
		}
		result, err = tx.ExecContext(ctx, `UPDATE usage SET rows_count=rows_count-1,bytes_count=bytes_count-? WHERE kind='events' AND rows_count>=1 AND bytes_count>=?`, len(c.body), len(c.body))
		if err != nil {
			return 0, err
		}
		changed, err = result.RowsAffected()
		if err != nil {
			return 0, err
		}
		if changed != 1 {
			return 0, errors.New("event archive usage mismatch")
		}
		count++
	}
	if err = tx.Commit(); err != nil {
		return 0, err
	}
	return count, nil
}
