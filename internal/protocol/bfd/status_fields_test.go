package bfd

import (
	"encoding/json"
	"fmt"
	"math"
	"reflect"
	"testing"
	"time"
)

func TestCurrentStatusFieldsPreserveLegacyShapesAndOriginalReceiveProof(t *testing.T) {
	cfg := Normalize(Config{Resource: "edge", Interface: "eth0", SourceAddress: "192.0.2.1", Peers: []Peer{{Address: "192.0.2.2"}}})
	at := sampleClock(int64(10 * time.Second))
	session := newSession(cfg.Peers[0], 1, 49152, at.BoottimeNanoseconds, func(uint32) uint32 { return 0 })
	if _, _, err := session.receive(control{state: wireDown, multiplier: 3, my: 2, minTx: 300000, minRx: 300000}, "192.0.2.2", at, at.BoottimeNanoseconds); err != nil {
		t.Fatal(err)
	}
	at = sampleClock(int64(10100 * time.Millisecond))
	if _, _, err := session.receive(control{state: wireInit, multiplier: 3, my: 2, your: 1, minTx: 300000, minRx: 300000}, "192.0.2.2", at, at.BoottimeNanoseconds); err != nil {
		t.Fatal(err)
	}
	sample := Snapshot{Resource: cfg.Resource, Interface: cfg.Interface, SourceAddress: cfg.SourceAddress, BootID: testBootID, Active: true, UpdatedAt: at.WallTime, ObservedNanos: uint64(at.BoottimeNanoseconds), ExpiresNanos: uint64(at.BoottimeNanoseconds) + uint64(StatusLifetime), Peers: []PeerState{session.snapshot(true)}}
	if err := ValidateSnapshot(cfg, sample); err != nil {
		t.Fatal(err)
	}
	peer := map[string]any{"address": "192.0.2.2", "localAddress": "192.0.2.1", "state": "Up", "interface": "eth0", "minRx": 300, "minTx": 300, "detectMultiplier": 3}
	peerText, _ := json.Marshal([]string{fmt.Sprint(peer)})
	want := map[string]string{"phase": "Up", "peerStates": "map[192.0.2.2:Up]", "peers": string(peerText), "observedAt": sample.UpdatedAt.UTC().Format(time.RFC3339Nano)}
	var proof StatusValue
	for field, text := range want {
		got, known := CurrentStatus(cfg, sample, field, at)
		if !known || got.Text != text || got.ObservedNanos != uint64(at.BoottimeNanoseconds) || got.ExpiresNanos != uint64(sample.Peers[0].DeadlineBootNanoseconds) || got.Continuity == "" {
			t.Fatal(field, got, known, text)
		}
		if field == "phase" {
			proof = got
		}
	}
	// Local periodic publication is not a new receive witness for an old Up.
	later := at
	later.BoottimeNanoseconds += int64(100 * time.Millisecond)
	later.WallTime = later.WallTime.Add(time.Hour)
	sample.ObservedNanos = uint64(later.BoottimeNanoseconds)
	sample.ExpiresNanos = sample.ObservedNanos + uint64(StatusLifetime)
	sample.UpdatedAt = later.WallTime
	refreshed, known := CurrentStatus(cfg, sample, "phase", later)
	if !known || !reflect.DeepEqual(refreshed, proof) {
		t.Fatal("status refresh renewed packet proof or hold identity", refreshed, proof, known)
	}
	later.BoottimeNanoseconds = sample.Peers[0].DeadlineBootNanoseconds
	if got, known := CurrentStatus(cfg, sample, "phase", later); known {
		t.Fatal("expired Up became current phase", got)
	}
	for _, field := range []string{"unknown", "peerStates.192.0.2.2", "phase.extra"} {
		if StatusFieldSupported(field) {
			t.Fatal("unsupported path accepted", field)
		}
	}
	sample.Peers[0].Transitions = math.MaxUint64
	if _, known := CurrentStatus(cfg, sample, "phase", Clock{BootID: at.BootID, BoottimeNanoseconds: int64(sample.ObservedNanos), WallTime: at.WallTime}); known {
		t.Fatal("saturated continuity counter certified a hold")
	}
}

func TestCurrentDownStatusNeedsFiniteProducerObservation(t *testing.T) {
	cfg := Normalize(Config{Resource: "edge", Interface: "eth0", SourceAddress: "192.0.2.1", Peers: []Peer{{Address: "192.0.2.2"}}})
	at := sampleClock(int64(10 * time.Second))
	session := newSession(cfg.Peers[0], 1, 49152, at.BoottimeNanoseconds, func(uint32) uint32 { return 0 })
	sample := Snapshot{Resource: cfg.Resource, Interface: cfg.Interface, SourceAddress: cfg.SourceAddress, BootID: testBootID, Active: true, UpdatedAt: at.WallTime, ObservedNanos: uint64(at.BoottimeNanoseconds), ExpiresNanos: uint64(at.BoottimeNanoseconds) + uint64(StatusLifetime), Peers: []PeerState{session.snapshot(true)}}
	got, known := CurrentStatus(cfg, sample, "phase", at)
	if !known || got.Text != "Down" || got.ObservedNanos != sample.ObservedNanos || got.ExpiresNanos != sample.ExpiresNanos {
		t.Fatal(got, known)
	}
	for _, mutate := range []func(*Snapshot){func(s *Snapshot) { s.ExpiresNanos = 0 }, func(s *Snapshot) { s.ObservedNanos++ }, func(s *Snapshot) { s.BootID = "other" }, func(s *Snapshot) { s.Active = false }} {
		bad := sample
		mutate(&bad)
		if _, known := CurrentStatus(cfg, bad, "phase", at); known {
			t.Fatal("invalid observation became current")
		}
	}
	at.BoottimeNanoseconds = int64(sample.ExpiresNanos)
	if _, known := CurrentStatus(cfg, sample, "phase", at); known {
		t.Fatal("old Down was renewed by a read")
	}
}
