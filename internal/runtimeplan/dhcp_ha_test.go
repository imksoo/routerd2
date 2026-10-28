package runtimeplan

import (
	"encoding/json"
	"net/netip"
	"testing"
	"time"

	"github.com/imksoo/routerd2/internal/config"
	"github.com/imksoo/routerd2/internal/network"
	"github.com/imksoo/routerd2/internal/peertls"
	"github.com/imksoo/routerd2/internal/protocol/bfd"
	"github.com/imksoo/routerd2/internal/protocol/dhcp6config"
	"github.com/imksoo/routerd2/internal/protocol/dhcpserverconfig"
	"github.com/imksoo/routerd2/internal/protocol/vrrp"
	"github.com/imksoo/routerd2/internal/resourceprogram"
	"github.com/imksoo/routerd2/internal/serviceconfig"
)

func TestHADHCPStopsServingWhenVRRPOwnerLeavesProjection(t *testing.T) {
	owner := serviceconfig.Service{Name: "ha", VRRP: &vrrp.Config{Interface: "lan0", VIPInterface: "vip0", Family: "ipv4", SourceAddress: "192.0.2.2", VirtualRouterID: 42, Addresses: []string{"192.0.2.1/24"}, AcceptMode: true}}
	server := serviceconfig.Service{Name: "leases", DHCPServer: &serviceconfig.DHCPServer{Interface: "vip0", ServerAddress: "192.0.2.1", Primary: dhcpserverconfig.Scope{Prefix: "192.0.2.0/24", Start: "192.0.2.100", End: "192.0.2.110"}, HA: &serviceconfig.LeaseHA{Owner: "ha", Replication: peertls.Config{ListenAddress: netip.MustParseAddrPort("192.0.2.2:8443"), PeerAddress: netip.MustParseAddrPort("192.0.2.3:8443"), CertificateFile: "/keys/local.crt", PrivateKeyFile: "/keys/local.key", PeerCertificateFile: "/keys/peer.crt"}}}}
	doc := config.Native{Network: network.Desired{Interfaces: []network.Interface{{Name: "lan0", Addresses: []string{"192.0.2.2/24"}}}}, Services: []serviceconfig.Service{server, owner}}
	if _, err := config.Compile(config.FromNative(doc)); err != nil {
		t.Fatal(err)
	}
	protocols, err := prepareProtocols(doc, "/run/routerd", "/var/lib/routerd")
	if err != nil {
		t.Fatal(err)
	}
	snapshot := bootVRRPSnapshot(t, owner.VRRPConfig())
	body, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	clock, err := bfd.ReadClock()
	if err != nil {
		t.Fatal(err)
	}
	records := map[string]serviceconfig.Update{owner.ModuleName(): {Name: owner.ModuleName(), Role: "vrrp", ConfigDigest: protocols.instances[owner.ModuleName()].digest, Snapshot: body}}
	check := func(wantStandby bool) {
		t.Helper()
		p, err := projectWithFacts(doc, protocols, records, time.Now(), projectionFacts{bfdClock: clock})
		if err != nil {
			t.Fatal(err)
		}
		for _, b := range p.instances {
			if b.instance.Name == server.ModuleName() {
				var cfg dhcpserverconfig.Config
				if err := json.Unmarshal(b.instance.Config, &cfg); err != nil || cfg.Standby != wantStandby {
					t.Fatal("HA DHCP mode did not follow the projected owner", cfg.Standby, err)
				}
				return
			}
		}
		t.Fatal("standby replication worker was removed")
	}
	check(false)
	// Election alone cannot start LAN allocation before VIP publication.
	published := records[owner.ModuleName()]
	snapshot.Active = false
	body, err = json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	u := records[owner.ModuleName()]
	u.Snapshot = body
	records[owner.ModuleName()] = u
	check(true)
	// A conditional owner can leave the projection before its last active
	// observation expires. That old record cannot keep DHCP allocating.
	records[owner.ModuleName()] = published
	doc.Services = []serviceconfig.Service{server}
	check(true)
}

func TestHAPDAcquiresWhileGracefulVIPPublicationAwaitsPrefix(t *testing.T) {
	owner := serviceconfig.Service{Name: "ha", VRRP: &vrrp.Config{
		Interface: "lan0", VIPInterface: "vip0", Family: "ipv4", SourceAddress: "192.0.2.2", VirtualRouterID: 42, Addresses: []string{"192.0.2.1/24"},
		GracefulActivation: &vrrp.GracefulActivation{ReadyWhen: resourceprogram.FactCondition{Fact: "service.pd.active"}},
	}}
	pd := serviceconfig.Service{Name: "pd", DHCP6: &serviceconfig.DHCP6{
		Interface: "wan0", RequestPrefix: true, IAID: 1, ClientDUID: "00030001020000000042",
		Delegations: []serviceconfig.Delegation{{Name: "b4", Interface: "wan-ha", PrefixLength: 128, HostID: 33}},
		HA: &serviceconfig.LeaseHA{Owner: "ha", Replication: peertls.Config{
			ListenAddress: netip.MustParseAddrPort("192.0.2.2:8444"), PeerAddress: netip.MustParseAddrPort("192.0.2.3:8444"),
			CertificateFile: "/keys/local.crt", PrivateKeyFile: "/keys/local.key", PeerCertificateFile: "/keys/peer.crt",
		}},
	}}
	doc := config.Native{Network: network.Desired{Interfaces: []network.Interface{{Name: "lan0", Addresses: []string{"192.0.2.2/24"}}, {Name: "wan0"}}}, Services: []serviceconfig.Service{pd, owner}}
	if _, err := config.Compile(config.FromNative(doc)); err != nil {
		t.Fatal(err)
	}
	protocols, err := prepareProtocols(doc, "/run/routerd", "/var/lib/routerd")
	if err != nil {
		t.Fatal(err)
	}
	snapshot := bootVRRPSnapshot(t, owner.VRRPConfig())
	snapshot.Active = false
	clock, err := bfd.ReadClock()
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	records := map[string]serviceconfig.Update{owner.ModuleName(): {Name: owner.ModuleName(), Role: "vrrp", ConfigDigest: protocols.instances[owner.ModuleName()].digest, Snapshot: body}}
	check := func(standby bool) {
		t.Helper()
		p, err := projectWithFacts(doc, protocols, records, time.Now(), projectionFacts{bfdClock: clock})
		if err != nil {
			t.Fatal(err)
		}
		if len(p.network.Links) != 0 {
			t.Fatal("PD preparation published an unadmitted VIP", p.network.Links)
		}
		for _, iface := range p.network.Interfaces {
			if iface.Name == "wan-ha" {
				t.Fatal("PD without an active grant managed its delegation destination")
			}
		}
		for _, b := range p.instances {
			if b.instance.Name == pd.ModuleName() {
				var cfg dhcp6config.Config
				if err := json.Unmarshal(b.instance.Config, &cfg); err != nil || cfg.Standby != standby {
					t.Fatal("PD acquisition did not follow election independently of publication", cfg.Standby, err)
				}
				return
			}
		}
		t.Fatal("PD replication worker was removed")
	}
	check(false)
	doc.Services = []serviceconfig.Service{pd}
	check(true)
	doc.Services = []serviceconfig.Service{pd, owner}
	clock.BoottimeNanoseconds = int64(snapshot.ExpiresNanos)
	check(true)
}
