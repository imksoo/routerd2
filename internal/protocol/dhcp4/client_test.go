//go:build linux

package dhcp4

import (
	"context"
	"encoding/binary"
	"errors"
	"net"
	"net/netip"
	"testing"
	"time"

	wire "github.com/imksoo/routerd/pkg/dhcpv4client"
	"github.com/imksoo/routerd2/internal/protocol/bfd"
)

type sent struct {
	data []byte
	to   net.IP
}
type fakeConn struct{ sent []sent }

func (f *fakeConn) ReadFromUDP([]byte) (int, *net.UDPAddr, error) {
	panic("unit test must not read a socket")
}
func (f *fakeConn) WriteToUDP(b []byte, a *net.UDPAddr) (int, error) {
	f.sent = append(f.sent, sent{append([]byte(nil), b...), append(net.IP(nil), a.IP...)})
	return len(b), nil
}
func (*fakeConn) SetReadDeadline(time.Time) error { return nil }
func (*fakeConn) Close() error                    { return nil }
func fixture(t *testing.T) (*client, *fakeConn, *time.Time, *[]Snapshot) {
	t.Helper()
	now := time.Unix(1700000000, 0).UTC()
	origin := now
	conn := &fakeConn{}
	var updates []Snapshot
	x := uint32(100)
	c := &client{config: Config{Resource: "wan", Interface: "wan0", StatePath: t.TempDir() + "/lease.json"}, hardware: net.HardwareAddr{2, 0, 0, 0, 0, 1}, conn: conn, readClock: func() (bfd.Clock, error) {
		return bfd.Clock{BootID: "11111111-2222-4333-8444-555555555555", BoottimeNanoseconds: int64(time.Hour + now.Sub(origin)), WallTime: now}, nil
	}, xid: func() uint32 { x++; return x }, persist: func(Snapshot) error { return nil }, effects: EffectsFunc(func(_ context.Context, s Snapshot) error { updates = append(updates, s); return nil })}
	return c, conn, &now, &updates
}
func reply(c *client, typ byte) []byte {
	u32 := func(v uint32) []byte { b := make([]byte, 4); binary.BigEndian.PutUint32(b, v); return b }
	opts := map[byte][]byte{wire.OptionServerID: {192, 0, 2, 1}, wire.OptionSubnetMask: {255, 255, 255, 0}, wire.OptionRouter: {192, 0, 2, 1}, wire.OptionDNSServer: {192, 0, 2, 53}, wire.OptionNTPServer: {192, 0, 2, 123}, wire.OptionLeaseTime: u32(100), wire.OptionT1: u32(40), wire.OptionT2: u32(80)}
	b := wire.EncodeRequest(typ, c.transaction, c.hardware, opts)
	b[0] = 2
	copy(b[16:20], []byte{192, 0, 2, 9})
	return b
}
func bind(t *testing.T, c *client) {
	t.Helper()
	ctx := context.Background()
	if err := c.start(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if err := c.handle(ctx, reply(c, wire.MessageOffer)); err != nil {
		t.Fatal(err)
	}
	if err := c.handle(ctx, reply(c, wire.MessageACK)); err != nil {
		t.Fatal(err)
	}
}
func TestLeaseRenewRebindExpireAndWithdraw(t *testing.T) {
	c, conn, now, updates := fixture(t)
	bind(t, c)
	if !c.snapshot().Active || c.snapshot().DefaultGateway != "192.0.2.1" || c.snapshot().DNSServers[0] != "192.0.2.53" || c.snapshot().NTPServers[0] != "192.0.2.123" {
		t.Fatalf("bad lease %+v", c.snapshot())
	}
	*now = now.Add(40 * time.Second)
	if err := c.tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	last := conn.sent[len(conn.sent)-1]
	m, err := wire.Decode(last.data)
	if err != nil {
		t.Fatal(err)
	}
	if c.state != wire.StateRenewing || !c.snapshot().Active || !last.to.Equal(net.ParseIP("192.0.2.1")) || m.CIAddr != netip.MustParseAddr("192.0.2.9") || len(m.Options[wire.OptionRequestedIP]) != 0 || len(m.Options[wire.OptionServerID]) != 0 || m.Flags != 0 {
		t.Fatalf("invalid renewal %+v %+v", m, last)
	}
	*now = now.Add(40 * time.Second)
	if err := c.tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if c.state != wire.StateRebinding || !conn.sent[len(conn.sent)-1].to.Equal(net.IPv4bcast) {
		t.Fatal("T2 did not rebind by broadcast")
	}
	*now = now.Add(20 * time.Second)
	if err := c.tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if c.snapshot().Active || c.lease.Address.IsValid() || c.state != wire.StateDiscovering {
		t.Fatalf("expired lease still usable %+v", c.snapshot())
	}
	found := false
	for _, s := range *updates {
		if s.State == wire.StateExpired && !s.Active && s.CurrentAddress == "" {
			found = true
		}
	}
	if !found {
		t.Fatal("no explicit expiry withdrawal")
	}
}
func TestRestartRequiresMatchingMACAndACK(t *testing.T) {
	c, _, _, _ := fixture(t)
	bind(t, c)
	s := c.snapshot()
	fresh, conn, _, _ := fixture(t)
	if err := fresh.start(context.Background(), &s); err != nil {
		t.Fatal(err)
	}
	m, err := wire.Decode(conn.sent[0].data)
	if err != nil {
		t.Fatal(err)
	}
	if fresh.snapshot().Active || m.MessageType() != wire.MessageRequest || len(m.Options[wire.OptionServerID]) != 0 || net.IP(m.Options[wire.OptionRequestedIP]).String() != s.CurrentAddress {
		t.Fatal("restart did not enter INIT-REBOOT")
	}
	// A second crash before the ACK must retain the server-issued lease
	// identity even though the last published candidate is inactive.
	pending := fresh.snapshot()
	again, againConn, _, _ := fixture(t)
	if err := again.start(context.Background(), &pending); err != nil {
		t.Fatal(err)
	}
	retry, _ := wire.Decode(againConn.sent[0].data)
	if retry.MessageType() != wire.MessageRequest || again.snapshot().Active {
		t.Fatal("second restart forgot INIT-REBOOT lease or installed it without ACK")
	}
	if err := fresh.handle(context.Background(), reply(fresh, wire.MessageACK)); err != nil {
		t.Fatal(err)
	}
	if !fresh.snapshot().Active {
		t.Fatal("reboot ACK did not restore lease")
	}
	foreign, conn2, _, _ := fixture(t)
	foreign.hardware = net.HardwareAddr{2, 0, 0, 0, 0, 2}
	if err := foreign.start(context.Background(), &s); err != nil {
		t.Fatal(err)
	}
	m, _ = wire.Decode(conn2.sent[0].data)
	if m.MessageType() != wire.MessageDiscover {
		t.Fatal("adopted foreign MAC lease")
	}
}

func TestConcatenatedAndOverloadedOptions(t *testing.T) {
	c, _, _, _ := fixture(t)
	b := reply(c, wire.MessageOffer)
	// RFC 3396 concatenation preserves both DNS addresses.
	b = append(b[:len(b)-1], wire.OptionDNSServer, 4, 192, 0, 2, 54, 255)
	m, err := decode(b)
	if err != nil || len(m.Options[wire.OptionDNSServer]) != 8 {
		t.Fatalf("fragmented option lost: %v %+v", err, m.Options)
	}
	// File/sname overload order follows the normal options field.
	b = append(b[:len(b)-1], 52, 1, 3, 255)
	copy(b[108:236], []byte{wire.OptionDNSServer, 4, 192, 0, 2, 55, 255})
	copy(b[44:108], []byte{wire.OptionDNSServer, 4, 192, 0, 2, 56, 255})
	m, err = decode(b)
	if err != nil || len(m.Options[wire.OptionDNSServer]) != 16 {
		t.Fatalf("overloaded options lost: %v %+v", err, m.Options)
	}
	bad := append(append([]byte(nil), b[:len(b)-1]...), wire.OptionServerID, 4, 192, 0, 2, 77, 255)
	if _, err := decode(bad); err == nil {
		t.Fatal("duplicate singleton server identity accepted")
	}
	bad = append(append([]byte(nil), b[:len(b)-1]...), wire.OptionT1, 1, 1, 255)
	if _, err := decode(bad); err != nil {
		t.Fatal("unusable optional timer discarded an otherwise usable message", err)
	}
}
func TestOptionalDataDoesNotDiscardUsableACK(t *testing.T) {
	c, _, _, _ := fixture(t)
	if err := c.start(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	if err := c.handle(context.Background(), reply(c, wire.MessageOffer)); err != nil {
		t.Fatal(err)
	}
	ack := reply(c, wire.MessageACK)
	ack = append(ack[:len(ack)-1], wire.OptionT1, 1, 1, wire.OptionDNSServer, 1, 255, wire.OptionMTU, 1, 255, wire.OptionEnd)
	if err := c.handle(context.Background(), ack); err != nil {
		t.Fatal(err)
	}
	if c.state != wire.StateBound || c.lease.T1 != c.lease.LeaseTime/2 || c.lease.MTU != 0 || len(c.lease.DNSServers) != 1 || c.lease.DNSServers[0].String() != "192.0.2.53" {
		t.Fatal("independent optional data lost the address, standard timer or complete DNS entry", c.state, c.lease)
	}
}

func TestRejectForeignAndMalformedReplies(t *testing.T) {
	c, _, _, _ := fixture(t)
	if err := c.start(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	bad := reply(c, wire.MessageOffer)
	bad[28]++
	_ = c.handle(context.Background(), bad)
	if c.state != wire.StateDiscovering {
		t.Fatal("foreign MAC accepted")
	}
	bad = reply(c, wire.MessageOffer)
	bad[2] = 255
	_ = c.handle(context.Background(), bad)
	bad = reply(c, wire.MessageACK)
	_ = c.handle(context.Background(), bad)
	if c.state != wire.StateDiscovering {
		t.Fatal("unsolicited ACK accepted")
	}
	bad = reply(c, wire.MessageOffer)
	bad = append(bad[:len(bad)-1], 99, 10, 1)
	if _, err := decode(bad); err == nil {
		t.Fatal("truncated option accepted")
	}
	for size := 0; size < 80; size++ {
		f := make([]byte, size)
		if size > 14 {
			f[12] = 8
			f[14] = 0x45
		}
		(&linuxDHCPv4PacketConn{}).dhcpPayload(f)
	}
}
func TestEffectFailureIsReturned(t *testing.T) {
	c, _, _, _ := fixture(t)
	want := errors.New("kernel compiler failed")
	c.effects = EffectsFunc(func(context.Context, Snapshot) error { return want })
	if err := c.start(context.Background(), nil); !errors.Is(err, want) {
		t.Fatalf("lost effect error: %v", err)
	}
}

func TestInitialFrameWorksWithoutIPv4RouteAndChecksums(t *testing.T) {
	c, _, _, _ := fixture(t)
	payload := reply(c, wire.MessageOffer)
	frame := initialFrame(c.hardware, payload)
	if checksum(frame[14:34]) != 0 || binary.BigEndian.Uint32(frame[26:30]) != 0 || !net.IP(frame[30:34]).Equal(net.IPv4bcast) {
		t.Fatal("incorrect initial IP header")
	}
	if binary.BigEndian.Uint16(frame[34:36]) != 68 || binary.BigEndian.Uint16(frame[36:38]) != 67 {
		t.Fatal("incorrect initial DHCP ports")
	}
	// Convert into a server reply to verify the AF_PACKET receive path.
	copy(frame[6:12], []byte{2, 0, 0, 0, 0, 2})
	binary.BigEndian.PutUint16(frame[34:36], 67)
	binary.BigEndian.PutUint16(frame[36:38], 68)
	reader := &linuxDHCPv4PacketConn{hwaddr: c.hardware}
	got, _ := reader.dhcpPayload(frame)
	if len(got) != len(payload) {
		t.Fatal("valid packet rejected")
	}
	binary.BigEndian.PutUint16(frame[40:42], 1)
	if p, _ := reader.dhcpPayload(frame); len(p) != 0 {
		t.Fatal("bad UDP checksum accepted without kernel auxdata")
	}
	if p, _ := reader.dhcpPayloadWithChecksumState(frame, true); len(p) == 0 {
		t.Fatal("kernel CHECKSUM_PARTIAL packet rejected")
	}
	frame[24] ^= 1
	if p, _ := reader.dhcpPayloadWithChecksumState(frame, true); len(p) != 0 {
		t.Fatal("bad IP checksum accepted")
	}
}
