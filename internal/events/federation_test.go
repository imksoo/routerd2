package events

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"
)

func TestFederationRetentionEndsOnlyOldestDeliveryWhenAnotherConsumerNeedsHistory(t *testing.T) {
	ctx := context.Background()
	s, err := Open(Options{Directory: filepath.Join(t.TempDir(), "events")})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.Cursor(ctx, "independent-consumer"); err != nil {
		t.Fatal(err)
	}
	peer := "event-group:offline"
	cursor, err := s.Cursor(ctx, peer)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	for i, id := range []string{"newest", "oldest", "middle"} {
		age := []time.Duration{0, 2 * time.Hour, time.Hour}[i]
		record, err := s.Append(ctx, Event{ID: id, Topic: "fixture.observed", Source: "event_group", ObservedAt: now.Add(-age), Federation: &Federation{ID: id, Group: "group", SourceNode: "local"}})
		if err != nil {
			t.Fatal(err)
		}
		if err := s.Schedule(ctx, peer, cursor, record.Cursor, []Job{{Key: id, Payload: json.RawMessage(`{}`)}}); err != nil {
			t.Fatal(err)
		}
		cursor = record.Cursor
	}
	if n, err := s.PruneFederation(ctx, "group", 0, 2, now); err != nil || n != 0 {
		t.Fatal("removed history still needed by the independent consumer", n, err)
	}
	pending, err := s.Pending(ctx, peer, 8)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 2 || pending[0].Key != "newest" || pending[1].Key != "middle" {
		t.Fatal("retention canceled retained delivery work", pending)
	}
}
