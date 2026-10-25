package vrrp

import (
	"context"
	"net/netip"
	"testing"
	"time"

	"github.com/imksoo/routerd2/internal/boottime"
)

func trackedConfig() Config {
	c := config4()
	c.AdvertIntervalCentiseconds = 20
	c.Track = []Track{{Service: "probe", Penalty: 100, MaxAgeMillis: 1000}}
	return Normalize(c)
}

func TestTrackPriorityRecoveryPreemptsLowerPeerWithoutRestartingElection(t *testing.T) {
	c := trackedConfig()
	m := newMachine(c, netip.MustParseAddr(c.SourceAddress), "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	now := time.Now()
	m.start(now)
	p := advertisement{source: netip.MustParseAddr("192.0.2.3"), priority: 120, interval: c.interval()}
	m.receive(now.Add(time.Millisecond), p)
	oldDeadline := m.deadline
	m.track[0].Penalized = false
	m.retimePriority(now.Add(2*time.Millisecond), 50)
	if !m.deadline.Before(oldDeadline) || m.priority() != 150 {
		t.Fatal("recovery retained the lower priority's down timer")
	}
	deadline := m.deadline
	m.receive(now.Add(3*time.Millisecond), p)
	m.tick(deadline)
	if m.state != StateActive {
		t.Fatal("inferior advertisements prevented preemption")
	}
	m.track[0].Penalized = true
	a := m.receive(deadline.Add(time.Millisecond), p)
	if m.priority() != 50 || m.state != StateBackup || !a.update {
		t.Fatal("penalized owner did not withdraw for the higher-priority peer")
	}
}

func TestExpiredTrackDecisionCannotRestoreHigherPriority(t *testing.T) {
	c := trackedConfig()
	m := newMachine(c, netip.MustParseAddr(c.SourceAddress), "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	m.start(time.Now())
	err := publishOwnership(context.Background(), m, EffectsFunc(func(context.Context, Snapshot) (Decision, error) {
		return Decision{Track: []TrackStatus{{Service: "probe", Known: true, ExpiresNanos: 1}}}, nil
	}))
	if err != nil || m.priority() != 50 || m.track[0].Known {
		t.Fatal("expired response raised native priority", m.track, err)
	}
}

func TestAdvertisementExpiresTrackingAfterOwnershipVerification(t *testing.T) {
	c := trackedConfig()
	m := newMachine(c, netip.MustParseAddr(c.SourceAddress), "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	m.enter(StateActive, time.Now())
	clock, err := boottime.Read()
	if err != nil {
		t.Fatal(err)
	}
	if err := m.stampObservation(clock); err != nil {
		t.Fatal(err)
	}
	m.track[0] = TrackStatus{Service: "probe", Known: true, ExpiresNanos: 1}
	device := newFakeTransport()
	if err := sendAdvertisement(context.Background(), m, device, false); err != nil {
		t.Fatal(err)
	}
	frame := <-device.sent
	if frame[14+20+2] != 50 || m.track[0].Known {
		t.Fatal("advertisement used an expired Health decision", frame, m.track)
	}
}

func TestTrackConfigurationAndPriorityFloor(t *testing.T) {
	c := trackedConfig()
	c.Track = append(c.Track, Track{Service: "second", Penalty: 100, MaxAgeMillis: 1000})
	if err := Validate(c); err != nil {
		t.Fatal(err)
	}
	if priority := effectivePriority(c, initialTracks(c)); priority != 1 {
		t.Fatal("tracking consumed the priority-zero withdrawal value", priority)
	}
	for _, edit := range []func(*Config){
		func(c *Config) { c.AddressOwner, c.Priority = true, 255 },
		func(c *Config) { c.Track[0].MaxAgeMillis = 0 },
		func(c *Config) { c.Track[1].Service = c.Track[0].Service },
		func(c *Config) { c.Track[0].Penalty = -1 },
	} {
		bad := Normalize(c)
		edit(&bad)
		if err := Validate(bad); err == nil {
			t.Fatal("invalid track policy accepted", bad.Track)
		}
	}
}
