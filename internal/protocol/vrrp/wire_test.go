package vrrp

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"net/netip"
	"strings"
	"testing"
)

func config4() Config {
	return Normalize(Config{Resource: "gateway4", Interface: "lan0", Family: "ipv4", VirtualRouterID: 42, Priority: 150, SourceAddress: "192.0.2.2", Addresses: []string{"192.0.2.1/24"}, AcceptMode: true})
}

func config6() Config {
	return Normalize(Config{Resource: "gateway6", Interface: "lan0", Family: "ipv6", VirtualRouterID: 7, Priority: 100, SourceAddress: "fe80::2", Addresses: []string{"fe80::1/64", "2001:db8::1/64"}, AcceptMode: true})
}

func TestRFC9568WireChecksumsAndVirtualMAC(t *testing.T) {
	fixtures := []struct {
		config Config
		wire   string
		mac    string
	}{
		// The IPv4 fixture's checksum is ~fold(0x312a+0x9601+0x0064+
		// 0xc000+0x0201) = 0x766e; it contains no IP pseudo-header.
		{config4(), "312a96010064766ec0000201", "00:00:5e:00:01:2a"},
		// This fixture additionally includes fe80::2 -> ff02::12 and protocol
		// 112 in the IPv6 pseudo-header. Independently calculated reference.
		{config6(), "3107640200644026fe80000000000000000000000000000120010db8000000000000000000000001", "00:00:5e:00:02:07"},
	}
	for _, fixture := range fixtures {
		t.Run(fixture.config.Family, func(t *testing.T) {
			c := fixture.config
			source := netip.MustParseAddr(c.SourceAddress)
			wire := marshalAdvertisement(c, source, multicastGroup(c.Family), uint8(c.Priority))
			if hex.EncodeToString(wire) != fixture.wire || VirtualMAC(c.Family, c.VirtualRouterID).String() != fixture.mac {
				t.Fatalf("wire mismatch: %x", wire)
			}
			frame := advertisementFrame(c, source, uint8(c.Priority))
			packet, err := parseFrame(c, frame)
			if err != nil || packet.source != source || int(packet.priority) != c.Priority || packet.interval != c.interval() || len(packet.addresses) != len(c.Addresses) {
				t.Fatalf("cannot receive reference advertisement: %+v %v", packet, err)
			}
		})
	}
}

func TestAcceptsKeepalivedIPv4ChecksumsIndependentOfTransmitChoice(t *testing.T) {
	c := config4()
	c.Addresses = []string{"192.0.2.254/32"}
	for _, sample := range []struct {
		transmit IPv4Checksum
		packet   string
	}{
		// Actual keepalived 2.2.8 advertisements captured on the unicast link:
		// default pseudo-header and explicit v3_checksum_as_v2, respectively.
		{ChecksumRFC9568, "312ac8010014bf3ec00002fe"},
		{ChecksumRFC5798, "312ac801001443c1c00002fe"},
	} {
		c.IPv4Checksum = sample.transmit
		payload, err := hex.DecodeString(sample.packet)
		if err != nil {
			t.Fatal(err)
		}
		p, err := parseAdvertisement(c, netip.MustParseAddr("192.0.2.3"), netip.MustParseAddr("192.0.2.2"), payload)
		if err != nil || p.priority != 200 {
			t.Fatalf("receive with transmit choice %s: %+v %v", sample.transmit, p, err)
		}
	}
}

