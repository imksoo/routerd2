package protocolevents

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/imksoo/routerd2/internal/events"
	"github.com/imksoo/routerd2/internal/serviceconfig"
)

func stoppedRelays(ctx context.Context, names []string, drain func(map[string]bool) error) error {
	allowed := map[string]bool{}
	for _, name := range names {
		allowed[name] = true
	}
	return drain(allowed)
}

func TestMissingRegisteredQueueIsNotReplacedByAnInventedEmptyStore(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	journal, err := events.Open(events.Options{Directory: filepath.Join(root, "events")})
	if err != nil {
		t.Fatal(err)
	}
	defer journal.Close()
	c := NewRetirement(journal, root)
	if err = c.Register(ctx, []string{"wan"}); err != nil {
		t.Fatal(err)
	}
	path := serviceconfig.HealthEventDirectory(root, "wan")
	if err = os.Rename(path, path+".held"); err != nil {
		t.Fatal(err)
	}
	if err = c.Recover(ctx, nil, stoppedRelays); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("missing owned queue was hidden", err)
	}
	if _, err = os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("missing queue was recreated", err)
	}
	if c.Status().Retiring != 1 {
		t.Fatal("missing queue registration was forgotten", c.Status())
	}
}

func TestRetiredRelaySurvivesControlRestartAndDoesNotTouchActiveOrUnregisteredStores(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	journal, err := events.Open(events.Options{Directory: filepath.Join(root, "events")})
	if err != nil {
		t.Fatal(err)
	}
	defer journal.Close()
	c := NewRetirement(journal, root)
	if err = c.Register(ctx, []string{"wan"}); err != nil {
		t.Fatal(err)
	}
	worker, err := events.OpenExisting(events.Options{Directory: serviceconfig.HealthEventDirectory(root, "wan")})
	if err != nil {
		t.Fatal("registration did not initialize queue before worker launch", err)
	}
	box := NewOutbox(worker, "wan")
	if err = box.Enqueue(ctx, probeEvent("wan")); err != nil {
		t.Fatal(err)
	}
	foreign, err := events.Open(events.Options{Directory: serviceconfig.HealthEventDirectory(root, "foreign")})
	if err != nil {
		t.Fatal(err)
	}
	defer foreign.Close()
	if err = NewOutbox(foreign, "foreign").Enqueue(ctx, probeEvent("foreign")); err != nil {
		t.Fatal(err)
	}
	if err = foreign.Close(); err != nil {
		t.Fatal(err)
	} // an unregistered, unlocked store must still be ignored
	if err = c.Recover(ctx, []string{"wan"}, func(context.Context, []string, func(map[string]bool) error) error {
		return nil // fresh runtime guard withholds every active worker
	}); err != nil {
		t.Fatal(err)
	}
	if err = c.Recover(ctx, nil, stoppedRelays); err != nil {
		t.Fatal("worker lock should defer recovery", err)
	}
	if rows, err := journal.List(ctx, 0, 16); err != nil || len(rows) != 0 {
		t.Fatal("collector raced live ownership", rows, err)
	}
	if err = worker.Close(); err != nil {
		t.Fatal(err)
	}
	c = NewRetirement(journal, root) // no in-memory source history survives
	if err = c.Recover(ctx, nil, stoppedRelays); err != nil {
		t.Fatal(err)
	}
	rows, err := journal.List(ctx, 0, 16)
	if err != nil || len(rows) != 1 || rows[0].Event.Topic != "routerd.healthcheck.wan.passed" {
		t.Fatal(rows, err)
	}
	if c.Status().Registered != 0 || c.Status().Retiring != 0 {
		t.Fatal("empty retired queue kept registration", c.Status())
	}
	foreign, err = events.OpenExisting(events.Options{Directory: serviceconfig.HealthEventDirectory(root, "foreign")})
	if err != nil {
		t.Fatal(err)
	}
	defer foreign.Close()
	pending, err := NewOutbox(foreign, "foreign").Pending(ctx)
	if err != nil || len(pending) != 1 {
		t.Fatal("unregistered state was adopted", pending, err)
	}
	// No filesystem deletion is needed or permitted to prove drainage.
	worker, err = events.OpenExisting(events.Options{Directory: serviceconfig.HealthEventDirectory(root, "wan")})
	if err != nil {
		t.Fatal(err)
	}
	defer worker.Close()
	pending, err = NewOutbox(worker, "wan").Pending(ctx)
	if err != nil || len(pending) != 0 {
		t.Fatal(pending, err)
	}
}

