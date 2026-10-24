package serviceconfig

import (
	"encoding/json"
	"testing"

	"github.com/imksoo/routerd2/internal/protocol/bfd"
	"github.com/imksoo/routerd2/internal/protocol/bgpconfig"
)

func bfdService(name, source, peer string) Service {
	return Service{Name: name, BFD: &bfd.Config{Interface: "wan0", SourceAddress: source, Peers: []bfd.Peer{{Address: peer}}}}
}

func TestBGPBFDDependencyRequiresTheSameConfiguredPath(t *testing.T) {
	makeServices := func() []Service {
		return []Service{bfdService("wan-bfd", "192.0.2.1", "192.0.2.2"), {Name: "router", BGP: &BGP{Runtime: bgpconfig.AppliedConfig{Global: bgpconfig.AppliedGlobal{ASN: 65001, RouterID: "192.0.2.1", ListenPort: -1}, Peers: map[string]bgpconfig.AppliedPeer{"192.0.2.2": {Address: "192.0.2.2", ASN: 65002}}}, PeerBFD: map[string]bgpconfig.BFDReference{"192.0.2.2": {Service: "wan-bfd"}}, PeerTransports: map[string]bgpconfig.PeerTransport{"192.0.2.2": {LocalAddress: "192.0.2.1", BindInterface: "wan0"}}}}}
	}
	if err := Validate(makeServices()); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		edit func([]Service)
	}{
		{"missing-service", func(s []Service) { s[1].BGP.PeerBFD["192.0.2.2"] = bgpconfig.BFDReference{Service: "missing"} }},
		{"missing-peer", func(s []Service) {
			s[1].BGP.PeerBFD["192.0.2.2"] = bgpconfig.BFDReference{Service: "wan-bfd", Address: "192.0.2.3"}
		}},
		{"unrelated-peer", func(s []Service) {
			s[0].BFD.Peers = append(s[0].BFD.Peers, bfd.Peer{Address: "192.0.2.3"})
			s[1].BGP.PeerBFD["192.0.2.2"] = bgpconfig.BFDReference{Service: "wan-bfd", Address: "192.0.2.3"}
		}},
		{"implicit-local-address", func(s []Service) {
			tr := s[1].BGP.PeerTransports["192.0.2.2"]
			tr.LocalAddress = ""
			s[1].BGP.PeerTransports["192.0.2.2"] = tr
		}},
		{"other-source", func(s []Service) {
			tr := s[1].BGP.PeerTransports["192.0.2.2"]
			tr.LocalAddress = "192.0.2.3"
			s[1].BGP.PeerTransports["192.0.2.2"] = tr
		}},
		{"implicit-interface", func(s []Service) {
			tr := s[1].BGP.PeerTransports["192.0.2.2"]
			tr.BindInterface = ""
			s[1].BGP.PeerTransports["192.0.2.2"] = tr
		}},
		{"other-interface", func(s []Service) {
			tr := s[1].BGP.PeerTransports["192.0.2.2"]
			tr.BindInterface = "wan1"
			s[1].BGP.PeerTransports["192.0.2.2"] = tr
		}},
		{"multihop", func(s []Service) {
			p := s[1].BGP.Runtime.Peers["192.0.2.2"]
			p.EbgpMultihop = 10
			s[1].BGP.Runtime.Peers["192.0.2.2"] = p
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := makeServices()
			tc.edit(s)
			if err := Validate(s); err == nil {
				t.Fatal("unbound BFD path accepted")
			}
		})
	}
	s := makeServices()
	tr := s[1].BGP.PeerTransports["192.0.2.2"]
	tr.BindInterface = ""
	s[1].BGP.PeerTransports["192.0.2.2"] = tr
	s[1].BGP.BindInterface = "wan0"
	if err := Validate(s); err != nil {
		t.Fatal("explicit global interface binding should be accepted", err)
	}
}

func TestBFDInstanceAndSocketOwnership(t *testing.T) {
	v4 := bfdService("wan4", "192.0.2.1", "192.0.2.2")
	v6 := bfdService("wan6", "2001:db8::1", "2001:db8::2")
	if err := Validate([]Service{v4, v6}); err != nil {
		t.Fatal(err)
	}
	instance, err := v4.Instance("/run/routerd", "/var/lib/routerd")
	if err != nil || instance.Role != "bfd" || instance.Name != "service.wan4" {
		t.Fatalf("instance identity: %+v, %v", instance, err)
	}
	var cfg bfd.Config
	if err := json.Unmarshal(instance.Config, &cfg); err != nil || cfg.Resource != v4.Name || cfg.Peers[0].MinTxMilliseconds != 300 {
		t.Fatalf("resolved immutable intent: %+v, %v", cfg, err)
	}
	if v4.BFD.Resource != "" || v4.BFD.Peers[0].MinTxMilliseconds != 0 {
		t.Fatal("instance normalization modified the candidate")
	}
	other := bfdService("other", "192.0.2.3", "192.0.2.4")
	if err := Validate([]Service{v4, other}); err == nil {
		t.Fatal("two workers competing for an interface/family socket were accepted")
	}
	other.BFD.Interface = "wan1"
	if err := Validate([]Service{v4, other}); err != nil {
		t.Fatal(err)
	}
	other.BFD.Resource = "forged"
	if err := Validate([]Service{other}); err == nil {
		t.Fatal("resource spoof accepted")
	}
}
