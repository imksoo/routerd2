package events

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"github.com/imksoo/routerd/pkg/federation"
	"slices"
	"strings"
	"time"
)

// Federation preserves the cross-node envelope without a second history store.
type Federation struct {
	ID         string `json:"id"`
	Group      string `json:"group"`
	SourceNode string `json:"source_node"`
	Subject    string `json:"subject,omitempty"`
	DedupeKey  string `json:"dedupe_key,omitempty"`
}

// PruneFederation applies the configured group's limit. The existing deletion
// operation retains events still needed by any journal consumer or outbox.
func (s *Store) PruneFederation(ctx context.Context, group string, maxAge time.Duration, maxEvents int, now time.Time) (int, error) {
	if maxAge == 0 && maxEvents == 0 {
		return 0, nil
	}
	type candidate struct {
		cursor   uint64
		observed time.Time
		id       string
	}
	var rows []candidate
	var after uint64
	for {
		page, err := s.List(ctx, after, 256)
		if err != nil {
			return 0, err
		}
		for _, r := range page {
			after = r.Cursor
			if f := r.Event.Federation; f != nil && f.Group == group {
				rows = append(rows, candidate{r.Cursor, r.Event.ObservedAt, r.Event.Federation.ID})
			}
		}
		if len(page) < 256 {
			break
		}
	}
	slices.SortFunc(rows, func(a, b candidate) int {
		if order := a.observed.Compare(b.observed); order != 0 {
			return order
		}
		return strings.Compare(a.id, b.id)
	})
	deleted := 0
	for i, row := range rows {
		if !(maxEvents > 0 && i < len(rows)-maxEvents) && !(maxAge > 0 && row.observed.Before(now.Add(-maxAge))) {
			continue
		}
		s.mu.RLock()
		if s.closed {
			s.mu.RUnlock()
			return deleted, ErrClosed
		}
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			s.mu.RUnlock()
			return deleted, err
		}
		// The configured retention limit also ends federation delivery for this
		// fact. Other consumers and their pending work remain protected.
		if _, err = tx.ExecContext(ctx, `UPDATE outbox SET done=1 WHERE event_cursor=? AND consumer LIKE 'event-group:%'`, row.cursor); err != nil {
			_ = tx.Rollback()
			s.mu.RUnlock()
			return deleted, err
		}
		changed, err := removeHistory(ctx, tx, row.cursor)
		if err == nil {
			err = tx.Commit()
		} else {
			err = errors.Join(err, tx.Rollback())
		}
		s.mu.RUnlock()
		if err != nil {
			return deleted, err
		}
		if changed {
			deleted++
		}
	}
	return deleted, nil
}

// Journal keeps wire identity separate from the journal's global identity;
// different configured groups may legitimately use the same external ID.
func FromFederation(e federation.Event) Event {
	sum := sha256.Sum256([]byte(e.Group + "\x00" + e.ID))
	return Event{ID: "federation:" + hex.EncodeToString(sum[:]), Topic: e.Type, Source: "event_group", ObservedAt: e.ObservedAt, ExpiresAt: e.ExpiresAt, Attributes: e.Payload, Federation: &Federation{ID: e.ID, Group: e.Group, SourceNode: e.SourceNode, Subject: e.Subject, DedupeKey: e.DedupeKey}}
}
func ToFederation(e Event) federation.Event {
	f := e.Federation
	return federation.Event{ID: f.ID, Group: f.Group, SourceNode: f.SourceNode, Subject: f.Subject, DedupeKey: f.DedupeKey, Type: e.Topic, Payload: e.Attributes, ObservedAt: e.ObservedAt, ExpiresAt: e.ExpiresAt}
}

// RetireFederationPeers releases only this feature's cursors. Pending jobs are
// marked complete and reclaimed by the existing compactor, not a second queue.
func (s *Store) RetireFederationPeers(ctx context.Context, keep []string) error {
	active := make(map[string]bool, len(keep))
	for _, name := range keep {
		active[name] = true
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
	rows, err := tx.QueryContext(ctx, `SELECT name,length(body) FROM consumers WHERE name LIKE 'event-group:%'`)
	if err != nil {
		return err
	}
	type entry struct {
		name string
		size int64
	}
	var retired []entry
	for rows.Next() {
		var value entry
		if err = rows.Scan(&value.name, &value.size); err != nil {
			break
		}
		if !active[value.name] {
			retired = append(retired, value)
		}
	}
	if err = errors.Join(err, rows.Err(), rows.Close()); err != nil {
		return err
	}
	for _, v := range retired {
		if _, err = tx.ExecContext(ctx, `UPDATE outbox SET done=1 WHERE consumer=?`, v.name); err != nil {
			return err
		}
		result, err := tx.ExecContext(ctx, `DELETE FROM consumers WHERE name=? AND NOT EXISTS(SELECT 1 FROM outbox WHERE consumer=?)`, v.name, v.name)
		if err != nil {
			return err
		}
		changed, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if changed != 0 {
			if _, err = tx.ExecContext(ctx, `UPDATE usage SET rows_count=rows_count-1,bytes_count=bytes_count-? WHERE kind='consumer_state'`, v.size); err != nil {
				return err
			}
		}
	}
	return tx.Commit()
}
