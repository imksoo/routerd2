package vrrp

import (
	"net/netip"
	"testing"
)

func TestUnicastUsesActualDestinationAndOnlyConfiguredPeers(t *testing.T) {
	for _, c := range []Config{config4(), config6()} {
		local := netip.MustParseAddr(c.SourceAddress)
		peer, stranger := netip.MustParseAddr("192.0.2.3"), netip.MustParseAddr("192.0.2.4")
		if c.Family == "ipv6" {
			local, peer, stranger = netip.MustParseAddr("2001:db8::2"), netip.MustParseAddr("2001:db8::3"), netip.MustParseAddr("2001:db8::4")
			c.SourceAddress = local.String()
		}
		c.UnicastPeers = []netip.Addr{peer}
		if err := Validate(c); err != nil {
			t.Fatal(err)
		}
		p := datagram{source: peer, destination: local, hopLimit: 255, payload: marshalAdvertisement(c, peer, local, 200)}
		if _, err := parseDatagram(c, local, p); err != nil {
			t.Fatal("configured unicast peer rejected", err)
		}
		for _, bad := range []datagram{
			{source: stranger, destination: local, hopLimit: 255, payload: marshalAdvertisement(c, stranger, local, 200)},
			{source: peer, destination: stranger, hopLimit: 255, payload: marshalAdvertisement(c, peer, stranger, 200)},
			{source: peer, destination: multicastGroup(c.Family), hopLimit: 255, payload: marshalAdvertisement(c, peer, multicastGroup(c.Family), 200)},
			{source: peer, destination: local, hopLimit: 254, payload: p.payload},
		} {
			if _, err := parseDatagram(c, local, bad); err == nil {
				t.Fatal("unconfigured endpoint or off-link packet accepted", bad)
			}
		}
		if c.Family == "ipv6" {
			p.payload = marshalAdvertisement(c, peer, group6, 200)
			if _, err := parseDatagram(c, local, p); err == nil {
				t.Fatal("multicast pseudo-header accepted for unicast")
			}
		}
	}
}

func TestUnicastConfigurationRequiresAnExplicitSourceAndDistinctPeers(t *testing.T) {
	for _, peers := range [][]netip.Addr{
		{},
		{netip.MustParseAddr("192.0.2.2")},
		{netip.MustParseAddr("192.0.2.3"), netip.MustParseAddr("192.0.2.3")},
		{group4}, {netip.MustParseAddr("fe80::3")}, {netip.Addr{}},
	} {
		c := config4()
		c.UnicastPeers = peers
		if err := Validate(c); err == nil {
			t.Fatal("invalid peer set accepted", peers)
		}
	}
	c := config4()
	c.SourceAddress = ""
	c.UnicastPeers = []netip.Addr{netip.MustParseAddr("192.0.2.3")}
	if err := Validate(c); err == nil {
		t.Fatal("unicast source was selected implicitly")
	}
}
