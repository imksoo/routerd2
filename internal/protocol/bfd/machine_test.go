// SPDX-License-Identifier: BSD-3-Clause

package bfd

import (
	"math"
	"testing"
	"time"
)

const testBootID = "01234567-89ab-cdef-0123-456789abcdef"

func testConfig() Config {
	return Normalize(Config{Resource: "bfd-uplink", Interface: "eth-test", SourceAddress: "192.0.2.1", Peers: []Peer{{Address: "192.0.2.2", MinTxMilliseconds: 50, MinRxMilliseconds: 50, DetectMultiplier: 3}}})
}

func sampleClock(ns int64) Clock {
	return Clock{BootID: testBootID, BoottimeNanoseconds: ns, WallTime: time.Date(2026, 10, 24, 0, 0, 0, 0, time.UTC).Add(time.Duration(ns))}
}

func testSession() *session {
	return newSession(testConfig().Peers[0], 17, 49160, int64(time.Second), func(uint32) uint32 { return 0 })
}

func peerPacket(state uint8, your uint32) control {
	return control{state: state, my: 29, your: your, multiplier: 3, minTx: 50000, minRx: 50000}
}

func receiveTest(t *testing.T, s *session, p control, at int64) (bool, bool) {
	t.Helper()
	changed, final, err := s.receive(p, s.config.Address, sampleClock(at), at)
	if err != nil {
		t.Fatal(err)
	}
	return changed, final
}

func TestSessionStateTransitions(t *testing.T) {
	want := [4][4]uint8{
		{wireAdminDown, wireAdminDown, wireAdminDown, wireAdminDown},
		{wireDown, wireInit, wireUp, wireDown},
		{wireDown, wireInit, wireUp, wireUp},
		{wireDown, wireDown, wireUp, wireUp},
	}
	for local := uint8(0); local < 4; local++ {
		for remote := uint8(0); remote < 4; remote++ {
			t.Run(stateName(local)+"/"+stateName(remote), func(t *testing.T) {
				s := testSession()
				s.state = local
				receiveTest(t, s, peerPacket(remote, s.local), int64(time.Second))
				if s.state != want[local][remote] {
					t.Fatalf("state %s; want %s", stateName(s.state), stateName(want[local][remote]))
				}
				if local == wireUp && s.state != wireUp && s.downTransitions != 1 {
					t.Fatal("Up loss was not retained in cumulative transition counter")
				}
			})
		}
	}
}

func TestHandshakePollFinalAndRemoteRestart(t *testing.T) {
	s := testSession()
	if p := s.packet(false); p.your != 0 || p.state != wireDown || p.minTx != 1000000 || p.flags != 0 {
		t.Fatalf("invalid active initial control: %+v", p)
	}
	now := int64(time.Second)
	receiveTest(t, s, peerPacket(wireDown, 0), now)
	if s.state != wireInit || s.polling {
		t.Fatal("Down/Down should enter Init at initial conservative interval")
	}
	now += int64(time.Millisecond)
	p := peerPacket(wireInit, s.local)
	p.flags = flagFinal // Unsolicited Final cannot complete a not-yet-sent Poll.
	receiveTest(t, s, p, now)
	if s.state != wireUp || !s.polling || s.pollSent || s.packet(false).minTx != 50000 {
		t.Fatal("Up must advertise active interval and start a fresh Poll")
	}
	receiveTest(t, s, p, now+1)
	if !s.polling {
		t.Fatal("unsent Poll was acknowledged")
	}
	s.sent(now+2, false, true)
	p.flags = flagPoll
	_, final := receiveTest(t, s, p, now+3)
	if !final || !s.polling || s.packet(true).flags != flagFinal {
		t.Fatal("simultaneous Poll must respond with Final only, keeping own Poll")
	}
	s.sent(now+3, true, true)
	p.flags = flagFinal
	changed, _ := receiveTest(t, s, p, now+4)
	if s.polling || s.pollSent || !changed {
		t.Fatal("transmitted Poll did not terminate on Final")
	}
	p = peerPacket(wireDown, 0)
	p.my = 99
	receiveTest(t, s, p, now+5)
	if s.state != wireDown || s.diagnostic != 3 || s.remote != 99 || s.downTransitions != 1 {
		t.Fatal("new remote discriminator/restart did not drive loss")
	}
	p.state, p.your = wireInit, s.local
	receiveTest(t, s, p, now+6)
	if s.state != wireUp || s.remote != 99 || s.upTransitions != 2 || s.downTransitions != 1 {
		t.Fatal("remote restart failed to recover while preserving transition history")
	}
}

func TestReceiveAnchoredExpiryAndDiscriminatorReset(t *testing.T) {
	s := testSession()
	now := int64(time.Second)
	receiveTest(t, s, peerPacket(wireInit, s.local), now)
	deadline := now + int64(150*time.Millisecond)
	if s.deadline != deadline || s.expire(deadline-1) {
		t.Fatal("incorrect detection deadline")
	}
	for i := 0; i < 3; i++ {
		if s.snapshot(true).DeadlineBootNanoseconds != deadline {
			t.Fatal("redisplaying snapshot refreshed health")
		}
	}
	if !s.expire(deadline) || s.state != wireDown || s.remote != 0 || s.deadline != 0 || s.diagnostic != 1 || s.downTransitions != 1 {
		t.Fatalf("expiration did not withdraw session identity/health: %+v", s)
	}
	p := peerPacket(wireUp, s.local)
	if _, _, err := s.receive(p, s.config.Address, sampleClock(now), deadline); err == nil || s.state != wireDown || s.received != 1 {
		t.Fatal("expired queued packet refreshed state")
	}
	// Even a Down/Up packet that does not establish the local session has a
	// remote discriminator lifetime. A later timeout must clear YourDiscr.
	receiveTest(t, s, p, deadline+1)
	if s.state != wireDown || s.remote == 0 {
		t.Fatal("Down must not shortcut directly to Up on a remote Up")
	}
	if !s.expire(deadline+1+int64(150*time.Millisecond)) || s.remote != 0 || s.packet(false).your != 0 {
		t.Fatal("Down state retained an expired discriminator")
	}
}

