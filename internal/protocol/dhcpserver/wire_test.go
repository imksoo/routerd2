package dhcpserver

import (
	"encoding/binary"
	"github.com/imksoo/routerd2/internal/ethernet"
	"net"
	"net/netip"
	"reflect"
	"testing"

	"golang.org/x/sys/unix"
)

func TestStrictRequestDecoderBoundsAndReservedFlags(t *testing.T) {
	base := requestPacket(msgDiscover, 42, testMAC, map[byte][]byte{61: testID}, "").Data
	for _, tc := range []struct {
		name string
		edit func([]byte) []byte
	}{
		{"short", func(b []byte) []byte { return b[:239] }},
		{"overlong", func(b []byte) []byte { return append(b, make([]byte, 4097-len(b))...) }},
		{"hardware-overrun", func(b []byte) []byte { b[2] = 255; return b }},
		{"not-client", func(b []byte) []byte { b[0] = 2; return b }},
		{"bad-cookie", func(b []byte) []byte { b[236] = 0; return b }},
		{"hops", func(b []byte) []byte { b[3] = 17; return b }},
		{"truncated-option", func(b []byte) []byte { return append(b[:len(b)-1], 12, 200, 1) }},
		{"no-end", func(b []byte) []byte { return b[:len(b)-1] }},
		{"duplicate-type", func(b []byte) []byte { return append(b[:len(b)-1], 53, 1, msgDiscover, 255) }},
		{"short-client-id", func(b []byte) []byte { return append(b[:240], 53, 1, 1, 61, 1, 1, 255) }},
		{"bad-maxsize", func(b []byte) []byte { return append(b[:len(b)-1], 57, 2, 0, 1, 255) }},
		{"bad-server-id", func(b []byte) []byte { return append(b[:len(b)-1], 54, 4, 0, 0, 0, 0, 255) }},
		{"multicast-chaddr", func(b []byte) []byte { b[28] = 1; return b }},
		{"server-field", func(b []byte) []byte { b[20] = 192; return b }},
		{"rapid-commit", func(b []byte) []byte { return append(b[:len(b)-1], 80, 0, 255) }},
		{"authentication", func(b []byte) []byte { return append(b[:len(b)-1], 90, 1, 0, 255) }},
		{"overload", func(b []byte) []byte { return append(b[:len(b)-1], 52, 1, 4, 255) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := tc.edit(append([]byte(nil), base...))
			if _, err := decodeRequest(b); err == nil {
				t.Fatal("invalid client packet accepted")
			}
		})
	}
	b := append([]byte(nil), base...)
	binary.BigEndian.PutUint16(b[10:12], 0xffff)
	m, err := decodeRequest(b)
	if err != nil || m.Flags != 0x8000 {
		t.Fatalf("reserved flags must be ignored: %v %#x", err, m.Flags)
	}
}
func TestOptionOverloadAndConcatenation(t *testing.T) {
	b := requestPacket(msgDiscover, 1, testMAC, nil, "").Data
	b = append(b[:len(b)-1], 52, 1, 3, 12, 3, 'h', 'o', 's', 255)
	copy(b[108:236], []byte{12, 1, 't', 61, 2, 1, 2, 255})
	copy(b[44:108], []byte{61, 2, 3, 4, 255})
	m, err := decodeRequest(b)
	if err != nil {
		t.Fatal(err)
	}
	if string(m.Options[12]) != "host" || !reflect.DeepEqual(m.Options[61], []byte{1, 2, 3, 4}) {
		t.Fatalf("%#v", m.Options)
	}
	copy(b[44:108], []byte{52, 1, 1, 255})
	if _, err := decodeRequest(b); err == nil {
		t.Fatal("nested overload accepted")
	}
}
func TestClientIdentifierEchoSizingAndClasslessRoutes(t *testing.T) {
	c := testConfig(t)
	req := requestPacket(msgDiscover, 1, testMAC, map[byte][]byte{61: testID, 55: {121, 3, 33, 6, 224}, 57: {5, 220}}, "")
	m, err := decodeRequest(req.Data)
	if err != nil {
		t.Fatal(err)
	}
	opts, err := replyOptions(c, c.Primary, nil, m, msgOffer, 60)
	if err != nil {
		t.Fatal(err)
	}
	opts[121] = []byte{0, 192, 0, 2, 1}
	opts[33] = []byte{198, 51, 100, 0, 192, 0, 2, 1}
	opts[224] = make([]byte, 600)
	b, err := encodeReply(m, msgOffer, netip.MustParseAddr("192.0.2.100"), netip.MustParseAddr(c.ServerAddress), opts, 1500)
	if err != nil {
		t.Fatal(err)
	}
	decoded := map[byte][]byte{}
	if err := decodeOptions(b[240:], decoded, false); err != nil {
		t.Fatal(err)
	}
	if len(decoded[224]) != 600 || string(decoded[61]) != string(testID) || len(decoded[121]) == 0 || len(decoded[3]) != 0 || len(decoded[33]) != 0 || len(b)+28 > 1500 {
		t.Fatalf("classless concatenation/sizing: %d %#v", len(b), decoded)
	}
	delete(m.Options, 57)
	m.Options[55] = []byte{121, 3}
	opts[121] = make([]byte, 600)
	b, err = encodeReply(m, msgOffer, netip.MustParseAddr("192.0.2.100"), netip.MustParseAddr(c.ServerAddress), opts, 1500)
	if err != nil {
		t.Fatal(err)
	}
	decoded = map[byte][]byte{}
	if err := decodeOptions(b[240:], decoded, false); err != nil {
		t.Fatal(err)
	}
	if len(b)+28 > 576 || len(decoded[121]) != 0 || len(decoded[3]) == 0 {
		t.Fatal("oversized classless option suppressed the usable router option")
	}
	m.Options[61] = make([]byte, 255)
	m.Options[61][0] = 1
	m.Options[82] = make([]byte, 255)
	opts, err = replyOptions(c, c.Primary, nil, m, msgNAK, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = encodeReply(m, msgNAK, zero4, netip.MustParseAddr(c.ServerAddress), opts, 1500); err == nil {
		t.Fatal("oversized mandatory echo silently truncated")
	}
}
func TestResponseDeliveryAndIndependentPacketChecksums(t *testing.T) {
	c := testConfig(t)
	req := requestPacket(msgDiscover, 7, testMAC, map[byte][]byte{61: testID}, "")
	binary.BigEndian.PutUint16(req.Data[10:12], 0)
	m, err := decodeRequest(req.Data)
	if err != nil {
		t.Fatal(err)
	}
	yi := netip.MustParseAddr("192.0.2.100")
	source := netip.MustParseAddr(c.ServerAddress)
	opts, _ := replyOptions(c, c.Primary, nil, m, msgOffer, 60)
	payload, err := encodeReply(m, msgOffer, yi, source, opts, 1500)
	if err != nil {
		t.Fatal(err)
	}
	r := makeResponse(m, msgOffer, payload, yi)
	if r.Routed || r.Destination != yi || string(r.MAC) != string(testMAC) {
		t.Fatal("pre-address unicast must use client Ethernet MAC")
	}
	serverMAC := net.HardwareAddr{2, 0, 0, 0, 0, 2}
	frames, err := ethernet.UDP4(serverMAC, r.MAC, source, yi, 67, 68, 5, 1500, payload)
	if err != nil {
		t.Fatal(err)
	}
	if len(frames) != 1 {
		t.Fatal("small reply was fragmented", len(frames))
	}
	frame := frames[0]
	if binary.BigEndian.Uint16(frame[12:14]) != unix.ETH_P_IP || binary.BigEndian.Uint16(frame[34:36]) != 67 || binary.BigEndian.Uint16(frame[36:38]) != 68 || binary.BigEndian.Uint16(frame[16:18]) != uint16(len(frame)-14) {
		t.Fatal("wrong packet lengths/ports")
	}
	// An independent folded-sum verifier includes the transmitted checksum.
	check := func(parts ...[]byte) bool {
		var sum uint64
		var pending byte
		odd := false
		for _, p := range parts {
			for _, v := range p {
				if !odd {
					pending = v
					odd = true
				} else {
					sum += uint64(pending)<<8 | uint64(v)
					odd = false
				}
			}
		}
		if odd {
			sum += uint64(pending) << 8
		}
		for sum>>16 != 0 {
			sum = (sum & 65535) + (sum >> 16)
		}
		return sum == 65535
	}
	pseudo := append([]byte(nil), frame[26:34]...)
	pseudo = append(pseudo, 0, 17, frame[38], frame[39])
	if !check(frame[14:34]) || !check(pseudo, frame[34:]) {
		t.Fatal("invalid IPv4 or UDP checksum")
	}
	r = makeResponse(m, msgNAK, payload, zero4)
	if r.Destination != broadcast4 || string(r.MAC) != string(broadcastMAC) {
		t.Fatal("NAK is not broadcast")
	}
	m.GIAddr = netip.MustParseAddr("192.0.2.2")
	b, err := encodeReply(m, msgNAK, zero4, source, map[byte][]byte{53: {6}, 54: source.AsSlice(), 61: testID}, 1500)
	if err != nil {
		t.Fatal(err)
	}
	if binary.BigEndian.Uint16(b[10:12])&0x8000 == 0 {
		t.Fatal("relayed NAK must set broadcast flag")
	}
}
func TestARPConflictRequiresAnotherStationClaim(t *testing.T) {
	server := net.HardwareAddr{2, 0, 0, 0, 0, 2}
	a := netip.MustParseAddr("192.0.2.100")
	frame := arpProbe(server, a)
	if arpConflict(frame, a, server, testMAC) {
		t.Fatal("server saw its own probe as conflict")
	}
	other := net.HardwareAddr{2, 0, 0, 0, 0, 99}
	copy(frame[22:28], other)
	if !arpConflict(frame, a, server, testMAC) {
		t.Fatal("another station's concurrent probe ignored")
	}
	binary.BigEndian.PutUint16(frame[20:22], 2)
	putAddress(frame[28:32], a)
	putAddress(frame[38:42], zero4)
	if !arpConflict(frame, a, server, testMAC) {
		t.Fatal("ARP reply claim ignored")
	}
	copy(frame[22:28], testMAC)
	if arpConflict(frame, a, server, testMAC) {
		t.Fatal("existing client's own IP was treated as conflict")
	}
}

func FuzzRequestDecoder(f *testing.F) {
	f.Add(requestPacket(msgDiscover, 1, testMAC, map[byte][]byte{61: testID}, "").Data)
	f.Add([]byte{1, 2, 3})
	f.Fuzz(func(t *testing.T, b []byte) { _, _ = decodeRequest(b) })
}
