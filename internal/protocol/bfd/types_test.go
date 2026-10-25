// SPDX-License-Identifier: BSD-3-Clause

package bfd

import (
	"encoding/json"
	"math"
	"testing"
	"time"
)

func TestConfigDefaultsAndExplicitUnsupportedModes(t *testing.T) {
	original := Config{Resource: "test", Interface: "eth0", SourceAddress: "192.0.2.1", Peers: []Peer{{Address: "192.0.2.2"}}}
	c := Normalize(original)
	if original.Peers[0].MinTxMilliseconds != 0 || c.Peers[0].MinTxMilliseconds != 300 || c.Peers[0].MinRxMilliseconds != 300 || c.Peers[0].DetectMultiplier != 3 || c.EffectTimeoutMilliseconds != 1000 || Validate(c) != nil {
		t.Fatal("normalization did not preserve caller or canonical defaults")
	}
	tests := map[string]func(*Config){
		"empty interface":          func(c *Config) { c.Interface = "" },
		"invalid interface":        func(c *Config) { c.Interface = "eth0:1" },
		"empty source":             func(c *Config) { c.SourceAddress = "" },
		"mapped source":            func(c *Config) { c.SourceAddress = "::ffff:192.0.2.1" },
		"multicast source":         func(c *Config) { c.SourceAddress = "224.0.0.1" },
		"loopback source":          func(c *Config) { c.SourceAddress = "127.0.0.1" },
		"noncanonical peer":        func(c *Config) { c.Peers[0].Address = "2001:0DB8::1" },
		"family mismatch":          func(c *Config) { c.Peers[0].Address = "2001:db8::2" },
		"broadcast peer":           func(c *Config) { c.Peers[0].Address = "255.255.255.255" },
		"peer is local":            func(c *Config) { c.Peers[0].Address = c.SourceAddress },
		"duplicate peer":           func(c *Config) { c.Peers = append(c.Peers, c.Peers[0]) },
		"empty peers":              func(c *Config) { c.Peers = nil },
		"too many peers":           func(c *Config) { c.Peers = make([]Peer, MaxPeers+1) },
		"too fast tx":              func(c *Config) { c.Peers[0].MinTxMilliseconds = 49 },
		"too slow rx":              func(c *Config) { c.Peers[0].MinRxMilliseconds = 60001 },
		"negative multiplier":      func(c *Config) { c.Peers[0].DetectMultiplier = -1 },
		"overflow multiplier":      func(c *Config) { c.Peers[0].DetectMultiplier = 256 },
		"authentication":           func(c *Config) { c.Authentication = "sha1" },
		"demand":                   func(c *Config) { c.DemandMode = true },
		"echo":                     func(c *Config) { c.Echo = true },
		"multihop":                 func(c *Config) { c.Multihop = true },
		"passive":                  func(c *Config) { c.Passive = true },
		"unbounded effect timeout": func(c *Config) { c.EffectTimeoutMilliseconds = 5001 },
		"ambiguous point to point": func(c *Config) { c.PointToPoint = true; c.Peers = append(c.Peers, Peer{Address: "192.0.2.3"}) },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			copy := Normalize(original)
			mutate(&copy)
			if err := Validate(copy); err == nil {
				t.Fatal("invalid/unsupported configuration accepted")
			}
		})
	}
	c.SourceAddress, c.Peers[0].Address = "fe80::1", "fe80::2"
	if err := Validate(c); err != nil {
		t.Fatalf("interface-scoped IPv6 link local rejected: %v", err)
	}
}

func validSnapshot(t *testing.T) (Config, Snapshot) {
	t.Helper()
	c := testConfig()
	s := testSession()
	receiveTest(t, s, peerPacket(wireInit, s.local), int64(time.Second))
	snapshot := Snapshot{Resource: c.Resource, Interface: c.Interface, SourceAddress: c.SourceAddress, BootID: testBootID, Active: true, TimestampChanges: 1, ObservedNanos: uint64(time.Second), ExpiresNanos: uint64(time.Second) + uint64(StatusLifetime), UpdatedAt: sampleClock(int64(time.Second)).WallTime, Peers: []PeerState{s.snapshot(true)}}
	if err := ValidateSnapshot(c, snapshot); err != nil {
		t.Fatal(err)
	}
	return c, snapshot
}

func TestFiniteHealthUsesBootIdentityNotRedisplayOrWallTime(t *testing.T) {
	c, s := validSnapshot(t)
	now := sampleClock(int64(time.Second + 25*time.Millisecond))
	if !Healthy(c, s, c.Peers[0].Address, now) {
		t.Fatal("fresh Up health rejected")
	}
	for _, jump := range []time.Duration{-365 * 24 * time.Hour, 365 * 24 * time.Hour} {
		current := now
		current.WallTime = current.WallTime.Add(jump)
		if !Healthy(c, s, c.Peers[0].Address, current) {
			t.Fatal("wall-clock step changed monotonic health")
		}
	}
	current := now
	current.BootID = "ffffffff-ffff-ffff-ffff-ffffffffffff"
	if Healthy(c, s, c.Peers[0].Address, current) {
		t.Fatal("previous kernel boot granted health")
	}
	current = now
	current.BoottimeNanoseconds = s.Peers[0].LastReceivedBootNanoseconds - 1
	if Healthy(c, s, c.Peers[0].Address, current) {
		t.Fatal("future receive event granted health")
	}
	current.BoottimeNanoseconds = s.Peers[0].DeadlineBootNanoseconds
	s.UpdatedAt = now.WallTime.Add(24 * time.Hour)
	if Healthy(c, s, c.Peers[0].Address, current) {
		t.Fatal("redisplay refreshed expired lease")
	}
	if Healthy(c, s, "192.0.2.99", now) {
		t.Fatal("unconfigured peer granted health")
	}
	s.Active = false
	if Healthy(c, s, c.Peers[0].Address, now) {
		t.Fatal("withdrawn snapshot granted health")
	}
}

