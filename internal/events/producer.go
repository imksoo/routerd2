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

// LookupProducer observes a registration without allocating one. Inactive
// optional components can discover retained state without creating new work.
func (s *Store) LookupProducer(ctx context.Context, name string) (ProducerState, bool, error) {
	if !token(name, 128) {
		return ProducerState{}, false, errors.New("invalid observation producer")
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed {
		return ProducerState{}, false, ErrClosed
	}
	var result ProducerState
	err := s.db.QueryRowContext(ctx, `SELECT version,body FROM producers WHERE name=?`, name).Scan(&result.Version, &result.State)
	if errors.Is(err, sql.ErrNoRows) {
		return ProducerState{}, false, nil
	}
	if err != nil {
		return ProducerState{}, false, err
	}
	if !validState(result.State) || result.Version > math.MaxInt64 {
		return ProducerState{}, false, errors.New("invalid durable producer state")
	}
	return result, true, nil
}

// ProducerCheckpoint registers or reads a producer independently of journal
// cursors. Producer and consumer names occupy separate API namespaces, while
// registrations, state bytes and jobs share the store's capacity budgets.
func (s *Store) ProducerCheckpoint(ctx context.Context, name string) (ProducerState, error) {
	if !token(name, 128) {
		return ProducerState{}, errors.New("invalid observation producer")
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed {
		return ProducerState{}, ErrClosed
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ProducerState{}, err
	}
	defer tx.Rollback()
	var result ProducerState
	err = tx.QueryRowContext(ctx, `SELECT version,body FROM producers WHERE name=?`, name).Scan(&result.Version, &result.State)
	if errors.Is(err, sql.ErrNoRows) {
		if err = s.reserve(ctx, tx, "consumer_state", 1, 2, s.options.MaxConsumers, s.options.MaxConsumerStateBytes); err != nil {
			return result, err
		}
		result.State = json.RawMessage(`{}`)
		if _, err = tx.ExecContext(ctx, `INSERT INTO producers(name) VALUES(?)`, name); err != nil {
			return result, err
		}
	} else if err != nil {
		return result, err
	}
	if !validState(result.State) || result.Version > math.MaxInt64 {
		return ProducerState{}, errors.New("invalid durable producer state")
	}
	return result, tx.Commit()
}

func validState(state json.RawMessage) bool {
	return len(state) > 0 && len(state) <= maxBody && utf8.Valid(state) && json.Valid(state)
}

// ProducerTransition atomically replaces producer state and queues its stable
// outputs. A version conflict requires reloading and reevaluation. Caller-side
// current-source fencing and target-side durable idempotency remain required.
func (s *Store) ProducerTransition(ctx context.Context, name string, expected ProducerState, state json.RawMessage, jobs []Job) error {
	if !token(name, 128) || expected.Version >= math.MaxInt64 || !validState(state) {
		return errors.New("invalid bounded producer state or version")
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
	var version uint64
	var stateBytes int64
	if err = tx.QueryRowContext(ctx, `SELECT version,length(body) FROM producers WHERE name=?`, name).Scan(&version, &stateBytes); err != nil {
		return errors.Join(ErrConflict, err)
	}
	if version != expected.Version {
		return ErrConflict
	}
	for _, job := range normalized {
		var prior []byte
		err = tx.QueryRowContext(ctx, `SELECT payload FROM producer_outbox WHERE producer=? AND key=?`, name, job.Key).Scan(&prior)
		if err == nil {
			if !bytes.Equal(prior, job.Payload) {
				return ErrConflict
			}
			continue // Retain the first state version and completed tombstone.
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		_, digest, lookupErr := completedIdentity(ctx, tx, 1, name, job.Key)
		if lookupErr == nil {
			if !sameCompletedPayload(digest, job.Payload) {
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
		if _, err = tx.ExecContext(ctx, `INSERT INTO producer_outbox(producer,key,state_version,payload) VALUES(?,?,?,?)`, name, job.Key, version+1, []byte(job.Payload)); err != nil {
			return err
		}
	}
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
	result, err := tx.ExecContext(ctx, `UPDATE producers SET version=version+1,body=? WHERE name=? AND version=?`, []byte(state), name, version)
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

func (s *Store) ProducerPending(ctx context.Context, name string, limit int) ([]ProducerDelivery, error) {
	if !token(name, 128) || !pageLimit(limit) {
		return nil, errors.New("invalid producer outbox page")
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed {
		return nil, ErrClosed
	}
	rows, err := s.db.QueryContext(ctx, `SELECT key,state_version,payload FROM producer_outbox WHERE producer=? AND done=0 ORDER BY state_version,key LIMIT ?`, name, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]ProducerDelivery, 0)
	for rows.Next() {
		var d ProducerDelivery
		if err = rows.Scan(&d.Key, &d.StateVersion, &d.Payload); err != nil {
			return nil, err
		}
		if !validState(d.Payload) || d.StateVersion == 0 || d.StateVersion > math.MaxInt64 {
			return nil, errors.New("invalid stored producer output")
		}
		result = append(result, d)
	}
	return result, rows.Err()
}

func (s *Store) ProducerComplete(ctx context.Context, name, key string) error {
	if !token(name, 128) || !token(key, 256) {
		return ErrConflict
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed {
		return ErrClosed
	}
	result, err := s.db.ExecContext(ctx, `UPDATE producer_outbox SET done=1 WHERE producer=? AND key=? AND done=0`, name, key)
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
	err = s.db.QueryRowContext(ctx, `SELECT done FROM producer_outbox WHERE producer=? AND key=?`, name, key).Scan(&done)
	if errors.Is(err, sql.ErrNoRows) {
		return s.completedAcknowledged(ctx, 1, name, key)
	}
	if err != nil || !done {
		return errors.Join(ErrConflict, err)
	}
	return nil
}
