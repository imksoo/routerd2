package serviceconfig

import (
	"net/netip"
	"strings"
	"testing"

	"github.com/imksoo/routerd2/internal/peertls"
	"github.com/imksoo/routerd2/internal/protocol/dhcpserverconfig"
	"github.com/imksoo/routerd2/internal/protocol/vrrp"
)

func TestBridgedDHCPIngressOwnsUDP67(t *testing.T) {
	owner := Service{Name: "ha", VRRP: &vrrp.Config{Interface: "lan0", VIPInterface: "vip0", Family: "ipv4", SourceAddress: "192.0.2.2", VirtualRouterID: 42, Addresses: []string{"192.0.2.1/24"}, AcceptMode: true}}
	server := Service{Name: "leases", DHCPServer: &DHCPServer{
		Interface: "vip0", IngressInterface: "lan0", ServerAddress: "192.0.2.1",
		Primary: dhcpserverconfig.Scope{Prefix: "192.0.2.0/24", Start: "192.0.2.100", End: "192.0.2.110"},
		HA:      &DHCPServerHA{Owner: "ha", Replication: peertls.Config{ListenAddress: netip.MustParseAddrPort("192.0.2.2:8443"), PeerAddress: netip.MustParseAddrPort("192.0.2.3:8443"), CertificateFile: "/keys/local.crt", PrivateKeyFile: "/keys/local.key", PeerCertificateFile: "/keys/peer.crt"}},
	}}
	if err := Validate([]Service{owner, server}); err != nil {
		t.Fatal(err)
	}
	server.DHCPServer.IngressInterface = ""
	if err := Validate([]Service{owner, server}); err == nil || !strings.Contains(err.Error(), "physical ingress") {
		t.Fatalf("HA DHCP accepted no physical ingress: %v", err)
	}
	server.DHCPServer.IngressInterface = "lan0"
	other := Service{Name: "other", DHCPServer: &DHCPServer{Interface: "lan1", ServerAddress: "198.51.100.1", Primary: dhcpserverconfig.Scope{Prefix: "198.51.100.0/24", Start: "198.51.100.100", End: "198.51.100.110"}}}
	if err := Validate([]Service{owner, server, other}); err == nil || !strings.Contains(err.Error(), "cannot share UDP/67") {
		t.Fatalf("bridged DHCP accepted a competing listener: %v", err)
	}
}
