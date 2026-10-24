//go:build linux

package ra

import (
	"encoding/binary"
	"net"
	"net/netip"
	"testing"
	"time"
)

func example() Config {
	return Config{Resource: "lan-ra", Interface: "lan0", MinIntervalSeconds: 3, MaxIntervalSeconds: 4, RouterLifetimeSeconds: 30, DNSSLifetimeSeconds: 20, Prefixes: []Prefix{{Prefix: "2001:db8:1::/64", ValidLifetimeSeconds: 600, PreferredLifetimeSeconds: 300, OnLink: true, Autonomous: true}}, RDNSS: []string{"2001:db8:1::1"}, DNSSL: []string{"lan.example"}, MTU: 1500, Managed: true, OtherConfig: true, RouterPreference: "high"}
}
func TestAdvertisementLifetimesAndWithdrawal(t *testing.T) {
	cfg := example()
	now := time.Unix(1700000000, 0).UTC()
	cfg.Prefixes[0].ValidUntil = now.Add(100 * time.Second)
	cfg.Prefixes[0].PreferredUntil = now.Add(50 * time.Second)
	if err := Validate(cfg); err != nil {
		t.Fatal(err)
	}
	check := func(withdraw bool) {
		t.Helper()
		b, err := encode(cfg, net.HardwareAddr{2, 0, 0, 0, 0, 1}, now.Add(10*time.Second), withdraw)
		if err != nil {
			t.Fatal(err)
		}
		if b[0] != 134 || b[5] != 0xc8 {
			t.Fatalf("incorrect RA flags %x", b[:16])
		}
		life := binary.BigEndian.Uint16(b[6:])
		if withdraw && life != 0 || !withdraw && life != 30 {
			t.Fatalf("router life=%d", life)
		}
		seen := map[byte]bool{}
		for at := 16; at < len(b); {
			code := b[at]
			size := int(b[at+1]) * 8
			if size == 0 || at+size > len(b) {
				t.Fatal("malformed RA option")
			}
			opt := b[at : at+size]
			seen[code] = true
			switch code {
			case 3:
				valid, pref := binary.BigEndian.Uint32(opt[4:]), binary.BigEndian.Uint32(opt[8:])
				if withdraw && (valid != 0 || pref != 0) || !withdraw && (valid != 90 || pref != 40) {
					t.Fatalf("prefix lifetime %d/%d", valid, pref)
				}
			case 25, 31:
				life := binary.BigEndian.Uint32(opt[4:])
				if withdraw && life != 0 || !withdraw && life != 20 {
					t.Fatalf("DNS lifetime %d", life)
				}
			}
			at += size
		}
		for _, code := range []byte{1, 3, 5, 25, 31} {
			if !seen[code] {
				t.Fatalf("missing option %d", code)
			}
		}
	}
	check(false)
	check(true)
}
func TestSolicitationValidation(t *testing.T) {
	rs := []byte{133, 0, 0, 0, 0, 0, 0, 0}
	ll := netip.MustParseAddr("fe80::1")
	if !solicitation(rs, ll, 255, 4, 4) || !solicitation(rs, netip.IPv6Unspecified(), 255, 4, 4) || !solicitation(rs, netip.MustParseAddr("2001:db8::1"), 255, 4, 4) {
		t.Fatal("valid RS rejected")
	}
	for _, v := range []struct {
		p          []byte
		a          netip.Addr
		hop, index int
	}{{rs, ll, 254, 4}, {rs, ll, 255, 5}, {rs, netip.MustParseAddr("ff02::1"), 255, 4}, {append(append([]byte(nil), rs...), 1, 0), ll, 255, 4}, {append(append([]byte(nil), rs...), 1, 1, 2, 0, 0, 0, 0, 1), netip.IPv6Unspecified(), 255, 4}} {
		if solicitation(v.p, v.a, v.hop, v.index, 4) {
			t.Fatalf("invalid RS accepted %+v", v)
		}
	}
}

func TestDeprecatedPrefixUsesFiniteDeadlineAndDNSRetainsItsCap(t *testing.T) {
	now := time.Unix(1700000000, 0).UTC()
	cfg := example()
	cfg.Prefixes[0].ValidLifetimeSeconds = ^uint32(0)
	cfg.Prefixes[0].PreferredLifetimeSeconds = 0
	cfg.Prefixes[0].ValidUntil = now.Add(80 * time.Second)
	cfg.Prefixes[0].PreferredUntil = time.Unix(1, 0).UTC()
	for _, elapsed := range []time.Duration{10 * time.Second, 81 * time.Second} {
		packet, err := encode(cfg, nil, now.Add(elapsed), false)
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for at := 16; at < len(packet); at += int(packet[at+1]) * 8 {
			if packet[at] != 3 {
				continue
			}
			found = true
			want := uint32(70)
			if elapsed > 80*time.Second {
				want = 0
			}
			if valid, preferred := binary.BigEndian.Uint32(packet[at+4:]), binary.BigEndian.Uint32(packet[at+8:]); valid != want || preferred != 0 {
				t.Fatalf("deprecated prefix on wire has lifetime %d/%d, wanted %d/0", valid, preferred, want)
			}
		}
		if !found {
			t.Fatal("retained prefix is absent from advertisement")
		}
	}
	farFuture := now.AddDate(200, 0, 0)
	if value := lifetime(1800, farFuture, now); value != 1800 {
		t.Fatal("distant absolute deadline removed the configured DNS cap", value)
	}
	if value := lifetime(0, farFuture, now); value != 0 {
		t.Fatal("distant absolute deadline undid explicit withdrawal", value)
	}
}
func TestValidateRejectsUnrepresentableAdvertisements(t *testing.T) {
	bad := example()
	bad.Prefixes[0].Prefix = "2001:db8::1/64"
	if Validate(bad) == nil {
		t.Fatal("host-bits prefix accepted")
	}
	bad = example()
	bad.Prefixes[0].Prefix = "2001:db8::/56"
	if Validate(bad) == nil {
		t.Fatal("autonomous non-/64 accepted")
	}
	bad = example()
	bad.DNSSL = []string{"bad..example"}
	if Validate(bad) == nil {
		t.Fatal("bad DNSSL accepted")
	}
	bad = example()
	bad.RouterLifetimeSeconds = 2
	if Validate(bad) == nil {
		t.Fatal("short router lifetime accepted")
	}
}
