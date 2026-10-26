package config

import (
	"testing"

	"github.com/imksoo/routerd2/internal/network"
	"github.com/imksoo/routerd2/internal/protocol/bfd"
	"github.com/imksoo/routerd2/internal/protocol/bgpconfig"
	"github.com/imksoo/routerd2/internal/serviceconfig"
)

func TestBFDRequiresCommittedSourceOnItsInterface(t *testing.T) {
	doc := Native{Services: []serviceconfig.Service{{Name: "edge", BFD: &bfd.Config{Interface: "wan0", SourceAddress: "192.0.2.1", Peers: []bfd.Peer{{Address: "192.0.2.2"}}}}}}
	if err := Validate(FromNative(doc)); err == nil {
		t.Fatal("unowned source accepted")
	}
	doc.Network.Interfaces = []network.Interface{{Name: "wan1", Addresses: []string{"192.0.2.1/24"}}}
	if err := Validate(FromNative(doc)); err == nil {
		t.Fatal("address on another interface accepted")
	}
	doc.Network.Interfaces[0].Name = "wan0"
	if err := Validate(FromNative(doc)); err != nil {
		t.Fatal(err)
	}
	doc.Services[0].BFD.Multihop, doc.Services[0].BFD.Interface = true, ""
	doc.Network.Interfaces[0].Name = "lo"
	if err := Validate(FromNative(doc)); err != nil {
		t.Fatal("routed loopback source rejected", err)
	}
	doc.Network.Interfaces[0].Addresses = nil
	if err := Validate(FromNative(doc)); err == nil {
		t.Fatal("multihop source absent from committed intent accepted")
	}
}

func TestBFDRoutingDomainMustMatchCommittedSourceAndBGPFIB(t *testing.T) {
	blue := "blue"
	doc := Native{Network: network.Desired{
		Links:      []network.Link{{Name: blue, Kind: "vrf", VRFTable: 1001}, {Name: "blue-src", Kind: "dummy"}},
		Interfaces: []network.Interface{{Name: blue}, {Name: "blue-src", Master: &blue, Addresses: []string{"192.0.2.1/32"}}},
	}, Services: []serviceconfig.Service{
		{Name: "watch", BFD: &bfd.Config{Multihop: true, VRF: blue, SourceAddress: "192.0.2.1", Peers: []bfd.Peer{{Address: "192.0.2.2"}}}},
		{Name: "edge", BGP: &serviceconfig.BGP{Table: 1001, Runtime: bgpconfig.AppliedConfig{Global: bgpconfig.AppliedGlobal{ASN: 65001, RouterID: "192.0.2.1", ListenPort: -1}, Peers: map[string]bgpconfig.AppliedPeer{"192.0.2.2": {Address: "192.0.2.2", ASN: 65002, EbgpMultihop: 5}}}, PeerBFD: map[string]bgpconfig.BFDReference{"192.0.2.2": {Service: "watch"}}, PeerTransports: map[string]bgpconfig.PeerTransport{"192.0.2.2": {LocalAddress: "192.0.2.1", BindInterface: blue}}}},
	}}
	if err := Validate(FromNative(doc)); err != nil {
		t.Fatal(err)
	}
	doc.Services[1].BGP.Table = 254
	if err := Validate(FromNative(doc)); err == nil {
		t.Fatal("BFD-bound BGP installed its VRF paths into the default FIB")
	}
	doc.Services[1].BGP.Table = 1001
	doc.Network.Interfaces[1].Master = nil
	if err := Validate(FromNative(doc)); err == nil {
		t.Fatal("VRF BFD borrowed the default-domain source")
	}
	doc.Network.Interfaces[1].Master = &blue
	doc.Services[0].BFD.VRF = ""
	tr := doc.Services[1].BGP.PeerTransports["192.0.2.2"]
	tr.BindInterface = ""
	doc.Services[1].BGP.PeerTransports["192.0.2.2"] = tr
	if err := Validate(FromNative(doc)); err == nil {
		t.Fatal("default-domain BFD borrowed a VRF source")
	}
}
