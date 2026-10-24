package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/imksoo/routerd2/internal/network"
)

func largeDocument(routes int) Document {
	d := Document{SchemaVersion: 1, Network: network.Desired{Interfaces: []network.Interface{{Name: "eth0"}}}}
	for i := 0; i < routes; i++ {
		d.Network.Routes = append(d.Network.Routes, network.Route{
			Destination: fmt.Sprintf("10.%d.%d.%d/32", i>>16, i>>8&255, i&255),
			Gateway:     "192.0.2.1", Interface: "eth0", Metric: 100, Table: 254,
		})
	}
	return d
}

func TestLargeConfirmedRecordSurvivesSaveLoadAndLegacyIndentation(t *testing.T) {
	doc := largeDocument(12000)
	if err := Validate(doc); err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(doc)
	if err != nil || len(encoded) > MaxSize || len(encoded) < MaxSize/2 {
		t.Fatalf("fixture must be a large individually valid document: %d, %v", len(encoded), err)
	}
	record := Committed{Revision: 2, Config: doc, LastRequestID: "large-confirmed", LastRequestHash: strings.Repeat("a", 64),
		Confirmation: &Confirmation{Previous: doc, PreviousRevision: 1, RequestID: "large-confirmed", Deadline: time.Now().Add(time.Minute).UTC()}}
	store := &Store{Dir: t.TempDir()}
	if err := store.Stage(record); err != nil {
		t.Fatal(err)
	}
	if err := store.Save(record); err != nil {
		t.Fatal(err)
	}
	stored, err := os.ReadFile(filepath.Join(store.Dir, "config.json"))
	if err != nil || len(stored) <= MaxSize || len(stored) > MaxRecordSize || bytes.Contains(stored, []byte("\n  \"")) {
		t.Fatalf("record must use its own compact size budget: %d, %v", len(stored), err)
	}
	got, err := store.Load()
	if err != nil || !reflect.DeepEqual(got, record) {
		t.Fatalf("confirmed record cannot be recovered: %v", err)
	}
	pending, err := os.ReadFile(filepath.Join(store.Dir, "pending.json"))
	if err != nil || !bytes.Equal(stored, pending) {
		t.Fatal("pending and committed record encodings disagree", err)
	}
	// Accept a prior version's indented JSON even when whitespace alone takes
	// it beyond the new compact write limit. Each embedded document is bounded.
	if err := AtomicJSON(filepath.Join(store.Dir, "config.json"), record); err != nil {
		t.Fatal(err)
	}
	got, err = store.Load()
	if err != nil || !reflect.DeepEqual(got, record) {
		t.Fatalf("previous indented record cannot be recovered: %v", err)
	}
}

func TestRecordRefusesUnrecoverableWritesBeforeReplacement(t *testing.T) {
	store := &Store{Dir: t.TempDir()}
	baseline := Committed{Revision: 1, Config: Document{SchemaVersion: 1}}
	if err := store.Save(baseline); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(filepath.Join(store.Dir, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, invalid := range []Committed{
		{Revision: 2, Config: largeDocument(30000)},
		{Revision: 2, Config: baseline.Config, LastRequestID: strings.Repeat("x", 129)},
		{Revision: 2, Config: baseline.Config, LastRequestID: "pending", Confirmation: &Confirmation{Previous: baseline.Config, PreviousRevision: 2, RequestID: "pending", Deadline: time.Now()}},
		{Revision: 0, Config: baseline.Config},
	} {
		if err := store.Save(invalid); err == nil {
			t.Fatal("accepted unrecoverable committed record")
		}
		if err := store.Stage(invalid); err == nil {
			t.Fatal("accepted unrecoverable pending record")
		}
	}
	after, err := os.ReadFile(filepath.Join(store.Dir, "config.json"))
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("rejected record replaced the durable baseline", err)
	}
	if _, err := os.Stat(filepath.Join(store.Dir, "pending.json")); !os.IsNotExist(err) {
		t.Fatal("rejected record created a pending file", err)
	}
}

func TestRecordStrictJSONAndReadBound(t *testing.T) {
	store := &Store{Dir: t.TempDir()}
	for _, data := range []string{
		`{"revision":1,"revision":2,"config":{"schema_version":1,"network":{}}}`,
		`{"revision":1,"config":{"schema_version":1,"network":{"interfaces":[],"interfaces":[]}}}`,
		`{"revision":1,"typo":true,"config":{"schema_version":1,"network":{}}}`,
		`{"revision":1,"config":{"schema_version":1,"network":{}}} {}`,
		`revision: 1`,
		strings.Repeat(" ", MaxStoredReadSize+1),
	} {
		if err := os.WriteFile(filepath.Join(store.Dir, "config.json"), []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := store.Load(); err == nil {
			t.Fatal("accepted ambiguous, unknown, non-JSON or oversized stored state")
		}
	}
}
