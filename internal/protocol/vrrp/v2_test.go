package vrrp

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"net/netip"
	"testing"
	"time"
)

func config2() Config {
	c := config4()
	c.Version = 2
	return c
}

func TestVRRPv2WireIncludesAuthenticationWordsAndWholeSeconds(t *testing.T) {
	for _, fixture := range []struct{ password, wire string }{
		{"", "212a9601000186d1c00002010000000000000000"},
		{"labpass", "212a96010101e28bc00002016c61627061737300"},
	} {
		c := config2()
		c.AuthPassword = fixture.password
		source := netip.MustParseAddr(c.SourceAddress)
		wire := marshalAdvertisement(c, source, group4, 150)
		if hex.EncodeToString(wire) != fixture.wire {
			t.Fatalf("VRRPv2 wire mismatch: %x", wire)
		}
		p, err := parseAdvertisement(c, source, group4, wire)
		if err != nil || p.priority != 150 || p.interval != time.Second || len(p.addresses) != 1 {
			t.Fatal(p, err)
		}
		for n := 0; n < len(wire); n++ {
			if _, err := parseAdvertisement(c, source, group4, wire[:n]); err == nil {
				t.Fatal("truncated VRRPv2 packet accepted", n)
			}
		}
		other := c
		other.Version = 3
		if _, err := parseAdvertisement(other, source, group4, wire); err == nil {
			t.Fatal("wire version mismatch accepted")
		}
	}
}

func TestVRRPv2RejectsWrongPasswordTypeAndInterval(t *testing.T) {
	c := config2()
	c.AuthPassword = "labpass"
	source := netip.MustParseAddr(c.SourceAddress)
	wire := marshalAdvertisement(c, source, group4, 150)
	for _, change := range []func([]byte){
		func(b []byte) { b[4] = 0 },
		func(b []byte) { b[4] = 2 },
		func(b []byte) { b[5] = 2 },
		func(b []byte) { b[len(b)-1] = 1 },
		func(b []byte) { b[len(b)-8] ^= 1 },
	} {
		bad := bytes.Clone(wire)
		change(bad)
		bad[6], bad[7] = 0, 0
		binary.BigEndian.PutUint16(bad[6:], checksum(bad))
		if _, err := parseAdvertisement(c, source, group4, bad); err == nil {
			t.Fatal("VRRPv2 accepted a mismatched peer")
		}
	}
	c.AuthPassword = ""
	if _, err := parseAdvertisement(c, source, group4, wire); err == nil {
		t.Fatal("PASS accepted by an unauthenticated instance")
	}
	wire = marshalAdvertisement(c, source, group4, 150)
	copy(wire[len(wire)-8:], "ignored!")
	wire[6], wire[7] = 0, 0
	binary.BigEndian.PutUint16(wire[6:], checksum(wire))
	if _, err := parseAdvertisement(c, source, group4, wire); err != nil {
		t.Fatal("type zero must ignore authentication data", err)
	}
}

func TestVRRPv2UsesOneSecondSkewAndDiscardsInferiorMaster(t *testing.T) {
	c := config2()
	c.AdvertIntervalCentiseconds = 200
	m := newMachine(c, netip.MustParseAddr(c.SourceAddress), "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	now := time.Now()
	m.start(now)
	want := 6*time.Second + 106*time.Second/256
	if m.deadline.Sub(now) != want || ownershipLifetime(c) != want {
		t.Fatal("VRRPv2 used advertisement-scaled skew", m.deadline.Sub(now), ownershipLifetime(c))
	}
	m.enter(StateActive, now)
	p := advertisement{source: netip.MustParseAddr("192.0.2.3"), priority: 100, interval: 2 * time.Second}
	if a := m.receive(now, p); a.advertise || a.update || m.state != StateActive {
		t.Fatal("VRRPv2 answered an inferior nonzero advertisement", a)
	}
	p.priority = 0
	if a := m.receive(now, p); !a.advertise {
		t.Fatal("priority zero did not trigger an immediate assertion")
	}
}

func TestVRRPv2ConfigurationRejectsUnsupportedWireSettings(t *testing.T) {
	for _, edit := range []func(*Config){
		func(c *Config) { c.Version = 1 },
		func(c *Config) { c.Family = "ipv6" },
		func(c *Config) { c.AdvertIntervalCentiseconds = 150 },
		func(c *Config) { c.AdvertIntervalCentiseconds = 25600 },
		func(c *Config) { c.AuthPassword = "ninebytes" },
		func(c *Config) { c.AuthPassword = "bad\n" },
		func(c *Config) { c.Version = 3; c.AuthPassword = "labpass" },
	} {
		c := config2()
		edit(&c)
		if err := Validate(c); err == nil {
			t.Fatal("unsupported VRRP wire settings accepted")
		}
	}
	c := config2()
	c.AdvertIntervalCentiseconds = 25500
	c.AuthPassword = "eightbyt"
	if err := Validate(c); err != nil {
		t.Fatal(err)
	}
}
