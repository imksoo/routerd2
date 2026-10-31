package runtimeplan

import (
	"context"
	"reflect"

	"encoding/json"
	"github.com/imksoo/routerd2/internal/boottime"
	"github.com/imksoo/routerd2/internal/lifetime"
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
	server := serviceconfig.Service{Name: "leases", DHCPServer: &serviceconfig.DHCPServer{Interface: "vip0", IngressInterface: "lan0", ServerAddress: "192.0.2.1", Primary: dhcpserverconfig.Scope{Prefix: "192.0.2.0/24", Start: "192.0.2.100", End: "192.0.2.110"}, HA: &serviceconfig.DHCPServerHA{Owner: "ha", Replication: peertls.Config{ListenAddress: netip.MustParseAddrPort("192.0.2.2:8443"), PeerAddress: netip.MustParseAddrPort("192.0.2.3:8443"), CertificateFile: "/keys/local.crt", PrivateKeyFile: "/keys/local.key", PeerCertificateFile: "/keys/peer.crt"}}}}
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
				if err := json.Unmarshal(b.instance.Config, &cfg); err != nil || cfg.Standby != wantStandby || cfg.IngressInterface != "lan0" {
					t.Fatal("HA DHCP configuration did not retain owner or ingress", cfg.Standby, cfg.IngressInterface, err)
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

// Configuration edits unrelated to PD must not erase a lease whose producer
// keeps running. Its original deadline must survive the configuration cutover.
func TestHAPDLeaseSurvivesUnrelatedConfigChange(t *testing.T) {
	owner := serviceconfig.Service{Name: "ha", VRRP: &vrrp.Config{Interface: "lan0", VIPInterface: "vip0", Family: "ipv4", SourceAddress: "192.0.2.2", VirtualRouterID: 42, Addresses: []string{"192.0.2.1/24"}, AcceptMode: true}}
	pd := serviceconfig.Service{Name: "pd", DHCP6: &serviceconfig.DHCP6{Interface: "wan0", RequestPrefix: true, IAID: 1, ClientDUID: "00030001020000000042", Delegations: []serviceconfig.Delegation{{Name: "lan", Interface: "lan0"}}, HA: &serviceconfig.LeaseHA{Owner: "ha", Replication: peertls.Config{ListenAddress: netip.MustParseAddrPort("192.0.2.2:8444"), PeerAddress: netip.MustParseAddrPort("192.0.2.3:8444"), CertificateFile: "/keys/local.crt", PrivateKeyFile: "/keys/local.key", PeerCertificateFile: "/keys/peer.crt"}}}}
	doc := config.Native{Network: network.Desired{Interfaces: []network.Interface{{Name: "lan0", Addresses: []string{"192.0.2.2/24"}}, {Name: "wan0"}}}, Services: []serviceconfig.Service{pd, owner}}
	r, n, _ := fixture(t, doc)
	ctx := context.Background()
	if _, err := applyAndDrain(t, r, update(t, r, "service.ha", 1, bootVRRPSnapshot(t, owner.VRRPConfig()))); err != nil {
		t.Fatal(err)
	}
	clock, err := boottime.Read()
	if err != nil {
		t.Fatal(err)
	}
	lease := dhcp6config.Snapshot{BootID: clock.BootID, ObservedNanos: uint64(clock.BoottimeNanoseconds), Resource: "pd", Interface: "wan0", IAID: 1, ClientDUID: pd.DHCP6.ClientDUID, State: "bound", Prefixes: []dhcp6config.Prefix{{Prefix: "2001:db8:80::/56", PreferredUntil: lifetime.After(clock, 300), ValidUntil: lifetime.After(clock, 600)}}}
	accepted := update(t, r, "service.pd", 1, lease)
	if err := observeAndProject(ctx, r, accepted); err != nil {
		t.Fatal(err)
	}
	before := n.applied
	binding := r.bindings["service.pd"].digest
	// Advance the source revision without altering PD. The same
	// workers and network must remain usable without a new lease publication.
	source := config.FromNative(doc)
	if err := r.ApplyConfig(ctx, 2, source); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(r.records["service.pd"], accepted) {
		t.Fatal("configuration cutover discarded or rewrote the current PD lease")
	}
	if r.bindings["service.pd"].digest != binding || !reflect.DeepEqual(n.applied, before) {
		t.Fatal("unchanged PD worker or delegated network changed on cutover")
	}
	// A real change of the WAN association must discard the old observation.
	pd.DHCP6.IAID++
	if err := r.ApplyConfig(ctx, 3, config.FromNative(doc)); err != nil {
		t.Fatal(err)
	}
	if _, retained := r.records["service.pd"]; retained {
		t.Fatal("changed PD association retained the old lease")
	}
}
