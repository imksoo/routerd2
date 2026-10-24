package config

import (
	"testing"

	"github.com/imksoo/routerd2/internal/network"
	"github.com/imksoo/routerd2/internal/protocol/bfd"
	"github.com/imksoo/routerd2/internal/serviceconfig"
)

func TestBFDRequiresCommittedSourceOnItsInterface(t *testing.T) {
	doc := Document{SchemaVersion: 1, Services: []serviceconfig.Service{{Name: "edge", BFD: &bfd.Config{Interface: "wan0", SourceAddress: "192.0.2.1", Peers: []bfd.Peer{{Address: "192.0.2.2"}}}}}}
	if err := Validate(doc); err == nil {
		t.Fatal("unowned source accepted")
	}
	doc.Network.Interfaces = []network.Interface{{Name: "wan1", Addresses: []string{"192.0.2.1/24"}}}
	if err := Validate(doc); err == nil {
		t.Fatal("address on another interface accepted")
	}
	doc.Network.Interfaces[0].Name = "wan0"
	if err := Validate(doc); err != nil {
		t.Fatal(err)
	}
}
