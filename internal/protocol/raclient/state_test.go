// SPDX-License-Identifier: BSD-3-Clause

package raclient

import (
	"encoding/binary"
	"net"
	"net/netip"
	"reflect"
	"testing"
	"time"

	"github.com/imksoo/routerd2/internal/boottime"
	"github.com/imksoo/routerd2/internal/lifetime"
)

func testConfig() Config {
	return Config{Resource: "wan-ra", Interface: "wan0", StatePath: "/state/wan-ra.json", InterfaceID: "::1234"}
}
func testClock(seconds int) boottime.Clock {
	return boottime.Clock{BootID: "11111111-2222-4333-8444-555555555555", WallTime: time.Unix(1700000000+int64(seconds), 0).UTC(), BoottimeNanoseconds: int64(1000+seconds) * int64(time.Second)}
}
func testState(t *testing.T, cfg Config) *state {
	t.Helper()
	s, err := newState(cfg, linkInfo{index: 2, mtu: 1500, hardware: net.HardwareAddr{2, 0, 0, 0, 0, 1}, source: netip.MustParseAddr("fe80::1")})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func pio(prefix string, flags byte, valid, preferred uint32) []byte {
	p := netip.MustParsePrefix(prefix)
	b := make([]byte, 32)
	b[0], b[1], b[2], b[3] = 3, 4, byte(p.Bits()), flags
	binary.BigEndian.PutUint32(b[4:], valid)
	binary.BigEndian.PutUint32(b[8:], preferred)
	raw := p.Addr().As16()
	copy(b[16:], raw[:])
	return b
}
func rio(prefix string, pref byte, life uint32) []byte {
	p := netip.MustParsePrefix(prefix)
	size := 8
	if p.Bits() > 64 {
		size = 24
	} else if p.Bits() > 0 {
		size = 16
	}
	b := make([]byte, size)
	b[0], b[1], b[2], b[3] = 24, byte(size/8), byte(p.Bits()), pref
	binary.BigEndian.PutUint32(b[4:], life)
	raw := p.Addr().As16()
	copy(b[8:], raw[:])
	return b
}
func rdnss(life uint32, addresses ...string) []byte {
	b := make([]byte, 8+16*len(addresses))
	b[0], b[1] = 25, byte(len(b)/8)
	binary.BigEndian.PutUint32(b[4:], life)
	for i, address := range addresses {
		raw := netip.MustParseAddr(address).As16()
		copy(b[8+i*16:], raw[:])
	}
	return b
}
func dnssl(life uint32) []byte {
	b := []byte{31, 3, 0, 0, 0, 0, 0, 0, 3, 'l', 'a', 'n', 7, 'e', 'x', 'a', 'm', 'p', 'l', 'e', 0, 0, 0, 0}
	binary.BigEndian.PutUint32(b[4:], life)
	return b
}
func advert(router string, life uint16, flags byte, opts ...[]byte) packet {
	b := make([]byte, 16)
	b[0], b[4], b[5] = 134, 64, flags
	binary.BigEndian.PutUint16(b[6:], life)
	for _, opt := range opts {
		b = append(b, opt...)
	}
	p := packet{payload: b, source: netip.MustParseAddr(router), destination: netip.MustParseAddr("ff02::1"), hopLimit: 255, ifindex: 2}
	recalc(&p)
	return p
}
func recalc(p *packet) {
	p.payload[2], p.payload[3] = 0, 0
	binary.BigEndian.PutUint16(p.payload[2:], checksum(p.payload, p.source, p.destination))
}
func mustHandle(t *testing.T, s *state, p packet, now boottime.Clock) Snapshot {
	t.Helper()
	if err := s.handle(p, now); err != nil {
		t.Fatal(err)
	}
	snapshot := s.snapshot(now)
	if err := ValidateSnapshot(s.config, snapshot); err != nil {
		t.Fatal(err)
	}
	return snapshot
}

func TestRALifetimesAreIndependentAndAbsolute(t *testing.T) {
	s := testState(t, testConfig())
	now := testClock(0)
	snap := mustHandle(t, s, advert("fe80::2", 3, 0xc8, pio("2001:db8:1::/64", 0xc0, 30, 10), rio("2001:db8:2::/48", 0, 5), rdnss(4, "fe80::53"), dnssl(8)), now)
	if len(snap.Addresses) != 1 || snap.Addresses[0].Address != "2001:db8:1::1234/128" || len(snap.Prefixes) != 1 || len(snap.Routes) != 2 || !snap.Managed || !snap.OtherConfig || len(snap.DNSServers) != 1 || len(snap.SearchDomains) != 1 {
		t.Fatalf("incomplete RA projection: %+v", snap)
	}
	if snap.Addresses[0].ValidUntil != lifetime.After(now, 30) || snap.Addresses[0].PreferredUntil != lifetime.After(now, 10) {
		t.Fatal("PIO deadlines shifted")
	}
	if !s.expire(testClock(4)) {
		t.Fatal("finite router/DNS expiration was not published")
	}
	after := s.snapshot(testClock(4))
	if len(after.Routes) != 1 || after.Routes[0].Destination != "2001:db8:2::/48" || len(after.DNSServers) != 0 || len(after.SearchDomains) != 1 || len(after.Addresses) != 1 {
		t.Fatal("expiration removed unrelated independently timed state")
	}
	if after.Addresses[0].ValidUntil != snap.Addresses[0].ValidUntil {
		t.Fatal("periodic publication extended PIO lifetime")
	}
	if !s.expire(testClock(11)) || !s.snapshot(testClock(11)).Addresses[0].Deprecated {
		t.Fatal("preferred lifetime expiry did not deprecate retained address")
	}
	// A later RA omitting options must not replace the full learned set.
	mustHandle(t, s, advert("fe80::2", 0, 0), testClock(12))
	if len(s.learned.Addresses) != 1 || len(s.learned.Prefixes) != 1 {
		t.Fatal("omitted PIO withdrew prior learning")
	}
	if !s.expire(testClock(30)) || len(s.learned.Addresses)+len(s.learned.Prefixes) != 0 {
		t.Fatal("valid expiry retained obsolete address/prefix")
	}
}

func TestTwoHourRuleAndImmediateDeprecation(t *testing.T) {
	for _, tc := range []struct {
		name          string
		old, next     uint32
		elapsed, want int
	}{
		{"long-to-zero", 10000, 0, 100, 7300},
		{"short-to-zero", 3600, 0, 100, 3600},
		{"exact-two-hours", 7200, 100, 0, 7200},
		{"extension-under-two-hours", 1000, 2000, 0, 2000},
		{"explicit-long-reduction", 10000, 8000, 0, 8000},
		{"infinite-to-zero", ^uint32(0), 0, 100, 7300},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := testState(t, testConfig())
			mustHandle(t, s, advert("fe80::2", 30, 0, pio("2001:db8:1::/64", 0xc0, tc.old, tc.old)), testClock(0))
			snap := mustHandle(t, s, advert("fe80::2", 0, 0, pio("2001:db8:1::/64", 0xc0, tc.next, 0)), testClock(tc.elapsed))
			if len(snap.Addresses) != 1 || snap.Addresses[0].ValidUntil.Nanos != uint64(testClock(tc.want).BoottimeNanoseconds) || !snap.Addresses[0].Deprecated {
				t.Fatalf("wrong protected valid/deprecated lifetime: %+v", snap.Addresses)
			}
			if tc.next == 0 && (len(snap.Prefixes) != 0 || len(snap.Routes) != 0) {
				t.Fatal("two-hour address protection incorrectly protected on-link/default routes")
			}
		})
	}
	s := testState(t, testConfig())
	snap := mustHandle(t, s, advert("fe80::2", 0, 0, pio("2001:db8:1::/64", 0xc0, 0, 0)), testClock(0))
	if len(snap.Addresses) != 0 {
		t.Fatal("zero valid lifetime created a new address")
	}
}

