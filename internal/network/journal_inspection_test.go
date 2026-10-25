//go:build linux

package network

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"golang.org/x/sys/unix"
)

func TestJournalInspectionReadsRealPersistedStateWhileWriterOwnsLock(t *testing.T) {
	k := fake()
	e, opts := engineFor(t, k)
	if _, err := e.Apply(context.Background(), 7, exampleDesired()); err != nil {
		t.Fatal(err)
	}
	// Persist applying as a real interrupted operation; the inspector reports
	// disk state, while reopening the engine degrades it only in memory.
	e.state.Status = "applying"
	if err := e.save(); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(opts.StatePath)
	if err != nil {
		t.Fatal(err)
	}
	stat, err := os.Stat(opts.StatePath)
	if err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(filepath.Dir(opts.StatePath))
	if err != nil {
		t.Fatal(err)
	}
	writes := len(k.writes)
	got, err := InspectJournal(context.Background(), opts.StatePath)
	digest := sha256.Sum256(before)
	if err != nil || !got.Present || got.Version != JournalVersion || got.Revision != 7 || got.AttemptedRevision != 7 || got.Status != "applying" || got.SHA256 != hex.EncodeToString(digest[:]) {
		t.Fatalf("wrong persisted observation: %+v %v", got, err)
	}
	method, err := e.InspectJournal(context.Background())
	if err != nil || method != got {
		t.Fatal("worker and path inspection disagree", err)
	}
	after, err := os.ReadFile(opts.StatePath)
	if err != nil || string(before) != string(after) {
		t.Fatal("inspection rewrote journal", err)
	}
	now, err := os.Stat(opts.StatePath)
	if err != nil || !os.SameFile(stat, now) || stat.Mode() != now.Mode() || stat.ModTime() != now.ModTime() {
		t.Fatal("inspection replaced or changed journal", err)
	}
	newEntries, err := os.ReadDir(filepath.Dir(opts.StatePath))
	if err != nil || !reflect.DeepEqual(entries, newEntries) || len(k.writes) != writes {
		t.Fatal("inspection created state or touched kernel", err)
	}
	if other, err := newEngine(opts, k); err == nil {
		other.Close()
		t.Fatal("inspection released writer lock")
	}
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := e.InspectJournal(context.Background()); err == nil {
		t.Fatal("closed worker inspected a journal")
	}
	reopened, err := newEngine(opts, k)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if reopened.state.Status != "degraded" {
		t.Fatal("restart lost interrupted-apply handling")
	}
}

func TestJournalInspectionDistinguishesAbsenceFromUninspectableParent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "network.json")
	got, err := InspectJournal(context.Background(), path)
	if err != nil || got != (JournalInspection{}) {
		t.Fatal("inspected absence is not explicit", got, err)
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil || len(entries) != 0 {
		t.Fatal("absence inspection initialized a store", err)
	}
	if _, err := InspectJournal(context.Background(), filepath.Join(filepath.Dir(path), "missing", "network.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("uninspectable parent was called an absent journal", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := InspectJournal(ctx, path); !errors.Is(err, context.Canceled) {
		t.Fatal("canceled observation proceeded", err)
	}
}

func TestJournalInspectionRejectsSpecialAndOversizeFiles(t *testing.T) {
	for _, kind := range []string{"fifo", "oversize", "empty"} {
		t.Run(kind, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "network.json")
			if kind == "fifo" {
				if err := unix.Mkfifo(path, 0600); err != nil {
					t.Fatal(err)
				}
			} else {
				f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
				if err != nil {
					t.Fatal(err)
				}
				if kind == "oversize" {
					if err := f.Truncate(MaxJournalBytes + 1); err != nil {
						t.Fatal(err)
					}
				}
				if err := f.Close(); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := InspectJournal(context.Background(), path); err == nil {
				t.Fatal("invalid journal inspected as present or absent")
			}
			if e, err := newEngine(Options{StatePath: path}, fake()); err == nil {
				e.Close()
				t.Fatal("reader disagreed with inspector")
			}
		})
	}
}

func TestJournalInspectionDoesNotRenewBootScopedAddressAuthority(t *testing.T) {
	k := fake()
	e, opts := engineFor(t, k)
	// Use a past, syntactically valid BOOTTIME fence. This tests preservation,
	// not current kernel ownership, and never reads or adjusts the host clock.
	deadline := BootLifetime{BootID: "11111111-2222-4333-8444-555555555555", PreferredNanos: 1, ValidNanos: 2}
	e.state.Addresses = []ownedAddress{{Interface: "wan0", Address: "192.0.2.1/24", Options: AddressOptions{BootLifetime: deadline}}}
	if err := e.save(); err != nil {
		t.Fatal(err)
	}
	if _, err := InspectJournal(context.Background(), opts.StatePath); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(opts.StatePath)
	if err != nil {
		t.Fatal(err)
	}
	var persisted journal
	if err := json.Unmarshal(raw, &persisted); err != nil {
		t.Fatal(err)
	}
	if persisted.Addresses[0].Options.BootLifetime != deadline || len(k.writes) != 0 {
		t.Fatal("inspection renewed or projected expired authority")
	}
}