func TestNegotiationAsymmetryZeroRxAndIntervalExtremes(t *testing.T) {
	s := testSession()
	s.config.DetectMultiplier = 1
	p := peerPacket(wireInit, s.local)
	p.multiplier, p.minTx, p.minRx, p.minEchoRx = 7, 80000, 120000, 100000
	now := int64(time.Second)
	receiveTest(t, s, p, now)
	if s.detectionTime() != 560000 || s.txInterval() != 120000 || s.snapshot(true).RemoteMinEchoRxMicroseconds != 100000 || s.packet(false).minEchoRx != 0 {
		t.Fatal("receive multiplier, independent directional interval or echo capability is wrong")
	}
	s.sent(now, false, true)
	if s.nextTx != now+int64(90*time.Millisecond) {
		t.Fatalf("75%% jitter of negotiated 120ms not honored: %d", s.nextTx-now)
	}
	p.minRx, p.flags = 0, flagPoll
	_, final := receiveTest(t, s, p, now+1)
	if !final || s.nextTx != 0 || s.packet(true).flags != flagFinal {
		t.Fatal("zero required RX must suppress periodic TX but still respond Final")
	}
	s.sent(now+1, true, true)
	if s.nextTx != 0 {
		t.Fatal("Final restarted suppressed periodic transmission")
	}
	p.minRx, p.minTx, p.multiplier, p.flags = math.MaxUint32, math.MaxUint32, math.MaxUint8, 0
	receiveTest(t, s, p, now+2)
	want := uint64(math.MaxUint32) * 255
	if s.detectionTime() != want || s.deadline != now+2+int64(want*1000) || s.txInterval() != math.MaxUint32 {
		t.Fatal("remote wire interval overflowed or incorrectly used local multiplier")
	}
	p.minRx = 50000
	receiveTest(t, s, p, now+int64(200*time.Millisecond))
	if s.nextTx != now+int64(200*time.Millisecond) {
		t.Fatal("remote faster receive interval did not cause an overdue immediate transmit")
	}
}

func TestJitterBoundariesAndDeadlineOverflow(t *testing.T) {
	for _, mult := range []int{1, 3, 255} {
		for _, high := range []bool{false, true} {
			s := testSession()
			s.state, s.config.DetectMultiplier = wireUp, mult
			s.jitter = func(limit uint32) uint32 {
				if high {
					return limit - 1
				}
				return 0
			}
			want := int64(37500 * time.Microsecond)
			if high {
				want = int64(50 * time.Millisecond)
				if mult == 1 {
					want = int64(45 * time.Millisecond)
				}
			}
			if got := s.jitteredInterval(); got != want {
				t.Fatalf("mult=%d high=%t jitter=%d want=%d", mult, high, got, want)
			}
		}
	}
	for _, input := range []struct {
		at       int64
		interval uint64
	}{{0, 1}, {-1, 1}, {1, 0}, {math.MaxInt64 - 999, 1}, {1, math.MaxUint64}} {
		if _, err := addInterval(input.at, input.interval); err == nil {
			t.Fatalf("overflow/invalid interval accepted: %+v", input)
		}
	}
	if deadline, err := addInterval(math.MaxInt64-1000, 1); err != nil || deadline != math.MaxInt64 {
		t.Fatal("exact final safe deadline rejected")
	}
	s := testSession()
	s.lastTx = math.MaxInt64 - 1
	s.schedule(math.MaxInt64 - 1)
	if s.nextTx != math.MaxInt64 {
		t.Fatal("transmit deadline wrapped")
	}
}

func FuzzSessionSnapshot(f *testing.F) {
	for _, state := range []uint8{wireDown, wireInit, wireUp, wireAdminDown} {
		p := peerPacket(state, 17)
		f.Add(p.bytes())
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > 4096 {
			return
		}
		cfg, s, now := testConfig(), testSession(), int64(time.Second)
		check := func() {
			snapshot := Snapshot{Resource: cfg.Resource, Interface: cfg.Interface, SourceAddress: cfg.SourceAddress, BootID: testBootID, Active: true, ObservedNanos: uint64(now), ExpiresNanos: uint64(now) + uint64(StatusLifetime), UpdatedAt: sampleClock(now).WallTime, Peers: []PeerState{s.snapshot(true)}}
			if err := ValidateSnapshot(cfg, snapshot); err != nil {
				t.Fatalf("machine emitted invalid boundary: %v; %+v", err, snapshot)
			}
		}
		check()
		for at := 0; at+24 <= len(data); at += 24 {
			p, err := parseControl(data[at : at+24])
			if err != nil {
				continue
			}
			if p.your != 0 {
				p.your = s.local
			}
			now += int64(data[at+5]) * int64(time.Millisecond)
			s.expire(now)
			received := sampleClock(now - int64(data[at+4])*int64(time.Millisecond))
			_, final, _ := s.receive(p, cfg.Peers[0].Address, received, now)
			if final {
				s.sent(now, true, true)
			}
			if s.nextTx != 0 && now >= s.nextTx {
				s.sent(now, false, true)
			}
			check()
		}
	})
}
