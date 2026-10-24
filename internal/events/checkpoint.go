package events

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"math"
	"unicode/utf8"
)

// Checkpoint registers a stateful consumer at the journal tip or reads its
// existing state and cursor atomically. Use Transition, not Drain/Schedule, for
// every subsequent change, including a timer without a new source event.
func (s *Store) Checkpoint(ctx context.Context, name string) (Checkpoint, error) {
	return s.consumer(ctx, name, true)
}

func (s *Store) consumer(ctx context.Context, name string, stateful bool) (Checkpoint, error) {
	if !token(name, 128) {
		return Checkpoint{}, errors.New("invalid event consumer")
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed {
		return Checkpoint{}, ErrClosed
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Checkpoint{}, err
	}
	defer tx.Rollback()
	var result Checkpoint
	var registered bool
	var stateCursor uint64
	err = tx.QueryRowContext(ctx, `SELECT cursor,version,stateful,state_cursor,body FROM consumers WHERE name=?`, name).Scan(&result.Cursor, &result.Version, &registered, &stateCursor, &result.State)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return result, err
	}
	var tip uint64
	if queryErr := tx.QueryRowContext(ctx, `SELECT coalesce(max(cursor),0) FROM events`).Scan(&tip); queryErr != nil {
		return result, queryErr
	}
	if err == nil {
		if result.Cursor > tip || registered && (!stateful || result.Cursor != stateCursor) {
			return result, ErrConflict
		}
		if len(result.State) > maxBody || !utf8.Valid(result.State) || !json.Valid(result.State) {
			return result, errors.New("invalid durable consumer state")
		}
		if stateful && !registered {
			if _, err = tx.ExecContext(ctx, `UPDATE consumers SET stateful=1,state_cursor=cursor WHERE name=?`, name); err != nil {
				return result, err
			}
		}
	} else {
		if err = s.reserve(ctx, tx, "consumer_state", 2, s.options.MaxConsumers, s.options.MaxConsumerStateBytes); err != nil {
			return result, err
		}
		result = Checkpoint{Cursor: tip, State: json.RawMessage(`{}`)}
		if _, err = tx.ExecContext(ctx, `INSERT INTO consumers(name,cursor,stateful,state_cursor) VALUES(?,?,?,?)`, name, tip, stateful, tip); err != nil {
			return result, err
		}
	}
	return result, tx.Commit()
}

// Transition commits state, source cursor and work together. Keeping the
// cursor unchanged supports timers; the version must still match. A transition
// cannot skip a source event or use cursor zero as the source of a job.
// This API schedules work; executing it still needs target-side idempotency.
func (s *Store) Transition(ctx context.Context, name string, expected Checkpoint, eventCursor uint64, state json.RawMessage, jobs []Job) error {
	if expected.Version >= math.MaxInt64 || len(state) == 0 || len(state) > maxBody || !utf8.Valid(state) || !json.Valid(state) {
		return errors.New("invalid bounded consumer state or version")
	}
	return s.schedule(ctx, name, expected.Cursor, eventCursor, jobs, &expected, state)
}