func TestRejectsMalformedOrOffLinkDatagrams(t *testing.T) {
	for _, c := range []Config{config4(), config6()} {
		source := netip.MustParseAddr(c.SourceAddress)
		destination := multicastGroup(c.Family)
		p := datagram{source: source, destination: destination, hopLimit: 255, payload: marshalAdvertisement(c, source, destination, uint8(c.Priority))}
		for name, bad := range map[string]datagram{
			"hop":         {source: source, destination: destination, hopLimit: 254, payload: p.payload},
			"destination": {source: source, destination: source, hopLimit: 255, payload: p.payload},
			"source":      {source: netip.Addr{}, destination: destination, hopLimit: 255, payload: p.payload},
		} {
			if _, err := parseDatagram(c, source, bad); err == nil {
				t.Fatal(c.Family, name, "unsafe datagram accepted")
			}
		}
		for _, offset := range []int{0, 3, len(p.payload) - 1} {
			bad := p
			bad.payload = bytes.Clone(p.payload)
			bad.payload[offset] ^= 1
			if _, err := parseDatagram(c, source, bad); err == nil {
				t.Fatal("malformed VRRP payload accepted", c.Family, offset)
			}
		}
		for n := 0; n < len(p.payload); n++ {
			bad := p
			bad.payload = p.payload[:n]
			if _, err := parseDatagram(c, source, bad); err == nil {
				t.Fatal("truncated payload accepted", c.Family, n)
			}
		}
	}
}

func TestReservedBitsAcceptedAndDifferentVirtualRouterIgnored(t *testing.T) {
	c := config4()
	source := netip.MustParseAddr(c.SourceAddress)
	payload := marshalAdvertisement(c, source, multicastGroup(c.Family), 150)
	payload[4] |= 0xf0
	payload[6], payload[7] = 0, 0
	binary.BigEndian.PutUint16(payload[6:], checksum(payload))
	packet, err := parseAdvertisement(c, source, group4, payload)
	if err != nil || packet.interval != c.interval() {
		t.Fatalf("reserved bits were not ignored: %+v %v", packet, err)
	}
	payload[1]++
	if _, err := parseAdvertisement(c, source, group4, payload); !errors.Is(err, errOtherVirtualRouter) {
		t.Fatalf("another VRID was not distinguished: %v", err)
	}
	other := c
	other.Addresses = []string{"192.0.2.200/24"}
	if _, err := parseAdvertisement(c, source, group4, marshalAdvertisement(other, source, multicastGroup(other.Family), 150)); err == nil {
		t.Fatal("VRID collision with a different virtual address set was accepted")
	}
}

func TestGratuitousARPAndUnsolicitedNA(t *testing.T) {
	c := config4()
	frames := notificationFrames(c)
	if len(frames) != 1 || binary.BigEndian.Uint16(frames[0][12:14]) != 0x0806 || !bytes.Equal(frames[0][:6], bytes.Repeat([]byte{255}, 6)) {
		t.Fatalf("bad GARP frame: %x", frames)
	}
	arp := frames[0][14:]
	mac := VirtualMAC(c.Family, c.VirtualRouterID)
	if binary.BigEndian.Uint16(arp[6:8]) != 1 || !bytes.Equal(arp[8:14], mac) || !bytes.Equal(arp[18:24], mac) || !bytes.Equal(arp[14:18], arp[24:28]) {
		t.Fatalf("GARP did not advertise the virtual identity: %x", arp)
	}
	c = config6()
	frames = notificationFrames(c)
	if len(frames) != 2 {
		t.Fatal("NA not sent for every IPv6 virtual address")
	}
	for i, frame := range frames {
		ip, na := frame[14:54], frame[54:]
		address := c.addresses()[i]
		if ip[6] != 58 || ip[7] != 255 || na[0] != 136 || na[4] != 0xa0 || na[24] != 2 || na[25] != 1 || !bytes.Equal(na[26:], VirtualMAC(c.Family, c.VirtualRouterID)) {
			t.Fatalf("NA flags/options invalid: %x", frame)
		}
		if !bytes.Equal(ip[8:24], address.AsSlice()) || !bytes.Equal(na[8:24], address.AsSlice()) || checksum(pseudo6(address, all6, len(na), 58), na) != 0 {
			t.Fatal("NA source, target or checksum invalid")
		}
	}
}

