//go:build linux && integration

package integration

import (
	"context"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/imksoo/routerd2/internal/config"
	"github.com/imksoo/routerd2/internal/control"
	"github.com/imksoo/routerd2/internal/legacyconfig"
	"github.com/imksoo/routerd2/internal/network"
	"github.com/miekg/dns"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
	"golang.org/x/sys/unix"
)

func TestDaemonLocalDNSRedirect(t *testing.T) {
	if !isolateFixtureTimeout(t, "TestDaemonLocalDNSRedirect", 70*time.Second) {
		return
	}
	wan := fixturePeerNamespace(t, "wan0", "server0")
	lan := fixturePeerNamespace(t, "lan0", "client0")
	for _, peer := range []struct {
		ns                  netns.NsHandle
		name                string
		addresses, gateways []string
	}{
		{wan, "server0", []string{"192.0.2.1/24", "198.51.100.20/32", "198.51.100.21/32", "2001:db8:2::1/64", "2001:db8:2::20/128", "2001:db8:2::21/128"}, []string{"192.0.2.2", "2001:db8:2::2"}},
		{lan, "client0", []string{"198.18.0.2/24", "2001:db8:1::2/64"}, []string{"198.18.0.1", "2001:db8:1::1"}},
	} {
		h, err := netlink.NewHandleAt(peer.ns, unix.NETLINK_ROUTE)
		if err != nil {
			t.Fatal(err)
		}
		defer h.Close()
		link, err := h.LinkByName(peer.name)
		if err != nil {
			t.Fatal(err)
		}
		for _, cidr := range peer.addresses {
			address, _ := netlink.ParseAddr(cidr)
			if err := h.AddrAdd(link, address); err != nil {
				t.Fatal(err)
			}
		}
		for _, gateway := range peer.gateways {
			if err := h.RouteAdd(&netlink.Route{LinkIndex: link.Attrs().Index, Gw: net.ParseIP(gateway)}); err != nil {
				t.Fatal(err)
			}
		}
	}
	hosts := filepath.Join(t.TempDir(), "hosts")
	writeHosts := func(v4, v6 string) {
		t.Helper()
		if err := os.WriteFile(hosts, []byte(v4+" redirected.test\n"+v6+" redirected.test\n"), 0644); err != nil {
			t.Fatal(err)
		}
	}
	writeHosts("198.51.100.20", "2001:db8:2::20")
	peer := startFixtureDNSMasq(t, wan, []string{"--no-hosts", "--no-resolv", "--addn-hosts=" + hosts, "--local-ttl=2", "--host-record=who.probe.test,203.0.113.99"})
	wanCapture := healthPBRCapture(t, "wan0")
	_ = healthPBRCapture(t, "lan0")
	result, err := legacyconfig.Compile([]byte(`apiVersion: routerd.net/v1alpha1
kind: Router
metadata: {name: local-dns}
spec:
  resources:
    - apiVersion: net.routerd.net/v1alpha1
      kind: Interface
      metadata: {name: lan}
      spec: {ifname: lan0, managed: true, adminUp: true}
    - apiVersion: net.routerd.net/v1alpha1
      kind: Interface
      metadata: {name: wan}
      spec: {ifname: wan0, managed: true, adminUp: true}
    - apiVersion: net.routerd.net/v1alpha1
      kind: IPv4StaticAddress
      metadata: {name: router}
      spec: {interface: lan, address: 198.18.0.1/24}
    - apiVersion: net.routerd.net/v1alpha1
      kind: IPAddressSet
      metadata: {name: public-dns}
      spec: {names: [redirected.test], refreshInterval: 1s}
    - apiVersion: firewall.routerd.net/v1alpha1
      kind: LocalServiceRedirect
      metadata: {name: lan-local-services}
      spec:
        interface: lan
        rules: [{name: public-dns, protocols: [tcp, udp], destinationSetRef: IPAddressSet/public-dns, destinationPort: 53, redirectPort: 5353}]
    - apiVersion: net.routerd.net/v1alpha1
      kind: DNSResolver
      metadata: {name: dns}
      spec:
        listen: [{name: local, addresses: [198.18.0.1, '2001:db8:1::1'], port: 5353, sources: [local-zone]}, {name: core, addresses: [127.0.0.1], port: 53, sources: [membership]}]
    - apiVersion: net.routerd.net/v1alpha1
      kind: DNSUpstream
      metadata: {name: member-peer}
      spec: {protocol: udp, address: 192.0.2.1, port: 53, sourceInterface: wan}
    - apiVersion: net.routerd.net/v1alpha1
      kind: DNSForwarder
      metadata: {name: membership}
      spec: {resolver: DNSResolver/dns, match: [redirected.test], upstreams: [DNSUpstream/member-peer]}
    - apiVersion: net.routerd.net/v1alpha1
      kind: DNSZone
      metadata: {name: zone}
      spec: {zone: probe.test, records: [{hostname: who, ipv4: 203.0.113.7}]}
    - apiVersion: net.routerd.net/v1alpha1
      kind: DNSForwarder
      metadata: {name: local-zone}
      spec: {resolver: DNSResolver/dns, match: [probe.test], zoneRefs: [DNSZone/zone]}
    - apiVersion: firewall.routerd.net/v1alpha1
      kind: FirewallZone
      metadata: {name: clients}
      spec: {role: untrust, interfaces: [Interface/lan]}
    - apiVersion: firewall.routerd.net/v1alpha1
      kind: FirewallZone
      metadata: {name: uplink}
      spec: {role: untrust, interfaces: [Interface/wan]}
    - apiVersion: firewall.routerd.net/v1alpha1
      kind: FirewallRule
      metadata: {name: reject-local}
      spec: {fromZone: clients, toZone: self, protocol: tcp, port: 5353, action: drop}
`))
	if err != nil {
		t.Fatal(err)
	}
	plan, err := config.Compile(result.Document)
	if err != nil {
		t.Fatal(err)
	}
	doc := plan.Native
	for i := range doc.Network.Interfaces {
		if doc.Network.Interfaces[i].Name == "lan0" {
			doc.Network.Interfaces[i].Addresses = append(doc.Network.Interfaces[i].Addresses, "2001:db8:1::1/64")
		} else {
			doc.Network.Interfaces[i].Addresses = []string{"192.0.2.2/24", "2001:db8:2::2/64"}
		}
	}
	forward := true
	doc.Network.Forwarding.IPv4, doc.Network.Forwarding.IPv6 = &forward, &forward
	doc.Network.Routes = []network.Route{{Destination: "0.0.0.0/0", Interface: "wan0", Gateway: "192.0.2.1"}, {Destination: "::/0", Interface: "wan0", Gateway: "2001:db8:2::1"}}
	f := startFixtureDaemon(t, config.FromNative(doc))
	ready := func() {
		t.Helper()
		until(t, 10*time.Second, func() bool { return f.control.Do(context.Background(), "GET", "/v1/health", nil, nil) == nil })
	}
	ready()
	query := func(address, transport string) (netip.Addr, error) {
		var answer netip.Addr
		err := fixtureInsideNamespace(lan, func() error {
			message := new(dns.Msg).SetQuestion("who.probe.test.", dns.TypeA)
			client := dns.Client{Net: transport, Timeout: time.Second}
			reply, _, err := client.Exchange(message, net.JoinHostPort(address, "53"))
			if err != nil {
				return err
			}
			for _, record := range reply.Answer {
				if a, ok := record.(*dns.A); ok {
					answer, _ = netip.AddrFromSlice(a.A)
					answer = answer.Unmap()
				}
			}
			return nil
		})
		return answer, err
	}
	check := func(address, want string) {
		t.Helper()
		for _, transport := range []string{"tcp", "udp"} {
			until(t, 10*time.Second, func() bool { answer, err := query(address, transport); return err == nil && answer.String() == want })
		}
	}
	check("198.51.100.20", "203.0.113.7")
	check("2001:db8:2::20", "203.0.113.7")
	for _, target := range []string{"198.51.100.20", "2001:db8:2::20"} {
		for _, protocol := range []byte{6, 17} {
			source := "198.18.0.2"
			if netip.MustParseAddr(target).Is6() {
				source = "2001:db8:1::2"
			}
			if wanCapture.count(source, target, protocol, 53) != 0 {
				t.Fatal("selected DNS traffic escaped onto WAN", target, protocol)
			}
		}
	}
	check("198.51.100.21", "203.0.113.99")
	check("2001:db8:2::21", "203.0.113.99")
	writeHosts("198.51.100.21", "2001:db8:2::21")
	if err := peer.Process.Signal(unix.SIGHUP); err != nil {
		t.Fatal(err)
	}
	check("198.51.100.21", "203.0.113.7")
	check("2001:db8:2::21", "203.0.113.7")
	check("198.51.100.20", "203.0.113.99")
	check("2001:db8:2::20", "203.0.113.99")
	f = f.restart()
	ready()
	check("198.51.100.21", "203.0.113.7")
	check("2001:db8:2::21", "203.0.113.7")
	doc.Network.Firewall.LocalRedirects = nil
	if err := f.control.Do(context.Background(), "POST", "/v1/config/commit", control.CommitRequest{ExpectedRevision: 1, RequestID: "remove-local-redirect", Config: config.FromNative(doc)}, nil); err != nil {
		t.Fatal(err)
	}
	check("198.51.100.21", "203.0.113.99")
	check("2001:db8:2::21", "203.0.113.99")
}
