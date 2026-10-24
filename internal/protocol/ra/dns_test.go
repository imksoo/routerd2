//go:build linux

package ra

import (
	"encoding/binary"
	"fmt"
	"net/netip"
	"reflect"
	"strings"
	"testing"
	"time"
)

// Parse every name rather than assuming an option order or one common lifetime.
// A duplicate is invalid for these tests, including a positive/zero conflict.
func dnsLifetimes(t *testing.T, packet []byte) (map[string]uint32, map[string]uint32) {
	t.Helper()
	servers, domains := map[string]uint32{}, map[string]uint32{}
	for at := 16; at < len(packet); {
		if at+2 > len(packet) || packet[at+1] == 0 || at+int(packet[at+1])*8 > len(packet) {
			t.Fatal("invalid RA option framing")
		}
		option := packet[at : at+int(packet[at+1])*8]
		at += len(option)
		if option[0] != 25 && option[0] != 31 {
			continue
		}
		if len(option) < 8 {
			t.Fatal("short DNS option")
		}
		life := binary.BigEndian.Uint32(option[4:])
		if option[0] == 25 {
			if len(option) < 24 || (len(option)-8)%16 != 0 {
				t.Fatal("invalid RDNSS option")
			}
			for pos := 8; pos < len(option); pos += 16 {
				name := netip.AddrFrom16([16]byte(option[pos : pos+16])).String()
				if _, exists := servers[name]; exists {
					t.Fatalf("RDNSS %s appears twice", name)
				}
				servers[name] = life
			}
			continue
		}
		for rest := option[8:]; len(rest) != 0 && rest[0] != 0; {
			var labels []string
			for {
				if len(rest) == 0 {
					t.Fatal("unterminated DNSSL name")
				}
				n := int(rest[0])
				rest = rest[1:]
				if n == 0 {
					break
				}
				if n > 63 || n > len(rest) {
					t.Fatal("invalid DNSSL label")
				}
				labels = append(labels, string(rest[:n]))
				rest = rest[n:]
			}
			name := strings.Join(labels, ".")
			if _, exists := domains[name]; exists {
				t.Fatalf("DNSSL %s appears twice", name)
			}
			domains[name] = life
		}
	}
	return servers, domains
}

func TestLearnedDNSLifetimesAndStaticPrecedence(t *testing.T) {
	now := time.Unix(1700000000, 0).UTC()
	c := example()
	c.LearnedRDNSS = []DNSServer{
		{Address: "2001:0db8:1::1", ValidUntil: now.Add(time.Second)},
		{Address: "2001:db8::53", ValidUntil: now.Add(5 * time.Second)},
		{Address: "2001:db8::53", ValidUntil: now.Add(12 * time.Second)},
		{Address: "2001:db8::54", ValidUntil: now.Add(25 * time.Second)},
		{Address: "2001:db8::55"}, // explicitly infinite source, still wire-capped
	}
	c.LearnedDNSSL = []SearchDomain{
		{Domain: "LAN.EXAMPLE.", ValidUntil: now.Add(time.Second)},
		{Domain: "Wan.Example.", ValidUntil: now.Add(12 * time.Second)},
		{Domain: "wan.example", ValidUntil: now.Add(5 * time.Second)},
		{Domain: "later.example", ValidUntil: now.Add(25 * time.Second)},
		{Domain: "infinite.example"},
	}
	if err := Validate(c); err != nil {
		t.Fatal(err)
	}
	c = normalized(c)
	for _, check := range []struct {
		elapsed time.Duration
		first   uint32
		second  uint32
	}{{0, 12, 20}, {4250 * time.Millisecond, 7, 20}, {12 * time.Second, 0, 13}, {26 * time.Second, 0, 0}} {
		packet, err := encode(c, nil, now.Add(check.elapsed), false)
		if err != nil {
			t.Fatal(err)
		}
		servers, domains := dnsLifetimes(t, packet)
		wantServers := map[string]uint32{"2001:db8:1::1": 20, "2001:db8::53": check.first, "2001:db8::54": check.second, "2001:db8::55": 20}
		wantDomains := map[string]uint32{"lan.example": 20, "wan.example": check.first, "later.example": check.second, "infinite.example": 20}
		if !reflect.DeepEqual(servers, wantServers) || !reflect.DeepEqual(domains, wantDomains) {
			t.Fatalf("after %s, got RDNSS=%v DNSSL=%v", check.elapsed, servers, domains)
		}
	}
	packet, err := encode(c, nil, now, true)
	if err != nil {
		t.Fatal(err)
	}
	servers, domains := dnsLifetimes(t, packet)
	for _, entries := range []map[string]uint32{servers, domains} {
		for name, value := range entries {
			if value != 0 {
				t.Fatalf("shutdown retained %s for %d seconds", name, value)
			}
		}
	}
}

