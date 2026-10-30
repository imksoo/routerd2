//go:build linux && integration

package network

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"net/netip"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/nftables"
	"github.com/google/nftables/expr"
	"github.com/vishvananda/netns"
	"golang.org/x/sys/unix"
)

// This fixture is called only by the child of the mandatory isolated-netns
// harness in kernel_integration_test.go; it never operates in the host netns.
func runNativeACLNATFixture(t *testing.T, e *Engine, base Desired, clientNS, serverNS netns.NsHandle) {
	t.Helper()
	routerNS, err := netns.Get()
	if err != nil {
		t.Fatal(err)
	}
	defer routerNS.Close()
	held := openNativeTCP(t, clientNS, serverNS, "tcp4", "203.0.113.2:41402", "", "198.51.100.1", nil)
	defer held.close()
	held.exchange(t)
	d := cloneDesired(base)
	nd := FirewallRule{Match: PacketMatch{Protocol: "icmpv6", ICMPTypes: []int{133, 134, 135, 136}}, Action: "accept"}
	// NFLOG sampling must not gate the independent accept/drop verdicts.
	d.Firewall.Input = &FirewallChain{Policy: "drop", Log: &PacketLog{Prefix: "routerd2 test input deny ", Group: 1, Every: 2}, Rules: []FirewallRule{
		{Match: PacketMatch{InInterface: "lo"}, Action: "accept"}, nd,
		{Match: PacketMatch{Family: 4, InInterface: "lan0", Source: "192.0.2.0/24", Destination: "192.0.2.1/32", Protocol: "tcp", SourcePorts: []PortRange{{Start: 20000, End: 65535}}, DestinationPorts: []PortRange{{Start: 41000, End: 41002}}}, Action: "accept", Log: &PacketLog{Prefix: "routerd2 test management ", Group: 1, Every: 2}},
		{Match: PacketMatch{Family: 6, InInterface: "lan0", Source: "2001:db8:1::/64", Destination: "2001:db8:1::1/128", Protocol: "tcp", SourcePorts: []PortRange{{Start: 20000, End: 65535}}, DestinationPorts: []PortRange{{Start: 41000, End: 41002}}}, Action: "accept"},
		{Match: PacketMatch{InInterface: "lan0", Protocol: "udp", SourcePorts: []PortRange{{Start: 43000, End: 43002}}, DestinationPorts: []PortRange{{Start: 42000, End: 42002}}}, Action: "accept"},
		{Match: PacketMatch{InInterface: "lan0", Protocol: "tcp", DestinationPorts: []PortRange{{Start: 41003}}}, Action: "reject"},
	}}
	d.Firewall.Output = &FirewallChain{Policy: "drop", Rules: []FirewallRule{
		{Match: PacketMatch{OutInterface: "lo"}, Action: "accept"}, nd,
		// Permit control/error messages explicitly when OUTPUT defaults to drop.
		{Match: PacketMatch{Protocol: "icmpv6", ICMPTypes: []int{1, 2, 3, 4}}, Action: "accept"},
		{Match: PacketMatch{Protocol: "icmp", ICMPTypes: []int{3, 11, 12}}, Action: "accept"},
		{Match: PacketMatch{Family: 4, OutInterface: "wan0", Source: "198.51.100.1/32", Destination: "203.0.113.0/24", Protocol: "tcp", DestinationPorts: []PortRange{{Start: 41100, End: 41102}}}, Action: "accept"},
		{Match: PacketMatch{Family: 6, OutInterface: "wan0", Source: "2001:db8:2::1/128", Destination: "2001:db8:3::/64", Protocol: "tcp", DestinationPorts: []PortRange{{Start: 41100, End: 41102}}}, Action: "accept"},
	}}
	d.Firewall.ForwardChain = &FirewallChain{Policy: "drop", Rules: []FirewallRule{
		{Match: PacketMatch{InInterface: "lan0", OutInterface: "wan0", Protocol: "tcp", DestinationPorts: []PortRange{{Start: 41401}}}, Action: "reject"},
		{Match: PacketMatch{InInterface: "lan0", OutInterface: "wan0", Protocol: "tcp", DestinationPorts: []PortRange{{Start: 41402}}}, Action: "drop"},
	}}
	// Multiple exclusions are ANDed in one NAT rule; peers have explicit
	// return routes for the untranslated client sources.
	d.Firewall.NAT[0].Match.ExcludeDestinations = []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8"), netip.MustParsePrefix("203.0.113.0/24")}
	d.Firewall.NAT[1].Match.ExcludeDestinations = []netip.Prefix{netip.MustParsePrefix("2001:db8:ffff::/48"), netip.MustParsePrefix("2001:db8:3::/64")}
	if _, err := e.Apply(context.Background(), 30, d); err != nil {
		t.Fatalf("native ACL install: %v", err)
	}
	for _, v := range []struct{ network, udp, lan, wan, client, server string }{
		{"tcp4", "udp4", "192.0.2.1", "198.51.100.1", "192.0.2.2", "203.0.113.2"},
		{"tcp6", "udp6", "2001:db8:1::1", "2001:db8:2::1", "2001:db8:1::2", "2001:db8:3::2"},
	} {
		pass := openNativeTCP(t, clientNS, routerNS, v.network, net.JoinHostPort(v.lan, "41001"), "", v.client, nil)
		pass.exchange(t)
		pass.close()
		assertNativeTCPBlocked(t, clientNS, routerNS, v.network, net.JoinHostPort(v.lan, "41004"), false, false)
		assertNativeTCPBlocked(t, clientNS, routerNS, v.network, net.JoinHostPort(v.lan, "41003"), true, false)
		assertNativeTCPBlocked(t, serverNS, routerNS, v.network, net.JoinHostPort(v.wan, "41001"), false, false)
		nativeUDPExchange(t, clientNS, routerNS, v.udp, net.JoinHostPort(v.client, "43001"), net.JoinHostPort(v.lan, "42001"), v.client, PortRange{Start: 43001})
		pass = openNativeTCP(t, routerNS, serverNS, v.network, net.JoinHostPort(v.server, "41101"), "", v.wan, nil)
		pass.exchange(t)
		pass.close()
		assertNativeTCPBlocked(t, routerNS, serverNS, v.network, net.JoinHostPort(v.server, "41104"), false, true)
		assertNativeTCPBlocked(t, clientNS, serverNS, v.network, net.JoinHostPort(v.server, "41401"), true, false)
		assertNativeTCPBlocked(t, clientNS, serverNS, v.network, net.JoinHostPort(v.server, "41402"), false, false)
	}
	held.exchange(t)
	if _, err := e.Apply(context.Background(), 30, d); err != nil {
		t.Fatalf("native ACL reapply: %v", err)
	}
	held.exchange(t)
	t.Log("INPUT/OUTPUT IPv4/IPv6 CIDR/interface/TCP/UDP port-range ACLs, drop/reject, ordered FORWARD rules and established-flow preservation passed")

	tcpExchange(t, clientNS, serverNS, "tcp4", "203.0.113.2", "192.0.2.2")
	tcpExchange(t, clientNS, serverNS, "tcp6", "2001:db8:3::2", "2001:db8:1::2")

	nat := cloneDesired(d)
	defaultNAT := nat.Firewall.NAT
	nat.Firewall.NAT = nil
	for _, v := range []struct {
		family                           int
		lan, wan, client, source, server string
		hostBits                         string
	}{
		{4, "192.0.2.1", "198.51.100.1", "192.0.2.2", "198.51.100.2", "203.0.113.2", "/32"},
		{6, "2001:db8:1::1", "2001:db8:2::1", "2001:db8:1::2", "2001:db8:2::2", "2001:db8:3::2", "/128"},
	} {
		nat.Firewall.NAT = append(nat.Firewall.NAT,
			NATRule{Type: "dnat", Match: PacketMatch{Family: v.family, InInterface: "wan0", Destination: v.wan + v.hostBits, Protocol: "tcp", DestinationPorts: []PortRange{{Start: 45000}}}, Address: v.client, Ports: &PortRange{Start: 45001}},
			NATRule{Type: "snat", Match: PacketMatch{Family: v.family, OutInterface: "lan0", Source: v.source + v.hostBits, Destination: v.client + v.hostBits, Protocol: "tcp", DestinationPorts: []PortRange{{Start: 45001}}}, Address: v.lan, Ports: &PortRange{Start: 46000, End: 46010}},
			NATRule{Type: "masquerade", Match: PacketMatch{Family: v.family, OutInterface: "wan0", Source: v.client + v.hostBits, Destination: v.server + v.hostBits, Protocol: "udp", DestinationPorts: []PortRange{{Start: 47001}}}, Ports: &PortRange{Start: 48000, End: 48010}},
		)
		nat.Firewall.ForwardChain.Rules = append(nat.Firewall.ForwardChain.Rules, FirewallRule{Match: PacketMatch{Family: v.family, InInterface: "wan0", OutInterface: "lan0", Source: v.source + v.hostBits, Destination: v.client + v.hostBits, Protocol: "tcp", DestinationPorts: []PortRange{{Start: 45001}}}, Action: "accept"})
	}
	// Specific translations precede the general outbound masquerade rules.
	nat.Firewall.NAT = append(nat.Firewall.NAT, defaultNAT...)
	if _, err := e.Apply(context.Background(), 31, nat); err != nil {
		t.Fatalf("native DNAT/SNAT install: %v", err)
	}
	var heldNAT []*nativeTCPFlow
	for _, v := range []struct{ network, udp, lan, wan, client, source, server string }{
		{"tcp4", "udp4", "192.0.2.1", "198.51.100.1", "192.0.2.2", "198.51.100.2", "203.0.113.2"},
		{"tcp6", "udp6", "2001:db8:1::1", "2001:db8:2::1", "2001:db8:1::2", "2001:db8:2::2", "2001:db8:3::2"},
	} {
		flow := openNativeTCP(t, serverNS, clientNS, v.network, net.JoinHostPort(v.client, "45001"), net.JoinHostPort(v.wan, "45000"), v.lan, &PortRange{Start: 46000, End: 46010})
		flow.exchange(t)
		defer flow.close()
		heldNAT = append(heldNAT, flow)
		nativeUDPExchange(t, clientNS, serverNS, v.udp, net.JoinHostPort(v.client, "43001"), net.JoinHostPort(v.server, "47001"), v.wan, PortRange{Start: 48000, End: 48010})
	}
	// Keep only NAT hooks and replace the target port. Existing sessions must
	// retain their original mapping; new sessions must use the new backend.
	natOnly := cloneDesired(nat)
	natOnly.Firewall = &Firewall{DisableForward: true, NAT: natOnly.Firewall.NAT}
	for i := range natOnly.Firewall.NAT {
		r := &natOnly.Firewall.NAT[i]
		if r.Type == "dnat" {
			r.Ports = &PortRange{Start: 45004}
		}
		if r.Type == "snat" {
			r.Match.DestinationPorts = []PortRange{{Start: 45004}}
		}
	}
	if _, err := e.Apply(context.Background(), 32, natOnly); err != nil {
		t.Fatalf("NAT-only backend update: %v", err)
	}
	for _, flow := range heldNAT {
		flow.exchange(t)
	}
	for _, v := range []struct{ network, lan, wan, client string }{{"tcp4", "192.0.2.1", "198.51.100.1", "192.0.2.2"}, {"tcp6", "2001:db8:1::1", "2001:db8:2::1", "2001:db8:1::2"}} {
		flow := openNativeTCP(t, serverNS, clientNS, v.network, net.JoinHostPort(v.client, "45004"), net.JoinHostPort(v.wan, "45000"), v.lan, &PortRange{Start: 46000, End: 46010})
		flow.exchange(t)
		flow.close()
	}
	chains, err := (&nftables.Conn{}).ListChains()
	if err != nil {
		t.Fatal(err)
	}
	for _, chain := range chains {
		if chain.Table.Name == FirewallTable && chain.Type == nftables.ChainTypeFilter {
			t.Fatalf("NAT-only policy installed a filter hook: %s", chain.Name)
		}
	}
	if _, err := e.Apply(context.Background(), 1, base); err != nil {
		t.Fatalf("restore ACL/NAT fixture: %v", err)
	}
	tcpExchange(t, clientNS, serverNS, "tcp4", "203.0.113.2", "198.51.100.1")
	tcpExchange(t, clientNS, serverNS, "tcp6", "2001:db8:3::2", "2001:db8:2::1")
	t.Log("IPv4/IPv6 TCP preserves source for excluded destinations and resumes NAT after removal")
	t.Log("IPv4/IPv6 DNAT+SNAT TCP, translated source port ranges, UDP masquerade ranges, NAT-only hooks and existing NAT-session preservation passed")
}

