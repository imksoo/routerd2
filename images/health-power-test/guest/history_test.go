// SPDX-License-Identifier: BSD-3-Clause
//go:build linux

package main

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/imksoo/routerd2/internal/events"
)

func TestHistoryWitnessPreservesFullContentAndStableCursor(t *testing.T) {
	first := events.Event{ID: "health/wan/" + strings.Repeat("a", 32), Topic: "routerd.healthcheck.wan.passed", Source: "routerd-healthcheck", ObservedAt: time.Date(2026, 10, 24, 0, 0, 0, 0, time.UTC)}
	second := first
	second.ID = "health/wan/" + strings.Repeat("b", 32)
	want := []observation{{ID: first.ID, SHA256: eventHash(first)}, {ID: second.ID, SHA256: eventHash(second)}}
	records := []events.Record{{Cursor: 5, Event: first}, {Cursor: 6, Event: second}}
	got, err := verifyHistory(want, records, false)
	if err != nil || len(got) != 2 || got[0].Cursor != 5 || got[1].Cursor != 6 {
		t.Fatal(got, err)
	}
	if restored, err := verifyHistory(got, records, true); err != nil || !reflect.DeepEqual(got, restored) {
		t.Fatal("second power cut changed immutable history", restored, err)
	}
	for name, mutate := range map[string]func([]events.Record) []events.Record{
		"missing":      func(v []events.Record) []events.Record { return v[:1] },
		"duplicate ID": func(v []events.Record) []events.Record { return append(v, v[0]) },
		"changed body": func(v []events.Record) []events.Record {
			v[0].Event.ObservedAt = v[0].Event.ObservedAt.Add(time.Second)
			return v
		},
		"changed cursor":   func(v []events.Record) []events.Record { v[0].Cursor += 10; return v },
		"zero cursor":      func(v []events.Record) []events.Record { v[0].Cursor = 0; return v },
		"duplicate cursor": func(v []events.Record) []events.Record { v[0].Cursor = v[1].Cursor; return v },
	} {
		t.Run(name, func(t *testing.T) {
			copy := append([]events.Record(nil), records...)
			if _, err := verifyHistory(got, mutate(copy), true); err == nil {
				t.Fatal("invalid history witness was accepted")
			}
		})
	}
}
