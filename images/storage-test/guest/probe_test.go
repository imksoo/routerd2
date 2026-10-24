package main

import (
	"testing"
	"time"

	"github.com/imksoo/routerd2/internal/config"
	"github.com/imksoo/routerd2/internal/control"
	"github.com/imksoo/routerd2/internal/events"
)

func TestStorageWitnessWaitsForEventAndProducerAcknowledgement(t *testing.T) {
	s := control.State{Committed: config.Committed{Revision: 2, EventOutbox: []config.ChangeEvent{{ID: "pending"}}}, Healthy: true}
	p := control.ObservationPage{Records: []events.Record{{Cursor: 1, Event: events.Event{ID: "persisted", Topic: "configuration.committed", Source: "control", ObservedAt: time.Now().UTC(), Attributes: map[string]string{"revision": "2"}}}}}
	if _, ok := settledConfigEvent(s, p); ok {
		t.Fatal("raw config hash sampled before producer ACK removed pending metadata")
	}
	s.EventOutbox = nil
	if _, ok := settledConfigEvent(s, control.ObservationPage{}); ok {
		t.Fatal("empty producer outbox substituted for durable publication evidence")
	}
	if r, ok := settledConfigEvent(s, p); !ok || r.Event.ID != "persisted" {
		t.Fatal("settled publication not witnessed", r, ok)
	}
	p.Records[0].Event.Attributes["revision"] = "1"
	if _, ok := settledConfigEvent(s, p); ok {
		t.Fatal("an older event satisfied current configuration acceptance")
	}
}
