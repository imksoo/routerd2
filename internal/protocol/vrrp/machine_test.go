package vrrp

import (
	"net/netip"
	"testing"
	"time"
)

func TestPrioritySkewElectionAndMissingAdvertisements(t *testing.T) {
	start := time.Unix(1000, 0)
	highConfig, lowConfig := config4(), config4()
	lowConfig.Priority = 100
	high := newMachine(highConfig, netip.MustParseAddr("192.0.2.2"), "high")
	low := newMachine(lowConfig, netip.MustParseAddr("192.0.2.3"), "low")
	high.start(start)
	low.start(start)
	if high.skew() != 414062500*time.Nanosecond || low.skew() != 609375000*time.Nanosecond || !high.deadline.Before(low.deadline) {
		t.Fatal("priority skew or master-down interval is incorrect")
	}
	election := high.deadline
	if a := high.tick(election.Add(-time.Nanosecond)); a.advertise || a.update {
		t.Fatal("backup acquired ownership before the deadline")
	}
	if a := high.tick(election); !a.update || !a.advertise || !a.notify || high.state != StateActive {
		t.Fatal("highest-priority expired backup did not activate")
	}
	low.receive(election, advertisement{source: high.source, priority: 150, interval: time.Second})
	deadline := low.deadline
	if a := low.tick(deadline.Add(-time.Nanosecond)); a.update || low.state != StateBackup {
		t.Fatal("backup took over before the active peer timed out")
	}
	if a := low.tick(deadline); !a.advertise || low.state != StateActive {
		t.Fatal("backup failed to take over after advertisements stopped")
	}
}

func TestPreemptionNonpreemptionAndPriorityZero(t *testing.T) {
	start := time.Unix(2000, 0)
	for _, preempt := range []bool{false, true} {
		c := config4()
		c.Preempt = &preempt
		m := newMachine(c, netip.MustParseAddr(c.SourceAddress), "instance")
		m.start(start)
		original := m.deadline
		m.receive(start.Add(time.Second), advertisement{source: netip.MustParseAddr("192.0.2.3"), priority: 100, interval: 2 * time.Second})
		if preempt && m.deadline != original || !preempt && !m.deadline.After(original) {
			t.Fatalf("preempt=%t changed the wrong timer: %v", preempt, m.deadline)
		}
		if m.intervalMismatch != 1 {
			t.Fatal("interval mismatch was not observable")
		}
		shutdown := start.Add(2 * time.Second)
		m.receive(shutdown, advertisement{source: netip.MustParseAddr("192.0.2.3"), priority: 0, interval: time.Second})
		if m.deadline.Sub(shutdown) != m.skew() {
			t.Fatal("priority zero did not shorten failover to skew time")
		}
	}
}

func TestActiveTieBreakAndImmediateAssertion(t *testing.T) {
	start := time.Unix(3000, 0)
	c := config4()
	m := newMachine(c, netip.MustParseAddr("192.0.2.2"), "instance")
	m.start(start)
	m.tick(m.deadline)
	before := m.deadline
	if a := m.receive(start.Add(4*time.Second), advertisement{source: netip.MustParseAddr("192.0.2.1"), priority: 150, interval: time.Second}); !a.advertise || a.update || m.state != StateActive || m.deadline != before {
		t.Fatal("active router did not immediately assert itself against an inferior equal-priority peer")
	}
	if a := m.receive(start.Add(5*time.Second), advertisement{source: netip.MustParseAddr("192.0.2.3"), priority: 150, interval: time.Second}); !a.update || a.advertise || m.state != StateBackup {
		t.Fatal("higher source address did not win an equal-priority active tie")
	}
	if m.snapshot().Peer != "192.0.2.3" || m.snapshot().Active {
		t.Fatal("withdrawal snapshot misrepresented the winning peer")
	}
}

func TestAddressOwnerAndFamiliesAreIndependent(t *testing.T) {
	c := config4()
	c.AddressOwner, c.Priority = true, 255
	noPreempt := false
	c.Preempt = &noPreempt
	m := newMachine(c, netip.MustParseAddr(c.SourceAddress), "owner")
	if a := m.start(time.Unix(4000, 0)); !a.advertise || !a.notify || m.state != StateActive {
		t.Fatal("address owner did not activate immediately")
	}
	m.receive(time.Unix(4001, 0), advertisement{source: netip.MustParseAddr("192.0.2.200"), priority: 255, interval: time.Second})
	if m.state != StateActive || m.rejected != 1 {
		t.Fatal("address owner accepted a peer advertisement")
	}
	v6 := config6()
	v6.VirtualRouterID = c.VirtualRouterID
	m6 := newMachine(v6, netip.MustParseAddr(v6.SourceAddress), "ipv6")
	m6.start(time.Unix(4000, 0))
	if m6.state != StateBackup || m.state != StateActive || VirtualMAC("ipv4", 42).String() == VirtualMAC("ipv6", 42).String() {
		t.Fatal("IPv4 and IPv6 instances were not independent")
	}
}
