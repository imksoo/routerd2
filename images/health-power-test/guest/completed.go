// SPDX-License-Identifier: BSD-3-Clause
//go:build linux

package main

import (
	"context"
	"database/sql"
	"encoding/hex"
	"errors"
	"net/url"
	"path/filepath"
)

type completedObservation struct {
	Kind     int    `json:"kind"`
	Name     string `json:"name"`
	Key      string `json:"key"`
	Position uint64 `json:"position"`
	SHA256   string `json:"sha256"`
}

type completedKey struct {
	kind      int
	name, key string
}

func completedMap(items []completedObservation) (map[completedKey]completedObservation, error) {
	if len(items) < 1 || len(items) > 128 {
		return nil, errors.New("empty or oversized completed identity witness")
	}
	out := make(map[completedKey]completedObservation, len(items))
	for _, item := range items {
		digest, err := hex.DecodeString(item.SHA256)
		if err != nil || len(digest) != 32 || len(item.Name) < 1 || len(item.Name) > 128 || len(item.Key) < 1 || len(item.Key) > 256 || item.Position == 0 || item.Position > 1<<63-1 || (item.Kind != 0 && item.Kind != 1) {
			return nil, errors.New("invalid completed work identity witness")
		}
		key := completedKey{item.Kind, item.Name, item.Key}
		if _, exists := out[key]; exists {
			return nil, errors.New("duplicate completed work identity witness")
		}
		out[key] = item
	}
	return out, nil
}

func verifyCompleted(want, got []completedObservation) error {
	prior, err := completedMap(want)
	if err != nil {
		return err
	}
	current, err := completedMap(got)
	if err != nil {
		return err
	}
	for key, item := range prior {
		if current[key] != item {
			return errors.New("power cut lost or changed completed work identity")
		}
	}
	return nil
}

func readCompleted(ctx context.Context) ([]completedObservation, error) {
	// Read-only witness acquisition cannot commit/checkpoint/fsync product state.
	uri := url.URL{Scheme: "file", Path: filepath.Join(stateDir, "events", "journal.sqlite3"), RawQuery: url.Values{"mode": {"ro"}, "_pragma": {"busy_timeout(2000)"}}.Encode()}
	db, err := sql.Open("sqlite", uri.String())
	if err != nil {
		return nil, err
	}
	defer db.Close()
	rows, err := db.QueryContext(ctx, `SELECT kind,name,key,position,lower(hex(digest)) FROM completed_jobs ORDER BY kind,name,key LIMIT 129`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []completedObservation{}
	for rows.Next() {
		var item completedObservation
		if err = rows.Scan(&item.Kind, &item.Name, &item.Key, &item.Position, &item.SHA256); err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

func captureCompleted(ctx context.Context, w *witness, both bool) error {
	return await(ctx, "completed output compaction before power cut", func() error {
		current, err := readCompleted(ctx)
		if err != nil {
			return err
		}
		if _, err = completedMap(current); err != nil {
			return err
		}
		if len(w.Completed) > 0 {
			if err = verifyCompleted(w.Completed, current); err != nil {
				return err
			}
		}
		if both {
			kinds := map[int]bool{}
			for _, item := range current {
				kinds[item.Kind] = true
			}
			if !kinds[0] || !kinds[1] {
				return errors.New("real consumer and producer outputs not yet compacted")
			}
		}
		w.Completed = current
		return nil
	})
}