func TestValidateRefusesAmbiguousOrUnsafeIdentity(t *testing.T) {
	for _, c := range []Config{config4(), config6()} {
		if err := Validate(c); err != nil {
			t.Fatal(err)
		}
		if len(Normalize(c).VIPInterface) > 15 {
			t.Fatal("generated virtual link name exceeds kernel limit")
		}
	}
	mutations := []func(*Config){
		func(c *Config) { c.Resource = "" }, func(c *Config) { c.Interface = "../wrong" },
		func(c *Config) { c.VIPInterface = c.Interface }, func(c *Config) { c.VirtualRouterID = 256 },
		func(c *Config) { c.Priority = 255 }, func(c *Config) { c.AddressOwner = true },
		func(c *Config) { c.Addresses = nil }, func(c *Config) { c.Addresses = []string{"192.0.2.1"} },
		func(c *Config) { c.Addresses = []string{"192.0.2.1/24", "192.0.2.1/32"} },
		func(c *Config) { c.SourceAddress = "0.0.0.0" }, func(c *Config) { c.AdvertIntervalCentiseconds = 4096 },
		func(c *Config) { c.EffectTimeoutMilliseconds = -1 }, func(c *Config) { c.Resource = strings.Repeat("x", 257) },
	}
	for i, change := range mutations {
		c := config4()
		change(&c)
		if err := Validate(c); err == nil {
			t.Errorf("invalid configuration %d accepted", i)
		}
	}
	c := config6()
	c.Addresses = []string{"2001:db8::1/64"}
	if err := Validate(c); err == nil {
		t.Fatal("IPv6 instance missing first link-local VIP accepted")
	}
}

// Capture helpers verify actual Ethernet frames in the native socket tests.
func advertisementFrame(c Config, source netip.Addr, priority uint8) []byte {
	destination := multicastGroup(c.Family)
	return datagramFrame(c.Family, c.VirtualRouterID, datagram{source: source, destination: destination, hopLimit: 255, payload: marshalAdvertisement(c, source, destination, priority)})
}

func parseFrame(c Config, frame []byte) (advertisement, error) {
	if len(frame) < 14 || !bytes.Equal(frame[:6], groupMAC(c.Family)) || !bytes.Equal(frame[6:12], VirtualMAC(c.Family, c.VirtualRouterID)) {
		return advertisement{}, errors.New("invalid VRRP Ethernet address")
	}
	ip := frame[14:]
	if c.Family == "ipv4" {
		if binary.BigEndian.Uint16(frame[12:]) != 0x0800 || len(ip) < 20 || ip[0]>>4 != 4 {
			return advertisement{}, errors.New("invalid VRRP IPv4 header")
		}
		ihl, length := int(ip[0]&15)*4, int(binary.BigEndian.Uint16(ip[2:4]))
		if ihl < 20 || ihl > len(ip) || length < ihl+8 || length > len(ip) || ip[8] != 255 || ip[9] != protocol || binary.BigEndian.Uint16(ip[6:8])&0x3fff != 0 {
			return advertisement{}, errors.New("invalid VRRP IPv4 size, TTL, protocol or fragmentation")
		}
		if checksum(ip[:ihl]) != 0 {
			return advertisement{}, errors.New("invalid IPv4 header checksum")
		}
		source := netip.AddrFrom4([4]byte(ip[12:16]))
		destination := netip.AddrFrom4([4]byte(ip[16:20]))
		if !addressInFamily(source, "ipv4") || destination != group4 {
			return advertisement{}, errors.New("invalid VRRP IPv4 source or destination")
		}
		return parseAdvertisement(c, source, destination, ip[ihl:length])
	}
	if binary.BigEndian.Uint16(frame[12:]) != 0x86dd || len(ip) < 48 || ip[0]>>4 != 6 || ip[6] != protocol || ip[7] != 255 {
		return advertisement{}, errors.New("invalid VRRP IPv6 header, hop limit or next header")
	}
	length := 40 + int(binary.BigEndian.Uint16(ip[4:6]))
	if length < 48 || length > len(ip) {
		return advertisement{}, errors.New("incomplete VRRP IPv6 payload")
	}
	source := netip.AddrFrom16([16]byte(ip[8:24]))
	destination := netip.AddrFrom16([16]byte(ip[24:40]))
	if !source.IsLinkLocalUnicast() || destination != group6 {
		return advertisement{}, errors.New("invalid VRRP IPv6 source or destination")
	}
	return parseAdvertisement(c, source, destination, ip[40:length])
}
