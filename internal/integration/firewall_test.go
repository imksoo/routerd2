//go:build linux && integration

package integration

import (
	"errors"
	"fmt"
	"net"
	"testing"
	"time"

	"github.com/imksoo/routerd2/internal/legacyconfig"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
)

func TestDaemonFirewallSourceCIDRs(t *testing.T) {
	if !isolateFixtureTimeout(t, "TestDaemonFirewallSourceCIDRs", 40*time.Second) {
		return
	}
	client := fixturePeerNamespace(t, "wan0", "client0")
	_ = healthPBRCapture(t, "wan0")
	if err := fixtureInsideNamespace(client, func() error {
		link, err := netlink.LinkByName("client0")
		if err != nil {
			return err
		}
		for _, cidr := range []string{"198.51.100.2/24", "198.51.100.3/24"} {
			address, _ := netlink.ParseAddr(cidr)
			if err := netlink.AddrAdd(link, address); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	result, err := legacyconfig.Compile([]byte(`apiVersion: routerd.net/v1alpha1
kind: Router
metadata: {name: source-filter}
spec:
  resources:
    - apiVersion: net.routerd.net/v1alpha1
      kind: Interface
      metadata: {name: wan}
      spec: {ifname: wan0, managed: true, adminUp: true}
    - apiVersion: net.routerd.net/v1alpha1
      kind: IPv4StaticAddress
      metadata: {name: router}
      spec: {interface: wan, address: 198.51.100.20/24}
    - apiVersion: firewall.routerd.net/v1alpha1
      kind: FirewallZone
      metadata: {name: clients}
      spec: {role: untrust, interfaces: [Interface/wan]}
    - apiVersion: firewall.routerd.net/v1alpha1
      kind: FirewallRule
      metadata: {name: allow-tcp}
      spec: {fromZone: clients, toZone: self, sourceCIDRs: [198.51.100.2/32], protocol: tcp, port: 18080, action: accept}
    - apiVersion: firewall.routerd.net/v1alpha1
      kind: FirewallRule
      metadata: {name: allow-udp}
      spec: {fromZone: clients, toZone: self, sourceCIDRs: [198.51.100.2/32], protocol: udp, port: 18081, action: accept}
`))
	if err != nil {
		t.Fatal(err)
	}
	_ = startFixtureDaemon(t, result.Document)
	router, err := netns.Get()
	if err != nil {
		t.Fatal(err)
	}
	defer router.Close()
	healthPBRListener(t, router, "198.51.100.20:18080", "S")
	healthPBRUDP(t, router, "S")
	for _, protocol := range []string{"tcp4", "udp4"} {
		for _, source := range []string{"198.51.100.2", "198.51.100.3"} {
			err := fixtureInsideNamespace(client, func() error {
				var local net.Addr = &net.TCPAddr{IP: net.ParseIP(source)}
				port := 18080
				if protocol == "udp4" {
					local = &net.UDPAddr{IP: net.ParseIP(source)}
					port = 18081
				}
				dialer := net.Dialer{LocalAddr: local, Timeout: 500 * time.Millisecond}
				conn, err := dialer.Dial(protocol, net.JoinHostPort("198.51.100.20", fmt.Sprint(port)))
				if err != nil {
					return err
				}
				defer conn.Close()
				if err := conn.SetDeadline(time.Now().Add(500 * time.Millisecond)); err != nil {
					return err
				}
				if protocol == "udp4" {
					if _, err := conn.Write([]byte("probe")); err != nil {
						return err
					}
				}
				var response [1]byte
				_, err = conn.Read(response[:])
				if err == nil && response[0] != 'S' {
					return fmt.Errorf("unexpected peer response %q", response[:])
				}
				return err
			})
			if source == "198.51.100.2" {
				if err != nil {
					t.Fatalf("allowed %s source: %v", protocol, err)
				}
			} else {
				var failure net.Error
				if !errors.As(err, &failure) || !failure.Timeout() {
					t.Fatalf("denied %s source should time out: %v", protocol, err)
				}
			}
		}
	}
}