type nativeTCPFlow struct{ client, server net.Conn }

func (f *nativeTCPFlow) close() { f.client.Close(); f.server.Close() }
func (f *nativeTCPFlow) exchange(t *testing.T) {
	t.Helper()
	for _, c := range []net.Conn{f.client, f.server} {
		_ = c.SetDeadline(time.Now().Add(2 * time.Second))
	}
	if _, err := f.client.Write([]byte("probe")); err != nil {
		t.Fatal(err)
	}
	buffer := make([]byte, 5)
	if _, err := io.ReadFull(f.server, buffer); err != nil {
		t.Fatal(err)
	}
	if string(buffer) != "probe" {
		t.Fatalf("unexpected TCP payload %q", buffer)
	}
	if _, err := f.server.Write(buffer); err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadFull(f.client, buffer); err != nil {
		t.Fatal(err)
	}
	if string(buffer) != "probe" {
		t.Fatalf("unexpected TCP echo %q", buffer)
	}
}

func openNativeTCP(t *testing.T, clientNS, serverNS netns.NsHandle, network, listen, destination, wantSource string, ports *PortRange) *nativeTCPFlow {
	t.Helper()
	var listener net.Listener
	if err := inNamespace(serverNS, func() error { var err error; listener, err = net.Listen(network, listen); return err }); err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	if destination == "" {
		destination = listener.Addr().String()
	}
	var client net.Conn
	if err := inNamespace(clientNS, func() error {
		var err error
		client, err = net.DialTimeout(network, destination, 2*time.Second)
		return err
	}); err != nil {
		t.Fatalf("TCP dial %s to %s: %v", listen, destination, err)
	}
	_ = listener.(*net.TCPListener).SetDeadline(time.Now().Add(2 * time.Second))
	server, err := listener.Accept()
	if err != nil {
		client.Close()
		t.Fatal(err)
	}
	source, port, err := net.SplitHostPort(server.RemoteAddr().String())
	if err != nil || source != wantSource {
		client.Close()
		server.Close()
		t.Fatalf("TCP translated source %s want %s: %v", source, wantSource, err)
	}
	if ports != nil {
		n, err := strconv.Atoi(port)
		p := normalizedPortRange(*ports)
		if err != nil || n < p.Start || n > p.End {
			client.Close()
			server.Close()
			t.Fatalf("TCP translated port %s outside %+v", port, p)
		}
	}
	return &nativeTCPFlow{client, server}
}