func TestPIOAutonomousAndOnLinkAreIndependent(t *testing.T) {
	s := testState(t, testConfig())
	snap := mustHandle(t, s, advert("fe80::2", 10, 0, pio("2001:db8:1::/64", 0x40, 60, 30), pio("2001:db8:2::/56", 0xc0, 60, 30)), testClock(0))
	if len(snap.Addresses) != 1 || snap.Addresses[0].Address != "2001:db8:1::1234/128" || len(snap.Prefixes) != 1 || snap.Prefixes[0].Prefix != "2001:db8:2::/56" {
		t.Fatal("A/L-bit or /64 autoconfiguration requirement changed")
	}
	snap = mustHandle(t, s, advert("fe80::2", 10, 0, pio("2001:db8:1::/64", 0, 0, 0), pio("2001:db8:2::/56", 0, 0, 0)), testClock(1))
	if len(snap.Addresses) != 1 || len(snap.Prefixes) != 1 {
		t.Fatal("clear A/L flags were incorrectly treated as withdrawals")
	}
}

func TestRIOOverridesHeaderAndRoutersRemainIndependent(t *testing.T) {
	s := testState(t, testConfig())
	snap := mustHandle(t, s, advert("fe80::2", 30, 0x08, rio("::/0", 0x18, 60)), testClock(0))
	if len(snap.Routes) != 1 || snap.Routes[0].Preference != -1 || snap.Routes[0].ValidUntil.Nanos != uint64(testClock(60).BoottimeNanoseconds) {
		t.Fatal("RIO ::/0 did not override the RA header")
	}
	snap = mustHandle(t, s, advert("fe80::3", 100, 0x08, rdnss(200, "2001:db8::53")), testClock(1))
	if len(snap.Routes) != 2 {
		t.Fatal("new router replaced an unrelated router")
	}
	snap = mustHandle(t, s, advert("fe80::2", 0, 0), testClock(2))
	if len(snap.Routes) != 1 || snap.Routes[0].Gateway != "fe80::3" || len(snap.DNSServers) != 1 {
		t.Fatal("router withdrawal discarded unrelated routes/DNS")
	}
	// RDNSS lifetime ownership is per interface/server, not per advertiser.
	snap = mustHandle(t, s, advert("fe80::2", 0, 0, rdnss(0, "2001:db8::53")), testClock(3))
	if len(snap.DNSServers) != 0 {
		t.Fatal("zero RDNSS lifetime did not withdraw shared server entry")
	}
}

