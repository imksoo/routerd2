//go:build linux

// SPDX-License-Identifier: BSD-3-Clause

package bfd

import (
	"testing"
	"time"
)

func TestReceiveClockUsesConservativeArrivalBound(t *testing.T) {
	empty := sampleClock(int64(time.Second))
	arrival := sampleClock(int64(time.Second + 4*time.Millisecond))
	now := sampleClock(int64(time.Second + 5*time.Millisecond))
	received, err := receiveClock(now, empty, arrival.WallTime)
	if err != nil || received.BoottimeNanoseconds != empty.BoottimeNanoseconds || !received.WallTime.Equal(arrival.WallTime) {
		t.Fatalf("arrival bound is not conservative: %+v %v", received, err)
	}
	// With the supported minimum local receive interval and DetectMult=1, the
	// 5ms polling allowance shortens this 50ms health grant, never enlarges it.
	s := testSession()
	p := peerPacket(wireInit, s.local)
	p.multiplier = 1
	if _, _, err := s.receive(p, s.config.Address, received, now.BoottimeNanoseconds); err != nil {
		t.Fatal(err)
	}
	want := int64(time.Second + 50*time.Millisecond)
	if s.deadline != want || s.deadline > arrival.BoottimeNanoseconds+int64(50*time.Millisecond) {
		t.Fatal("minimum-interval lease was renewed by receive processing")
	}
}

func TestQueuedReceiveCannotRenewAfterSuspendOrBackwardClockStep(t *testing.T) {
	empty := sampleClock(int64(time.Second))
	arrival := sampleClock(int64(time.Second + time.Millisecond))
	now := sampleClock(int64(3 * time.Second))
	// Kernel realtime jumped backward 1.9 seconds after this old packet arrived.
	// A naive current BOOTTIME minus realtime age maps it near 2.9 seconds.
	now.WallTime = now.WallTime.Add(-1900 * time.Millisecond)
	received, err := receiveClock(now, empty, arrival.WallTime)
	if err != nil {
		t.Fatal(err)
	}
	if received.BoottimeNanoseconds > arrival.BoottimeNanoseconds {
		t.Fatal("wall step moved old receive into the future")
	}
	s := testSession()
	if _, _, err := s.receive(peerPacket(wireInit, s.local), s.config.Address, received, now.BoottimeNanoseconds); err == nil || s.state != wireDown {
		t.Fatal("queued pre-suspension packet established fresh Up")
	}
	// A verified empty queue after the step starts a new safe receive bound.
	empty = now
	arrival = Clock{BootID: now.BootID, BoottimeNanoseconds: now.BoottimeNanoseconds + int64(2*time.Millisecond), WallTime: now.WallTime.Add(2 * time.Millisecond)}
	now.BoottimeNanoseconds += int64(3 * time.Millisecond)
	now.WallTime = now.WallTime.Add(3 * time.Millisecond)
	received, err = receiveClock(now, empty, arrival.WallTime)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.receive(peerPacket(wireInit, s.local), s.config.Address, received, now.BoottimeNanoseconds); err != nil || s.state != wireUp {
		t.Fatalf("fresh packet after draining queue failed to recover: %v", err)
	}
}

func TestContinuousNonemptyQueueCannotExtendHealth(t *testing.T) {
	empty := sampleClock(int64(time.Second))
	s := testSession()
	p := peerPacket(wireInit, s.local)
	receiveTest(t, s, p, empty.BoottimeNanoseconds)
	deadline := s.deadline
	p.state = wireUp
	for n := 1; n <= 20; n++ {
		now := sampleClock(int64(time.Second) + int64(n)*int64(10*time.Millisecond))
		received, err := receiveClock(now, empty, now.WallTime.Add(-time.Millisecond))
		if err != nil {
			t.Fatal(err)
		}
		s.expire(now.BoottimeNanoseconds)
		_, _, _ = s.receive(p, s.config.Address, received, now.BoottimeNanoseconds)
		if s.state == wireUp && s.deadline != deadline {
			t.Fatal("nonempty flood queue renewed lease")
		}
	}
	if s.state != wireDown || s.downTransitions != 1 {
		t.Fatal("nonempty flood queue prevented expiry")
	}
}

func TestInvalidReceiveClockFailsClosed(t *testing.T) {
	now, empty := sampleClock(int64(time.Second)), sampleClock(int64(900*time.Millisecond))
	for _, timestamp := range []time.Time{{}, now.WallTime.Add(time.Second), now.WallTime.Add(-time.Second)} {
		if _, err := receiveClock(now, empty, timestamp); err == nil {
			t.Fatal("invalid kernel timestamp accepted")
		}
	}
	empty.BootID = "ffffffff-ffff-ffff-ffff-ffffffffffff"
	if _, err := receiveClock(now, empty, now.WallTime); err == nil {
		t.Fatal("different boot empty-queue evidence accepted")
	}
}