func TestRetirementPartialTargetFailureKeepsRegistryAndOriginalPendingPrefix(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	opts := events.Options{Directory: filepath.Join(root, "events"), MaxEvents: 1}
	journal, err := events.Open(opts)
	if err != nil {
		t.Fatal(err)
	}
	c := NewRetirement(journal, root)
	if err = c.Register(ctx, []string{"wan"}); err != nil {
		t.Fatal(err)
	}
	worker, err := events.OpenExisting(events.Options{Directory: serviceconfig.HealthEventDirectory(root, "wan")})
	if err != nil {
		t.Fatal(err)
	}
	box := NewOutbox(worker, "wan")
	for range 2 {
		if err = box.Enqueue(ctx, probeEvent("wan")); err != nil {
			t.Fatal(err)
		}
	}
	original, err := box.Pending(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err = worker.Close(); err != nil {
		t.Fatal(err)
	}
	if err = c.Recover(ctx, nil, stoppedRelays); !errors.Is(err, events.ErrCapacity) {
		t.Fatal("partial target failure was acknowledged", err)
	}
	if c.Status().Registered != 1 || c.Status().Retiring != 1 {
		t.Fatal(c.Status())
	}
	if err = journal.Close(); err != nil {
		t.Fatal(err)
	}
	opts.MaxEvents = 10
	journal, err = events.Open(opts)
	if err != nil {
		t.Fatal(err)
	}
	defer journal.Close()
	c = NewRetirement(journal, root)
	if err = c.Recover(ctx, nil, stoppedRelays); err != nil {
		t.Fatal(err)
	}
	rows, err := journal.List(ctx, 0, 16)
	if err != nil || len(rows) != 2 || rows[0].Event.ID != original[0].ID || rows[1].Event.ID != original[1].ID {
		t.Fatal("restart changed or duplicated queued history", rows, err)
	}
}

func TestRegistryRemovalFailureRetainsRecoveryWithoutReplayingAnAcknowledgedQueue(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	journal, err := events.Open(events.Options{Directory: filepath.Join(root, "events")})
	if err != nil {
		t.Fatal(err)
	}
	defer journal.Close()
	c := NewRetirement(journal, root)
	if err = c.Register(ctx, []string{"wan"}); err != nil {
		t.Fatal(err)
	}
	worker, err := events.OpenExisting(events.Options{Directory: serviceconfig.HealthEventDirectory(root, "wan")})
	if err != nil {
		t.Fatal(err)
	}
	if err = NewOutbox(worker, "wan").Enqueue(ctx, probeEvent("wan")); err != nil {
		t.Fatal(err)
	}
	worker.Close()
	injector, err := sql.Open("sqlite", filepath.Join(root, "events", "journal.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer injector.Close()
	if _, err = injector.Exec(`CREATE TRIGGER fail_retirement_registry BEFORE UPDATE ON producers WHEN NEW.name='control-health-relays' BEGIN SELECT RAISE(ABORT,'injected retirement registry failure'); END`); err != nil {
		t.Fatal(err)
	}
	if err = c.Recover(ctx, nil, stoppedRelays); err == nil {
		t.Fatal("registry failure hidden")
	}
	if c.Status().Retiring != 1 {
		t.Fatal("registration forgotten before durable save", c.Status())
	}
	if _, err = injector.Exec(`DROP TRIGGER fail_retirement_registry`); err != nil {
		t.Fatal(err)
	}
	c = NewRetirement(journal, root)
	if err = c.Recover(ctx, nil, stoppedRelays); err != nil {
		t.Fatal(err)
	}
	rows, err := journal.List(ctx, 0, 16)
	if err != nil || len(rows) != 1 || c.Status().Retiring != 0 {
		t.Fatal(rows, c.Status(), err)
	}
}

func TestRetirementRotatesPastBusyQueuesWithinItsBatchBound(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	journal, err := events.Open(events.Options{Directory: filepath.Join(root, "events")})
	if err != nil {
		t.Fatal(err)
	}
	defer journal.Close()
	c := NewRetirement(journal, root)
	names := []string{}
	for i := 0; i < 10; i++ {
		names = append(names, fmt.Sprintf("wan%d", i))
	}
	if err = c.Register(ctx, names); err != nil {
		t.Fatal(err)
	}
	for i, name := range names {
		s, err := events.OpenExisting(events.Options{Directory: serviceconfig.HealthEventDirectory(root, name)})
		if err != nil {
			t.Fatal(err)
		}
		if err = NewOutbox(s, name).Enqueue(ctx, probeEvent(name)); err != nil {
			t.Fatal(err)
		}
		if i < 8 {
			defer s.Close()
		} else {
			s.Close()
		}
	}
	guard := func(ctx context.Context, names []string, drain func(map[string]bool) error) error {
		if len(names) > 8 {
			t.Fatal("unbounded recovery batch", len(names))
		}
		return stoppedRelays(ctx, names, drain)
	}
	for range 2 {
		if err = c.Recover(ctx, nil, guard); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := journal.List(ctx, 0, 16)
	if err != nil || len(rows) != 2 {
		t.Fatal("busy first entries starved later queues", rows, err)
	}
}
