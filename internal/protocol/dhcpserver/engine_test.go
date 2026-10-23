package dhcpserver

import (
	"context"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	wire "github.com/imksoo/routerd/pkg/dhcpv4client"
	"github.com/imksoo/routerd2/internal/protocol/dhcpserverconfig"
)

var testMAC = net.HardwareAddr{2, 0, 0, 0, 0, 1}
var testID = []byte{1, 2, 0, 0, 0, 0, 1}

func TestManyOptionRoutersDoNotCauseFalsePoolExhaustion(t *testing.T) {
	c := testConfig(t)
	c.Primary.End = "192.0.2.120"
	var routers []string
	for i := 100; i < 120; i++ {
		routers = append(routers, fmt.Sprintf("192.0.2.%d", i))
	}
	c.Primary.Options = []dhcpserverconfig.Option{{Name: "router", Value: strings.Join(routers, ",")}}
	now := time.Now().UTC()
	e := newTestEngine(t, c, &now, nil)
	for i := 0; i < 64; i++ {
		address, ok := e.chooseAddress(c.Primary, nil, message{}, fmt.Sprintf("client-%d", i), now)
		if !ok || address.String() != "192.0.2.120" {
			t.Fatalf("client %d: a free address remains after option-router exclusions: %v %v", i, address, ok)
		}
	}
}

func testConfig(t *testing.T) Config {
	t.Helper()
	return Normalize(Config{Resource: "DHCPv4Server/test", Interface: "d4server", ServerAddress: "192.0.2.1", Primary: Scope{Prefix: "192.0.2.0/24", Start: "192.0.2.100", End: "192.0.2.102", LeaseSeconds: 60, Router: "192.0.2.1", DNSServers: []string{"192.0.2.1"}, Domain: "test.example"}, StatePath: filepath.Join(t.TempDir(), "private", "leases.json"), Authoritative: true})
}
func requestPacket(kind byte, xid uint32, mac net.HardwareAddr, options map[byte][]byte, ci string) packet {
	opts := map[byte][]byte{}
	for k, v := range options {
		opts[k] = append([]byte(nil), v...)
	}
	// Independent client fixture: unlike the original helper this does not
	// prepend a second implicit parameter request list.
	b := make([]byte, 240)
	b[0], b[1], b[2] = 1, 1, 6
	binary.BigEndian.PutUint32(b[4:8], xid)
	binary.BigEndian.PutUint16(b[10:12], 0x8000)
	copy(b[28:34], mac)
	copy(b[236:240], []byte{99, 130, 83, 99})
	b = append(b, 53, 1, kind)
	var codes []int
	for code := range opts {
		codes = append(codes, int(code))
	}
	sort.Ints(codes)
	for _, code := range codes {
		v := opts[byte(code)]
		b = append(b, byte(code), byte(len(v)))
		b = append(b, v...)
	}
	b = append(b, 255)
	source, dest := zero4, broadcast4
	if ci != "" {
		source = netip.MustParseAddr(ci)
		dest = netip.MustParseAddr("192.0.2.1")
		copy(b[12:16], source.AsSlice())
		binary.BigEndian.PutUint16(b[10:12], 0)
	}
	return packet{Data: b, Source: source, SourcePort: 68, Destination: dest, InterfaceIndex: 1}
}
func responseMessage(t *testing.T, r *response, typ byte) wire.Message {
	t.Helper()
	if r == nil {
		t.Fatal("missing DHCP response")
	}
	m, err := wire.Decode(r.Data)
	if err != nil || m.Op != 2 || m.MessageType() != typ {
		t.Fatalf("response: %+v %v", m, err)
	}
	return m
}
func newTestEngine(t *testing.T, c Config, now *time.Time, persist func(journal) error) *engine {
	t.Helper()
	if err := Validate(c); err != nil {
		t.Fatal(err)
	}
	if persist == nil {
		persist = func(journal) error { return nil }
	}
	e := newEngine(c, journal{}, func() time.Time { return *now }, persist)
	e.probe = func(context.Context, netip.Addr, net.HardwareAddr) (bool, error) { return false, nil }
	return e
}
func acquire(t *testing.T, e *engine, mac net.HardwareAddr, id []byte, xid uint32) (Lease, *response) {
	t.Helper()
	opts := map[byte][]byte{61: id, 12: []byte("client"), 55: {1, 3, 6, 15, 119}}
	if len(id) == 0 {
		delete(opts, 61)
	}
	o, err := e.handle(context.Background(), requestPacket(msgDiscover, xid, mac, opts, ""))
	if err != nil {
		t.Fatal(err)
	}
	offer := responseMessage(t, o, msgOffer)
	opts[50] = offer.YIAddr.AsSlice()
	opts[54] = offer.Options[54]
	r, err := e.handle(context.Background(), requestPacket(msgRequest, xid, mac, opts, ""))
	if err != nil {
		t.Fatal(err)
	}
	ack := responseMessage(t, r, msgACK)
	if ack.YIAddr != offer.YIAddr {
		t.Fatal("ACK differs from offer")
	}
	s := e.snapshot("0123456789abcdef0123456789abcdef", 1, e.now(), true)
	if err := dhcpserverconfig.ValidateSnapshot(e.config, s); err != nil {
		t.Fatal(err)
	}
	for _, l := range s.Leases {
		if l.IPAddress == ack.YIAddr.String() {
			return l, r
		}
	}
	t.Fatal("ACK has no published durable lease")
	return Lease{}, nil
}