func TestSnapshotBoundaryRejectsFabricatedLeaseAndCrossScope(t *testing.T) {
	c, valid := validSnapshot(t)
	tests := map[string]func(*Snapshot){
		"resource":                func(s *Snapshot) { s.Resource += "-other" },
		"interface":               func(s *Snapshot) { s.Interface = "eth-other" },
		"source":                  func(s *Snapshot) { s.SourceAddress = "192.0.2.3" },
		"boot identity":           func(s *Snapshot) { s.BootID = "boot" },
		"missing peer":            func(s *Snapshot) { s.Peers = nil },
		"peer identity":           func(s *Snapshot) { s.Peers[0].Address = "192.0.2.99" },
		"observed source":         func(s *Snapshot) { s.Peers[0].ObservedSource = "192.0.2.3" },
		"missing discriminator":   func(s *Snapshot) { s.Peers[0].RemoteDiscriminator = 0 },
		"missing receive":         func(s *Snapshot) { s.Peers[0].Received = 0 },
		"fake state":              func(s *Snapshot) { s.Peers[0].State = "READY" },
		"remote admin down":       func(s *Snapshot) { s.Peers[0].RemoteState = StateAdminDown },
		"impossible counters":     func(s *Snapshot) { s.Peers[0].DownTransitions = 2 },
		"wrong multiplier":        func(s *Snapshot) { s.Peers[0].RemoteDetectMultiplier++ },
		"wrong negotiated tx":     func(s *Snapshot) { s.Peers[0].TransmitIntervalMicroseconds++ },
		"unanchored lease":        func(s *Snapshot) { s.Peers[0].DeadlineBootNanoseconds++ },
		"wall-only renewed lease": func(s *Snapshot) { s.Peers[0].ValidUntil = s.Peers[0].ValidUntil.Add(time.Second) },
		"withdrawn up":            func(s *Snapshot) { s.Active = false },
		"unbounded error":         func(s *Snapshot) { s.LastError = string(make([]byte, 257)) },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			copy := valid
			copy.Peers = append([]PeerState(nil), valid.Peers...)
			mutate(&copy)
			if err := ValidateSnapshot(c, copy); err == nil {
				t.Fatal("fabricated snapshot accepted")
			}
		})
	}
	data, err := json.Marshal(valid)
	if err != nil {
		t.Fatal(err)
	}
	var roundTrip Snapshot
	if err := json.Unmarshal(data, &roundTrip); err != nil || ValidateSnapshot(c, roundTrip) != nil {
		t.Fatalf("IPC JSON round trip changed validation: %v", err)
	}
}

func TestExpiredRemoteFactsAndInactiveSnapshotAreValid(t *testing.T) {
	c := testConfig()
	s := testSession()
	receiveTest(t, s, peerPacket(wireInit, s.local), int64(time.Second))
	s.expire(int64(2 * time.Second))
	snapshot := Snapshot{Resource: c.Resource, Interface: c.Interface, SourceAddress: c.SourceAddress, BootID: testBootID, Active: true, TimestampChanges: 1, ObservedNanos: uint64(2 * time.Second), ExpiresNanos: uint64(2*time.Second) + uint64(StatusLifetime), UpdatedAt: sampleClock(int64(2 * time.Second)).WallTime, Peers: []PeerState{s.snapshot(true)}}
	if err := ValidateSnapshot(c, snapshot); err != nil {
		t.Fatalf("expired discriminator should retain remote historical facts: %v", err)
	}
	s.enter(wireAdminDown, 7, int64(2*time.Second))
	snapshot.Active, snapshot.Peers[0] = false, s.snapshot(false)
	if err := ValidateSnapshot(c, snapshot); err != nil {
		t.Fatalf("final AdminDown fence rejected: %v", err)
	}
}

func TestSnapshotUpDownCounterBalanceAndSaturation(t *testing.T) {
	cfg, valid := validSnapshot(t)
	for _, counters := range []struct {
		up, down uint64
		accept   bool
	}{{1, 0, true}, {1, 1, false}, {2, 0, false}, {2, 1, true}, {math.MaxUint64, math.MaxUint64 - 1, true}, {math.MaxUint64, math.MaxUint64, true}, {math.MaxUint64 - 1, math.MaxUint64 - 1, false}} {
		s := valid
		s.Peers = append([]PeerState(nil), valid.Peers...)
		s.Peers[0].Transitions, s.Peers[0].UpTransitions, s.Peers[0].DownTransitions = math.MaxUint64, counters.up, counters.down
		if err := ValidateSnapshot(cfg, s); (err == nil) != counters.accept {
			t.Fatalf("Up counters %+v: %v", counters, err)
		}
	}
	s := testSession()
	receiveTest(t, s, peerPacket(wireInit, s.local), int64(time.Second))
	s.expire(int64(2 * time.Second))
	for _, counters := range []struct {
		up, down uint64
		accept   bool
	}{{1, 1, true}, {1, 0, false}, {2, 1, false}, {math.MaxUint64, math.MaxUint64, true}, {math.MaxUint64, math.MaxUint64 - 1, false}} {
		p := s.snapshot(true)
		p.Transitions, p.UpTransitions, p.DownTransitions = math.MaxUint64, counters.up, counters.down
		snapshot := valid
		snapshot.Peers = []PeerState{p}
		if err := ValidateSnapshot(cfg, snapshot); (err == nil) != counters.accept {
			t.Fatalf("Down counters %+v: %v", counters, err)
		}
	}
}
