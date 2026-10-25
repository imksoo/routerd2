package bgpconfig

import (
	"strings"
	"testing"
	"time"

	"github.com/imksoo/routerd2/internal/protocol/bfd"
)

func statusFixture() (Config, Snapshot, bfd.Clock) {
	c := Config{Resource: "edge", Runtime: AppliedConfig{Peers: map[string]AppliedPeer{"192.0.2.2": {Address: "192.0.2.2", ASN: 65002}, "192.0.2.3": {Address: "192.0.2.3", ASN: 65003}}}}
	clock := bfd.Clock{BootID: "11111111-2222-4333-8444-555555555555", BoottimeNanoseconds: int64(10 * time.Second), WallTime: time.Date(2026, 10, 25, 0, 0, 0, 0, time.UTC)}
	s := Snapshot{Resource: c.Resource, Epoch: strings.Repeat("a", 32), Sequence: 1, Active: true, Peers: []Peer{{Address: "192.0.2.2", ASN: 65002, State: "SESSION_STATE_ESTABLISHED", AdminState: "ADMIN_STATE_UP"}, {Address: "192.0.2.3", ASN: 65003, State: "SESSION_STATE_IDLE", AdminState: "ADMIN_STATE_UP"}}}
	StampObservation(&s, clock)
	return c, s, clock
}

func TestCurrentBGPRouterScalarsUseCompletePeersAndFIBObservation(t *testing.T) {
	cfg, s, clock := statusFixture()
	for field, text := range map[string]string{"phase": "Degraded", "establishedPeers": "1", "backend": "gobgp", "observedAt": clock.WallTime.Format(time.RFC3339Nano)} {
		got, known := CurrentStatus(cfg, s, field, clock, 0)
		if !known || got.Text != text || got.ObservedNanos != uint64(clock.BoottimeNanoseconds) || got.ExpiresNanos != uint64(clock.BoottimeNanoseconds)+uint64(ObservationLifetime) {
			t.Fatal(field, got, known)
		}
	}
	s.Peers[1].State = "SESSION_STATE_ESTABLISHED"
	if got, known := CurrentStatus(cfg, s, "phase", clock, 0); !known || got.Text != "Established" {
		t.Fatal(got, known)
	}
	if got, known := CurrentStatus(cfg, s, "phase", clock, 1); !known || got.Text != "Degraded" {
		t.Fatal("unresolved FIB was hidden by session health", got, known)
	}
	if _, known := CurrentStatus(cfg, s, "phase", clock, -1); known {
		t.Fatal("unapplied projection supplied current phase")
	}
	for i := range s.Peers {
		s.Peers[i].State = "SESSION_STATE_IDLE"
	}
	if got, known := CurrentStatus(cfg, s, "phase", clock, 0); !known || got.Text != "Down" {
		t.Fatal(got, known)
	}
	cfg.Runtime.Peers, s.Peers = nil, nil
	if got, known := CurrentStatus(cfg, s, "phase", clock, 0); !known || got.Text != "Pending" {
		t.Fatal(got, known)
	}
	for _, field := range []string{"acceptedPrefixes", "fibRoutes", "prefixes", "peers.state", "unknown"} {
		if StatusFieldSupported(field) {
			t.Fatal("unobserved status contract was fabricated", field)
		}
	}
}

func TestBGPCurrentStatusCannotRenewAgeFromClockOrRedisplay(t *testing.T) {
	cfg, s, clock := statusFixture()
	want, known := CurrentStatus(cfg, s, "phase", clock, 0)
	if !known {
		t.Fatal("fixture unknown")
	}
	clock.BoottimeNanoseconds += int64(time.Second)
	clock.WallTime = clock.WallTime.Add(-time.Hour)
	got, known := CurrentStatus(cfg, s, "phase", clock, 0)
	if !known || got != want {
		t.Fatal("wall correction/redisplay changed original age", got, want, known)
	}
	clock.BoottimeNanoseconds = int64(s.ExpiresNanos)
	if _, known := CurrentStatus(cfg, s, "phase", clock, 0); known {
		t.Fatal("old UTC deadline revived expired local observation")
	}
	for _, change := range []func(*Snapshot){func(s *Snapshot) { s.BootID = "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee" }, func(s *Snapshot) { s.ObservedNanos++ }, func(s *Snapshot) { s.ExpiresNanos++ }, func(s *Snapshot) { s.Peers = s.Peers[:1] }, func(s *Snapshot) { s.Peers[1] = s.Peers[0] }, func(s *Snapshot) { s.Peers[0].State = "invented" }, func(s *Snapshot) { s.Peers[0].ASN++ }, func(s *Snapshot) { s.Active = false }} {
		cfg, bad, clock := statusFixture()
		change(&bad)
		if _, known := CurrentStatus(cfg, bad, "phase", clock, 0); known {
			t.Fatal("invalid/missing peer or time proof became known", bad)
		}
	}
}
