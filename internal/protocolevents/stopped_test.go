package protocolevents

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/imksoo/routerd2/internal/events"
	"github.com/imksoo/routerd2/internal/serviceconfig"
)

func TestStoppedDeclaredRelayDrainsWithoutDiscardingFutureWorkerOwnership(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	opts := events.Options{Directory: filepath.Join(root, "events"), MaxEvents: 1}
	journal, err := events.Open(opts)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = journal.Close() }()
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
	registered, _, err := journal.LookupProducer(ctx, retirementProducer)
	if err != nil {
		t.Fatal(err)
	}
	if err = c.Recover(ctx, []string{"wan"}, stoppedRelays); !errors.Is(err, events.ErrCapacity) {
		t.Fatal("stopped declared relay did not attempt actual history delivery", err)
	}
	if c.Status().StoppedPending != 1 || len(c.Recovered()) != 0 {
		t.Fatal("partial delivery claimed complete recovery", c.Status(), c.Recovered())
	}
	if err = journal.Close(); err != nil {
		t.Fatal(err)
	}
	opts.MaxEvents = 10
	journal, err = events.Open(opts)
	if err != nil {
		t.Fatal(err)
	}
	c = NewRetirement(journal, root) // both source and queue survive control replacement
	if err = c.Recover(ctx, []string{"wan"}, stoppedRelays); err != nil {
		t.Fatal(err)
	}
	if c.Status().StoppedPending != 0 || !reflect.DeepEqual(c.Recovered(), []string{"wan"}) {
		t.Fatal("completed recovery did not release the stopped delivery alarm", c.Status(), c.Recovered())
	}
	rows, err := journal.List(ctx, 0, 16)
	if err != nil || len(rows) != 2 || !reflect.DeepEqual(rows[0].Event, original[0]) || !reflect.DeepEqual(rows[1].Event, original[1]) {
		t.Fatal("stopped history changed, disappeared or duplicated", rows, err)
	}
	after, found, err := journal.LookupProducer(ctx, retirementProducer)
	if err != nil || !found || !reflect.DeepEqual(registered, after) || c.Status().Registered != 1 || c.Status().Retiring != 0 {
		t.Fatal("drainage discarded a declared worker's durable ownership", after, c.Status(), err)
	}
	// A later worker can own the same queue without a new registration. Its
	// accepted event must remain discoverable if it subsequently stops again.
	worker, err = events.OpenExisting(events.Options{Directory: serviceconfig.HealthEventDirectory(root, "wan")})
	if err != nil {
		t.Fatal(err)
	}
	if err = NewOutbox(worker, "wan").Enqueue(ctx, probeEvent("wan")); err != nil {
		t.Fatal(err)
	}
	if err = c.Recover(ctx, []string{"wan"}, stoppedRelays); err != nil {
		t.Fatal("exclusive worker ownership must defer collection", err)
	}
	worker.Close()
	if err = c.Recover(ctx, []string{"wan"}, stoppedRelays); err != nil {
		t.Fatal(err)
	}
	rows, err = journal.List(ctx, 0, 16)
	if err != nil || len(rows) != 3 {
		t.Fatal(rows, err)
	}
}

func TestStoppedDeclaredRecoveryRemainsPendingBetweenBoundedPages(t *testing.T) {
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
	for range PageSize + 1 {
		if err = NewOutbox(worker, "wan").Enqueue(ctx, probeEvent("wan")); err != nil {
			t.Fatal(err)
		}
	}
	worker.Close()
	if err = c.Recover(ctx, []string{"wan"}, stoppedRelays); err != nil || c.Status().StoppedPending != 1 || len(c.Recovered()) != 0 {
		t.Fatal("one page hid pending history", c.Status(), c.Recovered(), err)
	}
	rows, err := journal.List(ctx, 0, 64)
	if err != nil || len(rows) != PageSize {
		t.Fatal("recovery ignored its per-queue page bound", len(rows), err)
	}
	if err = c.Recover(ctx, []string{"wan"}, stoppedRelays); err != nil || c.Status().StoppedPending != 0 || len(c.Recovered()) != 1 {
		t.Fatal(c.Status(), c.Recovered(), err)
	}
}
