package events

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
)

// Completed identities keep the normalized payload hash and original source
// position. They are never expired/reused; discarding them would permit replay.
// The byte budget accounts for kind(1), position(8), digest(32), name and key;
// physical SQLite allocation is observed separately through Capacity.
const tombstoneOverhead int64 = 41

func completedIdentity(ctx context.Context, tx *sql.Tx, kind int, name, key string) (uint64, []byte, error) {
	var position uint64
	var digest []byte
	err := tx.QueryRowContext(ctx, `SELECT position,digest FROM completed_jobs WHERE kind=? AND name=? AND key=?`, kind, name, key).Scan(&position, &digest)
	if err == nil && (position == 0 || len(digest) != sha256.Size) {
		err = errors.New("invalid completed work identity")
	}
	return position, digest, err
}

func sameCompletedPayload(digest, payload []byte) bool {
	sum := sha256.Sum256(payload)
	return bytes.Equal(digest, sum[:])
}

// CompactCompleted atomically replaces at most limit completed consumer and
// producer payloads with bounded SHA-256 identities. Pending work and all
// checkpoints, event history, source positions and dedup keys remain unchanged.
// A full tombstone budget preserves the original payload rather than forgetting
// identity. A fitting prefix may commit; if none fits, ErrCapacity is returned.
func (s *Store) CompactCompleted(ctx context.Context, limit int) (int, error) {
	if !pageLimit(limit) {
		return 0, errors.New("invalid completed work compaction limit")
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed {
		return 0, ErrClosed
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	// Existing indexes lead with done, so waiting work is not scanned. Read a
	// bounded batch before writes; no database/sql reader holds the connection.
	consumerLimit := (limit + 1) / 2
	producerLimit := limit - consumerLimit
	if producerLimit == 0 {
		producerLimit = 1
	}
	rows, err := tx.QueryContext(ctx, `SELECT * FROM (SELECT 0,consumer,key,event_cursor,payload FROM outbox WHERE done=1 LIMIT ?)
UNION ALL SELECT * FROM (SELECT 1,producer,key,state_version,payload FROM producer_outbox WHERE done=1 LIMIT ?) LIMIT ?`, consumerLimit, producerLimit, limit)
	if err != nil {
		return 0, err
	}
	type candidate struct {
		kind      int
		name, key string
		position  uint64
		payload   []byte
	}
	batch := make([]candidate, 0, limit)
	for rows.Next() {
		var c candidate
		if err = rows.Scan(&c.kind, &c.name, &c.key, &c.position, &c.payload); err != nil {
			break
		}
		batch = append(batch, c)
	}
	err = errors.Join(err, rows.Err(), rows.Close())
	if err != nil {
		return 0, err
	}
	count := 0
	for _, c := range batch {
		if !token(c.name, 128) || !token(c.key, 256) || c.position == 0 || !validState(c.payload) {
			return 0, errors.New("invalid completed work payload")
		}
		size := tombstoneOverhead + int64(len(c.name)+len(c.key))
		err = s.reserve(ctx, tx, "dedup", size, s.options.MaxTombstones, s.options.MaxTombstoneBytes)
		if errors.Is(err, ErrCapacity) && count > 0 {
			break
		}
		if err != nil {
			return 0, err
		}
		sum := sha256.Sum256(c.payload)
		if _, err = tx.ExecContext(ctx, `INSERT INTO completed_jobs(kind,name,key,position,digest) VALUES(?,?,?,?,?)`, c.kind, c.name, c.key, c.position, sum[:]); err != nil {
			return 0, err
		}
		query := `DELETE FROM outbox WHERE consumer=? AND key=? AND done=1`
		if c.kind == 1 {
			query = `DELETE FROM producer_outbox WHERE producer=? AND key=? AND done=1`
		}
		result, err := tx.ExecContext(ctx, query, c.name, c.key)
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
		result, err = tx.ExecContext(ctx, `UPDATE usage SET rows_count=rows_count-1,bytes_count=bytes_count-? WHERE kind='outbox' AND rows_count>=1 AND bytes_count>=?`, len(c.payload), len(c.payload))
		if err != nil {
			return 0, err
		}
		changed, err = result.RowsAffected()
		if err != nil {
			return 0, err
		}
		if changed != 1 {
			return 0, errors.New("completed work usage mismatch")
		}
		count++
	}
	if err = tx.Commit(); err != nil {
		return 0, err
	}
	return count, nil
}

func (s *Store) completedAcknowledged(ctx context.Context, kind int, name, key string) error {
	var position uint64
	var digest []byte
	err := s.db.QueryRowContext(ctx, `SELECT position,digest FROM completed_jobs WHERE kind=? AND name=? AND key=?`, kind, name, key).Scan(&position, &digest)
	if err != nil {
		return errors.Join(ErrConflict, err)
	}
	if position == 0 || len(digest) != sha256.Size {
		return errors.New("invalid completed work identity")
	}
	return nil
}
