package protocolevents

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
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

func TestTargetSavedProbeSurvivesActualLocalACKTransactionFailure(t *testing.T) {
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "relay")
	store, err := events.Open(events.Options{Directory: dir})
	if err != nil {
		t.Fatal(err)
	}
	target, err := events.Open(events.Options{Directory: filepath.Join(t.TempDir(), "target")})
	if err != nil {
		t.Fatal(err)
	}
	defer target.Close()
	outbox := NewOutbox(store, "wan")
	if err = outbox.Enqueue(ctx, probeEvent("wan")); err != nil {
		t.Fatal(err)
	}
	injector, err := sql.Open("sqlite", filepath.Join(dir, "journal.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer injector.Close()
	if _, err = injector.Exec(`CREATE TRIGGER fail_relay_ack BEFORE UPDATE ON producers WHEN json_array_length(CAST(NEW.body AS TEXT),'$.pending')<json_array_length(CAST(OLD.body AS TEXT),'$.pending') BEGIN SELECT RAISE(ABORT,'injected local relay ACK failure'); END`); err != nil {
		t.Fatal(err)
	}
	send := func(ctx context.Context, page []events.Event) error {
		for _, e := range page {
			if _, err := target.Append(ctx, e); err != nil {
				return err
			}
		}
		return nil
	}
	if err = outbox.Flush(ctx, send); err == nil {
		t.Fatal("local ACK failure was hidden")
	}
	first, err := target.List(ctx, 0, 16)
	if err != nil || len(first) != 1 {
		t.Fatal(first, err)
	}
	if err = store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = events.Open(events.Options{Directory: dir})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err = injector.Exec(`DROP TRIGGER fail_relay_ack`); err != nil {
		t.Fatal(err)
	}
	if err = NewOutbox(store, "wan").Flush(ctx, send); err != nil {
		t.Fatal(err)
	}
	second, err := target.List(ctx, 0, 16)
	if err != nil || len(second) != 1 || second[0].Event.ID != first[0].Event.ID {
		t.Fatal("local ACK retry changed target history", second, err)
	}
}

func TestRelayByteCapacityPreservesEveryAcceptedObservation(t *testing.T) {
	ctx := context.Background()
	store, err := events.Open(events.Options{Directory: filepath.Join(t.TempDir(), "relay")})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	outbox := NewOutbox(store, "wan")
	large := probeEvent("wan")
	large.Message = strings.Repeat("x", 4096)
	accepted := 0
	for {
		err = outbox.Enqueue(ctx, large)
		if errors.Is(err, events.ErrCapacity) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		accepted++
		if accepted >= MaxPending {
			t.Fatal("byte limit was not applied")
		}
	}
	if accepted == 0 {
		t.Fatal("fixture did not accept a bounded event")
	}
	delivered := 0
	for delivered < accepted {
		if err = outbox.Flush(ctx, func(_ context.Context, page []events.Event) error {
			delivered += len(page)
			for _, e := range page {
				if e.Detail.Message != large.Message {
					t.Fatal("capacity failure changed an accepted event")
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	if delivered != accepted {
		t.Fatal(accepted, delivered)
	}
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