func learnedData(s Snapshot) any {
	return struct {
		A []Address
		P []Prefix
		R []Route
		D []DNSServer
		S []SearchDomain
	}{s.Addresses, s.Prefixes, s.Routes, s.DNSServers, s.SearchDomains}
}

func TestMalformedAndUnauthorizedRAIsAtomic(t *testing.T) {
	cfg := testConfig()
	cfg.AllowedRouters = []string{"fe80::2"}
	s := testState(t, cfg)
	mustHandle(t, s, advert("fe80::2", 30, 0, pio("2001:db8:1::/64", 0xc0, 60, 30)), testClock(0))
	before := learnedData(clone(s.learned))
	base := func() packet { return advert("fe80::2", 0, 0, pio("2001:db8:1::/64", 0xc0, 0, 0)) }
	cases := map[string]func(*packet){
		"checksum":                     func(p *packet) { p.payload[6] ^= 1 },
		"hop-limit":                    func(p *packet) { p.hopLimit = 254 },
		"interface":                    func(p *packet) { p.ifindex = 3 },
		"global-source":                func(p *packet) { p.source = netip.MustParseAddr("2001:db8::1"); recalc(p) },
		"not-allowed":                  func(p *packet) { p.source = netip.MustParseAddr("fe80::3"); recalc(p) },
		"wrong-destination":            func(p *packet) { p.destination = netip.MustParseAddr("ff02::2"); recalc(p) },
		"code":                         func(p *packet) { p.payload[1] = 1; recalc(p) },
		"truncated-after-valid-option": func(p *packet) { p.payload = append(p.payload, 25, 3); recalc(p) },
		"zero-option-size":             func(p *packet) { p.payload = append(p.payload, 25, 0, 0, 0, 0, 0, 0, 0); recalc(p) },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			p := base()
			mutate(&p)
			if s.handle(p, testClock(1)) == nil {
				t.Fatal("invalid RA accepted")
			}
			if !reflect.DeepEqual(before, learnedData(s.learned)) {
				t.Fatal("invalid trailing/header data partially withdrew valid state")
			}
		})
	}
	if s.learned.Rejected != uint64(len(cases)) {
		t.Fatal("invalid datagrams not accounted for")
	}
}

func TestUnusableDNSOptionsDoNotDiscardRouterAndPrefix(t *testing.T) {
	s := testState(t, testConfig())
	badDomain := dnssl(10)
	badDomain[8] = 0xc0
	snap := mustHandle(t, s, advert("fe80::2", 30, 0, pio("2001:db8:1::/64", 0xc0, 60, 30), rdnss(30, "ff02::1"), badDomain, rdnss(30, "fe80::53")), testClock(0))
	if len(snap.Addresses) != 1 || len(snap.Routes) == 0 || len(snap.DNSServers) != 1 || snap.DNSServers[0].Address != "fe80::53" || len(snap.SearchDomains) != 0 {
		t.Fatal("independent DNS errors discarded valid router, prefix or DNS", snap)
	}
}