func assertNativeTCPBlocked(t *testing.T, clientNS, serverNS netns.NsHandle, network, address string, reject, local bool) {
	t.Helper()
	var listener net.Listener
	if err := inNamespace(serverNS, func() error { var err error; listener, err = net.Listen(network, address); return err }); err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	var rejection net.PacketConn
	if reject {
		proto, bind := "ip4:icmp", "0.0.0.0"
		if network == "tcp6" {
			proto, bind = "ip6:ipv6-icmp", "::"
		}
		if err := inNamespace(clientNS, func() error { var err error; rejection, err = net.ListenPacket(proto, bind); return err }); err != nil {
			t.Fatal(err)
		}
		defer rejection.Close()
		_ = rejection.SetReadDeadline(time.Now().Add(2 * time.Second))
	}
	err := inNamespace(clientNS, func() error {
		c, err := net.DialTimeout(network, address, 350*time.Millisecond)
		if c != nil {
			c.Close()
		}
		return err
	})
	if reject {
		var timeout net.Error
		if err == nil || !errors.Is(err, unix.ECONNREFUSED) && (!errors.As(err, &timeout) || !timeout.Timeout()) {
			dumpNativeFirewallCounters(t)
			t.Fatalf("reject probe had an unexpected result for %s: %v", address, err)
		}
		// Verify the rejection packet itself. In a zero-delay veth fixture a
		// TCPv6 error can arrive while connect still owns the socket lock; Linux
		// then records a soft error and the short Dial timeout is not evidence
		// that the firewall silently dropped the SYN.
		host, port, _ := net.SplitHostPort(address)
		number, _ := strconv.Atoi(port)
		buffer := make([]byte, 2048)
		for {
			n, _, readErr := rejection.ReadFrom(buffer)
			if readErr != nil {
				dumpNativeFirewallCounters(t)
				t.Fatalf("reject did not deliver the matching ICMP error for %s: %v", address, readErr)
			}
			if network == "tcp6" {
				if n >= 52 && buffer[0] == 1 && buffer[1] == 4 && bytes.Equal(buffer[32:48], net.ParseIP(host).To16()) && binary.BigEndian.Uint16(buffer[50:52]) == uint16(number) {
					break
				}
			} else if n >= 32 && buffer[0] == 3 && buffer[1] == 3 {
				offset := 8 + int(buffer[8]&15)*4
				if n >= offset+4 && bytes.Equal(buffer[24:28], net.ParseIP(host).To4()) && binary.BigEndian.Uint16(buffer[offset+2:offset+4]) == uint16(number) {
					break
				}
			}
		}
		return
	}
	var timeout net.Error
	if err == nil || (!errors.As(err, &timeout) || !timeout.Timeout()) && !(local && (errors.Is(err, unix.EPERM) || errors.Is(err, unix.EACCES))) {
		t.Fatalf("drop was not a timeout/output-policy refusal for %s: %v", address, err)
	}
}

