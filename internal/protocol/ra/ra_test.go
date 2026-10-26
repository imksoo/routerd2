//go:build linux

package ra

import (
	"bytes"
	"encoding/binary"
	"net"
	"net/netip"
	"testing"
	"time"

	"github.com/imksoo/routerd2/internal/boottime"
	"github.com/imksoo/routerd2/internal/lifetime"
)

func example() Config {
	return Config{Resource: "lan-ra", Interface: "lan0", MinIntervalSeconds: 3, MaxIntervalSeconds: 4, RouterLifetimeSeconds: 30, DNSSLifetimeSeconds: 20, Prefixes: []Prefix{{Prefix: "2001:db8:1::/64", ValidLifetimeSeconds: 600, PreferredLifetimeSeconds: 300, OnLink: true, Autonomous: true}}, RDNSS: []string{"2001:db8:1::1"}, DNSSL: []string{"lan.example"}, MTU: 1500, Managed: true, OtherConfig: true, RouterPreference: "high"}
}
func TestAdvertisementLifetimesAndWithdrawal(t *testing.T) {
	cfg := example()
	now := time.Unix(1700000000, 0).UTC()
	cfg.Prefixes[0].ValidUntil = lifetime.Deadline{UTC: now.Add(100 * time.Second)}
	cfg.Prefixes[0].PreferredUntil = lifetime.Deadline{UTC: now.Add(50 * time.Second)}
	if err := Validate(cfg); err != nil {
		t.Fatal(err)
	}
	check := func(withdraw bool) {
		t.Helper()
		b, err := encode(cfg, net.HardwareAddr{2, 0, 0, 0, 0, 1}, boottime.Clock{WallTime: now.Add(10 * time.Second)}, withdraw)
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
	cfg.Prefixes[0].ValidUntil = lifetime.Deadline{UTC: now.Add(80 * time.Second)}
	cfg.Prefixes[0].PreferredUntil = lifetime.Deadline{UTC: time.Unix(1, 0).UTC()}
	for _, elapsed := range []time.Duration{10 * time.Second, 81 * time.Second} {
		packet, err := encode(cfg, nil, boottime.Clock{WallTime: now.Add(elapsed)}, false)
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
	if value := (lifetime.Deadline{UTC: farFuture}).Seconds(1800, boottime.Clock{WallTime: now}); value != 1800 {
		t.Fatal("distant absolute deadline removed the configured DNS cap", value)
	}
	if value := (lifetime.Deadline{UTC: farFuture}).Seconds(0, boottime.Clock{WallTime: now}); value != 0 {
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

func TestOriginalBootAuthorityBoundsAllAdvertisementsAcrossUTCSteps(t *testing.T) {
	clock := boottime.Clock{BootID: "11111111-2222-4333-8444-555555555555", BoottimeNanoseconds: int64(time.Hour), WallTime: time.Unix(1700000000, 0)}
	cfg := normalized(Config{Resource: "lan", Interface: "lan0", RouterLifetimeSeconds: 1800, DNSSLifetimeSeconds: 120, Authority: lifetime.After(clock, 30), Prefixes: []Prefix{{Prefix: "2001:db8:1::/64", OnLink: true, Autonomous: true, ValidLifetimeSeconds: 60, PreferredLifetimeSeconds: 20, ValidUntil: lifetime.After(clock, 60), PreferredUntil: lifetime.After(clock, 20)}}, LearnedRDNSS: []DNSServer{{Address: "2001:db8::53", ValidUntil: lifetime.After(clock, 120)}}})
	before, err := encode(cfg, nil, clock, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, shift := range []time.Duration{24 * time.Hour, -24 * time.Hour} {
		stepped := clock
		stepped.WallTime = stepped.WallTime.Add(shift)
		after, err := encode(cfg, nil, stepped, false)
		if err != nil || !bytes.Equal(before, after) {
			t.Fatal("UTC changed original RA budgets", err)
		}
	}
	clock.BoottimeNanoseconds += int64(30 * time.Second)
	expired, err := encode(cfg, nil, clock, false)
	if err != nil {
		t.Fatal(err)
	}
	if binary.BigEndian.Uint16(expired[6:]) != 0 || binary.BigEndian.Uint32(expired[20:]) != 0 || binary.BigEndian.Uint32(expired[52:]) != 0 {
		t.Fatal("expired authority continued advertising router/prefix/DNS lifetimes")
	}
}