func TestDORAAndRenewPersistBeforeReturningACK(t *testing.T) {
	c := testConfig(t)
	now := time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)
	var records []journal
	e := newTestEngine(t, c, &now, func(j journal) error { records = append(records, j); return nil })
	probes := 0
	e.probe = func(context.Context, netip.Addr, net.HardwareAddr) (bool, error) { probes++; return false, nil }
	l, ack := acquire(t, e, testMAC, testID, 42)
	if len(records) != 1 || len(records[0].Leases) != 1 || probes != 1 {
		t.Fatalf("journal/probes: %d %d", len(records), probes)
	}
	m := responseMessage(t, ack, msgACK)
	if string(m.Options[61]) != string(testID) || binary.BigEndian.Uint32(m.Options[51]) != 60 || binary.BigEndian.Uint32(m.Options[58]) != 30 || binary.BigEndian.Uint32(m.Options[59]) != 52 {
		t.Fatalf("bad ACK options: %#v", m.Options)
	}
	now = now.Add(31 * time.Second)
	r, err := e.handle(context.Background(), requestPacket(msgRequest, 43, testMAC, map[byte][]byte{61: testID}, l.IPAddress))
	if err != nil {
		t.Fatal(err)
	}
	renew := responseMessage(t, r, msgACK)
	if !r.Routed || r.Destination.String() != l.IPAddress || renew.CIAddr.String() != l.IPAddress || probes != 1 || len(records) != 2 {
		t.Fatalf("renew: %+v", r)
	}
	if got := e.leases[leaseKey(l)]; got.AcquiredAt != l.AcquiredAt || !got.RenewedAt.Equal(now) || !got.ExpiresAt.Equal(now.Add(time.Minute)) {
		t.Fatalf("renewed lifetime: %+v", got)
	}
}

