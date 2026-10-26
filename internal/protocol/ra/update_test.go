//go:build linux

package ra

import (
	"encoding/binary"
	"fmt"
	"net"
	"net/netip"
	"testing"
	"time"

	"github.com/imksoo/routerd2/internal/boottime"
	"github.com/imksoo/routerd2/internal/lifetime"
)

func prefixLifetimes(t *testing.T, packet []byte) map[string][2]uint32 {
	t.Helper()
	out := map[string][2]uint32{}
	for pos := 16; pos < len(packet); {
		if pos+2 > len(packet) {
			t.Fatal("short option")
		}
		size := int(packet[pos+1]) * 8
		if size == 0 || pos+size > len(packet) {
			t.Fatal("bad option length")
		}
		if packet[pos] == 3 {
			var addr [16]byte
			copy(addr[:], packet[pos+16:pos+32])
			name := netip.PrefixFrom(netip.AddrFrom16(addr), int(packet[pos+2])).String()
			if _, ok := out[name]; ok {
				t.Fatalf("duplicate prefix in advertisement: %s", name)
			}
			out[name] = [2]uint32{binary.BigEndian.Uint32(packet[pos+4:]), binary.BigEndian.Uint32(packet[pos+8:])}
		}
		pos += size
	}
	return out
}

func TestLiveUpdateKeepsRetainedPrefixAndWithdrawsOnlyRemovedOptions(t *testing.T) {
	now := time.Unix(1700000000, 0).UTC()
	old := normalized(example())
	old.Prefixes = append(old.Prefixes, Prefix{Prefix: "2001:db8:2::/64", ValidLifetimeSeconds: 600, PreferredLifetimeSeconds: 300, OnLink: true, Autonomous: true})
	next := normalized(old)
	next.Prefixes = next.Prefixes[:1]
	next.Prefixes[0].ValidUntil = lifetime.Deadline{UTC: now.Add(200 * time.Second)}
	next.Prefixes[0].PreferredUntil = lifetime.Deadline{UTC: now.Add(100 * time.Second)}
	next.Prefixes = append(next.Prefixes, Prefix{Prefix: "2001:db8:3::/64", ValidLifetimeSeconds: 500, PreferredLifetimeSeconds: 250, OnLink: true, Autonomous: true})
	next.RDNSS = []string{"2001:db8:3::1"}
	next.DNSSL = []string{"new.example"}
	active, pending, changed, err := planUpdate(old, next, withdrawals{}, nil, 1500, boottime.Clock{WallTime: now})
	if err != nil || !changed {
		t.Fatalf("update: %v changed=%v", err, changed)
	}
	for count := 0; count < 3; count++ {
		packet, err := encodeWithWithdrawals(active, pending, nil, boottime.Clock{WallTime: now.Add(time.Duration(10+count*10) * time.Second)}, false)
		if err != nil {
			t.Fatal(err)
		}
		if binary.BigEndian.Uint16(packet[6:]) != 30 {
			t.Fatal("live update withdrew the default router")
		}
		prefixes := prefixLifetimes(t, packet)
		if prefixes["2001:db8:1::/64"] != [2]uint32{uint32(190 - count*10), uint32(90 - count*10)} {
			t.Fatalf("retained deadline was changed or withdrawn: %v", prefixes)
		}
		if len(prefixes) != 3 || prefixes["2001:db8:2::/64"] != [2]uint32{} || prefixes["2001:db8:3::/64"][0] != 500 {
			t.Fatalf("wrong prefix withdrawal: %v", prefixes)
		}
		dnsLifetimes := map[byte][]uint32{}
		for pos := 16; pos < len(packet); pos += int(packet[pos+1]) * 8 {
			if packet[pos] == 25 || packet[pos] == 31 {
				dnsLifetimes[packet[pos]] = append(dnsLifetimes[packet[pos]], binary.BigEndian.Uint32(packet[pos+4:]))
			}
		}
		for _, code := range []byte{25, 31} {
			if lives := dnsLifetimes[code]; len(lives) != 2 || lives[0] != 20 || lives[1] != 0 {
				t.Fatalf("DNS code %d lifetimes %v", code, lives)
			}
		}
		pending.sent()
	}
	if pending.any() {
		t.Fatal("withdrawals not retired after three transmissions")
	}
	packet, err := encodeWithWithdrawals(active, pending, nil, boottime.Clock{WallTime: now.Add(40 * time.Second)}, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := prefixLifetimes(t, packet)["2001:db8:2::/64"]; ok {
		t.Fatal("removed prefix kept advertising after withdrawal repeats")
	}
}

func TestLiveUpdateRejectsIdentityInvalidAndOversizedTransition(t *testing.T) {
	old := normalized(example())
	for _, change := range []func(*Config){func(c *Config) { c.Resource = "other" }, func(c *Config) { c.Interface = "other0" }, func(c *Config) { c.SourceAddress = "fe80::1" }, func(c *Config) { c.MTU = 9000 }, func(c *Config) { c.Prefixes[0].PreferredLifetimeSeconds = 10000 }} {
		next := normalized(old)
		change(&next)
		if _, _, changed, err := planUpdate(old, next, withdrawals{}, nil, 1500, boottime.Clock{WallTime: time.Now()}); err == nil || changed {
			t.Fatalf("invalid update accepted: %+v", next)
		}
	}
	old.Prefixes = nil
	next := normalized(old)
	for i := 0; i < 36; i++ {
		p := Prefix{Prefix: fmt.Sprintf("2001:db8:%x::/64", i), ValidLifetimeSeconds: 100, PreferredLifetimeSeconds: 50, OnLink: true, Autonomous: true}
		old.Prefixes = append(old.Prefixes, p)
		p.Prefix = fmt.Sprintf("2001:db8:%x::/64", i+100)
		next.Prefixes = append(next.Prefixes, p)
	}
	if err := Validate(old); err != nil {
		t.Fatal(err)
	}
	if err := Validate(next); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := planUpdate(old, next, withdrawals{}, net.HardwareAddr{2, 0, 0, 0, 0, 1}, 1500, boottime.Clock{WallTime: time.Now()}); err == nil {
		t.Fatal("oversized transition accepted")
	}
}

func TestLiveUpdateReadditionCancelsPendingWithdrawal(t *testing.T) {
	old := normalized(example())
	removed := normalized(old)
	removed.Prefixes = nil
	active, pending, _, err := planUpdate(old, removed, withdrawals{}, nil, 1500, boottime.Clock{WallTime: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	restored, remaining, _, err := planUpdate(active, old, pending, nil, 1500, boottime.Clock{WallTime: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	if remaining.any() {
		t.Fatal("restored prefix still queued for withdrawal")
	}
	packet, err := encodeWithWithdrawals(restored, remaining, nil, boottime.Clock{WallTime: time.Now()}, false)
	if err != nil {
		t.Fatal(err)
	}
	if prefixLifetimes(t, packet)[old.Prefixes[0].Prefix][0] != 600 {
		t.Fatal("restored prefix advertised zero")
	}
}

func TestPollingAbsoluteDeadlineDoesNotTriggerRenewalOrExtendIt(t *testing.T) {
	now := time.Unix(1700000000, 0).UTC()
	old := normalized(example())
	old.Prefixes[0].ValidUntil = lifetime.Deadline{UTC: now.Add(100 * time.Second)}
	old.Prefixes[0].PreferredUntil = lifetime.Deadline{UTC: now.Add(50 * time.Second)}
	polled := normalized(old)
	polled.Prefixes[0].ValidLifetimeSeconds = 90
	polled.Prefixes[0].PreferredLifetimeSeconds = 40
	active, pending, changed, err := planUpdate(old, polled, withdrawals{}, nil, 1500, boottime.Clock{WallTime: now.Add(10 * time.Second)})
	if err != nil || changed {
		t.Fatalf("same deadline changed configuration: %v %v", changed, err)
	}
	packet, err := encodeWithWithdrawals(active, pending, nil, boottime.Clock{WallTime: now.Add(20 * time.Second)}, false)
	if err != nil {
		t.Fatal(err)
	}
	if prefixLifetimes(t, packet)[old.Prefixes[0].Prefix] != [2]uint32{80, 30} {
		t.Fatal("polling extended a delegated deadline")
	}
}