func dumpNativeFirewallCounters(t *testing.T) {
	t.Helper()
	c := &nftables.Conn{}
	if b, err := os.ReadFile("/proc/net/snmp6"); err == nil {
		for _, line := range strings.Split(string(b), "\n") {
			if strings.HasPrefix(line, "Icmp6") || strings.HasPrefix(line, "Ip6OutNoRoutes") {
				t.Log(line)
			}
		}
	}
	chains, err := c.ListChains()
	if err != nil {
		t.Log(err)
		return
	}
	for _, chain := range chains {
		if chain.Table.Name != FirewallTable {
			continue
		}
		rules, err := c.GetRules(chain.Table, chain)
		if err != nil {
			t.Log(err)
			continue
		}
		for i, rule := range rules {
			for _, expression := range rule.Exprs {
				if counter, ok := expression.(*expr.Counter); ok && counter.Packets > 0 {
					t.Logf("firewall counter %s rule %d packets=%d bytes=%d", chain.Name, i, counter.Packets, counter.Bytes)
				}
			}
		}
	}
}

func nativeUDPExchange(t *testing.T, clientNS, serverNS netns.NsHandle, network, local, destination, wantSource string, ports PortRange) {
	t.Helper()
	var server, client *net.UDPConn
	remote, err := net.ResolveUDPAddr(network, destination)
	if err != nil {
		t.Fatal(err)
	}
	if err := inNamespace(serverNS, func() error { var err error; server, err = net.ListenUDP(network, remote); return err }); err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	address, err := net.ResolveUDPAddr(network, local)
	if err != nil {
		t.Fatal(err)
	}
	if err := inNamespace(clientNS, func() error { var err error; client, err = net.DialUDP(network, address, remote); return err }); err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	for _, c := range []*net.UDPConn{client, server} {
		_ = c.SetDeadline(time.Now().Add(2 * time.Second))
	}
	if network == "udp4" {
		for _, item := range []struct {
			conn *net.UDPConn
			mode int
		}{{client, unix.IP_PMTUDISC_PROBE}, {server, unix.IP_PMTUDISC_DONT}} {
			raw, err := item.conn.SyscallConn()
			if err != nil {
				t.Fatal(err)
			}
			var optionErr error
			if err := raw.Control(func(fd uintptr) {
				optionErr = unix.SetsockoptInt(int(fd), unix.IPPROTO_IP, unix.IP_MTU_DISCOVER, item.mode)
			}); err != nil {
				t.Fatal(err)
			}
			if optionErr != nil {
				t.Fatal(optionErr)
			}
		}
	}
	payload := bytes.Repeat([]byte("u"), 1450)
	if _, err := client.Write(payload); err != nil {
		t.Fatal(err)
	}
	buffer := make([]byte, len(payload)+1)
	n, source, err := server.ReadFromUDP(buffer)
	if err != nil {
		t.Fatalf("UDP exchange %s: %v", destination, err)
	}
	p := normalizedPortRange(ports)
	if source.IP.String() != wantSource || source.Port < p.Start || source.Port > p.End {
		t.Fatalf("UDP source %s want %s ports %+v", source, wantSource, p)
	}
	if !bytes.Equal(buffer[:n], payload) {
		t.Fatalf("UDP payload %q", buffer[:n])
	}
	if _, err := server.WriteToUDP(buffer[:n], source); err != nil {
		t.Fatal(err)
	}
	n, err = client.Read(buffer)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(buffer[:n], payload) {
		t.Fatal("UDP echo mismatch")
	}
}