func TestUnknownInitRebootAndOtherServerDoNotAllocate(t *testing.T) {
	c := testConfig(t)
	now := time.Now().UTC()
	writes := 0
	e := newTestEngine(t, c, &now, func(journal) error { writes++; return nil })
	p := requestPacket(msgRequest, 1, testMAC, map[byte][]byte{61: testID, 50: {192, 0, 2, 101}}, "")
	if r, err := e.handle(context.Background(), p); err != nil || r != nil {
		t.Fatalf("unknown same-subnet INIT-REBOOT must be silent: %v %+v", err, r)
	}
	p = requestPacket(msgRequest, 2, testMAC, map[byte][]byte{61: testID, 50: {198, 51, 100, 9}}, "")
	r, err := e.handle(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	responseMessage(t, r, msgNAK)
	if r.Destination != broadcast4 {
		t.Fatal("NAK must broadcast")
	}
	o, err := e.handle(context.Background(), requestPacket(msgDiscover, 3, testMAC, map[byte][]byte{61: testID}, ""))
	if err != nil {
		t.Fatal(err)
	}
	offer := responseMessage(t, o, msgOffer)
	if len(e.offers) != 1 {
		t.Fatal("offer not held")
	}
	p = requestPacket(msgRequest, 3, testMAC, map[byte][]byte{61: testID, 50: offer.YIAddr.AsSlice(), 54: {192, 0, 2, 2}}, "")
	if r, err := e.handle(context.Background(), p); err != nil || r != nil || len(e.offers) != 0 || writes != 0 {
		t.Fatalf("other server selection: %v %+v", err, r)
	}
}

func TestReleaseAndDeclineAreIdentityCheckedAndDurable(t *testing.T) {
	c := testConfig(t)
	now := time.Now().UTC()
	writes := 0
	e := newTestEngine(t, c, &now, func(journal) error { writes++; return nil })
	l, _ := acquire(t, e, testMAC, testID, 1)
	now = now.Add(time.Second)
	for _, p := range []packet{
		requestPacket(msgRelease, 2, testMAC, map[byte][]byte{61: {1, 2, 3}, 54: {192, 0, 2, 1}}, l.IPAddress),
		requestPacket(msgDecline, 2, testMAC, map[byte][]byte{61: {1, 2, 3}, 54: {192, 0, 2, 1}, 50: netip.MustParseAddr(l.IPAddress).AsSlice()}, ""),
	} {
		r, err := e.handle(context.Background(), p)
		if err != nil || r != nil || len(e.leases) != 1 || writes != 1 {
			t.Fatal("foreign client changed binding", err)
		}
	}
	r, err := e.handle(context.Background(), requestPacket(msgRelease, 2, testMAC, map[byte][]byte{61: testID, 54: {192, 0, 2, 1}}, l.IPAddress))
	if err != nil || r != nil || len(e.leases) != 0 || writes != 2 {
		t.Fatalf("release: %v leases=%d writes=%d", err, len(e.leases), writes)
	}
	l, _ = acquire(t, e, testMAC, testID, 3)
	p := requestPacket(msgDecline, 3, testMAC, map[byte][]byte{61: testID, 54: {192, 0, 2, 1}, 50: netip.MustParseAddr(l.IPAddress).AsSlice()}, "")
	r, err = e.handle(context.Background(), p)
	if err != nil || r != nil || len(e.leases) != 0 || e.quarantines[l.IPAddress].Until.IsZero() || writes != 4 {
		t.Fatalf("decline: %v %+v", err, e.quarantines)
	}
	next, _ := acquire(t, e, testMAC, testID, 4)
	if next.IPAddress == l.IPAddress {
		t.Fatal("quarantined address reused")
	}
	now = now.Add(time.Duration(c.DeclineSeconds)*time.Second + time.Second)
	if err := e.sweep(now); err != nil || len(e.quarantines) != 0 {
		t.Fatal("quarantine did not expire", err)
	}
}

func TestReservationsScopesAndStickyHold(t *testing.T) {
	c := testConfig(t)
	c.StickyHoldDays = 1
	s := c.Primary
	s.Name = "voice"
	s.Start = "192.0.2.150"
	s.End = "192.0.2.152"
	s.LeaseSeconds = 120
	s.Match = dhcpserverconfig.Match{OUIPrefixes: []string{"02:aa:bb"}}
	c.Scopes = []Scope{s}
	reservedMAC := net.HardwareAddr{2, 0xaa, 0xbb, 0, 0, 1}
	c.Reservations = []Reservation{{MACAddress: reservedMAC.String(), IPAddress: "192.0.2.20", Hostname: "phone", LeaseSeconds: 180, Options: []dhcpserverconfig.Option{{Name: "mtu", Value: "1400"}}}}
	now := time.Now().UTC()
	e := newTestEngine(t, c, &now, nil)
	l, r := acquire(t, e, reservedMAC, nil, 1)
	if l.IPAddress != "192.0.2.20" || l.Scope != "voice" || !l.Reserved || l.Hostname != "phone" || !l.ExpiresAt.Equal(now.Add(180*time.Second)) {
		t.Fatalf("%+v", l)
	}
	m := responseMessage(t, r, msgACK)
	// Optional MTU is absent when the client's parameter request list did not
	// request it; reservation options are not forced into every client.
	if _, ok := m.Options[26]; ok {
		t.Fatal("unexpected nonrequested MTU")
	}
	dynamic, _ := acquire(t, e, testMAC, testID, 2)
	now = now.Add(61 * time.Second)
	if err := e.sweep(now); err != nil {
		t.Fatal(err)
	}
	if len(e.leases) != 2 || e.stats.Expired != 1 {
		t.Fatalf("sticky record not retained: %+v", e.leases)
	}
	other := net.HardwareAddr{2, 0, 0, 0, 0, 2}
	l2, _ := acquire(t, e, other, []byte{1, 2, 0, 0, 0, 0, 2}, 3)
	if l2.IPAddress == dynamic.IPAddress {
		t.Fatal("sticky address assigned to another client")
	}
	again, _ := acquire(t, e, testMAC, testID, 4)
	if again.IPAddress != dynamic.IPAddress {
		t.Fatal("sticky address not reused by owner")
	}
}

func TestJournalFailureReturnsNoACKAndConflictIsQuarantined(t *testing.T) {
	c := testConfig(t)
	now := time.Now().UTC()
	e := newTestEngine(t, c, &now, func(journal) error { return errors.New("disk full") })
	o, err := e.handle(context.Background(), requestPacket(msgDiscover, 1, testMAC, map[byte][]byte{61: testID}, ""))
	if err != nil {
		t.Fatal(err)
	}
	offer := responseMessage(t, o, msgOffer)
	r, err := e.handle(context.Background(), requestPacket(msgRequest, 1, testMAC, map[byte][]byte{61: testID, 50: offer.YIAddr.AsSlice(), 54: {192, 0, 2, 1}}, ""))
	if err == nil || r != nil {
		t.Fatal("ACK returned before durable storage")
	}
	var saved journal
	e = newTestEngine(t, c, &now, func(j journal) error { saved = j; return nil })
	e.probe = func(context.Context, netip.Addr, net.HardwareAddr) (bool, error) { return true, nil }
	r, err = e.handle(context.Background(), requestPacket(msgDiscover, 2, testMAC, map[byte][]byte{61: testID}, ""))
	if err != nil || r != nil || len(saved.Quarantines) != 1 || e.stats.Conflicts != 1 {
		t.Fatalf("conflict was offered: %v %+v", err, r)
	}
}

func TestRelayAllowlistAndINFORM(t *testing.T) {
	c := testConfig(t)
	now := time.Now().UTC()
	e := newTestEngine(t, c, &now, nil)
	p := requestPacket(msgDiscover, 1, testMAC, map[byte][]byte{61: testID, 82: {1, 3, 'a', 'b', 'c'}}, "")
	p.Source = netip.MustParseAddr("192.0.2.2")
	p.SourcePort = 67
	p.Destination = netip.MustParseAddr(c.ServerAddress)
	copy(p.Data[24:28], p.Source.AsSlice())
	if r, err := e.handle(context.Background(), p); err != nil || r != nil {
		t.Fatal("untrusted relay accepted", err)
	}
	e.config.TrustedRelays = []string{"192.0.2.2"}
	e.probe = func(context.Context, netip.Addr, net.HardwareAddr) (bool, error) {
		t.Error("relayed pool must not ARP-probe the local link")
		return false, nil
	}
	r, err := e.handle(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	offer := responseMessage(t, r, msgOffer)
	if !r.Routed || r.Port != 67 || r.Destination != p.Source || string(offer.Options[82]) != string([]byte{1, 3, 'a', 'b', 'c'}) {
		t.Fatalf("relay response %+v", r)
	}
	e.probe = func(context.Context, netip.Addr, net.HardwareAddr) (bool, error) { return false, nil }
	p = requestPacket(msgInform, 2, testMAC, map[byte][]byte{61: testID, 55: {1, 3, 6}}, "192.0.2.50")
	r, err = e.handle(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	ack := responseMessage(t, r, msgACK)
	if _, ok := ack.Options[51]; ok || !ack.YIAddr.IsUnspecified() || len(e.leases) != 0 {
		t.Fatal("INFORM allocated a lease")
	}
}

func TestChangedIntentRetainsAllocationExclusionButNotDNSOwnership(t *testing.T) {
	c := testConfig(t)
	now := time.Now().UTC()
	e := newTestEngine(t, c, &now, nil)
	l, _ := acquire(t, e, testMAC, testID, 1)
	e.config.Reservations = []Reservation{{MACAddress: "02:00:00:00:00:99", IPAddress: l.IPAddress, Hostname: "new-owner"}}
	s := e.snapshot("0123456789abcdef0123456789abcdef", 2, now, true)
	if len(s.Leases) != 0 || len(e.leases) != 1 {
		t.Fatal("old allocation must remain excluded but not claim new DNS intent")
	}
	mac, _ := net.ParseMAC("02:00:00:00:00:99")
	r, err := e.handle(context.Background(), requestPacket(msgDiscover, 2, mac, nil, ""))
	if err != nil || r != nil {
		t.Fatal("unexpired address stolen by new reservation", err)
	}
	if e.leases[leaseKey(l)].ClientID != hex.EncodeToString(testID) {
		t.Fatal("original lease identity lost")
	}
}

func TestOptionRoutersCannotBeAllocatedFromDynamicPool(t *testing.T) {
	c := testConfig(t)
	c.Primary.Options = []dhcpserverconfig.Option{{Name: "router", Value: "192.0.2.100,192.0.2.101"}}
	now := time.Now().UTC()
	e := newTestEngine(t, c, &now, nil)
	l, _ := acquire(t, e, testMAC, testID, 1)
	if l.IPAddress != "192.0.2.102" {
		t.Fatalf("configured router address allocated: %+v", l)
	}
}
