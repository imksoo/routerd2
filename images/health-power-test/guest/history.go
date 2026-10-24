// SPDX-License-Identifier: BSD-3-Clause
//go:build linux

package main

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"

	"github.com/imksoo/routerd2/internal/control"
	"github.com/imksoo/routerd2/internal/events"
	"github.com/imksoo/routerd2/internal/localapi"
	"github.com/imksoo/routerd2/internal/protocolevents"
	"github.com/imksoo/routerd2/internal/serviceconfig"
)

func eventHash(event events.Event) string {
	body, err := json.Marshal(event)
	if err != nil {
		panic(err)
	}
	hash := sha256.Sum256(body)
	return hex.EncodeToString(hash[:])
}

func verifyHistory(want []observation, records []events.Record, compareCursor bool) ([]observation, error) {
	if len(want) < 2 || len(want) > protocolevents.MaxPending {
		return nil, errors.New("invalid expected history size")
	}
	byID := map[string]int{}
	out := append([]observation(nil), want...)
	for i, e := range out {
		if _, exists := byID[e.ID]; exists {
			return nil, errors.New("duplicate expected ID")
		}
		byID[e.ID] = i
	}
	seen := map[string]bool{}
	cursors := map[uint64]bool{}
	for _, record := range records {
		i, match := byID[record.Event.ID]
		if !match {
			continue
		}
		if seen[record.Event.ID] || record.Cursor == 0 || cursors[record.Cursor] {
			return nil, errors.New("duplicate event identity/cursor or zero cursor")
		}
		if eventHash(record.Event) != out[i].SHA256 || compareCursor && record.Cursor != out[i].Cursor {
			return nil, errors.New("historical event content or cursor changed")
		}
		seen[record.Event.ID] = true
		cursors[record.Cursor] = true
		out[i].Cursor = record.Cursor
	}
	if len(seen) != len(want) {
		return nil, fmt.Errorf("only %d/%d accepted observations published", len(seen), len(want))
	}
	return out, nil
}

func pending(ctx context.Context) ([]events.Event, error) {
	uri := url.URL{Scheme: "file", Path: filepath.Join(serviceconfig.HealthEventDirectory(stateDir, "wan"), "journal.sqlite3"), RawQuery: "mode=ro"}
	db, err := sql.Open("sqlite", uri.String())
	if err != nil {
		return nil, err
	}
	defer db.Close()
	var body []byte
	if err = db.QueryRowContext(ctx, `SELECT body FROM producers WHERE name='health-probe-relay'`).Scan(&body); err != nil {
		return nil, err
	}
	var q struct {
		Schema  int            `json:"schema_version"`
		Pending []events.Event `json:"pending"`
	}
	if err = json.Unmarshal(body, &q); err != nil {
		return nil, err
	}
	if q.Schema != 1 || len(q.Pending) > protocolevents.MaxPending {
		return nil, errors.New("invalid queue checkpoint")
	}
	for _, e := range q.Pending {
		if err = protocolevents.ValidateHealth("wan", e); err != nil {
			return nil, err
		}
	}
	return q.Pending, nil
}

func observations(queue []events.Event) []observation {
	out := make([]observation, 0, len(queue))
	for _, e := range queue {
		out = append(out, observation{ID: e.ID, SHA256: eventHash(e)})
	}
	return out
}

func requirePendingOriginals(want []observation, queue []events.Event) error {
	byID := map[string]events.Event{}
	for _, e := range queue {
		byID[e.ID] = e
	}
	for _, e := range want {
		saved, found := byID[e.ID]
		if !found || eventHash(saved) != e.SHA256 {
			return errors.New("power cut lost or changed an accepted local observation")
		}
	}
	return nil
}

func history(ctx context.Context, client *localapi.Client) ([]events.Record, error) {
	var out []events.Record
	after := uint64(0)
	for {
		var page control.ObservationPage
		if err := client.Do(ctx, "GET", fmt.Sprintf("/v1/observations?after_cursor=%d&limit=16", after), nil, &page); err != nil {
			return nil, err
		}
		out = append(out, page.Records...)
		if len(out) > 10000 {
			return nil, errors.New("fixture journal unexpectedly large")
		}
		if !page.More {
			return out, nil
		}
		if page.NextCursor <= after {
			return nil, errors.New("history cursor did not advance")
		}
		after = page.NextCursor
	}
}

// This assertion runs after verified logical publication but before the second
// host cut. Every original raw record must already be in the cold table, not
// merely readable from a still-hot body. The later boot repeats the assertion.
func requireArchivedObservations(ctx context.Context, w *witness) error {
	uri := url.URL{Scheme: "file", Path: filepath.Join(stateDir, "events", "journal.sqlite3"), RawQuery: url.Values{"mode": {"ro"}, "_pragma": {"busy_timeout(2000)"}}.Encode()}
	db, err := sql.Open("sqlite", uri.String())
	if err != nil {
		return err
	}
	defer db.Close()
	for _, o := range w.Observations {
		var cursor uint64
		var hot, size int
		var digest []byte
		if err = db.QueryRowContext(ctx, `SELECT e.cursor,length(e.body),a.original_bytes,a.digest FROM events e JOIN event_archive a ON a.cursor=e.cursor WHERE e.event_id=?`, o.ID).Scan(&cursor, &hot, &size, &digest); err != nil {
			return err
		}
		if cursor != o.Cursor || hot != 0 || size <= 0 || hex.EncodeToString(digest) != o.SHA256 {
			return errors.New("raw history is not identically archived before power cut")
		}
	}
	w.ArchivedObservations = len(w.Observations)
	return nil
}
