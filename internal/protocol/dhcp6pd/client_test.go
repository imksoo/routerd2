//go:build linux

package dhcp6pd

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"net"
	"net/netip"
	"os"
	"testing"
	"time"

	"github.com/imksoo/routerd/pkg/pdclient"
)

type capture struct{ packets [][]byte }

func (*capture) SetWriteDeadline(time.Time) error { return nil }
func (c *capture) WriteToUDP(b []byte, _ *net.UDPAddr) (int, error) {
	c.packets = append(c.packets, append([]byte(nil), b...))
	return len(b), nil
}
func TestTransportRetransmissionAndConcurrentInformation(t *testing.T) {
	now := time.Unix(1700000000, 0)
	sock := &capture{}
	tr := &transport{conn: sock, interfaceName: "wan0", prefixLength: 60, now: func() time.Time { return now }}
	p := pdclient.OutboundPacket{Message: pdclient.Message{Type: pdclient.MessageRequest, TransactionID: 123, ClientDUID: []byte{0, 3, 0, 1, 2, 0, 0, 0, 0, 1}, ServerDUID: []byte{0, 3, 0, 1, 2, 0, 0, 0, 0, 2}}}
	if err := tr.Send(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	info := p
	info.Message.Type = pdclient.MessageInfoRequest
	info.Message.TransactionID = 456
	if err := tr.Send(context.Background(), info); err != nil {
		t.Fatal(err)
	}
	if tr.last.Message.TransactionID != 123 || tr.informationLast.Message.TransactionID != 456 {
		t.Fatal("information exchange replaced PD retransmission")
	}
	now = now.Add(2500 * time.Millisecond)
	if err := tr.Send(context.Background(), tr.last); err != nil {
		t.Fatal(err)
	}
	last := sock.packets[len(sock.packets)-1]
	if tr.requestCount != 2 {
		t.Fatalf("request retries reset: %d", tr.requestCount)
	}
	elapsed := -1
	oro := false
	for at := 4; at < len(last); {
		code := binary.BigEndian.Uint16(last[at:])
		n := int(binary.BigEndian.Uint16(last[at+2:]))
		at += 4
		if code == 8 {
			elapsed = int(binary.BigEndian.Uint16(last[at:]))
		}
		if code == 6 {
			oro = true
		}
		at += n
	}
	if elapsed != 250 || !oro {
		t.Fatalf("elapsed=%d oro=%v", elapsed, oro)
	}
}
func TestPDValidationLifetimeDefaultsAndIdentity(t *testing.T) {
	c := &pdclient.Client{Config: pdclient.Config{ClientDUID: []byte{0, 3, 0, 1, 2, 0, 0, 0, 0, 1}, IAID: 9}, State: pdclient.StateRequesting}
	m := pdclient.Message{Type: pdclient.MessageReply, TransactionID: 100, ClientDUID: c.Config.ClientDUID, ServerDUID: []byte{0, 3, 0, 1, 2, 0, 0, 0, 0, 2}, IAID: 9, Prefix: netip.MustParsePrefix("2001:db8:1200::/56"), Preferred: 100, Valid: 200}
	normal, ok := validReply(m, c, 0)
	if !ok || normal.T1 != 50 || normal.T2 != 80 {
		t.Fatalf("defaults %+v accepted=%v", normal, ok)
	}
	bad := m
	bad.IAID++
	if _, ok := validReply(bad, c, 0); ok {
		t.Fatal("foreign IAID accepted")
	}
	bad = m
	bad.ClientDUID = []byte{1}
	if _, ok := validReply(bad, c, 0); ok {
		t.Fatal("foreign DUID accepted")
	}
	bad = m
	bad.Preferred = 201
	if _, ok := validReply(bad, c, 0); ok {
		t.Fatal("preferred > valid accepted")
	}
	bad = m
	bad.T1 = 180
	bad.T2 = 100
	if _, ok := validReply(bad, c, 0); ok {
		t.Fatal("invalid renew/rebind timers accepted")
	}
	for _, timers := range [][2]uint32{{100, 100}, {200, 300}, {1, 0}, {0, 1}, {^uint32(0), ^uint32(0)}} {
		legal := m
		legal.T1, legal.T2 = timers[0], timers[1]
		if _, ok := validReply(legal, c, 0); !ok {
			t.Fatalf("legal server timers rejected: %v", timers)
		}
	}
	bad = m
	bad.Prefix = netip.MustParsePrefix("::/0")
	if _, ok := validReply(bad, c, 0); ok {
		t.Fatal("unspecified prefix accepted")
	}
	withdraw := m
	withdraw.Valid = 0
	withdraw.Preferred = 0
	if _, ok := validReply(withdraw, c, 0); !ok {
		t.Fatal("valid=0 withdrawal rejected")
	}
}

func TestInformationRefreshOptionAndStaleness(t *testing.T) {
	base := []byte{7, 0, 0, 1}
	encode := func(seconds uint32) []byte {
		b := append(append([]byte(nil), base...), 0, 32, 0, 4, 0, 0, 0, 0)
		binary.BigEndian.PutUint32(b[len(b)-4:], seconds)
		return b
	}
	for _, pair := range [][2]uint32{{1, 600}, {600, 600}, {7200, 7200}, {^uint32(0), ^uint32(0)}} {
		got, ok := informationRefresh(encode(pair[0]))
		if !ok || got != pair[1] {
			t.Fatalf("refresh %d = %d, %v", pair[0], got, ok)
		}
	}
	if got, ok := informationRefresh(base); !ok || got != 86400 {
		t.Fatal("missing refresh did not use RFC default")
	}
	if _, ok := informationRefresh(append(encode(600), 0, 32, 0, 4, 0, 0, 0, 1)); ok {
		t.Fatal("duplicate refresh option accepted")
	}
	if _, ok := informationRefresh([]byte{7, 0, 0, 1, 0, 32, 0, 3, 1, 2, 3}); ok {
		t.Fatal("malformed refresh option accepted")
	}
	now := time.Now()
	if !refreshAt(now, 600).Equal(now.Add(10*time.Minute)) || !refreshAt(now, ^uint32(0)).IsZero() {
		t.Fatal("refresh deadline/infinity wrong")
	}
	sock := &capture{}
	tr := &transport{conn: sock, interfaceName: "wan0", now: func() time.Time { return now }}
	p := pdclient.OutboundPacket{Message: pdclient.Message{Type: pdclient.MessageInfoRequest, TransactionID: 1, ClientDUID: []byte{0, 3, 0, 1}}}
	if err := tr.Send(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	found := false
	for options := sock.packets[0][4:]; len(options) > 0; {
		code, n := binary.BigEndian.Uint16(options), int(binary.BigEndian.Uint16(options[2:]))
		options = options[4:]
		if code == 6 {
			for at := 0; at < n; at += 2 {
				found = found || binary.BigEndian.Uint16(options[at:]) == 32
			}
		}
		options = options[n:]
	}
	if !found {
		t.Fatal("Information-request omitted required Information Refresh Time request")
	}
}
func TestRejectTopLevelAndNestedFailureStatus(t *testing.T) {
	base := []byte{7, 0, 0, 1}
	if !statusOK(base) {
		t.Fatal("empty option list invalid")
	}
	if statusOK(append(append([]byte(nil), base...), 0, 13, 0, 2, 0, 6)) {
		t.Fatal("NoPrefixAvail accepted")
	}
	pd := make([]byte, 12)
	pd = append(pd, 0, 13, 0, 2, 0, 3)
	b := append(append([]byte(nil), base...), 0, 25, 0, byte(len(pd)))
	b = append(b, pd...)
	if statusOK(b) {
		t.Fatal("nested NoBinding accepted")
	}
	if statusOK([]byte{7, 0, 0, 1, 0, 25, 0, 255}) {
		t.Fatal("truncated IA_PD accepted")
	}
}
func TestDurableIdentityAndExpiredRestore(t *testing.T) {
	now := time.Unix(1700000000, 0).UTC()
	duid := []byte{0, 3, 0, 1, 2, 0, 0, 0, 0, 1}
	c, err := pdclient.New(pdclient.Config{Resource: "pd", Interface: "wan0", ClientDUID: duid, IAID: 7, Now: func() time.Time { return now }}, nil)
	if err != nil {
		t.Fatal(err)
	}
	c.State = pdclient.StateBound
	c.Lease = pdclient.Lease{Prefix: netip.MustParsePrefix("2001:db8::/56"), ServerDUID: []byte{0, 3, 0, 1, 2, 0, 0, 0, 0, 2}, IAID: 7, T1: 40 * time.Second, T2: 80 * time.Second, Preferred: 90 * time.Second, Valid: 100 * time.Second, AcquiredAt: now}
	s := Snapshot{Snapshot: c.Snapshot(), Active: true}
	path := t.TempDir() + "/pd.json"
	if err := writeSnapshot(path, s); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var got Snapshot
	if err = json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if got.ClientDUID != s.ClientDUID || got.IAID != 7 || !got.ExpiresAt.Equal(now.Add(100*time.Second)) {
		t.Fatal("identity/lifetime not preserved")
	}
	now = now.Add(101 * time.Second)
	c.Restore(got.Snapshot)
	if c.Lease.Prefix.IsValid() {
		t.Fatal("expired prefix restored")
	}
	got.PreviousPrefixes = []pdclient.PreviousPrefixSnapshot{{Prefix: "2001:db8:100::/56", ExpiresAt: now.Add(90 * time.Second)}, {Prefix: "2001:db8:200::/56", ExpiresAt: now.Add(-time.Second)}}
	restorePrevious(c, got.Snapshot, now)
	if len(c.PreviousPrefixes) != 1 || c.PreviousPrefixes[0].Prefix.String() != "2001:db8:100::/56" {
		t.Fatal("restart lost still-valid previous prefix")
	}
}