func TestCapacityAndDADFailuresAreExplicit(t *testing.T) {
	s := testState(t, testConfig())
	var opts [][]byte
	for i := 0; i <= MaxAddresses; i++ {
		raw := [16]byte{0x20, 0x01, 0x0d, 0xb8}
		binary.BigEndian.PutUint16(raw[6:8], uint16(i))
		opts = append(opts, pio(netip.PrefixFrom(netip.AddrFrom16(raw), 64).String(), 0xc0, 60, 30))
	}
	if s.handle(advert("fe80::2", 100, 0, opts...), testClock(0)) == nil || len(s.learned.Addresses)+len(s.learned.Routes) != 0 || s.learned.LastError == "" {
		t.Fatal("capacity failure was silently truncated or partially applied")
	}
	snap := mustHandle(t, s, advert("fe80::2", 30, 0, pio("2001:db8:1::/64", 0x40, 60, 30)), testClock(0))
	if !s.dadFailed(snap.Addresses[0].Address) {
		t.Fatal("DAD failure not observed")
	}
	snap = mustHandle(t, s, advert("fe80::2", 30, 0, pio("2001:db8:1::/64", 0x40, 60, 30)), testClock(1))
	if !snap.Addresses[0].DADFailed || snap.LastError == "" {
		t.Fatal("renewal cleared persistent DAD failure without identity change")
	}
}

func TestSnapshotScopeAndEUI64Validation(t *testing.T) {
	cfg := testConfig()
	cfg.InterfaceID = ""
	s := testState(t, cfg)
	snap := mustHandle(t, s, advert("fe80::2", 20, 0, pio("2001:db8:1::/64", 0xc0, 60, 30)), testClock(0))
	if snap.InterfaceID != "::ff:fe00:1" || snap.Addresses[0].Address != "2001:db8:1::ff:fe00:1/128" {
		t.Fatal("incorrect modified EUI-64 identity")
	}
	for _, mutation := range []func(*Snapshot){
		func(v *Snapshot) { v.Resource = "other" }, func(v *Snapshot) { v.SourceAddress = "2001:db8::1" },
		func(v *Snapshot) { v.Addresses[0].Address = "2001:db8:1::beef/128" }, func(v *Snapshot) { v.Addresses[0].Address = "2001:db8:1::ff:fe00:1/64" },
		func(v *Snapshot) { v.Addresses[0].PreferredUntil = lifetime.Deadline{} }, func(v *Snapshot) { v.Routes[0].Gateway = "2001:db8::1" },
		func(v *Snapshot) { v.Routes = append(v.Routes, v.Routes[0]) }, func(v *Snapshot) { v.UpdatedAt = time.Time{} },
	} {
		v := clone(snap)
		mutation(&v)
		if ValidateSnapshot(cfg, v) == nil {
			t.Fatal("out-of-scope snapshot accepted")
		}
	}
	no := false
	cfg.SLAAC = &no
	if ValidateSnapshot(cfg, snap) == nil {
		t.Fatal("disabled SLAAC facts accepted")
	}
}

func FuzzRAAtomicParsing(f *testing.F) {
	for _, p := range []packet{advert("fe80::2", 30, 0), advert("fe80::2", 30, 0, pio("2001:db8:1::/64", 0xc0, 60, 30), rdnss(30, "fe80::53"), dnssl(30)), advert("fe80::2", 0, 0, rio("::/0", 0x18, 60))} {
		f.Add(p.payload)
	}
	f.Fuzz(func(t *testing.T, b []byte) {
		if len(b) < 16 || len(b) > 65535 {
			return
		}
		b = append([]byte(nil), b...)
		b[0], b[1] = 134, 0
		p := packet{payload: b, source: netip.MustParseAddr("fe80::2"), destination: netip.MustParseAddr("ff02::1"), ifindex: 2, hopLimit: 255}
		recalc(&p)
		s := testState(t, testConfig())
		if err := s.handle(p, testClock(0)); err == nil {
			if err := ValidateSnapshot(s.config, s.snapshot(testClock(0))); err != nil {
				t.Fatalf("accepted RA yielded invalid state: %v", err)
			}
		} else if len(s.learned.Addresses)+len(s.learned.Routes)+len(s.learned.Prefixes)+len(s.learned.DNSServers)+len(s.learned.SearchDomains) != 0 {
			t.Fatal("rejected RA left partial state")
		}
	})
}

