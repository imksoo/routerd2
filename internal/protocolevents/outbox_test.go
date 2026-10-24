package protocolevents

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/imksoo/routerd/pkg/api"
	"github.com/imksoo/routerd/pkg/daemonapi"
	old "github.com/imksoo/routerd/pkg/healthcheck"
	"github.com/imksoo/routerd2/internal/events"
)

func probeEvent(name string) daemonapi.DaemonEvent {
	return daemonapi.DaemonEvent{Time: time.Date(2026, 10, 24, 0, 0, 0, 0, time.UTC), Type: "routerd.healthcheck." + name + ".passed", Daemon: daemonapi.DaemonRef{Name: old.DaemonKind + "-" + name, Kind: old.DaemonKind, Instance: name}, Resource: &daemonapi.ResourceRef{APIVersion: api.NetAPIVersion, Kind: "HealthCheck", Name: name}, Severity: daemonapi.SeverityInfo, Reason: "HealthCheckProbe", Attributes: map[string]string{"result": old.ResultPassed}}
}

func TestPendingProbeSurvivesRestartFailedACKAndConcurrentEnqueue(t *testing.T) {
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "relay")
	store, err := events.Open(events.Options{Directory: dir})
	if err != nil {
		t.Fatal(err)
	}
	outbox := NewOutbox(store, "wan")
	if err = outbox.Enqueue(ctx, probeEvent("wan")); err != nil {
		t.Fatal(err)
	}
	var firstID string
	if err = outbox.Flush(ctx, func(_ context.Context, page []events.Event) error {
		if len(page) != 1 {
			t.Fatal(page)
		}
		firstID = page[0].ID
		return errors.New("target saved event but ACK was lost")
	}); err == nil {
		t.Fatal("failed ACK discarded work")
	}
	if err = store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = events.Open(events.Options{Directory: dir})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	outbox = NewOutbox(store, "wan")
	if err = outbox.Flush(ctx, func(_ context.Context, page []events.Event) error {
		if len(page) != 1 || page[0].ID != firstID {
			t.Fatal("restart changed retry identity", page)
		}
		return outbox.Enqueue(ctx, probeEvent("wan")) // arrives while the ACK is in flight
	}); err != nil {
		t.Fatal(err)
	}
	if err = outbox.Flush(ctx, func(_ context.Context, page []events.Event) error {
		if len(page) != 1 || page[0].ID == firstID {
			t.Fatal("ACK removed the unacknowledged tail", page)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err = outbox.Flush(ctx, func(context.Context, []events.Event) error { t.Fatal("completed work replayed"); return nil }); err != nil {
		t.Fatal(err)
	}
}

func TestRelayBoundsOnlyPendingWorkAndRejectsOtherResources(t *testing.T) {
	ctx := context.Background()
	store, err := events.Open(events.Options{Directory: filepath.Join(t.TempDir(), "relay"), MaxJobs: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	outbox := NewOutbox(store, "wan")
	if err = outbox.Enqueue(ctx, probeEvent("foreign")); err == nil {
		t.Fatal("foreign resource accepted")
	}
	// ACKed relay entries do not accumulate producer jobs/tombstones. Target
	// journal identities retain durable deduplication separately.
	for i := 0; i < 80; i++ {
		if err = outbox.Enqueue(ctx, probeEvent("wan")); err != nil {
			t.Fatal(i, err)
		}
		if err = outbox.Flush(ctx, func(context.Context, []events.Event) error { return nil }); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < MaxPending; i++ {
		if err = outbox.Enqueue(ctx, probeEvent("wan")); err != nil {
			t.Fatal(i, err)
		}
	}
	if err = outbox.Enqueue(ctx, probeEvent("wan")); !errors.Is(err, events.ErrCapacity) {
		t.Fatal("unbounded relay", err)
	}
	for i := 0; i < MaxPending/PageSize; i++ {
		if err = outbox.Flush(ctx, func(_ context.Context, page []events.Event) error {
			if len(page) != PageSize {
				t.Fatal("unbounded publication page", len(page))
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err = outbox.Enqueue(ctx, probeEvent("wan")); err != nil {
		t.Fatal("ACK did not release capacity", err)
	}
}
