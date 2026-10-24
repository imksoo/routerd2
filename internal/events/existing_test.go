package events

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestExistingStoreNeverCreatesMissingStateAndDistinguishesOwned(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "missing", "events")
	if _, err := OpenExisting(Options{Directory: path}); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("missing store was invented", err)
	}
	if _, err := os.Stat(filepath.Join(root, "missing")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("read created a directory", err)
	}
	s, err := Open(Options{Directory: path})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = OpenExisting(Options{Directory: path}); !errors.Is(err, ErrOwned) {
		t.Fatal("live ownership was not distinct", err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = OpenExisting(Options{Directory: path})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
}

func TestProducerLookupIsReadOnlyAndDoesNotReserveMissingNames(t *testing.T) {
	s, err := Open(Options{Directory: filepath.Join(t.TempDir(), "events"), MaxConsumers: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	if _, found, err := s.LookupProducer(ctx, "absent"); err != nil || found {
		t.Fatal(found, err)
	}
	cp, err := s.ProducerCheckpoint(ctx, "real")
	if err != nil {
		t.Fatal("lookup consumed registration capacity", err)
	}
	read, found, err := s.LookupProducer(ctx, "real")
	if err != nil || !found || read.Version != cp.Version || string(read.State) != string(cp.State) {
		t.Fatal(read, found, err)
	}
}
