//go:build linux && integration

package integration

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/netip"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/imksoo/routerd2/internal/boottime"
	"github.com/imksoo/routerd2/internal/config"
	"github.com/imksoo/routerd2/internal/lifetime"
	"github.com/imksoo/routerd2/internal/localapi"
	"github.com/imksoo/routerd2/internal/network"
	"github.com/imksoo/routerd2/internal/protocol/aftr"
	"github.com/imksoo/routerd2/internal/protocol/raclient"
	"github.com/imksoo/routerd2/internal/runtimeplan"
	"github.com/imksoo/routerd2/internal/serviceconfig"
	"github.com/imksoo/routerd2/internal/supervisor"
	"github.com/miekg/dns"
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

// An independent RA/DNS/Linux AFTR and a separate LAN client exercise the
// complete learned WAN path. Only actual product workers configure the router.
func TestDaemonRAAFTRForwardingAndOriginalExpiry(t *testing.T) {
	if !isolateFixture(t, "TestDaemonRAAFTRForwardingAndOriginalExpiry") {
		return
	}
	provider := fixturePeerNamespace(t, "wan0", "provider0")
	client := fixturePeerNamespace(t, "lan0", "client0")
	h, err := netlink.NewHandleAt(provider, unix.NETLINK_ROUTE)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	peer, err := h.LinkByName("provider0")
	if err != nil {
		t.Fatal(err)
	}
	for _, cidr := range []string{"fe80::2/64", "2001:db8:42::2/64", "2001:db8:42::53/64"} {
		a, _ := netlink.ParseAddr(cidr)
		a.Flags = unix.IFA_F_NODAD
		if err := h.AddrAdd(peer, a); err != nil {
			t.Fatal(err)
		}
	}
	_, renumbered, _ := net.ParseCIDR("2001:db8:43::/64")
	if err := h.RouteAdd(&netlink.Route{Dst: renumbered, LinkIndex: peer.Attrs().Index, Scope: netlink.SCOPE_LINK}); err != nil {
		t.Fatal(err)
	}
	lo, err := h.LinkByName("lo")
	if err != nil {
		t.Fatal(err)
	}
	backend, _ := netlink.ParseAddr("198.51.100.20/32")
	if err := h.AddrAdd(lo, backend); err != nil {
		t.Fatal(err)
	}
	tunnel := &netlink.Ip6tnl{LinkAttrs: netlink.LinkAttrs{Name: "aftr0"}, Link: uint32(peer.Attrs().Index), Local: net.ParseIP("2001:db8:42::2"), Remote: net.ParseIP("2001:db8:42::1234"), Proto: unix.IPPROTO_IPIP, Ttl: 64, Flags: 1}
	if err := h.LinkAdd(tunnel); err != nil {
		t.Fatal(err)
	}
	inner, _ := netlink.ParseAddr("169.254.42.1/32")
	inner.Peer = &net.IPNet{IP: net.ParseIP("169.254.42.2"), Mask: net.CIDRMask(32, 32)}
	if err := h.AddrAdd(tunnel, inner); err != nil {
		t.Fatal(err)
	}
	if err := h.LinkSetUp(tunnel); err != nil {
		t.Fatal(err)
	}
	vrrpVIPResponders(t, provider, "4", []string{"198.51.100.20"}, "S")
	ch, err := netlink.NewHandleAt(client, unix.NETLINK_ROUTE)
	if err != nil {
		t.Fatal(err)
	}
	defer ch.Close()
	lan, err := ch.LinkByName("client0")
	if err != nil {
		t.Fatal(err)
	}
	ca, _ := netlink.ParseAddr("198.18.0.2/24")
	if err := ch.AddrAdd(lan, ca); err != nil {
		t.Fatal(err)
	}
	if err := ch.RouteAdd(&netlink.Route{LinkIndex: lan.Attrs().Index, Gw: net.ParseIP("198.18.0.1")}); err != nil {
		t.Fatal(err)
	}
	var dnsSocket net.PacketConn
	raFD := -1
	if err := fixtureInsideNamespace(provider, func() error {
		var err error
		dnsSocket, err = net.ListenPacket("udp6", "[2001:db8:42::53]:53")
		if err != nil {
			return err
		}
		raFD, err = unix.Socket(unix.AF_INET6, unix.SOCK_RAW|unix.SOCK_CLOEXEC, unix.IPPROTO_ICMPV6)
		if err != nil {
			return err
		}
		if err := unix.SetsockoptInt(raFD, unix.IPPROTO_IPV6, unix.IPV6_MULTICAST_IF, peer.Attrs().Index); err != nil {
			return err
		}
		if err := unix.SetsockoptInt(raFD, unix.IPPROTO_IPV6, unix.IPV6_MULTICAST_HOPS, 255); err != nil {
			return err
		}
		return unix.Bind(raFD, &unix.SockaddrInet6{Addr: netip.MustParseAddr("fe80::2").As16(), ZoneId: uint32(peer.Attrs().Index)})
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { dnsSocket.Close(); unix.Close(raFD) })
	var mu sync.Mutex
	queries := map[string]int{}
	server := &dns.Server{PacketConn: dnsSocket, Handler: dns.HandlerFunc(func(w dns.ResponseWriter, q *dns.Msg) {
		if len(q.Question) != 1 || q.Question[0] != (dns.Question{Name: "aftr.test.", Qtype: dns.TypeAAAA, Qclass: dns.ClassINET}) {
			return
		}
		address, _, err := net.SplitHostPort(w.RemoteAddr().String())
		if err != nil {
			return
		}
		mu.Lock()
		queries[address]++
		mu.Unlock()
		answer := new(dns.Msg).SetReply(q)
		answer.Authoritative = true
		answer.Answer = []dns.RR{
			&dns.AAAA{Hdr: dns.RR_Header{Name: "aftr.test.", Rrtype: dns.TypeAAAA, Class: dns.ClassINET, Ttl: 60}, AAAA: net.ParseIP("ff02::1")},
			&dns.AAAA{Hdr: dns.RR_Header{Name: "aftr.test.", Rrtype: dns.TypeAAAA, Class: dns.ClassINET, Ttl: 60}, AAAA: net.ParseIP("2001:db8:42::2")},
		}
		_ = w.WriteMsg(answer)
	})}
	ready, done := make(chan struct{}), make(chan error, 1)
	server.NotifyStartedFunc = func() { close(ready) }
	go func() { done <- server.ActivateAndServe() }()
	t.Cleanup(func() {
		_ = server.Shutdown()
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(time.Second):
			t.Error("DNS peer did not stop")
		}
	})
	select {
	case <-ready:
	case err := <-done:
		t.Fatal(err)
	case <-time.After(time.Second):
		t.Fatal("DNS peer startup")
	}
	wanCapture, lanCapture := healthPBRCapture(t, "wan0"), healthPBRCapture(t, "lan0")
	yes := true
	doc := config.Native{Network: network.Desired{
		Interfaces: []network.Interface{{Name: "wan0", Addresses: []string{"fe80::1/64"}, AddressOptions: map[string]network.AddressOptions{"fe80::1/64": {NoDAD: true}}}, {Name: "lan0", Addresses: []string{"198.18.0.1/24"}}},
		Forwarding: network.Forwarding{IPv4: &yes}, Firewall: &network.Firewall{Forward: []network.Forward{{From: "lan0", To: "dslite0"}}, Masquerade: []network.Masquerade{{Source: "198.18.0.0/24", OutInterface: "dslite0"}}},
	}, Services: []serviceconfig.Service{
		{Name: "wan-ra", RAClient: &serviceconfig.RAClient{Interface: "wan0", SourceAddress: "fe80::1", InterfaceID: "::1234", AllowedRouters: []string{"fe80::2"}}},
		{Name: "dslite", DSLite: &serviceconfig.DSLite{Interface: "wan0", Tunnel: "dslite0", SourceFrom: "wan-ra", DNSFrom: []string{"wan-ra"}, Candidates: []string{"aftr.test"}, TimeoutSeconds: 1, RetrySeconds: 1, MTU: 1460, B4Address: "169.254.42.2/32", B4Peer: "169.254.42.1/32", RouteGateway: "169.254.42.1", Metric: 200}},
	}}
	f := startFixtureDaemon(t, config.FromNative(doc))
	module := func(name string) supervisor.ModuleStatus {
		t.Helper()
		var s supervisor.Status
		if err := f.supervisor.Do(context.Background(), "GET", "/v1/status", nil, &s); err != nil {
			t.Fatal(err)
		}
		for _, m := range s.Modules {
			if m.Name == name {
				return m
			}
		}
		return supervisor.ModuleStatus{}
	}
	status := func(name string) runtimeplan.InstanceStatus {
		t.Helper()
		var s runtimeplan.InstanceStatus
		if err := f.control.Do(context.Background(), "GET", "/v1/services/"+name, nil, &s); err != nil {
			t.Fatal(err)
		}
		return s
	}
	raState := func() raclient.Snapshot {
		t.Helper()
		s := status("service.wan-ra")
		var v raclient.Snapshot
		if err := json.Unmarshal(s.Snapshot, &v); err != nil {
			t.Fatal(err)
		}
		return v
	}
	aftrState := func() aftr.Snapshot {
		t.Helper()
		s := status("service.dslite")
		var v aftr.Snapshot
		if err := json.Unmarshal(s.Snapshot, &v); err != nil {
			t.Fatal(err)
		}
		return v
	}
	endpoint := func(source string) bool {
		t.Helper()
		l, err := netlink.LinkByName("dslite0")
		if err != nil {
			return false
		}
		v, ok := l.(*netlink.Ip6tnl)
		return ok && v.Local.String() == source && v.Remote.String() == "2001:db8:42::2" && v.Attrs().Flags&net.FlagUp != 0 && fixtureRoute(t, "0.0.0.0/0", "169.254.42.1", l.Attrs().Index)
	}
	traffic := func() {
		t.Helper()
		if err := fixtureInsideNamespace(client, func() error {
			for _, protocol := range []string{"tcp4", "udp4"} {
				port := "18093"
				if protocol == "udp4" {
					port = "18094"
				}
				c, err := net.DialTimeout(protocol, net.JoinHostPort("198.51.100.20", port), time.Second)
				if err != nil {
					return err
				}
				_ = c.SetDeadline(time.Now().Add(time.Second))
				if protocol == "udp4" {
					_, err = c.Write([]byte("LAN"))
				}
				var reply [1]byte
				if err == nil {
					_, err = io.ReadFull(c, reply[:])
				}
				c.Close()
				if err != nil {
					return err
				}
				if reply[0] != 'S' {
					return errors.New("wrong AFTR backend")
				}
			}
			return nil
		}); err != nil {
			t.Fatal("LAN -> learned RA/DNS -> AFTR TCP/UDP", err)
		}
	}
	send := func(prefix string, preferred, dnsLife uint32, withdrawOld bool) {
		t.Helper()
		packet := make([]byte, 16)
		packet[0], packet[4] = 134, 64
		binary.BigEndian.PutUint16(packet[6:], 60)
		pio := func(prefix string, valid, preferred uint32) {
			v := make([]byte, 32)
			v[0], v[1], v[2], v[3] = 3, 4, 64, 0xc0
			binary.BigEndian.PutUint32(v[4:], valid)
			binary.BigEndian.PutUint32(v[8:], preferred)
			p := netip.MustParseAddr(prefix).As16()
			copy(v[16:], p[:])
			packet = append(packet, v...)
		}
		if withdrawOld {
			pio("2001:db8:42::", 0, 0)
		}
		pio(prefix, 60, preferred)
		lla := []byte{1, 1, 0, 0, 0, 0, 0, 0}
		copy(lla[2:], peer.Attrs().HardwareAddr)
		packet = append(packet, lla...)
		dns := make([]byte, 24)
		dns[0], dns[1] = 25, 3
		binary.BigEndian.PutUint32(dns[4:], dnsLife)
		a := netip.MustParseAddr("2001:db8:42::53").As16()
		copy(dns[8:], a[:])
		packet = append(packet, dns...)
		if err := unix.Sendto(raFD, packet, 0, &unix.SockaddrInet6{Addr: netip.MustParseAddr("ff02::1").As16(), ZoneId: uint32(peer.Attrs().Index)}); err != nil {
			t.Fatal(err)
		}
	}
	if endpoint("2001:db8:42::1234") {
		t.Fatal("tunnel before RA authority")
	}
	mu.Lock()
	before := len(queries)
	mu.Unlock()
	if before != 0 {
		t.Fatal("DNS without learned source/resolver")
	}
	send("2001:db8:42::", 50, 4, false)
	until(t, 8*time.Second, func() bool { return endpoint("2001:db8:42::1234") })
	traffic()
	source := raState()
	resolved := aftrState()
	if len(source.DNSServers) != 1 || resolved.ValidUntil != source.DNSServers[0].ValidUntil {
		t.Fatal("AFTR lost original RDNSS deadline", source, resolved)
	}
	// Keep the owner pause shorter than the supervisor heartbeat timeout.
	// We are testing the writer's independent lease, not process-tree recovery.
	var stopped []int
	resume := func() {
		for i := len(stopped) - 1; i >= 0; i-- {
			_ = syscall.Kill(stopped[i], syscall.SIGCONT)
		}
		stopped = nil
	}
	t.Cleanup(resume)
	freeze := func() {
		t.Helper()
		for _, pid := range []int{f.pid, module("service.wan-ra").PID, module("control").PID} {
			if pid < 2 {
				t.Fatal("missing owner")
			}
			if err := syscall.Kill(pid, syscall.SIGSTOP); err != nil {
				t.Fatal(err)
			}
			stopped = append(stopped, pid)
		}
	}
	fenced := func(deadline lifetime.Deadline) {
		t.Helper()
		until(t, 13*time.Second, func() bool {
			l, err := netlink.LinkByName("dslite0")
			if err != nil {
				t.Fatal(err)
			}
			clock, err := boottime.Read()
			if err != nil {
				t.Fatal(err)
			}
			if deadline.Live(clock) {
				if l.Attrs().Flags&net.FlagUp == 0 {
					t.Fatal("early AFTR fencing")
				}
				return false
			}
			addresses, err := netlink.AddrList(l, netlink.FAMILY_V4)
			if err != nil {
				t.Fatal(err)
			}
			return l.Attrs().Flags&net.FlagUp == 0 && len(addresses) == 0 && !fixtureRoute(t, "0.0.0.0/0", "169.254.42.1", 0)
		})
	}
	original := status("service.wan-ra")
	freeze()
	fenced(resolved.ValidUntil)
	resume()
	until(t, 5*time.Second, func() bool {
		_, err := netlink.LinkByName("dslite0")
		var missing netlink.LinkNotFoundError
		return errors.As(err, &missing)
	})
	replay := healthPBRUpdate(original)
	if err := f.control.Do(context.Background(), "POST", "/v1/services/state", replay, nil); err != nil && !localapi.IsHTTPStatus(err, 409) {
		t.Fatal(err)
	}
	if endpoint("2001:db8:42::1234") {
		t.Fatal("RDNSS replay renewed resolver authority")
	}
	t.Log("learned RA source/RDNSS -> DNS -> AFTR -> forwarded LAN TCP/UDP; original RDNSS expiry fenced tunnel with control stopped")
	send("2001:db8:42::", 4, 30, false)
	until(t, 6*time.Second, func() bool { return endpoint("2001:db8:42::1234") })
	traffic()
	source = raState()
	resolved = aftrState()
	if resolved.ValidUntil != source.Addresses[0].PreferredUntil {
		t.Fatal("AFTR lost preferred source deadline", source, resolved)
	}
	freeze()
	fenced(resolved.ValidUntil)
	resume()
	until(t, 5*time.Second, func() bool {
		_, err := netlink.LinkByName("dslite0")
		var missing netlink.LinkNotFoundError
		return errors.As(err, &missing)
	})
	t.Log("source preference expired before address validity and DNS TTL; original source budget fenced DS-Lite")
	tunnel.Remote = net.ParseIP("2001:db8:43::1234")
	if err := h.LinkModify(tunnel); err != nil {
		t.Fatal(err)
	}
	send("2001:db8:43::", 50, 30, true)
	until(t, 8*time.Second, func() bool { return endpoint("2001:db8:43::1234") })
	traffic()
	previous := module("service.wan-ra")
	if err := syscall.Kill(previous.PID, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	until(t, 6*time.Second, func() bool {
		next := module("service.wan-ra")
		return next.Ready && next.PID > 1 && next.PID != previous.PID
	})
	until(t, 6*time.Second, func() bool { return endpoint("2001:db8:43::1234") })
	traffic()
	writer := module("network")
	if err := syscall.Kill(writer.PID, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	until(t, 6*time.Second, func() bool { next := module("network"); return next.Ready && next.PID > 1 && next.PID != writer.PID })
	until(t, 6*time.Second, func() bool { return endpoint("2001:db8:43::1234") })
	traffic()
	mu.Lock()
	oldQueries, newQueries := queries["2001:db8:42::1234"], queries["2001:db8:43::1234"]
	mu.Unlock()
	if oldQueries == 0 || newQueries == 0 {
		t.Fatal("DNS was not sent from both learned sources", oldQueries, newQueries)
	}
	until(t, time.Second, func() bool {
		wanCapture.mu.Lock()
		defer wanCapture.mu.Unlock()
		var tcp, udp bool
		for _, p := range wanCapture.packets {
			b := p.data
			if len(b) < 74 || binary.BigEndian.Uint16(b[12:14]) != unix.ETH_P_IPV6 || b[20] != unix.IPPROTO_IPIP || b[54]>>4 != 4 {
				continue
			}
			if !net.IP(b[66:70]).Equal(net.ParseIP("169.254.42.2")) || !net.IP(b[70:74]).Equal(net.ParseIP("198.51.100.20")) {
				continue
			}
			tcp = tcp || b[63] == unix.IPPROTO_TCP
			udp = udp || b[63] == unix.IPPROTO_UDP
		}
		return tcp && udp
	})
	if lanCapture.count("198.18.0.2", "198.51.100.20", unix.IPPROTO_TCP, 18093) == 0 || lanCapture.count("198.18.0.2", "198.51.100.20", unix.IPPROTO_UDP, 18094) == 0 {
		t.Fatal("LAN application capture missing")
	}
	t.Log("fresh renumbered RA recovered source-bound DNS and LAN TCP/UDP; RA/network worker replacements preserved communication; LAN/WAN captures show real NAT and IPv4-in-IPv6")
}
