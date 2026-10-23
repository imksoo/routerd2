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
			wire := marshalAdvertisement(c, source, uint8(c.Priority))
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

func fix4(frame []byte) {
	ip := frame[14:]
	ip[10], ip[11] = 0, 0
	binary.BigEndian.PutUint16(ip[10:], checksum(ip[:int(ip[0]&15)*4]))
}

func TestRejectsMalformedOrOffLinkAdvertisements(t *testing.T) {
	for _, c := range []Config{config4(), config6()} {
		frame := advertisementFrame(c, netip.MustParseAddr(c.SourceAddress), uint8(c.Priority))
		offset := 14 + 20
		if c.Family == "ipv6" {
			offset = 14 + 40
		}
		mutations := map[string]func([]byte){
			"checksum": func(b []byte) { b[len(b)-1] ^= 1 },
			"version":  func(b []byte) { b[offset] = 0x21 },
			"type":     func(b []byte) { b[offset] = 0x32 },
			"count":    func(b []byte) { b[offset+3] = 0 },
			"mac":      func(b []byte) { b[8] ^= 1 },
		}
		if c.Family == "ipv4" {
			mutations["ttl"] = func(b []byte) { b[14+8] = 254; fix4(b) }
			mutations["fragment"] = func(b []byte) { b[14+6] = 0x20; fix4(b) }
			mutations["ip checksum"] = func(b []byte) { b[14+10] ^= 1 }
			mutations["destination"] = func(b []byte) { b[14+19] = 19; fix4(b) }
		} else {
			mutations["hop limit"] = func(b []byte) { b[14+7] = 254 }
			mutations["fragment"] = func(b []byte) { b[14+6] = 44 }
			mutations["non link local source"] = func(b []byte) { b[14+8] = 0x20 }
			mutations["destination"] = func(b []byte) { b[14+39] = 19 }
		}
		for name, mutate := range mutations {
			t.Run(c.Family+"/"+name, func(t *testing.T) {
				bad := append([]byte(nil), frame...)
				mutate(bad)
				if _, err := parseFrame(c, bad); err == nil {
					t.Fatal("unsafe advertisement accepted")
				}
			})
		}
		for length := 0; length < len(frame); length++ {
			if _, err := parseFrame(c, frame[:length]); err == nil {
				t.Fatalf("truncated %s frame accepted at %d bytes", c.Family, length)
			}
		}
	}
}

func TestReservedBitsAcceptedAndDifferentVirtualRouterIgnored(t *testing.T) {
	c := config4()
	source := netip.MustParseAddr(c.SourceAddress)
	payload := marshalAdvertisement(c, source, 150)
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
	if _, err := parseAdvertisement(c, source, group4, marshalAdvertisement(other, source, 150)); err == nil {
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