func TestDNSRepositoryPreservesWireOrderAndPromotesNewEntries(t *testing.T) {
	s := testState(t, testConfig())
	mustHandle(t, s, advert("fe80::2", 30, 0, rdnss(100, "fe80::1", "fe80::2")), testClock(0))
	snap := mustHandle(t, s, advert("fe80::3", 30, 0, rdnss(200, "fe80::3", "fe80::4"), rdnss(300, "fe80::5")), testClock(1))
	want := []string{"fe80::3", "fe80::4", "fe80::5", "fe80::1", "fe80::2"}
	for i, v := range want {
		if snap.DNSServers[i].Address != v {
			t.Fatalf("new server order: %+v", snap.DNSServers)
		}
	}
	// A refresh does not invert the repository or put its last address first.
	snap = mustHandle(t, s, advert("fe80::2", 30, 0, rdnss(400, "fe80::1", "fe80::2")), testClock(2))
	for i, v := range want {
		if snap.DNSServers[i].Address != v {
			t.Fatalf("existing refresh changed order: %+v", snap.DNSServers)
		}
	}
	snap = mustHandle(t, s, advert("fe80::4", 0, 0, rdnss(0, "fe80::4")), testClock(3))
	if len(snap.DNSServers) != 4 || snap.DNSServers[1].Address != "fe80::5" {
		t.Fatalf("withdrawal retained or reordered DNS: %+v", snap.DNSServers)
	}
	// Search domains follow the same repository order as DNS addresses.
	values := []SearchDomain{{Domain: "old.example"}, {Domain: "new.example"}, {Domain: "first.example"}}
	promoteDNS(values, []string{"first.example", "new.example"}, func(v SearchDomain) string { return v.Domain })
	if values[0].Domain != "first.example" || values[1].Domain != "new.example" || values[2].Domain != "old.example" {
		t.Fatalf("DNSSL order: %+v", values)
	}
}

func TestRepeatedRIOUpdatesRouteInWireOrder(t *testing.T) {
	s := testState(t, testConfig())
	mustHandle(t, s, advert("fe80::2", 30, 0, rio("2001:db8:3::/48", 0, 50)), testClock(0))
	p := advert("fe80::2", 0, 0, rio("2001:db8:3::/48", 0, 0), rio("2001:db8:3::/48", 8, 100))
	snap := mustHandle(t, s, p, testClock(1))
	if len(snap.Routes) != 1 || snap.Routes[0].Destination != "2001:db8:3::/48" || snap.Routes[0].Preference != 1 || snap.Routes[0].ValidUntil != lifetime.After(testClock(1), 100) {
		t.Fatal("repeated RIO discarded the RA or did not update the route", snap.Routes)
	}
}

func TestWallClockStepsPreserveOriginalBootLifetimes(t *testing.T) {
	s := testState(t, testConfig())
	mustHandle(t, s, advert("fe80::2", 30, 0, pio("2001:db8:1::/64", 0xc0, 60, 30)), testClock(0))
	for _, shift := range []time.Duration{-24 * time.Hour, 24 * time.Hour} {
		now := testClock(20)
		now.WallTime = now.WallTime.Add(shift)
		snap := s.snapshot(now)
		if snap.Addresses[0].ValidUntil.Remaining(now) != 40*time.Second || snap.Routes[0].ValidUntil.Remaining(now) != 10*time.Second {
			t.Fatal("wall step changed remaining protocol lifetime")
		}
	}
}

func TestStatusOnlyEventsKeepExactAbsoluteDeadlines(t *testing.T) {
	s := testState(t, testConfig())
	first := mustHandle(t, s, advert("fe80::2", 30, 0, pio("2001:db8:1::/64", 0xc0, 60, 50), rio("2001:db8:2::/48", 0, 40), rdnss(45, "fe80::53"), dnssl(46)), testClock(0))
	bad := advert("fe80::2", 0, 0)
	bad.hopLimit = 254
	for i, jitter := range []time.Duration{7521 * time.Nanosecond, -12 * time.Microsecond, 800 * time.Microsecond, -500 * time.Millisecond} {
		now := testClock(i + 1)
		now.WallTime = now.WallTime.Add(jitter)
		if err := s.handle(bad, now); err == nil {
			t.Fatal("bad-hop RA did not generate the expected rejection event")
		}
		s.learned.Solicitations++
		after := s.snapshot(now)
		if err := ValidateSnapshot(s.config, after); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(learnedData(first), learnedData(after)) || !after.UpdatedAt.Equal(now.WallTime) {
			t.Fatalf("status-only event/jitter %v changed absolute lifetime facts: first=%+v next=%+v", jitter, first, after)
		}
		if after.Received != 1 || after.Rejected != uint64(i+1) || after.Solicitations != uint64(i+1) || after.LastError == "" {
			t.Fatal("status publication lost the counters")
		}
	}
}