func TestLearnedDNSUpdateWithdrawsOnlyRemovedNames(t *testing.T) {
	now := time.Unix(1700000000, 0).UTC()
	old := normalized(example())
	old.LearnedRDNSS = []DNSServer{{Address: "2001:db8::53", ValidUntil: now.Add(10 * time.Second)}, {Address: "2001:db8::54", ValidUntil: now.Add(10 * time.Second)}}
	old.LearnedDNSSL = []SearchDomain{{Domain: "keep.example", ValidUntil: now.Add(10 * time.Second)}, {Domain: "remove.example", ValidUntil: now.Add(10 * time.Second)}}
	next := normalized(old)
	next.LearnedRDNSS = []DNSServer{{Address: "2001:db8::53", ValidUntil: now.Add(40 * time.Second)}}
	next.LearnedDNSSL = []SearchDomain{{Domain: "keep.example", ValidUntil: now.Add(40 * time.Second)}}
	active, pending, changed, err := planUpdate(old, next, withdrawals{}, nil, 1500, now.Add(5*time.Second))
	if err != nil || !changed {
		t.Fatal(changed, err)
	}
	packet, err := encodeWithWithdrawals(active, pending, nil, now.Add(6*time.Second), false)
	if err != nil {
		t.Fatal(err)
	}
	servers, domains := dnsLifetimes(t, packet)
	if !reflect.DeepEqual(servers, map[string]uint32{"2001:db8:1::1": 20, "2001:db8::53": 20, "2001:db8::54": 0}) ||
		!reflect.DeepEqual(domains, map[string]uint32{"lan.example": 20, "keep.example": 20, "remove.example": 0}) {
		t.Fatal("renewal withdrew a retained name or retained a removed name", servers, domains)
	}
	_, _, changed, err = planUpdate(active, normalized(active), pending, nil, 1500, now.Add(8*time.Second))
	if err != nil || changed {
		t.Fatal("unchanged polling changes learned DNS", changed, err)
	}
	// Source ownership can move between configured and learned without a
	// conflicting zero option for a name that remains valid in the new intent.
	static := normalized(active)
	static.RDNSS = append(static.RDNSS, "2001:db8::53")
	static.DNSSL = append(static.DNSSL, "keep.example")
	static.LearnedRDNSS, static.LearnedDNSSL = nil, nil
	for _, pair := range [][2]Config{{active, static}, {static, active}} {
		cfg, queued, _, err := planUpdate(pair[0], pair[1], withdrawals{}, nil, 1500, now.Add(9*time.Second))
		if err != nil || queued.any() {
			t.Fatal("DNS ownership transfer caused withdrawal", queued, err)
		}
		packet, err := encodeWithWithdrawals(cfg, queued, nil, now.Add(10*time.Second), false)
		if err != nil {
			t.Fatal(err)
		}
		servers, domains := dnsLifetimes(t, packet)
		if servers["2001:db8::53"] == 0 || domains["keep.example"] == 0 {
			t.Fatal("retained name received a zero lifetime", servers, domains)
		}
	}
	// Restoring a removed learned name cancels its queued withdrawal.
	restored, queued, _, err := planUpdate(active, old, pending, nil, 1500, now.Add(7*time.Second))
	if err != nil || queued.any() {
		t.Fatal("re-added name retained a queued withdrawal", queued, err)
	}
	packet, err = encodeWithWithdrawals(restored, queued, nil, now.Add(8*time.Second), false)
	if err != nil {
		t.Fatal(err)
	}
	servers, domains = dnsLifetimes(t, packet)
	if servers["2001:db8::54"] != 2 || domains["remove.example"] != 2 {
		t.Fatal("re-added learned name lost its absolute deadline", servers, domains)
	}
}

func TestLearnedDNSValidationReservesFutureOptionGroups(t *testing.T) {
	c := example()
	c.Prefixes, c.RDNSS, c.DNSSL = nil, nil, nil
	c.MTU = 0
	c.DNSSLifetimeSeconds = 1800
	now := time.Now().UTC()
	for i := 0; i < 54; i++ {
		c.LearnedRDNSS = append(c.LearnedRDNSS, DNSServer{Address: fmt.Sprintf("2001:db8::%x", i+1), ValidUntil: now.Add(time.Hour + time.Duration(i)*time.Second)})
	}
	packet, err := encode(c, nil, now, false)
	if err != nil || len(packet) > 1240 {
		t.Fatal("fixture should initially fit one common lifetime", len(packet), err)
	}
	if Validate(c) == nil {
		t.Fatal("accepted config whose DNS groups may later exceed minimum MTU")
	}
	for i := range c.LearnedRDNSS {
		c.LearnedRDNSS[i].ValidUntil = c.LearnedRDNSS[0].ValidUntil
	}
	if err := Validate(c); err != nil {
		t.Fatal("identical deadlines do not require separate wire options", err)
	}
	c = example()
	c.LearnedRDNSS = []DNSServer{{Address: "192.0.2.53"}}
	if Validate(c) == nil {
		t.Fatal("learned IPv4 RDNSS accepted")
	}
	c.LearnedRDNSS = nil
	c.LearnedDNSSL = []SearchDomain{{Domain: "bad..example"}}
	if Validate(c) == nil {
		t.Fatal("invalid learned search domain accepted")
	}
}
